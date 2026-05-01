package auth

import (
	"bufio"
	"crypto/hmac"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrChunkSignatureMismatch is returned by the streaming reader's Read when
// the per-chunk SigV4 chain signature supplied by the client does not match
// the value computed from the buffered chunk payload. The middleware (and
// any handler reading r.Body for streaming uploads) translates this to
// 403 SignatureDoesNotMatch — and because the buffer-then-validate ordering
// guarantees no payload byte has reached the consumer yet, mutated bytes
// never touch the storage backend.
var ErrChunkSignatureMismatch = errors.New("aws-chunked: chunk signature mismatch")

// IsChunkSignatureMismatch reports whether err (or any error it wraps) is
// ErrChunkSignatureMismatch. Handlers that read r.Body during streaming
// uploads use this to map body-stream errors to 403 SignatureDoesNotMatch.
func IsChunkSignatureMismatch(err error) bool {
	return errors.Is(err, ErrChunkSignatureMismatch)
}

// maxChunkPayload caps the per-chunk buffer to defend against malformed
// framing claiming an absurd size. Typical SDK output uses 8 KiB chunks;
// AWS itself caps at 16 MiB. This is the streaming-shape rule's "no
// full-body buffer" guarantee — peak buffer is one chunk.
const maxChunkPayload = 16 * 1024 * 1024

// chunkSigner produces the chained per-chunk SigV4 signatures for
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD bodies. The chain is seeded with the
// outer SigV4 signature (from the request's Authorization header) and
// advances by one HMAC-SHA256 per chunk. Each Next call returns the
// expected hex signature for the next chunk and updates the internal
// previous-signature state.
//
// PRD listed Construction(seedSig, signingKey, scope); the AWS string-to-
// sign also embeds the X-Amz-Date timestamp, so the constructor takes a
// fourth isoDate argument (the X-Amz-Date timestamp, e.g.
// "20130524T000000Z"). scope is "<date>/<region>/<service>/aws4_request".
type chunkSigner struct {
	signingKey []byte
	isoDate    string
	scope      string
	prevSig    string
}

func newChunkSigner(seedSig string, signingKey []byte, isoDate, scope string) *chunkSigner {
	return &chunkSigner{
		signingKey: signingKey,
		isoDate:    isoDate,
		scope:      scope,
		prevSig:    seedSig,
	}
}

// Next returns the expected per-chunk signature for payload and advances
// the chain. AWS string-to-sign:
//
//	"AWS4-HMAC-SHA256-PAYLOAD\n" + isoDate + "\n" + scope + "\n" +
//	    prevSig + "\n" + hex(SHA256("")) + "\n" + hex(SHA256(payload))
func (c *chunkSigner) Next(payload []byte) string {
	sts := "AWS4-HMAC-SHA256-PAYLOAD\n" +
		c.isoDate + "\n" +
		c.scope + "\n" +
		c.prevSig + "\n" +
		emptyBodyHash + "\n" +
		sha256Hex(payload)
	sig := hex.EncodeToString(hmacSHA256(c.signingKey, []byte(sts)))
	c.prevSig = sig
	return sig
}

// streamingReader decodes aws-chunked bodies (the
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD framing) AND validates the chained
// per-chunk SigV4 signature. Each chunk is buffered in full before any
// byte is forwarded to the consumer — on signature mismatch the buffered
// bytes are dropped and ErrChunkSignatureMismatch is returned, so mutated
// payload never reaches the storage backend.
type streamingReader struct {
	src    io.ReadCloser
	br     *bufio.Reader
	signer *chunkSigner
	buf    []byte // current validated chunk payload, partially consumed
	pos    int
	done   bool
}

// newStreamingReader wraps body for the aws-chunked SigV4 streaming format.
// seedSig is the outer SigV4 Authorization-header signature; signingKey is
// the SigV4 derived key (deriveSigningKey output); isoDate is the
// X-Amz-Date value; scope is "<date>/<region>/<service>/aws4_request".
// Together they let the reader recompute and verify each chunk's chain
// signature.
func newStreamingReader(body io.ReadCloser, seedSig string, signingKey []byte, isoDate, scope string) io.ReadCloser {
	return &streamingReader{
		src:    body,
		br:     bufio.NewReader(body),
		signer: newChunkSigner(seedSig, signingKey, isoDate, scope),
	}
}

func (s *streamingReader) Read(p []byte) (int, error) {
	if s.pos < len(s.buf) {
		n := copy(p, s.buf[s.pos:])
		s.pos += n
		return n, nil
	}
	if s.done {
		return 0, io.EOF
	}
	if err := s.readNextChunk(); err != nil {
		return 0, err
	}
	if s.done && len(s.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.buf[s.pos:])
	s.pos += n
	return n, nil
}

// readNextChunk consumes one framed chunk from br, validates its
// chain signature, and stores the validated payload in s.buf. The empty
// trailer chunk (size == 0) is also chain-validated — a mutated trailer
// must not slip through.
func (s *streamingReader) readNextChunk() error {
	line, err := s.br.ReadString('\n')
	if err != nil {
		return err
	}
	line = strings.TrimRight(line, "\r\n")
	sizePart := line
	clientSig := ""
	if idx := strings.Index(line, ";"); idx >= 0 {
		sizePart = line[:idx]
		const prefix = "chunk-signature="
		if ext := line[idx+1:]; strings.HasPrefix(ext, prefix) {
			clientSig = ext[len(prefix):]
		}
	}
	size, err := strconv.ParseInt(sizePart, 16, 64)
	if err != nil {
		return fmt.Errorf("aws-chunked: bad size header %q", line)
	}
	if size < 0 {
		return fmt.Errorf("aws-chunked: negative chunk size %d", size)
	}
	if size > maxChunkPayload {
		return fmt.Errorf("aws-chunked: chunk size %d exceeds %d cap", size, maxChunkPayload)
	}
	payload := make([]byte, size)
	if size > 0 {
		if _, err := io.ReadFull(s.br, payload); err != nil {
			return err
		}
		// Trailing CRLF after non-empty payload.
		if _, err := s.br.ReadString('\n'); err != nil && err != io.EOF {
			return err
		}
	}
	expected := s.signer.Next(payload)
	if !hmac.Equal([]byte(expected), []byte(clientSig)) {
		return ErrChunkSignatureMismatch
	}
	if size == 0 {
		s.done = true
		s.buf = nil
		s.pos = 0
		return nil
	}
	s.buf = payload
	s.pos = 0
	return nil
}

func (s *streamingReader) Close() error {
	return s.src.Close()
}
