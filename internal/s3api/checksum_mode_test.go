package s3api_test

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// TestChecksumModeOmittedSingleHidesHeaders asserts AWS-parity for single-PUT
// objects: HEAD/GET without x-amz-checksum-mode must NOT echo any
// x-amz-checksum-* headers, even when the object stored a checksum.
func TestChecksumModeOmittedSingleHidesHeaders(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/cmk", ""), 200)

	payload := strings.Repeat("z", 64)
	digest := sha256.Sum256([]byte(payload))
	want := base64.StdEncoding.EncodeToString(digest[:])
	h.mustStatus(h.doString("PUT", "/cmk/k", payload, "x-amz-checksum-sha256", want), 200)

	for _, method := range []string{"HEAD", "GET"} {
		resp := h.doString(method, "/cmk/k", "")
		h.mustStatus(resp, 200)
		_ = h.readBody(resp)
		if got := resp.Header.Get("x-amz-checksum-sha256"); got != "" {
			t.Errorf("%s without ChecksumMode leaked x-amz-checksum-sha256=%q", method, got)
		}
		if got := resp.Header.Get("x-amz-checksum-type"); got != "" {
			t.Errorf("%s without ChecksumMode leaked x-amz-checksum-type=%q", method, got)
		}
	}
}

// TestChecksumModeEnabledMultipartComposite asserts that a multipart object
// initiated WITHOUT an explicit x-amz-checksum-type but WITH a per-part
// algorithm reports ChecksumType=COMPOSITE on HEAD/GET when ChecksumMode is
// ENABLED.
func TestChecksumModeEnabledMultipartComposite(t *testing.T) {
	const algo = "CRC32"
	hdr := "x-amz-checksum-" + strings.ToLower(algo)

	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/cmk", ""), 200)
	resp := h.doString("POST", "/cmk/k?uploads", "",
		"x-amz-checksum-algorithm", algo)
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<ChecksumAlgorithm>"+algo+"</ChecksumAlgorithm>") {
		t.Fatalf("Initiate body missing ChecksumAlgorithm: %s", body)
	}
	uploadID := uploadIDRE.FindStringSubmatch(body)[1]

	parts := [][]byte{
		[]byte(strings.Repeat("A", 1024)),
		[]byte(strings.Repeat("B", 1024)),
	}
	partB64 := make([]string, len(parts))
	var completeBody strings.Builder
	completeBody.WriteString("<CompleteMultipartUpload>")
	for i, p := range parts {
		pn := i + 1
		partB64[i] = partChecksum(t, algo, p)
		r := h.do("PUT", fmt.Sprintf("/cmk/k?uploadId=%s&partNumber=%d", uploadID, pn),
			byteReader(p), hdr, partB64[i])
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		fmt.Fprintf(&completeBody, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pn, etag)
	}
	completeBody.WriteString("</CompleteMultipartUpload>")
	want := composite(t, algo, partB64)
	h.mustStatus(h.doString("POST", "/cmk/k?uploadId="+uploadID, completeBody.String()), 200)

	for _, method := range []string{"HEAD", "GET"} {
		resp := h.doString(method, "/cmk/k", "", "x-amz-checksum-mode", "ENABLED")
		h.mustStatus(resp, 200)
		_ = h.readBody(resp)
		if got := resp.Header.Get(hdr); got != want {
			t.Errorf("%s composite header: got %q want %q", method, got, want)
		}
		if got := resp.Header.Get("x-amz-checksum-type"); got != "COMPOSITE" {
			t.Errorf("%s checksum-type: got %q want COMPOSITE", method, got)
		}
	}

	// Without the mode header — even on this multipart object — checksum
	// headers must not appear.
	bare := h.doString("HEAD", "/cmk/k", "")
	h.mustStatus(bare, 200)
	_ = h.readBody(bare)
	if got := bare.Header.Get(hdr); got != "" {
		t.Errorf("HEAD without ChecksumMode leaked %s=%q", hdr, got)
	}
	if got := bare.Header.Get("x-amz-checksum-type"); got != "" {
		t.Errorf("HEAD without ChecksumMode leaked x-amz-checksum-type=%q", got)
	}
}

// TestChecksumModeFullObjectMultipart asserts the FULL_OBJECT path: when the
// client sets x-amz-checksum-type=FULL_OBJECT on Initiate, Complete persists
// it and HEAD/GET with ChecksumMode=ENABLED reports it back.
func TestChecksumModeFullObjectMultipart(t *testing.T) {
	const algo = "CRC32"
	hdr := "x-amz-checksum-" + strings.ToLower(algo)

	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/cmk", ""), 200)
	resp := h.doString("POST", "/cmk/k?uploads", "",
		"x-amz-checksum-algorithm", algo,
		"x-amz-checksum-type", "FULL_OBJECT")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<ChecksumType>FULL_OBJECT</ChecksumType>") {
		t.Fatalf("Initiate body missing ChecksumType=FULL_OBJECT: %s", body)
	}
	if got := resp.Header.Get("x-amz-checksum-type"); got != "FULL_OBJECT" {
		t.Fatalf("Initiate checksum-type header: got %q want FULL_OBJECT", got)
	}
	uploadID := uploadIDRE.FindStringSubmatch(body)[1]

	parts := [][]byte{
		[]byte(strings.Repeat("X", 512)),
		[]byte(strings.Repeat("Y", 512)),
	}
	partB64 := make([]string, len(parts))
	var completeBody strings.Builder
	completeBody.WriteString("<CompleteMultipartUpload>")
	for i, p := range parts {
		pn := i + 1
		partB64[i] = partChecksum(t, algo, p)
		r := h.do("PUT", fmt.Sprintf("/cmk/k?uploadId=%s&partNumber=%d", uploadID, pn),
			byteReader(p), hdr, partB64[i])
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		fmt.Fprintf(&completeBody, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pn, etag)
	}
	completeBody.WriteString("</CompleteMultipartUpload>")
	complete := h.doString("POST", "/cmk/k?uploadId="+uploadID, completeBody.String())
	h.mustStatus(complete, 200)
	completeBodyStr := h.readBody(complete)
	if !strings.Contains(completeBodyStr, "<ChecksumType>FULL_OBJECT</ChecksumType>") {
		t.Fatalf("Complete body missing ChecksumType=FULL_OBJECT: %s", completeBodyStr)
	}
	if got := complete.Header.Get("x-amz-checksum-type"); got != "FULL_OBJECT" {
		t.Errorf("Complete checksum-type header: got %q want FULL_OBJECT", got)
	}

	for _, method := range []string{"HEAD", "GET"} {
		resp := h.doString(method, "/cmk/k", "", "x-amz-checksum-mode", "enabled")
		h.mustStatus(resp, 200)
		_ = h.readBody(resp)
		if got := resp.Header.Get("x-amz-checksum-type"); got != "FULL_OBJECT" {
			t.Errorf("%s checksum-type: got %q want FULL_OBJECT", method, got)
		}
		if got := resp.Header.Get(hdr); got == "" {
			t.Errorf("%s missing %s header on FULL_OBJECT object", method, hdr)
		}
	}
}
