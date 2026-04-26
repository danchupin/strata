package s3api_test

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var ssempUploadIDRE = regexp.MustCompile(`<UploadId>([^<]+)</UploadId>`)

func TestMultipartSSE3PartRoundTrip(t *testing.T) {
	h := newSSEHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/mp?uploads=", "",
		"x-amz-server-side-encryption", "AES256")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("Initiate sse echo: got %q", got)
	}
	m := ssempUploadIDRE.FindStringSubmatch(h.readBody(resp))
	if len(m) != 2 {
		t.Fatalf("no UploadId")
	}
	uploadID := m[1]

	r := rand.New(rand.NewSource(0xFEED))
	parts := [][]byte{
		makeBytes(r, 5*1024*1024),
		makeBytes(r, 5*1024*1024),
		makeBytes(r, 1024*1024),
	}
	var orig []byte
	var cb strings.Builder
	cb.WriteString("<CompleteMultipartUpload>")
	for i, p := range parts {
		pnum := i + 1
		url := fmt.Sprintf("/bkt/mp?uploadId=%s&partNumber=%d", uploadID, pnum)
		resp := h.do("PUT", url, bytes.NewReader(p))
		h.mustStatus(resp, 200)
		etag := strings.Trim(resp.Header.Get("ETag"), `"`)
		wantPartETag := hex.EncodeToString(md5sum(p))
		if etag != wantPartETag {
			t.Fatalf("part %d ETag: got %q want %q (plaintext MD5)", pnum, etag, wantPartETag)
		}
		if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
			t.Fatalf("part %d sse echo: got %q", pnum, got)
		}
		cb.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pnum, etag))
		orig = append(orig, p...)
	}
	cb.WriteString("</CompleteMultipartUpload>")

	resp = h.doString("POST", "/bkt/mp?uploadId="+uploadID, cb.String())
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("Complete sse echo: got %q", got)
	}

	resp = h.do("GET", "/bkt/mp", nil)
	h.mustStatus(resp, http.StatusOK)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("GET sse echo: got %q", got)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = resp.Body.Close()
	if !bytes.Equal(got, orig) {
		t.Fatalf("body mismatch: got %d bytes, want %d, first-diff %d", len(got), len(orig), firstDiff(got, orig))
	}

	resp = h.do("HEAD", "/bkt/mp", nil)
	h.mustStatus(resp, http.StatusOK)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("HEAD sse echo: got %q", got)
	}
}

func TestMultipartPlaintextStillWorks(t *testing.T) {
	h := newSSEHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/pt?uploads=", "")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "" {
		t.Fatalf("Initiate sse should be absent: got %q", got)
	}
	m := ssempUploadIDRE.FindStringSubmatch(h.readBody(resp))
	if len(m) != 2 {
		t.Fatalf("no UploadId")
	}
	uploadID := m[1]

	r := rand.New(rand.NewSource(0xCAFE))
	parts := [][]byte{makeBytes(r, 5*1024*1024), makeBytes(r, 1024)}
	var orig []byte
	var cb strings.Builder
	cb.WriteString("<CompleteMultipartUpload>")
	for i, p := range parts {
		pnum := i + 1
		resp := h.do("PUT", fmt.Sprintf("/bkt/pt?uploadId=%s&partNumber=%d", uploadID, pnum), bytes.NewReader(p))
		h.mustStatus(resp, 200)
		if got := resp.Header.Get("x-amz-server-side-encryption"); got != "" {
			t.Fatalf("part %d sse should be absent: got %q", pnum, got)
		}
		etag := strings.Trim(resp.Header.Get("ETag"), `"`)
		cb.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pnum, etag))
		orig = append(orig, p...)
	}
	cb.WriteString("</CompleteMultipartUpload>")

	resp = h.doString("POST", "/bkt/pt?uploadId="+uploadID, cb.String())
	h.mustStatus(resp, 200)

	resp = h.do("GET", "/bkt/pt", nil)
	h.mustStatus(resp, http.StatusOK)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "" {
		t.Fatalf("GET sse should be absent: got %q", got)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Equal(got, orig) {
		t.Fatalf("body mismatch: got %d, want %d, first-diff %d", len(got), len(orig), firstDiff(got, orig))
	}
}

func makeBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	r.Read(b)
	return b
}

var _ = md5.Sum
