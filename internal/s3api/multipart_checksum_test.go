package s3api_test

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"
)

func partChecksum(t *testing.T, algo string, payload []byte) string {
	t.Helper()
	switch algo {
	case "CRC32":
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], crc32.ChecksumIEEE(payload))
		return base64.StdEncoding.EncodeToString(b[:])
	case "CRC32C":
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], crc32.Checksum(payload, crc32.MakeTable(crc32.Castagnoli)))
		return base64.StdEncoding.EncodeToString(b[:])
	case "SHA1":
		s := sha1.Sum(payload)
		return base64.StdEncoding.EncodeToString(s[:])
	case "SHA256":
		s := sha256.Sum256(payload)
		return base64.StdEncoding.EncodeToString(s[:])
	default:
		t.Fatalf("unknown algo %s", algo)
		return ""
	}
}

// composite reproduces composeMultipartChecksums for the test side: take raw
// per-part digests, hash their concatenation, encode, append "-N".
func composite(t *testing.T, algo string, partB64 []string) string {
	t.Helper()
	var raw []byte
	for _, b := range partB64 {
		d, err := base64.StdEncoding.DecodeString(b)
		if err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
		raw = append(raw, d...)
	}
	switch algo {
	case "CRC32":
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], crc32.ChecksumIEEE(raw))
		return base64.StdEncoding.EncodeToString(b[:]) + fmt.Sprintf("-%d", len(partB64))
	case "CRC32C":
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], crc32.Checksum(raw, crc32.MakeTable(crc32.Castagnoli)))
		return base64.StdEncoding.EncodeToString(b[:]) + fmt.Sprintf("-%d", len(partB64))
	case "SHA1":
		s := sha1.Sum(raw)
		return base64.StdEncoding.EncodeToString(s[:]) + fmt.Sprintf("-%d", len(partB64))
	case "SHA256":
		s := sha256.Sum256(raw)
		return base64.StdEncoding.EncodeToString(s[:]) + fmt.Sprintf("-%d", len(partB64))
	default:
		t.Fatalf("unknown algo %s", algo)
		return ""
	}
}

func TestMultipartChecksumRoundTripCRC32(t *testing.T) {
	const algo = "CRC32"
	hdr := "x-amz-checksum-" + strings.ToLower(algo)

	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/mpc", ""), 200)

	resp := h.doString("POST", "/mpc/k?uploads", "")
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
		url := fmt.Sprintf("/mpc/k?uploadId=%s&partNumber=%d", uploadID, pn)
		r := h.do("PUT", url, byteReader(p), hdr, partB64[i])
		h.mustStatus(r, 200)
		if got := r.Header.Get(hdr); got != partB64[i] {
			t.Fatalf("part %d echo header: got %q want %q", pn, got, partB64[i])
		}
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		fmt.Fprintf(&completeBody, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pn, etag)
	}
	completeBody.WriteString("</CompleteMultipartUpload>")

	want := composite(t, algo, partB64)
	resp = h.doString("POST", "/mpc/k?uploadId="+uploadID, completeBody.String())
	h.mustStatus(resp, 200)
	if got := resp.Header.Get(hdr); got != want {
		t.Fatalf("complete header: got %q want %q", got, want)
	}
	body := h.readBody(resp)
	if !strings.Contains(body, "<ChecksumCRC32>"+want+"</ChecksumCRC32>") {
		t.Fatalf("complete body missing ChecksumCRC32=%s: %s", want, body)
	}

	// Object echoes the composite on subsequent GET / HEAD.
	get := h.doString("GET", "/mpc/k", "")
	h.mustStatus(get, 200)
	if got := get.Header.Get(hdr); got != want {
		t.Fatalf("GET composite header: got %q want %q", got, want)
	}
	_ = h.readBody(get)
}

func TestMultipartUploadPartChecksumMismatch(t *testing.T) {
	const algo = "CRC32"
	hdr := "x-amz-checksum-" + strings.ToLower(algo)

	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/mpc", ""), 200)
	resp := h.doString("POST", "/mpc/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	wrong := partChecksum(t, algo, []byte("not the payload"))
	r := h.do("PUT", fmt.Sprintf("/mpc/k?uploadId=%s&partNumber=1", uploadID),
		byteReader([]byte("hello world")), hdr, wrong)
	h.mustStatus(r, 400)
	if !strings.Contains(h.readBody(r), "BadDigest") {
		t.Fatal("expected BadDigest on per-part checksum mismatch")
	}

	// Listing parts should show the upload but no parts persisted.
	list := h.doString("GET", "/mpc/k?uploadId="+uploadID, "")
	h.mustStatus(list, 200)
	if strings.Contains(h.readBody(list), "<Part>") {
		t.Fatal("rejected part still persisted in ListParts")
	}
}

func TestMultipartCompleteWithoutChecksums(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/mpc", ""), 200)
	resp := h.doString("POST", "/mpc/k?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	var completeBody strings.Builder
	completeBody.WriteString("<CompleteMultipartUpload>")
	for i := 1; i <= 2; i++ {
		r := h.do("PUT", fmt.Sprintf("/mpc/k?uploadId=%s&partNumber=%d", uploadID, i),
			byteReader([]byte(strings.Repeat("x", 16))))
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		fmt.Fprintf(&completeBody, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, i, etag)
	}
	completeBody.WriteString("</CompleteMultipartUpload>")
	resp = h.doString("POST", "/mpc/k?uploadId="+uploadID, completeBody.String())
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	for _, algo := range []string{"crc32", "crc32c", "sha1", "sha256", "crc64nvme"} {
		if v := resp.Header.Get("x-amz-checksum-" + algo); v != "" {
			t.Fatalf("unexpected composite header %s=%s when no part checksums set", algo, v)
		}
	}
	if strings.Contains(body, "<Checksum") {
		t.Fatalf("unexpected Checksum* in body when no part checksums set: %s", body)
	}
}
