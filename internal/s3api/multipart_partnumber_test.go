package s3api_test

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// TestMultipartGetPartNumber covers US-002: ?partNumber=N (without uploadId)
// must serve the bytes of the Nth part using the per-part offsets recorded
// in Manifest.PartChunks. Range stacks part-relative; out-of-range returns
// 416 InvalidPartNumber; non-multipart objects 416 too.
func TestMultipartGetPartNumber(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	// Three 5MiB parts (min part size for non-last parts is 5MiB).
	const partSize = 5 << 20
	parts := make([][]byte, 3)
	for i := range parts {
		parts[i] = make([]byte, partSize)
		if _, err := rand.Read(parts[i]); err != nil {
			t.Fatal(err)
		}
	}

	resp := h.doString("POST", "/bkt/mp?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	var completeBody strings.Builder
	completeBody.WriteString("<CompleteMultipartUpload>")
	totalSize := int64(0)
	for i, p := range parts {
		pnum := i + 1
		url := fmt.Sprintf("/bkt/mp?uploadId=%s&partNumber=%d", uploadID, pnum)
		r := h.do("PUT", url, byteReader(p))
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		completeBody.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pnum, etag))
		totalSize += int64(len(p))
	}
	completeBody.WriteString("</CompleteMultipartUpload>")
	h.mustStatus(h.doString("POST", "/bkt/mp?uploadId="+uploadID, completeBody.String()), 200)

	t.Run("part-2-body", func(t *testing.T) {
		r := h.doString("GET", "/bkt/mp?partNumber=2", "")
		h.mustStatus(r, http.StatusPartialContent)
		if cnt := r.Header.Get("X-Amz-Mp-Parts-Count"); cnt != "3" {
			t.Errorf("parts-count: got %q want 3", cnt)
		}
		if cl := r.Header.Get("Content-Length"); cl != strconv.Itoa(partSize) {
			t.Errorf("content-length: got %q want %d", cl, partSize)
		}
		wantCR := fmt.Sprintf("bytes %d-%d/%d", partSize, 2*partSize-1, totalSize)
		if cr := r.Header.Get("Content-Range"); cr != wantCR {
			t.Errorf("content-range: got %q want %q", cr, wantCR)
		}
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != partSize {
			t.Fatalf("body len: got %d want %d", len(got), partSize)
		}
		for i := 0; i < partSize; i++ {
			if got[i] != parts[1][i] {
				t.Fatalf("body mismatch at %d", i)
			}
		}
	})

	t.Run("part-3-head", func(t *testing.T) {
		r := h.doString("HEAD", "/bkt/mp?partNumber=3", "")
		h.mustStatus(r, http.StatusPartialContent)
		if cnt := r.Header.Get("X-Amz-Mp-Parts-Count"); cnt != "3" {
			t.Errorf("parts-count: got %q want 3", cnt)
		}
		if cl := r.Header.Get("Content-Length"); cl != strconv.Itoa(partSize) {
			t.Errorf("content-length: got %q want %d", cl, partSize)
		}
		wantCR := fmt.Sprintf("bytes %d-%d/%d", 2*partSize, 3*partSize-1, totalSize)
		if cr := r.Header.Get("Content-Range"); cr != wantCR {
			t.Errorf("content-range: got %q want %q", cr, wantCR)
		}
	})

	t.Run("part-relative-range", func(t *testing.T) {
		// Range bytes=0-9 inside part 2 must yield part2[0:10].
		r := h.doString("GET", "/bkt/mp?partNumber=2", "", "Range", "bytes=0-9")
		h.mustStatus(r, http.StatusPartialContent)
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 10 {
			t.Fatalf("body len: got %d want 10", len(got))
		}
		for i := 0; i < 10; i++ {
			if got[i] != parts[1][i] {
				t.Fatalf("body mismatch at %d: got %x want %x", i, got[i], parts[1][i])
			}
		}
		// Content-Range absolute against whole-object total.
		wantCR := fmt.Sprintf("bytes %d-%d/%d", partSize, partSize+9, totalSize)
		if cr := r.Header.Get("Content-Range"); cr != wantCR {
			t.Errorf("content-range: got %q want %q", cr, wantCR)
		}
		if cl := r.Header.Get("Content-Length"); cl != "10" {
			t.Errorf("content-length: got %q want 10", cl)
		}
	})

	t.Run("part-etag-not-whole", func(t *testing.T) {
		whole := h.doString("HEAD", "/bkt/mp", "")
		h.mustStatus(whole, http.StatusOK)
		wholeETag := whole.Header.Get("Etag")
		r := h.doString("HEAD", "/bkt/mp?partNumber=1", "")
		h.mustStatus(r, http.StatusPartialContent)
		partETag := r.Header.Get("Etag")
		if partETag == "" {
			t.Fatalf("part etag empty")
		}
		if partETag == wholeETag {
			t.Errorf("part etag must differ from whole-object etag (%q == %q)", partETag, wholeETag)
		}
	})

	t.Run("out-of-range-high", func(t *testing.T) {
		r := h.doString("GET", "/bkt/mp?partNumber=4", "")
		h.mustStatus(r, http.StatusRequestedRangeNotSatisfiable)
	})

	t.Run("out-of-range-zero", func(t *testing.T) {
		r := h.doString("GET", "/bkt/mp?partNumber=0", "")
		h.mustStatus(r, http.StatusRequestedRangeNotSatisfiable)
	})

	t.Run("out-of-range-negative", func(t *testing.T) {
		r := h.doString("GET", "/bkt/mp?partNumber=-1", "")
		h.mustStatus(r, http.StatusRequestedRangeNotSatisfiable)
	})

	t.Run("non-multipart-object", func(t *testing.T) {
		h.mustStatus(h.doString("PUT", "/bkt/single", "hello"), 200)
		r := h.doString("GET", "/bkt/single?partNumber=1", "")
		h.mustStatus(r, http.StatusRequestedRangeNotSatisfiable)
	})
}
