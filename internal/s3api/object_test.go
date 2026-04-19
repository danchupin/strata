package s3api_test

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestObjectPutGetDelete(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/b", ""), 200)

	h.mustStatus(h.doString("PUT", "/b/greet.txt", "hello"), 200)

	resp := h.doString("GET", "/b/greet.txt", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "hello" {
		t.Errorf("GET body: got %q want %q", body, "hello")
	}

	resp = h.doString("HEAD", "/b/greet.txt", "")
	h.mustStatus(resp, 200)
	if resp.Header.Get("Etag") == "" {
		t.Errorf("HEAD missing ETag header")
	}

	h.mustStatus(h.doString("DELETE", "/b/greet.txt", ""), 204)
	h.mustStatus(h.doString("GET", "/b/greet.txt", ""), 404)
}

func TestObjectLargeRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/b", ""), 200)

	src := make([]byte, 9<<20)
	if _, err := rand.Read(src); err != nil {
		t.Fatal(err)
	}

	h.mustStatus(h.do("PUT", "/b/big.bin", bytes.NewReader(src)), 200)

	resp := h.doString("GET", "/b/big.bin", "")
	h.mustStatus(resp, 200)
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(src, got) {
		t.Fatalf("body mismatch: want %d bytes, got %d", len(src), len(got))
	}
}

func TestObjectRangeGet(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/b", ""), 200)
	h.mustStatus(h.doString("PUT", "/b/k", "0123456789"), 200)

	cases := []struct {
		name       string
		hdr        string
		wantStatus int
		wantBody   string
		wantCR     string
	}{
		{"range-0-4", "bytes=0-4", http.StatusPartialContent, "01234", "bytes 0-4/10"},
		{"range-3-7", "bytes=3-7", http.StatusPartialContent, "34567", "bytes 3-7/10"},
		{"range-5-", "bytes=5-", http.StatusPartialContent, "56789", "bytes 5-9/10"},
		{"range-suffix-3", "bytes=-3", http.StatusPartialContent, "789", "bytes 7-9/10"},
		{"no-range", "", http.StatusOK, "0123456789", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := []string{}
			if tc.hdr != "" {
				headers = append(headers, "Range", tc.hdr)
			}
			resp := h.doString("GET", "/b/k", "", headers...)
			h.mustStatus(resp, tc.wantStatus)
			if cr := resp.Header.Get("Content-Range"); cr != tc.wantCR {
				t.Errorf("Content-Range: got %q want %q", cr, tc.wantCR)
			}
			if body := h.readBody(resp); body != tc.wantBody {
				t.Errorf("body: got %q want %q", body, tc.wantBody)
			}
		})
	}
}

func TestListObjectsPrefixDelimiter(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/b", ""), 200)

	for _, k := range []string{"a.txt", "logs/2026/01/a.log", "logs/2026/01/b.log", "logs/2026/02/c.log"} {
		h.mustStatus(h.doString("PUT", "/b/"+k, "x"), 200)
	}

	resp := h.doString("GET", "/b?list-type=2&prefix=logs/&delimiter=/", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<CommonPrefixes><Prefix>logs/2026/</Prefix></CommonPrefixes>") {
		t.Errorf("expected CommonPrefix logs/2026/, got: %s", body)
	}

	resp = h.doString("GET", "/b?list-type=2&prefix=logs/2026/01/", "")
	h.mustStatus(resp, 200)
	body = h.readBody(resp)
	if !strings.Contains(body, "<Key>logs/2026/01/a.log</Key>") ||
		!strings.Contains(body, "<Key>logs/2026/01/b.log</Key>") ||
		strings.Contains(body, "<Key>logs/2026/02/c.log</Key>") {
		t.Errorf("prefix filter broken: %s", body)
	}
}

func TestStorageClassHeader(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/b", ""), 200)
	h.mustStatus(h.doString("PUT", "/b/k", "x", "x-amz-storage-class", "STANDARD_IA"), 200)

	resp := h.doString("HEAD", "/b/k", "")
	h.mustStatus(resp, 200)
	if sc := resp.Header.Get("X-Amz-Storage-Class"); sc != "STANDARD_IA" {
		t.Errorf("storage-class: got %q want STANDARD_IA", sc)
	}
}
