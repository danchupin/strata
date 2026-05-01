package s3api_test

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestMultipartCompleteSizeTooSmall mirrors s3-tests
// test_multipart_upload_size_too_small: a non-last part smaller than 5 MiB
// rejects the Complete with EntityTooSmall (HTTP 400).
func TestMultipartCompleteSizeTooSmall(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	// Two parts: first 1 MiB (non-last, below threshold), second 1 MiB.
	const small = 1 << 20
	var body strings.Builder
	body.WriteString("<CompleteMultipartUpload>")
	for pnum := 1; pnum <= 2; pnum++ {
		buf := make([]byte, small)
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		url := fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=%d", uploadID, pnum)
		r := h.do("PUT", url, byteReader(buf))
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		body.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pnum, etag))
	}
	body.WriteString("</CompleteMultipartUpload>")

	r := h.doString("POST", "/bkt/k?uploadId="+uploadID, body.String())
	h.mustStatus(r, http.StatusBadRequest)
	if got := h.readBody(r); !strings.Contains(got, "EntityTooSmall") {
		t.Errorf("expected EntityTooSmall in body, got: %s", got)
	}

	// Multipart upload still listed (LWT not flipped).
	listing := h.doString("GET", "/bkt?uploads", "")
	h.mustStatus(listing, http.StatusOK)
	if !strings.Contains(h.readBody(listing), uploadID) {
		t.Errorf("upload should remain in 'uploading' state after EntityTooSmall reject")
	}
}

// TestMultipartCompleteSingleSmallPart pins that a single-part multipart
// upload accepts a part below 5 MiB (the last part has no minimum).
func TestMultipartCompleteSingleSmallPart(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	buf := []byte("hello")
	url := fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=1", uploadID)
	r := h.do("PUT", url, byteReader(buf))
	h.mustStatus(r, 200)
	etag := strings.Trim(r.Header.Get("Etag"), `"`)

	body := fmt.Sprintf(
		`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`,
		etag,
	)
	cr := h.doString("POST", "/bkt/k?uploadId="+uploadID, body)
	h.mustStatus(cr, http.StatusOK)
}

// TestMultipartResendOverwrites mirrors s3-tests test_multipart_upload_resend_part:
// a second UploadPart for the same part number replaces the first (later
// write wins). Complete with the resent ETag succeeds; the body served on
// GET matches the resent bytes.
func TestMultipartResendOverwrites(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	const partSize = 5 << 20
	first := make([]byte, partSize)
	for i := range first {
		first[i] = 'A'
	}
	second := make([]byte, partSize)
	for i := range second {
		second[i] = 'B'
	}
	last := []byte("tail")

	url1 := fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=1", uploadID)
	r1a := h.do("PUT", url1, byteReader(first))
	h.mustStatus(r1a, 200)
	etag1a := strings.Trim(r1a.Header.Get("Etag"), `"`)
	r1b := h.do("PUT", url1, byteReader(second))
	h.mustStatus(r1b, 200)
	etag1b := strings.Trim(r1b.Header.Get("Etag"), `"`)
	if etag1a == etag1b {
		t.Fatalf("resend should produce a different ETag (got %q twice)", etag1a)
	}

	url2 := fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=2", uploadID)
	r2 := h.do("PUT", url2, byteReader(last))
	h.mustStatus(r2, 200)
	etag2 := strings.Trim(r2.Header.Get("Etag"), `"`)

	body := fmt.Sprintf("<CompleteMultipartUpload>"+
		`<Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part>`+
		`<Part><PartNumber>2</PartNumber><ETag>"%s"</ETag></Part>`+
		"</CompleteMultipartUpload>", etag1b, etag2)
	cr := h.doString("POST", "/bkt/k?uploadId="+uploadID, body)
	h.mustStatus(cr, http.StatusOK)

	// GET full object: bytes for part 1 must be the RESENT 'B's, not 'A's.
	g := h.doString("GET", "/bkt/k", "")
	h.mustStatus(g, http.StatusOK)
	got, err := io.ReadAll(g.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != partSize+len(last) {
		t.Fatalf("size: got %d want %d", len(got), partSize+len(last))
	}
	for i := 0; i < partSize; i++ {
		if got[i] != 'B' {
			t.Fatalf("part 1 at %d: got %q want 'B' (resend should overwrite original)", i, got[i])
		}
	}
}

// TestMultipartCompleteRejectsStaleETag pins that supplying the FIRST
// upload's ETag in CompleteMultipartUpload after a resend has overwritten
// it returns InvalidPart — the stored ETag is the second write.
func TestMultipartCompleteRejectsStaleETag(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	const partSize = 5 << 20
	first := make([]byte, partSize)
	for i := range first {
		first[i] = 'A'
	}
	second := make([]byte, partSize)
	for i := range second {
		second[i] = 'B'
	}

	url1 := fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=1", uploadID)
	r1a := h.do("PUT", url1, byteReader(first))
	h.mustStatus(r1a, 200)
	etag1a := strings.Trim(r1a.Header.Get("Etag"), `"`)
	r1b := h.do("PUT", url1, byteReader(second))
	h.mustStatus(r1b, 200)

	// Last part — small is fine.
	url2 := fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=2", uploadID)
	r2 := h.do("PUT", url2, byteReader([]byte("z")))
	h.mustStatus(r2, 200)
	etag2 := strings.Trim(r2.Header.Get("Etag"), `"`)

	// Use the STALE first ETag — must reject.
	body := fmt.Sprintf("<CompleteMultipartUpload>"+
		`<Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part>`+
		`<Part><PartNumber>2</PartNumber><ETag>"%s"</ETag></Part>`+
		"</CompleteMultipartUpload>", etag1a, etag2)
	cr := h.doString("POST", "/bkt/k?uploadId="+uploadID, body)
	h.mustStatus(cr, http.StatusBadRequest)
	if got := h.readBody(cr); !strings.Contains(got, "InvalidPart") {
		t.Errorf("expected InvalidPart, got: %s", got)
	}
}
