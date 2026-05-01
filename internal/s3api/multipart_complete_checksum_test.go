package s3api_test

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"hash/crc32"
	"strings"
	"testing"
)

// TestMultipartCompleteChecksumValidates covers US-010: a client-supplied
// composite `x-amz-checksum-<algo>` on CompleteMultipartUpload must be
// validated against the recomputed COMPOSITE digest. Matching value lets
// the Complete proceed (200); mismatch returns 400 BadDigest BEFORE the
// LWT flip.
func TestMultipartCompleteChecksumValidates(t *testing.T) {
	cases := []struct {
		name    string
		algo    string
		hdrName string
		newHash func() hash.Hash
	}{
		{"SHA256", "SHA256", "x-amz-checksum-sha256", sha256.New},
		{"SHA1", "SHA1", "x-amz-checksum-sha1", sha1.New},
		{"CRC32", "CRC32", "x-amz-checksum-crc32", func() hash.Hash { return crc32.NewIEEE() }},
		{"CRC32C", "CRC32C", "x-amz-checksum-crc32c", func() hash.Hash {
			return crc32.New(crc32.MakeTable(crc32.Castagnoli))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name+"_match", func(t *testing.T) {
			h := newHarness(t)
			h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

			resp := h.doString("POST", "/bkt/k?uploads", "",
				"x-amz-checksum-algorithm", tc.algo)
			h.mustStatus(resp, 200)
			uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

			const partSize = 5 << 20
			partBodies := make([][]byte, 2)
			partDigests := make([][]byte, 2)
			var completeBody strings.Builder
			completeBody.WriteString("<CompleteMultipartUpload>")
			for i := range partBodies {
				partBodies[i] = make([]byte, partSize)
				if _, err := rand.Read(partBodies[i]); err != nil {
					t.Fatal(err)
				}
				ph := tc.newHash()
				ph.Write(partBodies[i])
				partDigests[i] = ph.Sum(nil)
				url := fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=%d", uploadID, i+1)
				r := h.do("PUT", url, byteReader(partBodies[i]),
					tc.hdrName, base64.StdEncoding.EncodeToString(partDigests[i]))
				h.mustStatus(r, 200)
				etag := strings.Trim(r.Header.Get("Etag"), `"`)
				completeBody.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, i+1, etag))
			}
			completeBody.WriteString("</CompleteMultipartUpload>")

			expected := compositeExpected(tc.newHash, partDigests)
			cr := h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody.String(),
				tc.hdrName, expected)
			h.mustStatus(cr, 200)
		})

		t.Run(tc.name+"_mismatch", func(t *testing.T) {
			h := newHarness(t)
			h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

			resp := h.doString("POST", "/bkt/k?uploads", "",
				"x-amz-checksum-algorithm", tc.algo)
			h.mustStatus(resp, 200)
			uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

			const partSize = 5 << 20
			partBodies := make([][]byte, 2)
			var completeBody strings.Builder
			completeBody.WriteString("<CompleteMultipartUpload>")
			for i := range partBodies {
				partBodies[i] = make([]byte, partSize)
				if _, err := rand.Read(partBodies[i]); err != nil {
					t.Fatal(err)
				}
				ph := tc.newHash()
				ph.Write(partBodies[i])
				digest := ph.Sum(nil)
				url := fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=%d", uploadID, i+1)
				r := h.do("PUT", url, byteReader(partBodies[i]),
					tc.hdrName, base64.StdEncoding.EncodeToString(digest))
				h.mustStatus(r, 200)
				etag := strings.Trim(r.Header.Get("Etag"), `"`)
				completeBody.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, i+1, etag))
			}
			completeBody.WriteString("</CompleteMultipartUpload>")

			// Bad digest: base64 of zeros (right size for the algo, wrong value).
			bad := base64.StdEncoding.EncodeToString(make([]byte, tc.newHash().Size())) + "-2"
			cr := h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody.String(),
				tc.hdrName, bad)
			h.mustStatus(cr, 400)
			if !strings.Contains(h.readBody(cr), "BadDigest") {
				t.Errorf("expected BadDigest in error body")
			}
			// LWT did not flip: the upload still exists, no object materialised.
			h.mustStatus(h.doString("HEAD", "/bkt/k", ""), 404)
		})
	}
}
