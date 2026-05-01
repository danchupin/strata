package s3api_test

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var uploadIDRE = regexp.MustCompile(`<UploadId>([^<]+)</UploadId>`)

func TestMultipartFullLifecycle(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/mp?uploads", "")
	h.mustStatus(resp, 200)
	initBody := h.readBody(resp)
	m := uploadIDRE.FindStringSubmatch(initBody)
	if len(m) != 2 {
		t.Fatalf("no UploadId in %s", initBody)
	}
	uploadID := m[1]

	// Non-last parts must be ≥ 5 MiB (S3 spec, US-009 size-too-small);
	// last part can be smaller.
	parts := make([][]byte, 3)
	for i := range parts {
		size := 5 << 20
		if i == len(parts)-1 {
			size = 4 << 20
		}
		parts[i] = make([]byte, size)
		if _, err := rand.Read(parts[i]); err != nil {
			t.Fatal(err)
		}
	}
	var orig []byte
	var completeBody strings.Builder
	completeBody.WriteString("<CompleteMultipartUpload>")
	for i, p := range parts {
		pnum := i + 1
		url := fmt.Sprintf("/bkt/mp?uploadId=%s&partNumber=%d", uploadID, pnum)
		resp := h.do("PUT", url, byteReader(p))
		h.mustStatus(resp, 200)
		etag := strings.Trim(resp.Header.Get("Etag"), `"`)
		completeBody.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pnum, etag))
		orig = append(orig, p...)
	}
	completeBody.WriteString("</CompleteMultipartUpload>")

	resp = h.doString("POST", "/bkt/mp?uploadId="+uploadID, completeBody.String())
	h.mustStatus(resp, 200)
	complete := h.readBody(resp)
	if !regexp.MustCompile(`-3&#34;</ETag>`).MatchString(complete) {
		t.Errorf("expected composite etag with -3 suffix: %s", complete)
	}

	resp = h.doString("GET", "/bkt/mp", "")
	h.mustStatus(resp, http.StatusOK)
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(orig) {
		t.Fatalf("size mismatch: got %d want %d", len(got), len(orig))
	}
	for i := range orig {
		if orig[i] != got[i] {
			t.Fatalf("body mismatch at offset %d", i)
		}
	}
}

func TestMultipartAbort(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	h.mustStatus(h.do("PUT", "/bkt/k?uploadId="+uploadID+"&partNumber=1", byteReader([]byte("x"))), 200)

	h.mustStatus(h.doString("DELETE", "/bkt/k?uploadId="+uploadID, ""), 204)

	resp = h.doString("GET", "/bkt?uploads", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if strings.Contains(body, "<Upload>") {
		t.Errorf("abort left upload in listing: %s", body)
	}
}

func byteReader(b []byte) io.Reader {
	return &byteSliceReader{b: b}
}

type byteSliceReader struct {
	b []byte
	n int
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.n >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.n:])
	r.n += n
	return n, nil
}
