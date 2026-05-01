package s3api_test

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"hash/crc32"
	"net/http"
	"strings"
	"testing"
)

// TestMultipartCopyFlexibleChecksum covers US-004: when UploadPartCopy
// carries `x-amz-checksum-algorithm`, the gateway recomputes the digest
// over the streamed copy bytes (no buffering), echoes it on the response,
// stores it on the multipart_parts row, and validates it against any
// client-supplied `x-amz-checksum-<algo>` header (mismatch → BadDigest).
func TestMultipartCopyFlexibleChecksum(t *testing.T) {
	cases := []struct {
		name    string
		algo    string
		hdrName string
		newHash func() hash.Hash
	}{
		{"SHA256", "SHA256", "X-Amz-Checksum-Sha256", sha256.New},
		{"SHA1", "SHA1", "X-Amz-Checksum-Sha1", sha1.New},
		{"CRC32", "CRC32", "X-Amz-Checksum-Crc32", func() hash.Hash { return crc32.NewIEEE() }},
		{"CRC32C", "CRC32C", "X-Amz-Checksum-Crc32c", func() hash.Hash {
			return crc32.New(crc32.MakeTable(crc32.Castagnoli))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

			// Source object — single PUT.
			const partSize = 5 << 20
			src := make([]byte, partSize)
			if _, err := rand.Read(src); err != nil {
				t.Fatal(err)
			}
			h.mustStatus(h.do("PUT", "/bkt/source", byteReader(src)), 200)

			// Initiate destination MPU with checksum algorithm.
			resp := h.doString("POST", "/bkt/dst?uploads", "",
				"x-amz-checksum-algorithm", tc.algo)
			h.mustStatus(resp, 200)
			uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

			// UploadPartCopy with algorithm header. Gateway recomputes,
			// echoes header on response.
			hh := tc.newHash()
			hh.Write(src)
			rawDigest := hh.Sum(nil)
			expectedDigest := base64.StdEncoding.EncodeToString(rawDigest)

			url := fmt.Sprintf("/bkt/dst?uploadId=%s&partNumber=1", uploadID)
			r := h.do("PUT", url, nil,
				"x-amz-copy-source", "/bkt/source",
				"x-amz-checksum-algorithm", tc.algo)
			h.mustStatus(r, 200)
			if got := r.Header.Get(tc.hdrName); got != expectedDigest {
				t.Errorf("upload-part-copy echo: got %q want %q", got, expectedDigest)
			}
			etag := extractCopyPartETag(t, h.readBody(r))

			// Complete and verify composite-checksum response includes the
			// per-part digest in the COMPOSITE formula — proves the part's
			// ChecksumValue was persisted and surfaced to PartChunks.
			completeBody := fmt.Sprintf(
				`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`,
				etag,
			)
			cr := h.doString("POST", "/bkt/dst?uploadId="+uploadID, completeBody)
			h.mustStatus(cr, 200)
			completeXML := h.readBody(cr)
			expectedComposite := compositeExpected(tc.newHash, [][]byte{rawDigest})
			if !strings.Contains(completeXML, "<Checksum"+tc.algo+">"+expectedComposite+"</Checksum"+tc.algo+">") {
				t.Errorf("composite checksum missing or wrong; want %q in:\n%s", expectedComposite, completeXML)
			}
		})
	}
}

// TestMultipartCopyChecksumMismatch covers BadDigest when the client
// supplies a wrong `x-amz-checksum-<algo>` value alongside the algorithm.
func TestMultipartCopyChecksumMismatch(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	src := make([]byte, 1<<20)
	if _, err := rand.Read(src); err != nil {
		t.Fatal(err)
	}
	h.mustStatus(h.do("PUT", "/bkt/source", byteReader(src)), 200)

	resp := h.doString("POST", "/bkt/dst?uploads", "",
		"x-amz-checksum-algorithm", "SHA256")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	bogus := base64.StdEncoding.EncodeToString(make([]byte, 32))
	url := fmt.Sprintf("/bkt/dst?uploadId=%s&partNumber=1", uploadID)
	r := h.do("PUT", url, nil,
		"x-amz-copy-source", "/bkt/source",
		"x-amz-checksum-algorithm", "SHA256",
		"X-Amz-Checksum-Sha256", bogus)
	h.mustStatus(r, http.StatusBadRequest)
	if !strings.Contains(h.readBody(r), "BadDigest") {
		t.Errorf("expected BadDigest in body")
	}
}

// TestMultipartCopyNoChecksumHeader: without `x-amz-checksum-algorithm`,
// the gateway falls back to the multipart upload's algorithm so a
// subsequent COMPOSITE Complete still works (boto attaches the per-part
// header at UploadPart but may omit it on UploadPartCopy when the source
// already carries a digest).
func TestMultipartCopyInheritsUploadAlgorithm(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	src := make([]byte, 1<<20)
	if _, err := rand.Read(src); err != nil {
		t.Fatal(err)
	}
	h.mustStatus(h.do("PUT", "/bkt/source", byteReader(src)), 200)

	resp := h.doString("POST", "/bkt/dst?uploads", "",
		"x-amz-checksum-algorithm", "SHA256")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	url := fmt.Sprintf("/bkt/dst?uploadId=%s&partNumber=1", uploadID)
	r := h.do("PUT", url, nil, "x-amz-copy-source", "/bkt/source")
	h.mustStatus(r, 200)
	hh := sha256.New()
	hh.Write(src)
	want := base64.StdEncoding.EncodeToString(hh.Sum(nil))
	if got := r.Header.Get("X-Amz-Checksum-Sha256"); got != want {
		t.Errorf("inherit-algo echo: got %q want %q", got, want)
	}
}

// extractCopyPartETag pulls the ETag out of a CopyPartResult XML body.
// The element value is wrapped in quotes that are emitted as `&#34;` by
// encoding/xml — strip the entity, not individual chars (Trim with a
// cutset would eat hex digits).
func extractCopyPartETag(t *testing.T, body string) string {
	t.Helper()
	const open, close = "<ETag>", "</ETag>"
	i := strings.Index(body, open)
	j := strings.Index(body, close)
	if i < 0 || j < 0 || j <= i {
		t.Fatalf("no ETag in copy-part-result: %s", body)
	}
	v := body[i+len(open) : j]
	v = strings.ReplaceAll(v, "&#34;", "")
	return strings.Trim(v, `"`)
}
