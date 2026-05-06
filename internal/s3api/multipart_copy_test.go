package s3api_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestUploadPartCopyWithChecksumAlgorithm(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	body := strings.Repeat("z", 6<<20)
	h.mustStatus(h.do("PUT", "/bkt/src", strings.NewReader(body)), 200)

	resp := h.doString("POST", "/bkt/dst?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	resp = h.doString("PUT", "/bkt/dst?uploadId="+uploadID+"&partNumber=1", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-checksum-algorithm", "SHA256")
	h.mustStatus(resp, http.StatusOK)
	got := resp.Header.Get("x-amz-checksum-sha256")
	if got == "" {
		t.Fatalf("response missing x-amz-checksum-sha256 header: %v", resp.Header)
	}
	want := partChecksum(t, "SHA256", []byte(body))
	if got != want {
		t.Fatalf("sha256 hdr: got %q want %q", got, want)
	}
	xml := h.readBody(resp)
	if !strings.Contains(xml, "<ChecksumSHA256>"+want+"</ChecksumSHA256>") {
		t.Fatalf("CopyPartResult missing ChecksumSHA256: %s", xml)
	}
}

func TestUploadPartCopyChecksumMismatchReturnsBadDigest(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	body := strings.Repeat("y", 6<<20)
	h.mustStatus(h.do("PUT", "/bkt/src", strings.NewReader(body)), 200)

	resp := h.doString("POST", "/bkt/dst?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	bogus := partChecksum(t, "SHA256", []byte("not the body"))
	resp = h.doString("PUT", "/bkt/dst?uploadId="+uploadID+"&partNumber=1", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-checksum-algorithm", "SHA256",
		"x-amz-checksum-sha256", bogus)
	h.mustStatus(resp, http.StatusBadRequest)
	if !strings.Contains(h.readBody(resp), "<Code>BadDigest</Code>") {
		t.Fatalf("expected BadDigest")
	}
}

// TestUploadPartCopyImproperRangeReturnsInvalidArgument mirrors the s3-test
// `test_multipart_copy_improper_range`: every syntactically malformed range
// value must return 400 InvalidArgument, not 416 InvalidRange.
func TestUploadPartCopyImproperRangeReturnsInvalidArgument(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	body := strings.Repeat("a", 6<<20)
	h.mustStatus(h.do("PUT", "/bkt/src", strings.NewReader(body)), 200)

	resp := h.doString("POST", "/bkt/dst?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	improper := []string{
		"0-2",
		"bytes=0",
		"bytes=hello-world",
		"bytes=0-bar",
		"bytes=hello-",
		"bytes=0-2,3-5",
	}
	for _, spec := range improper {
		resp := h.doString("PUT", "/bkt/dst?uploadId="+uploadID+"&partNumber=1", "",
			"x-amz-copy-source", "/bkt/src",
			"x-amz-copy-source-range", spec)
		h.mustStatus(resp, http.StatusBadRequest)
		body := h.readBody(resp)
		if !strings.Contains(body, "<Code>InvalidArgument</Code>") {
			t.Fatalf("range %q: expected InvalidArgument, got %s", spec, body)
		}
	}
}

// TestUploadPartCopyOutOfBoundsRangeReturnsInvalidRange covers
// `test_multipart_copy_invalid_range` — syntactically valid but extends past
// the source size.
func TestUploadPartCopyOutOfBoundsRangeReturnsInvalidRange(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	h.mustStatus(h.doString("PUT", "/bkt/src", "12345"), 200)

	resp := h.doString("POST", "/bkt/dst?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	resp = h.doString("PUT", "/bkt/dst?uploadId="+uploadID+"&partNumber=1", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-copy-source-range", "bytes=0-21")
	h.mustStatus(resp, http.StatusRequestedRangeNotSatisfiable)
	body := h.readBody(resp)
	if !strings.Contains(body, "<Code>InvalidRange</Code>") {
		t.Fatalf("expected InvalidRange, got %s", body)
	}
}

// TestUploadPartCopySpecialSourceKeyNames covers `test_multipart_copy_special_names`:
// boto3 percent-encodes special chars (' ', '_', '__', '?versionId') in the
// `x-amz-copy-source` header. The path-vs-query split MUST happen before
// PathUnescape so a literal `?` in the key is not treated as a query
// separator.
func TestUploadPartCopySpecialSourceKeyNames(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	for _, srcKey := range []string{" ", "_", "__", "?versionId"} {
		body := strings.Repeat("x", 6<<20)
		// PUT to /bkt/<percent-encoded-key>
		h.mustStatus(h.do("PUT", "/bkt/"+url.PathEscape(srcKey), strings.NewReader(body)), 200)

		resp := h.doString("POST", "/bkt/dst?uploads", "")
		h.mustStatus(resp, 200)
		uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

		copySource := "/bkt/" + url.PathEscape(srcKey)
		resp = h.doString("PUT", "/bkt/dst?uploadId="+uploadID+"&partNumber=1", "",
			"x-amz-copy-source", copySource)
		h.mustStatus(resp, http.StatusOK)
	}
}

func TestUploadPartCopyChecksumPersistsToCompositeOnComplete(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	body := strings.Repeat("c", 6<<20)
	h.mustStatus(h.do("PUT", "/bkt/src", strings.NewReader(body)), 200)

	resp := h.doString("POST", "/bkt/dst?uploads", "",
		"x-amz-checksum-algorithm", "SHA256")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	resp = h.doString("PUT", "/bkt/dst?uploadId="+uploadID+"&partNumber=1", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-checksum-algorithm", "SHA256")
	h.mustStatus(resp, http.StatusOK)
	part1 := resp.Header.Get("x-amz-checksum-sha256")
	if part1 == "" {
		t.Fatal("missing per-part SHA256")
	}
	etag := strings.Trim(resp.Header.Get("Etag"), `"`)

	complete := "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>\"" + etag + "\"</ETag></Part></CompleteMultipartUpload>"
	resp = h.doString("POST", "/bkt/dst?uploadId="+uploadID, complete)
	h.mustStatus(resp, http.StatusOK)
	cb := h.readBody(resp)
	wantComposite := composite(t, "SHA256", []string{part1})
	if !strings.Contains(cb, "<ChecksumSHA256>"+wantComposite+"</ChecksumSHA256>") {
		t.Fatalf("complete body missing composite SHA256=%s: %s", wantComposite, cb)
	}
}
