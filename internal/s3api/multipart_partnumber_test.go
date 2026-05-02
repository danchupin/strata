package s3api_test

import (
	"crypto/rand"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
)

// TestMultipartGetByPartNumber rounds-trips a 3-part upload and fetches each
// part by ?partNumber=N, asserting Content-Length matches the part's plaintext
// size, Content-Range covers the right slice, x-amz-mp-parts-count echoes the
// total parts count, and the per-part body matches what was uploaded.
func TestMultipartGetByPartNumber(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/mpkey?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	parts := make([][]byte, 3)
	sizes := []int{6 << 20, 5 << 20, 1 << 10}
	for i, size := range sizes {
		parts[i] = make([]byte, size)
		if _, err := rand.Read(parts[i]); err != nil {
			t.Fatal(err)
		}
	}

	var completeBody strings.Builder
	completeBody.WriteString("<CompleteMultipartUpload>")
	for i, p := range parts {
		pn := i + 1
		url := fmt.Sprintf("/bkt/mpkey?uploadId=%s&partNumber=%d", uploadID, pn)
		r := h.do("PUT", url, byteReader(p))
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		fmt.Fprintf(&completeBody, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pn, etag)
	}
	completeBody.WriteString("</CompleteMultipartUpload>")
	h.mustStatus(h.doString("POST", "/bkt/mpkey?uploadId="+uploadID, completeBody.String()), 200)

	totalSize := 0
	for _, s := range sizes {
		totalSize += s
	}

	offsets := make([]int, len(parts))
	{
		acc := 0
		for i, s := range sizes {
			offsets[i] = acc
			acc += s
		}
	}

	for i, p := range parts {
		pn := i + 1
		get := h.do("GET", fmt.Sprintf("/bkt/mpkey?partNumber=%d", pn), nil)
		h.mustStatus(get, 206)
		if got := get.Header.Get("x-amz-mp-parts-count"); got != strconv.Itoa(len(parts)) {
			t.Errorf("part %d: x-amz-mp-parts-count=%q want %d", pn, got, len(parts))
		}
		if got := get.Header.Get("Content-Length"); got != strconv.Itoa(len(p)) {
			t.Errorf("part %d: Content-Length=%q want %d", pn, got, len(p))
		}
		wantRange := fmt.Sprintf("bytes %d-%d/%d", offsets[i], offsets[i]+len(p)-1, totalSize)
		if got := get.Header.Get("Content-Range"); got != wantRange {
			t.Errorf("part %d: Content-Range=%q want %q", pn, got, wantRange)
		}
		body, err := io.ReadAll(get.Body)
		_ = get.Body.Close()
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if len(body) != len(p) {
			t.Fatalf("part %d body size: got %d want %d", pn, len(body), len(p))
		}
		for j := range p {
			if body[j] != p[j] {
				t.Fatalf("part %d body mismatch at byte %d", pn, j)
			}
		}
	}
}

// TestMultipartGetByPartNumberPerPartChecksum asserts that GET ?partNumber=N
// echoes the per-part stored x-amz-checksum-<algo> header (not the composite
// object-level one) for each fetched part.
func TestMultipartGetByPartNumberPerPartChecksum(t *testing.T) {
	const algo = "CRC32"
	hdr := "x-amz-checksum-" + strings.ToLower(algo)

	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("POST", "/bkt/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	parts := [][]byte{
		[]byte(strings.Repeat("A", 1024)),
		[]byte(strings.Repeat("B", 1024)),
		[]byte(strings.Repeat("C", 1024)),
	}
	partB64 := make([]string, len(parts))
	var completeBody strings.Builder
	completeBody.WriteString("<CompleteMultipartUpload>")
	for i, p := range parts {
		pn := i + 1
		partB64[i] = partChecksum(t, algo, p)
		url := fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=%d", uploadID, pn)
		r := h.do("PUT", url, byteReader(p), hdr, partB64[i])
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		fmt.Fprintf(&completeBody, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pn, etag)
	}
	completeBody.WriteString("</CompleteMultipartUpload>")
	h.mustStatus(h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody.String()), 200)

	for i := range parts {
		pn := i + 1
		get := h.do("GET", fmt.Sprintf("/bkt/k?partNumber=%d", pn), nil, "x-amz-checksum-mode", "ENABLED")
		h.mustStatus(get, 206)
		if got := get.Header.Get(hdr); got != partB64[i] {
			t.Errorf("part %d per-part checksum: got %q want %q", pn, got, partB64[i])
		}
		// ?partNumber requests do not surface the object-level checksum-type;
		// the per-part raw digest is what the client gets.
		if got := get.Header.Get("x-amz-checksum-type"); got != "" {
			t.Errorf("part %d unexpected x-amz-checksum-type: %q", pn, got)
		}
		_, _ = io.Copy(io.Discard, get.Body)
		_ = get.Body.Close()
	}
}

// TestGetByPartNumberNonMultipartReturns400 asserts that ?partNumber=N on a
// single-PUT object yields 400 InvalidArgument (PartSizes is empty for
// non-multipart objects).
func TestGetByPartNumberNonMultipartReturns400(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.do("PUT", "/bkt/single", byteReader([]byte("hello"))), 200)

	get := h.do("GET", "/bkt/single?partNumber=1", nil)
	h.mustStatus(get, 400)
	if !strings.Contains(h.readBody(get), "InvalidArgument") {
		t.Fatal("expected InvalidArgument on partNumber against non-multipart object")
	}
}

// TestGetByPartNumberOutOfRange asserts partNumber > parts count yields 400.
func TestGetByPartNumberOutOfRange(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/mp?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	var completeBody strings.Builder
	completeBody.WriteString("<CompleteMultipartUpload>")
	for i := 1; i <= 2; i++ {
		r := h.do("PUT", fmt.Sprintf("/bkt/mp?uploadId=%s&partNumber=%d", uploadID, i), byteReader([]byte(strings.Repeat("x", 32))))
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		fmt.Fprintf(&completeBody, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, i, etag)
	}
	completeBody.WriteString("</CompleteMultipartUpload>")
	h.mustStatus(h.doString("POST", "/bkt/mp?uploadId="+uploadID, completeBody.String()), 200)

	for _, n := range []string{"0", "-1", "3", "abc"} {
		get := h.do("GET", "/bkt/mp?partNumber="+n, nil)
		if get.StatusCode != 400 {
			t.Errorf("partNumber=%s: status=%d want 400", n, get.StatusCode)
		}
		_ = get.Body.Close()
	}
}
