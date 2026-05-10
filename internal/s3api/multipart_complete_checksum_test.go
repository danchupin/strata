package s3api_test

import (
	"fmt"
	"strings"
	"testing"
)

// uploadPartsWithChecksum uploads two parts of `payload[i]` under `algo`
// and returns the per-part base64 digests + ETags + Complete XML body.
func uploadPartsWithChecksum(t *testing.T, h *testHarness, uploadID, key, algo string, payloads [][]byte) (digests []string, body string) {
	t.Helper()
	hdr := "x-amz-checksum-" + strings.ToLower(algo)
	digests = make([]string, len(payloads))
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for i, p := range payloads {
		pn := i + 1
		digests[i] = partChecksum(t, algo, p)
		url := fmt.Sprintf("/bkt/%s?uploadId=%s&partNumber=%d", key, uploadID, pn)
		r := h.do("PUT", url, byteReader(p), hdr, digests[i])
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		fmt.Fprintf(&b, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pn, etag)
	}
	b.WriteString("</CompleteMultipartUpload>")
	return digests, b.String()
}

// TestMultipartCompleteCompositeChecksumMatch validates that a client-supplied
// composite x-amz-checksum-<algo> on Complete that matches the server-computed
// composite is accepted across all 4 algos required by US-010.
func TestMultipartCompleteCompositeChecksumMatch(t *testing.T) {
	for _, algo := range []string{"CRC32", "CRC32C", "SHA1", "SHA256"} {
		t.Run(algo, func(t *testing.T) {
			h := newHarness(t)
			h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
			resp := h.doString("POST", "/bkt/k?uploads", "",
				"x-amz-checksum-algorithm", algo,
				"x-amz-checksum-type", "COMPOSITE")
			h.mustStatus(resp, 200)
			uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

			payloads := [][]byte{
				[]byte(strings.Repeat("A", 1024)),
				[]byte(strings.Repeat("B", 1024)),
			}
			digests, body := uploadPartsWithChecksum(t, h, uploadID, "k", algo, payloads)
			want := composite(t, algo, digests)

			hdr := "x-amz-checksum-" + strings.ToLower(algo)
			resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, body, hdr, want)
			h.mustStatus(resp, 200)
			if got := resp.Header.Get(hdr); got != want {
				t.Fatalf("complete echo header: got %q want %q", got, want)
			}
		})
	}
}

// TestMultipartCompleteCompositeChecksumMismatch validates that a wrong
// client-supplied composite returns BadDigest before the LWT flip — a
// subsequent Complete without the bad header still completes successfully,
// proving no `completing` state leaked.
func TestMultipartCompleteCompositeChecksumMismatch(t *testing.T) {
	for _, algo := range []string{"CRC32", "CRC32C", "SHA1", "SHA256"} {
		t.Run(algo, func(t *testing.T) {
			h := newHarness(t)
			h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
			resp := h.doString("POST", "/bkt/k?uploads", "",
				"x-amz-checksum-algorithm", algo,
				"x-amz-checksum-type", "COMPOSITE")
			h.mustStatus(resp, 200)
			uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

			payloads := [][]byte{
				[]byte(strings.Repeat("A", 1024)),
				[]byte(strings.Repeat("B", 1024)),
			}
			digests, body := uploadPartsWithChecksum(t, h, uploadID, "k", algo, payloads)
			want := composite(t, algo, digests)
			bogus := composite(t, algo, []string{digests[1], digests[0]})
			if bogus == want {
				t.Fatalf("test setup: bogus composite collided with correct one")
			}

			hdr := "x-amz-checksum-" + strings.ToLower(algo)
			resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, body, hdr, bogus)
			h.mustStatus(resp, 400)
			if !strings.Contains(h.readBody(resp), "<Code>BadDigest</Code>") {
				t.Fatalf("expected BadDigest on composite mismatch")
			}

			// The LWT did not flip — retry without the bad header should succeed.
			resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, body)
			h.mustStatus(resp, 200)
			if got := resp.Header.Get(hdr); got != want {
				t.Fatalf("retry composite header: got %q want %q", got, want)
			}
		})
	}
}

// TestMultipartCompleteCompositeChecksumMissingAlgo verifies that a client
// supplying x-amz-checksum-<algo> for an algo none of the parts carried
// returns BadDigest (composite[algo] is empty).
func TestMultipartCompleteCompositeChecksumMissingAlgo(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("POST", "/bkt/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	// Upload parts WITH SHA256, but Complete claims a SHA1 composite.
	payloads := [][]byte{
		[]byte(strings.Repeat("A", 1024)),
		[]byte(strings.Repeat("B", 1024)),
	}
	_, body := uploadPartsWithChecksum(t, h, uploadID, "k", "SHA256", payloads)

	bogus := composite(t, "SHA1", []string{
		partChecksum(t, "SHA1", payloads[0]),
		partChecksum(t, "SHA1", payloads[1]),
	})
	resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, body,
		"x-amz-checksum-sha1", bogus)
	h.mustStatus(resp, 400)
	if !strings.Contains(h.readBody(resp), "<Code>BadDigest</Code>") {
		t.Fatalf("expected BadDigest when client claims algo not present in parts")
	}
}
