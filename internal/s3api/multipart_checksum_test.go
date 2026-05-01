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
	"strconv"
	"strings"
	"testing"
)

// TestMultipartFlexibleChecksumComposite covers US-003: when CreateMultipartUpload
// sets `x-amz-checksum-algorithm` (default COMPOSITE), the gateway must:
//   - capture each per-part `x-amz-checksum-<algo>` header on UploadPart;
//   - compute the COMPOSITE hash-of-hashes per AWS formula on Complete;
//   - return <ChecksumSHA256>/<ChecksumType> in the CompleteMultipartUploadResult;
//   - echo the composite back on HEAD when `x-amz-checksum-mode: ENABLED`;
//   - echo the per-part digest on HEAD ?partNumber=N + ChecksumMode=ENABLED.
func TestMultipartFlexibleChecksumComposite(t *testing.T) {
	cases := []struct {
		name     string
		algo     string
		hdrName  string
		newHash  func() hash.Hash
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

			// Initiate with checksum-algorithm.
			resp := h.doString("POST", "/bkt/k?uploads", "",
				"x-amz-checksum-algorithm", tc.algo)
			h.mustStatus(resp, 200)
			uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

			const partSize = 5 << 20
			partBodies := make([][]byte, 3)
			partDigests := make([][]byte, 3)
			partChecksumB64 := make([]string, 3)
			var completeBody strings.Builder
			completeBody.WriteString("<CompleteMultipartUpload>")
			for i := range partBodies {
				partBodies[i] = make([]byte, partSize)
				if _, err := rand.Read(partBodies[i]); err != nil {
					t.Fatal(err)
				}
				hh := tc.newHash()
				hh.Write(partBodies[i])
				digest := hh.Sum(nil)
				partDigests[i] = digest
				partChecksumB64[i] = base64.StdEncoding.EncodeToString(digest)
				url := fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=%d", uploadID, i+1)
				r := h.do("PUT", url, byteReader(partBodies[i]),
					tc.hdrName, partChecksumB64[i])
				h.mustStatus(r, 200)
				etag := strings.Trim(r.Header.Get("Etag"), `"`)
				completeBody.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, i+1, etag))
				if got := r.Header.Get(tc.hdrName); got != partChecksumB64[i] {
					t.Errorf("upload-part echo: got %q want %q", got, partChecksumB64[i])
				}
			}
			completeBody.WriteString("</CompleteMultipartUpload>")

			// Complete returns composite checksum.
			cr := h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody.String())
			h.mustStatus(cr, 200)
			completeXML := h.readBody(cr)
			expectedComposite := compositeExpected(tc.newHash, partDigests)
			if !strings.Contains(completeXML, "<Checksum"+tc.algo+">"+expectedComposite+"</Checksum"+tc.algo+">") {
				t.Errorf("composite checksum missing or wrong; want %q in:\n%s", expectedComposite, completeXML)
			}
			if !strings.Contains(completeXML, "<ChecksumType>COMPOSITE</ChecksumType>") {
				t.Errorf("ChecksumType missing or wrong:\n%s", completeXML)
			}

			// HEAD with ChecksumMode=ENABLED returns composite.
			head := h.doString("HEAD", "/bkt/k", "", "x-amz-checksum-mode", "ENABLED")
			h.mustStatus(head, http.StatusOK)
			if got := head.Header.Get(tc.hdrName); got != expectedComposite {
				t.Errorf("HEAD composite: got %q want %q", got, expectedComposite)
			}
			if got := head.Header.Get("X-Amz-Checksum-Type"); got != "COMPOSITE" {
				t.Errorf("HEAD ChecksumType: got %q want COMPOSITE", got)
			}

			// HEAD without ChecksumMode header omits the checksum.
			head2 := h.doString("HEAD", "/bkt/k", "")
			if got := head2.Header.Get(tc.hdrName); got != "" {
				t.Errorf("HEAD w/o ChecksumMode: should be empty, got %q", got)
			}

			// HEAD ?partNumber=N + ENABLED returns per-part checksum.
			ph := h.doString("HEAD", "/bkt/k?partNumber=2", "",
				"x-amz-checksum-mode", "ENABLED")
			h.mustStatus(ph, http.StatusPartialContent)
			if got := ph.Header.Get(tc.hdrName); got != partChecksumB64[1] {
				t.Errorf("HEAD partNumber=2 checksum: got %q want %q", got, partChecksumB64[1])
			}
		})
	}
}

// TestMultipartFullObjectChecksum covers FULL_OBJECT type: the client
// supplies the whole-object digest on CompleteMultipartUpload via
// `x-amz-checksum-<algo>`; the gateway echoes it in the response and on
// HEAD with ChecksumMode=ENABLED.
func TestMultipartFullObjectChecksum(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads", "",
		"x-amz-checksum-algorithm", "CRC32",
		"x-amz-checksum-type", "FULL_OBJECT")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	const partSize = 5 << 20
	partBodies := make([][]byte, 2)
	whole := crc32.NewIEEE()
	var completeBody strings.Builder
	completeBody.WriteString("<CompleteMultipartUpload>")
	for i := range partBodies {
		partBodies[i] = make([]byte, partSize)
		if _, err := rand.Read(partBodies[i]); err != nil {
			t.Fatal(err)
		}
		whole.Write(partBodies[i])
		url := fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=%d", uploadID, i+1)
		r := h.do("PUT", url, byteReader(partBodies[i]))
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		completeBody.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, i+1, etag))
	}
	completeBody.WriteString("</CompleteMultipartUpload>")

	wholeDigest := base64.StdEncoding.EncodeToString(whole.Sum(nil))
	cr := h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody.String(),
		"x-amz-checksum-crc32", wholeDigest)
	h.mustStatus(cr, 200)
	completeXML := h.readBody(cr)
	if !strings.Contains(completeXML, "<ChecksumCRC32>"+wholeDigest+"</ChecksumCRC32>") {
		t.Errorf("full-object checksum missing; want %q in:\n%s", wholeDigest, completeXML)
	}
	if !strings.Contains(completeXML, "<ChecksumType>FULL_OBJECT</ChecksumType>") {
		t.Errorf("ChecksumType missing FULL_OBJECT:\n%s", completeXML)
	}

	head := h.doString("HEAD", "/bkt/k", "", "x-amz-checksum-mode", "ENABLED")
	h.mustStatus(head, http.StatusOK)
	if got := head.Header.Get("X-Amz-Checksum-Crc32"); got != wholeDigest {
		t.Errorf("HEAD whole checksum: got %q want %q", got, wholeDigest)
	}
}

// TestMultipartChecksumOptOut: an Initiate without checksum-algorithm must
// not stamp checksum fields on the manifest, and HEAD with ChecksumMode=ENABLED
// must NOT add a checksum header.
func TestMultipartChecksumOptOut(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	const partSize = 5 << 20
	partBodies := make([][]byte, 2)
	var completeBody strings.Builder
	completeBody.WriteString("<CompleteMultipartUpload>")
	for i := range partBodies {
		partBodies[i] = make([]byte, partSize)
		_, _ = rand.Read(partBodies[i])
		url := fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=%d", uploadID, i+1)
		r := h.do("PUT", url, byteReader(partBodies[i]))
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		completeBody.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, i+1, etag))
	}
	completeBody.WriteString("</CompleteMultipartUpload>")

	cr := h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody.String())
	h.mustStatus(cr, 200)
	body := h.readBody(cr)
	if strings.Contains(body, "<Checksum") {
		t.Errorf("opt-out should omit checksum tags; got:\n%s", body)
	}

	head := h.doString("HEAD", "/bkt/k", "", "x-amz-checksum-mode", "ENABLED")
	h.mustStatus(head, http.StatusOK)
	for _, hdr := range []string{"X-Amz-Checksum-Sha256", "X-Amz-Checksum-Sha1", "X-Amz-Checksum-Crc32", "X-Amz-Checksum-Crc32c"} {
		if v := head.Header.Get(hdr); v != "" {
			t.Errorf("opt-out HEAD: %s should be empty, got %q", hdr, v)
		}
	}
}

func compositeExpected(newHash func() hash.Hash, parts [][]byte) string {
	hh := newHash()
	for _, p := range parts {
		hh.Write(p)
	}
	return base64.StdEncoding.EncodeToString(hh.Sum(nil)) + "-" + strconv.Itoa(len(parts))
}
