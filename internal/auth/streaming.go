package auth

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

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
