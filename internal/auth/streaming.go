// Package auth — aws-chunked SigV4 streaming body decoder.
//
// Standard mode (STREAMING-AWS4-HMAC-SHA256-PAYLOAD):
//
//	<size-hex>;chunk-signature=<sig>\r\n<chunk-data>\r\n
//	...
//	0;chunk-signature=<final-sig>\r\n
//
// Trailer mode (STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER, US-009 + US-004 of
// ralph/auth-dx-trailer-lima): same as standard mode but the final 0-chunk is
// followed by trailer headers. Accepted trailing checksum algorithms:
// x-amz-checksum-{sha256, sha1, crc32, crc32c}. Unknown algos are rejected at
// middleware time with ErrUnsupportedChecksumAlgorithm. The trailer-signature
// stringToSign:
//
//	"AWS4-HMAC-SHA256-TRAILER" + "\n" +
//	<reqDate> + "\n" +
//	<scope> + "\n" +
//	<final-chunk-sig> + "\n" +
//	hex(SHA256(<canonical-trailer-headers>))
//
// canonical-trailer-headers is the concatenation of `<lower-name>:<trim-value>\n`
// for every non-signature trailer header, sorted by name. The per-algorithm
// body checksum is accumulated as the chunked body streams; on EOF it is
// base64-compared to the trailer's checksum value — mismatch surfaces as
// ErrSignatureInvalid (AWS parity — sig-invalid, not checksum-mismatch).
package auth

import (
	"bufio"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"sort"
	"strconv"
	"strings"
)

const (
	chunkPayloadAlgorithm       = "AWS4-HMAC-SHA256-PAYLOAD"
	chunkTrailerAlgorithm       = "AWS4-HMAC-SHA256-TRAILER"
	trailerHeaderSig            = "x-amz-trailer-signature"
	trailerHeaderChecksumSha256 = "x-amz-checksum-sha256"
	trailerHeaderChecksumSha1   = "x-amz-checksum-sha1"
	trailerHeaderChecksumCRC32  = "x-amz-checksum-crc32"
	trailerHeaderChecksumCRC32C = "x-amz-checksum-crc32c"
)

// crc32CastagnoliTable backs hash/crc32 in Castagnoli mode (the crc32c
// polynomial used by AWS S3 trailer checksums).
var crc32CastagnoliTable = crc32.MakeTable(crc32.Castagnoli)

// trailerHashSpec captures the per-algorithm decoder shape: the canonical
// trailer-header name to look up in the trailer block and the hash.Hash
// constructor used to accumulate the body checksum as the chunked body
// streams.
type trailerHashSpec struct {
	header string
	newH   func() hash.Hash
}

// selectTrailerHash maps the lower-cased X-Amz-Trailer header value to a
// trailerHashSpec. Returns ErrUnsupportedChecksumAlgorithm for any name
// outside the supported set so the caller can surface HTTP 400 InvalidRequest
// before the body is drained.
func selectTrailerHash(algoHeader string) (*trailerHashSpec, error) {
	switch strings.ToLower(strings.TrimSpace(algoHeader)) {
	case trailerHeaderChecksumSha256:
		return &trailerHashSpec{header: trailerHeaderChecksumSha256, newH: sha256.New}, nil
	case trailerHeaderChecksumSha1:
		return &trailerHashSpec{header: trailerHeaderChecksumSha1, newH: sha1.New}, nil
	case trailerHeaderChecksumCRC32:
		return &trailerHashSpec{header: trailerHeaderChecksumCRC32, newH: func() hash.Hash { return crc32.NewIEEE() }}, nil
	case trailerHeaderChecksumCRC32C:
		return &trailerHashSpec{header: trailerHeaderChecksumCRC32C, newH: func() hash.Hash { return crc32.New(crc32CastagnoliTable) }}, nil
	default:
		return nil, ErrUnsupportedChecksumAlgorithm
	}
}

type streamingReader struct {
	src        io.ReadCloser
	br         *bufio.Reader
	signingKey []byte
	reqDate    string
	scope      string
	prevSig    string
	chunk      []byte
	pos        int
	done       bool

	// Trailer-mode state (zero values disable trailer handling — standard
	// aws-chunked body keeps its existing semantics). trailerHeader is the
	// canonical `x-amz-checksum-<algo>` header name to look up in the
	// trailer block (set by selectTrailerHash); bodyHash accumulates the
	// per-algorithm checksum over plaintext chunks.
	trailerMode   bool
	bodyHash      hash.Hash
	trailerHeader string
}

func newStreamingReader(body io.ReadCloser, signingKey []byte, reqDate, scope, seedSig string) io.ReadCloser {
	return &streamingReader{
		src:        body,
		br:         bufio.NewReader(body),
		signingKey: signingKey,
		reqDate:    reqDate,
		scope:      scope,
		prevSig:    seedSig,
	}
}

// newStreamingTrailerReader wraps the body for STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER
// requests. Behaves identically to newStreamingReader for the chunk-signature
// chain, then validates the trailer headers + trailer signature + body
// checksum on EOF.
func newStreamingTrailerReader(body io.ReadCloser, signingKey []byte, reqDate, scope, seedSig string, spec *trailerHashSpec) io.ReadCloser {
	return &streamingReader{
		src:           body,
		br:            bufio.NewReader(body),
		signingKey:    signingKey,
		reqDate:       reqDate,
		scope:         scope,
		prevSig:       seedSig,
		trailerMode:   true,
		bodyHash:      spec.newH(),
		trailerHeader: spec.header,
	}
}

func (s *streamingReader) Read(p []byte) (int, error) {
	if s.done && s.pos >= len(s.chunk) {
		return 0, io.EOF
	}
	if s.pos >= len(s.chunk) {
		if err := s.readChunk(); err != nil {
			return 0, err
		}
		if s.done && len(s.chunk) == 0 {
			return 0, io.EOF
		}
	}
	n := copy(p, s.chunk[s.pos:])
	s.pos += n
	return n, nil
}

func (s *streamingReader) Close() error {
	return s.src.Close()
}

func (s *streamingReader) readChunk() error {
	s.chunk = nil
	s.pos = 0

	line, err := s.br.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			return ErrSignatureInvalid
		}
		return err
	}
	line = strings.TrimRight(line, "\r\n")

	sizePart := line
	chunkSig := ""
	if idx := strings.Index(line, ";"); idx >= 0 {
		sizePart = line[:idx]
		for _, kv := range strings.Split(line[idx+1:], ";") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			if strings.TrimSpace(k) == "chunk-signature" {
				chunkSig = strings.TrimSpace(v)
			}
		}
	}
	size, err := strconv.ParseInt(sizePart, 16, 64)
	if err != nil {
		return fmt.Errorf("aws-chunked: bad size header %q", line)
	}
	if chunkSig == "" {
		return ErrSignatureInvalid
	}

	var data []byte
	if size > 0 {
		data = make([]byte, size)
		if _, err := io.ReadFull(s.br, data); err != nil {
			return err
		}
		if _, err := s.br.ReadString('\n'); err != nil && err != io.EOF {
			return err
		}
	}

	expected := computeChunkSignature(s.signingKey, s.reqDate, s.scope, s.prevSig, data)
	if !constantTimeEqual(expected, chunkSig) {
		return ErrSignatureInvalid
	}
	s.prevSig = chunkSig

	if size == 0 {
		if s.trailerMode {
			if err := s.readAndValidateTrailers(); err != nil {
				return err
			}
		}
		s.done = true
		return nil
	}
	if s.trailerMode {
		// Accumulate body sha256 over the plaintext chunk so the trailer
		// validation can compare against the trailer's checksum header.
		s.bodyHash.Write(data)
	}
	s.chunk = data
	return nil
}

// readAndValidateTrailers consumes the trailer header block following the
// final 0-chunk and verifies (a) the trailer signature chained from prevSig
// (= final-chunk signature) over the canonical non-signature trailer headers,
// and (b) the per-algorithm body checksum matches the value carried in the
// trailer header chosen at request time (sha256 / sha1 / crc32 / crc32c).
// Mismatches surface as ErrSignatureInvalid per AWS parity.
func (s *streamingReader) readAndValidateTrailers() error {
	var (
		trailerSig  string
		checksumB64 string
		nonSigLines []string
		sawChecksum bool
	)
	for {
		line, err := s.br.ReadString('\n')
		if err != nil {
			if err == io.EOF && line == "" {
				return ErrSignatureInvalid
			}
			if err != io.EOF {
				return err
			}
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return ErrSignatureInvalid
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		switch name {
		case trailerHeaderSig:
			trailerSig = value
		case s.trailerHeader:
			checksumB64 = value
			sawChecksum = true
			nonSigLines = append(nonSigLines, name+":"+value+"\n")
		default:
			// Per AWS spec the trailer signature is computed over every
			// non-signature header carried in the trailer block. We sort
			// lexicographically below to match the canonical form.
			nonSigLines = append(nonSigLines, name+":"+value+"\n")
		}
	}
	if trailerSig == "" || !sawChecksum {
		return ErrSignatureInvalid
	}
	sort.Strings(nonSigLines)
	canonical := strings.Join(nonSigLines, "")
	expected := computeTrailerSignature(s.signingKey, s.reqDate, s.scope, s.prevSig, canonical)
	if !constantTimeEqual(expected, trailerSig) {
		return ErrSignatureInvalid
	}
	gotSum := s.bodyHash.Sum(nil)
	gotB64 := base64.StdEncoding.EncodeToString(gotSum)
	if !constantTimeEqual(gotB64, checksumB64) {
		return ErrSignatureInvalid
	}
	s.prevSig = trailerSig
	return nil
}

func computeChunkSignature(signingKey []byte, reqDate, scope, prevSig string, data []byte) string {
	sts := chunkPayloadAlgorithm + "\n" +
		reqDate + "\n" +
		scope + "\n" +
		prevSig + "\n" +
		emptyBodyHash + "\n" +
		sha256Hex(data)
	return hex.EncodeToString(hmacSHA256(signingKey, []byte(sts)))
}

// computeTrailerSignature builds the AWS4-HMAC-SHA256-TRAILER stringToSign
// chained from the final-chunk signature over the canonical non-signature
// trailer-header block. canonicalTrailers is the lower-name:trim-value\n
// concatenation, sorted by name.
func computeTrailerSignature(signingKey []byte, reqDate, scope, prevSig, canonicalTrailers string) string {
	sts := chunkTrailerAlgorithm + "\n" +
		reqDate + "\n" +
		scope + "\n" +
		prevSig + "\n" +
		sha256Hex([]byte(canonicalTrailers))
	return hex.EncodeToString(hmacSHA256(signingKey, []byte(sts)))
}

func IsChunkSignatureError(err error) bool {
	return errors.Is(err, ErrSignatureInvalid)
}
