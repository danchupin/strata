package s3api_test

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/s3api"
)

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// armMultipartMinPartSize re-enables the production 5 MiB lower bound
// (TestMain disables it by default for fast tests).
func armMultipartMinPartSize(t *testing.T) {
	t.Helper()
	restore := s3api.SetMultipartMinPartSizeForTest(5 * 1024 * 1024)
	t.Cleanup(restore)
}

// TestMultipartCompleteSizeTooSmall mirrors s3-tests
// test_multipart_upload_size_too_small. A non-last part below the AWS S3
// 5 MiB minimum must be rejected at Complete time with EntityTooSmall.
func TestMultipartCompleteSizeTooSmall(t *testing.T) {
	armMultipartMinPartSize(t)
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	smallPart := make([]byte, 1<<20)   // 1 MiB
	tailPart := make([]byte, 32<<10)   // 32 KiB
	for i := range smallPart {
		smallPart[i] = byte(i)
	}
	for i := range tailPart {
		tailPart[i] = byte(i + 1)
	}

	resp = h.do("PUT", fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=1", uploadID), byteReader(smallPart))
	h.mustStatus(resp, 200)
	etag1 := strings.Trim(resp.Header.Get("Etag"), `"`)
	resp = h.do("PUT", fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=2", uploadID), byteReader(tailPart))
	h.mustStatus(resp, 200)
	etag2 := strings.Trim(resp.Header.Get("Etag"), `"`)

	body := fmt.Sprintf(
		`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part><Part><PartNumber>2</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`,
		etag1, etag2,
	)
	resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, body)
	h.mustStatus(resp, 400)
	if !strings.Contains(h.readBody(resp), "<Code>EntityTooSmall</Code>") {
		t.Fatalf("expected EntityTooSmall")
	}
}

// TestMultipartCompleteLastPartCanBeSmall verifies the LAST part is exempt
// from the 5 MiB minimum (AWS S3 spec).
func TestMultipartCompleteLastPartCanBeSmall(t *testing.T) {
	armMultipartMinPartSize(t)
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	bigPart := make([]byte, 5<<20) // exactly 5 MiB
	tailPart := []byte("tail")
	for i := range bigPart {
		bigPart[i] = byte(i)
	}

	resp = h.do("PUT", fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=1", uploadID), byteReader(bigPart))
	h.mustStatus(resp, 200)
	etag1 := strings.Trim(resp.Header.Get("Etag"), `"`)
	resp = h.do("PUT", fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=2", uploadID), byteReader(tailPart))
	h.mustStatus(resp, 200)
	etag2 := strings.Trim(resp.Header.Get("Etag"), `"`)

	body := fmt.Sprintf(
		`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part><Part><PartNumber>2</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`,
		etag1, etag2,
	)
	resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, body)
	h.mustStatus(resp, 200)
}

// TestMultipartUploadResendPart mirrors s3-tests test_multipart_upload_resend_part.
// Re-uploading the same partNumber with new bytes must replace the previous
// part (last write wins). Complete with the latest ETag succeeds and the
// stored object reflects the second body.
func TestMultipartUploadResendPart(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	first := make([]byte, 5<<20)
	second := make([]byte, 5<<20)
	for i := range first {
		first[i] = byte(0xaa)
	}
	for i := range second {
		second[i] = byte(0x55)
	}
	tail := []byte("tail")

	resp = h.do("PUT", fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=1", uploadID), byteReader(first))
	h.mustStatus(resp, 200)
	etag1a := strings.Trim(resp.Header.Get("Etag"), `"`)
	resp = h.do("PUT", fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=1", uploadID), byteReader(second))
	h.mustStatus(resp, 200)
	etag1b := strings.Trim(resp.Header.Get("Etag"), `"`)
	if etag1a == etag1b {
		t.Fatalf("expected different ETag after resend, got identical %s", etag1b)
	}
	resp = h.do("PUT", fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=2", uploadID), byteReader(tail))
	h.mustStatus(resp, 200)
	etag2 := strings.Trim(resp.Header.Get("Etag"), `"`)

	// Completing with the SECOND (current) ETag succeeds; the prior write
	// is dropped per "last write wins" on resend.
	good := fmt.Sprintf(
		`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part><Part><PartNumber>2</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`,
		etag1b, etag2,
	)
	resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, good)
	h.mustStatus(resp, 200)

	// GET the assembled object, verify body is second+tail (NOT first+tail).
	resp = h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 200)
	got := []byte(h.readBody(resp))
	want := append(append([]byte{}, second...), tail...)
	if md5Hex(got) != md5Hex(want) {
		t.Fatalf("body mismatch: got md5 %s want %s", md5Hex(got), md5Hex(want))
	}
}

// TestMultipartResendFirstFinishesLast mirrors s3-tests
// test_multipart_resend_first_finishes_last. Two PUTs land for the same
// partNumber; the second write's stored ETag is what Complete must accept.
// Sequencing both writes synchronously here is sufficient to exercise the
// "last write wins" contract — the upstream test's racey scheduling is not
// reproducible deterministically, but the invariant we care about is that
// the most recent SavePart is what ListParts surfaces.
func TestMultipartResendFirstFinishesLast(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	a := make([]byte, 5<<20)
	b := make([]byte, 5<<20)
	for i := range a {
		a[i] = 0xa1
	}
	for i := range b {
		b[i] = 0xb2
	}
	tail := []byte("z")

	// Order: A → B → tail. B is the resend that "finishes last" for partNumber=1.
	h.mustStatus(h.do("PUT", fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=1", uploadID), byteReader(a)), 200)
	resp = h.do("PUT", fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=1", uploadID), byteReader(b))
	h.mustStatus(resp, 200)
	etagB := strings.Trim(resp.Header.Get("Etag"), `"`)
	resp = h.do("PUT", fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=2", uploadID), byteReader(tail))
	h.mustStatus(resp, 200)
	etagTail := strings.Trim(resp.Header.Get("Etag"), `"`)

	body := fmt.Sprintf(
		`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part><Part><PartNumber>2</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`,
		etagB, etagTail,
	)
	resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, body)
	h.mustStatus(resp, 200)

	resp = h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 200)
	got := []byte(h.readBody(resp))
	want := append(append([]byte{}, b...), tail...)
	if md5Hex(got) != md5Hex(want) {
		t.Fatalf("body md5 mismatch")
	}
}

// TestMultipartCompleteWrongETagInvalidPart locks the InvalidPart contract:
// supplying an ETag in the Complete body that does not match the stored
// part returns 400 InvalidPart.
func TestMultipartCompleteWrongETagInvalidPart(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	bigPart := make([]byte, 5<<20)
	tail := []byte("tail")
	for i := range bigPart {
		bigPart[i] = byte(i)
	}
	h.mustStatus(h.do("PUT", fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=1", uploadID), byteReader(bigPart)), 200)
	resp = h.do("PUT", fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=2", uploadID), byteReader(tail))
	h.mustStatus(resp, 200)
	etag2 := strings.Trim(resp.Header.Get("Etag"), `"`)

	body := fmt.Sprintf(
		`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part><Part><PartNumber>2</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`,
		"deadbeefdeadbeefdeadbeefdeadbeef", etag2,
	)
	resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, body)
	h.mustStatus(resp, 400)
	if !strings.Contains(h.readBody(resp), "<Code>InvalidPart</Code>") {
		t.Fatalf("expected InvalidPart for wrong ETag")
	}
}
