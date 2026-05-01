package auth

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
)

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

type streamingReader struct {
	src  io.ReadCloser
	br   *bufio.Reader
	rem  int64
	done bool
}

func newStreamingReader(body io.ReadCloser) io.ReadCloser {
	return &streamingReader{src: body, br: bufio.NewReader(body)}
}

func (s *streamingReader) Read(p []byte) (int, error) {
	if s.done {
		return 0, io.EOF
	}
	if s.rem == 0 {
		line, err := s.br.ReadString('\n')
		if err != nil {
			return 0, err
		}
		line = strings.TrimRight(line, "\r\n")
		sizePart := line
		if idx := strings.Index(line, ";"); idx >= 0 {
			sizePart = line[:idx]
		}
		size, err := strconv.ParseInt(sizePart, 16, 64)
		if err != nil {
			return 0, fmt.Errorf("aws-chunked: bad size header %q", line)
		}
		if size == 0 {
			s.done = true
			return 0, io.EOF
		}
		s.rem = size
	}
	toRead := int64(len(p))
	if toRead > s.rem {
		toRead = s.rem
	}
	n, err := s.br.Read(p[:toRead])
	s.rem -= int64(n)
	if s.rem == 0 && err == nil {
		if _, terr := s.br.ReadString('\n'); terr != nil && terr != io.EOF {
			return n, terr
		}
	}
	return n, err
}

func (s *streamingReader) Close() error {
	return s.src.Close()
}
