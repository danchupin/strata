package s3api_test

import (
	"bytes"
	"crypto/rand"
	"encoding/xml"
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

// TestMultipartGetByPartNumberEchoesWholeObjectETag asserts the
// AWS-parity wire shape for ?partNumber=N: the response ETag header
// equals the whole-object multipart ETag (`"<32hex>-<count>"`), NOT the
// per-part raw ETag. Mirrors s3-tests test_multipart_get_part /
// _single_get_part assertion `response['ETag'] == complete['ETag']`.
func TestMultipartGetByPartNumberEchoesWholeObjectETag(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	for _, tc := range []struct {
		name  string
		parts [][]byte
	}{
		{"single-part", [][]byte{bytes.Repeat([]byte("A"), 1<<10)}},
		{"multi-part", [][]byte{
			bytes.Repeat([]byte("A"), 5<<20),
			bytes.Repeat([]byte("B"), 1<<10),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := "k-" + tc.name
			resp := h.doString("POST", "/bkt/"+key+"?uploads", "")
			h.mustStatus(resp, 200)
			uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

			var completeBody strings.Builder
			completeBody.WriteString("<CompleteMultipartUpload>")
			for i, p := range tc.parts {
				pn := i + 1
				r := h.do("PUT", fmt.Sprintf("/bkt/%s?uploadId=%s&partNumber=%d", key, uploadID, pn), byteReader(p))
				h.mustStatus(r, 200)
				etag := strings.Trim(r.Header.Get("Etag"), `"`)
				fmt.Fprintf(&completeBody, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pn, etag)
			}
			completeBody.WriteString("</CompleteMultipartUpload>")
			complete := h.doString("POST", "/bkt/"+key+"?uploadId="+uploadID, completeBody.String())
			h.mustStatus(complete, 200)
			var completeRes struct {
				ETag string `xml:"ETag"`
			}
			if err := xml.Unmarshal([]byte(h.readBody(complete)), &completeRes); err != nil {
				t.Fatalf("decode complete response: %v", err)
			}
			wantETag := completeRes.ETag

			head := h.do("HEAD", fmt.Sprintf("/bkt/%s?partNumber=1", key), nil)
			h.mustStatus(head, 206)
			if got := head.Header.Get("Etag"); got != wantETag {
				t.Errorf("HEAD partNumber=1 ETag=%q want %q (whole-object multipart)", got, wantETag)
			}

			get := h.do("GET", fmt.Sprintf("/bkt/%s?partNumber=1", key), nil)
			h.mustStatus(get, 206)
			if got := get.Header.Get("Etag"); got != wantETag {
				t.Errorf("GET partNumber=1 ETag=%q want %q (whole-object multipart)", got, wantETag)
			}
			_, _ = io.Copy(io.Discard, get.Body)
			_ = get.Body.Close()
		})
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
		// AWS-parity: ?partNumber GET emits x-amz-checksum-type alongside the
		// per-part digest so boto3 `response['ChecksumType']` round-trips —
		// s3-tests `test_multipart_use_cksum_helper_*` depends on it.
		if got := get.Header.Get("x-amz-checksum-type"); got != "COMPOSITE" {
			t.Errorf("part %d x-amz-checksum-type: got %q want COMPOSITE", pn, got)
		}
		_, _ = io.Copy(io.Discard, get.Body)
		_ = get.Body.Close()
	}
}

// TestGetByPartNumberNonMultipart asserts AWS-parity for ?partNumber=N on a
// single-PUT object: partNumber=1 returns the whole object (200 OK with the
// single-PUT ETag); partNumber>1 returns 400 InvalidPart. Mirrors s3-tests
// `test_non_multipart_get_part`.
func TestGetByPartNumberNonMultipart(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	put := h.do("PUT", "/bkt/single", byteReader([]byte("hello")))
	h.mustStatus(put, 200)
	wantETag := put.Header.Get("Etag")

	get := h.do("GET", "/bkt/single?partNumber=1", nil)
	h.mustStatus(get, 200)
	if got := get.Header.Get("Etag"); got != wantETag {
		t.Errorf("partNumber=1 ETag=%q want %q", got, wantETag)
	}
	if body := h.readBody(get); body != "hello" {
		t.Errorf("partNumber=1 body=%q want %q", body, "hello")
	}

	bad := h.do("GET", "/bkt/single?partNumber=2", nil)
	h.mustStatus(bad, 400)
	if !strings.Contains(h.readBody(bad), "InvalidPart") {
		t.Fatal("expected InvalidPart on partNumber=2 against non-multipart object")
	}
}

// TestGetByPartNumberOutOfRange asserts partNumber out of range yields
// 400 InvalidPart (AWS-parity — see s3-tests test_multipart_get_part /
// _single_get_part: PartNumber > parts_count → 400 InvalidPart). Earlier
// behaviour returned 416 InvalidRange, which diverged from AWS.
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

// TestMultipartGetByPartNumberRange combines ?partNumber=N with Range: bytes=…
// and asserts the range is part-relative — offset + length resolve inside the
// part rather than the whole object.
func TestMultipartGetByPartNumberRange(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/mpr?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	parts := [][]byte{
		bytes.Repeat([]byte("A"), 5<<20),
		bytes.Repeat([]byte("B"), 1<<20),
	}
	var completeBody strings.Builder
	completeBody.WriteString("<CompleteMultipartUpload>")
	for i, p := range parts {
		pn := i + 1
		r := h.do("PUT", fmt.Sprintf("/bkt/mpr?uploadId=%s&partNumber=%d", uploadID, pn), byteReader(p))
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		fmt.Fprintf(&completeBody, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pn, etag)
	}
	completeBody.WriteString("</CompleteMultipartUpload>")
	h.mustStatus(h.doString("POST", "/bkt/mpr?uploadId="+uploadID, completeBody.String()), 200)

	totalSize := len(parts[0]) + len(parts[1])
	part2Off := len(parts[0])

	get := h.do("GET", "/bkt/mpr?partNumber=2", nil, "Range", "bytes=0-15")
	h.mustStatus(get, 206)
	wantRange := fmt.Sprintf("bytes %d-%d/%d", part2Off, part2Off+15, totalSize)
	if got := get.Header.Get("Content-Range"); got != wantRange {
		t.Errorf("Content-Range=%q want %q", got, wantRange)
	}
	if got := get.Header.Get("Content-Length"); got != "16" {
		t.Errorf("Content-Length=%q want 16", got)
	}
	body, err := io.ReadAll(get.Body)
	_ = get.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, bytes.Repeat([]byte("B"), 16)) {
		t.Fatalf("body=%q want 16 'B' bytes", body)
	}

	bad := h.do("GET", "/bkt/mpr?partNumber=2", nil, "Range", "bytes=99999999-")
	h.mustStatus(bad, 416)
	_ = bad.Body.Close()
}

// TestMultipartCompleteDedupesDuplicatePartNumber asserts AWS take-latest
// semantics on duplicate PartNumber in the Parts list — submitting the
// same PartNumber twice (resend) accepts and resolves against the LATEST
// stored part for that PartNumber (last UploadPart write wins on storage).
// Mirrors s3-tests test_multipart_resend_first_finishes_last.
func TestMultipartCompleteDedupesDuplicatePartNumber(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/mpdup?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	bodyA := bytes.Repeat([]byte("A"), 1024)
	rA := h.do("PUT", fmt.Sprintf("/bkt/mpdup?uploadId=%s&partNumber=1", uploadID), byteReader(bodyA))
	h.mustStatus(rA, 200)
	etagA := strings.Trim(rA.Header.Get("Etag"), `"`)

	bodyB := bytes.Repeat([]byte("B"), 1024)
	rB := h.do("PUT", fmt.Sprintf("/bkt/mpdup?uploadId=%s&partNumber=1", uploadID), byteReader(bodyB))
	h.mustStatus(rB, 200)
	etagB := strings.Trim(rB.Header.Get("Etag"), `"`)

	if etagA == etagB {
		t.Fatalf("test setup: re-uploaded part 1 with different bytes but same ETag (%s)", etagA)
	}

	var cb strings.Builder
	cb.WriteString("<CompleteMultipartUpload>")
	fmt.Fprintf(&cb, `<Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part>`, etagA)
	fmt.Fprintf(&cb, `<Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part>`, etagB)
	cb.WriteString("</CompleteMultipartUpload>")

	complete := h.doString("POST", "/bkt/mpdup?uploadId="+uploadID, cb.String())
	h.mustStatus(complete, 200)

	get := h.do("GET", "/bkt/mpdup", nil)
	h.mustStatus(get, 200)
	body, err := io.ReadAll(get.Body)
	_ = get.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, bodyB) {
		t.Fatalf("destination body mismatch: got %d bytes, expected %d (bodyB last-write)", len(body), len(bodyB))
	}
}

// TestMultipartCompleteRejectsOutOfOrderParts asserts a strict
// out-of-order Parts list (e.g. [1, 3, 2]) still returns InvalidPartOrder
// after the duplicate-dedupe relaxation. Only EQUAL adjacent PartNumbers
// (resend) are acceptable; reverse-decreasing positions are not.
func TestMultipartCompleteRejectsOutOfOrderParts(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/mporder?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	etags := make(map[int]string, 3)
	for _, pn := range []int{1, 2, 3} {
		body := bytes.Repeat([]byte{byte('a' + pn)}, 5<<20)
		r := h.do("PUT", fmt.Sprintf("/bkt/mporder?uploadId=%s&partNumber=%d", uploadID, pn), byteReader(body))
		h.mustStatus(r, 200)
		etags[pn] = strings.Trim(r.Header.Get("Etag"), `"`)
	}

	var cb strings.Builder
	cb.WriteString("<CompleteMultipartUpload>")
	for _, pn := range []int{1, 3, 2} {
		fmt.Fprintf(&cb, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pn, etags[pn])
	}
	cb.WriteString("</CompleteMultipartUpload>")

	bad := h.doString("POST", "/bkt/mporder?uploadId="+uploadID, cb.String())
	h.mustStatus(bad, 400)
	if !strings.Contains(h.readBody(bad), "InvalidPartOrder") {
		t.Fatalf("expected InvalidPartOrder on out-of-order parts list")
	}
}
