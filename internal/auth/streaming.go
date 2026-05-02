package auth

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const chunkPayloadAlgorithm = "AWS4-HMAC-SHA256-PAYLOAD"

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
		s.done = true
		return nil
	}
	s.chunk = data
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

func IsChunkSignatureError(err error) bool {
	return errors.Is(err, ErrSignatureInvalid)
}
