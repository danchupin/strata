package s3api_test

import (
	"net/http"
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
