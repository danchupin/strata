package s3api_test

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/s3api"
)

const (
	checksumBucket  = "/cbkt"
	checksumPayload = "the quick brown fox jumps over the lazy dog"
)

func b64u32(v uint32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return base64.StdEncoding.EncodeToString(b[:])
}

func b64u64(v uint64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return base64.StdEncoding.EncodeToString(b[:])
}

func computeExpected(t *testing.T, algo, payload string) string {
	t.Helper()
	switch algo {
	case "CRC32":
		return b64u32(crc32.ChecksumIEEE([]byte(payload)))
	case "CRC32C":
		return b64u32(crc32.Checksum([]byte(payload), crc32.MakeTable(crc32.Castagnoli)))
	case "SHA1":
		s := sha1.Sum([]byte(payload))
		return base64.StdEncoding.EncodeToString(s[:])
	case "SHA256":
		s := sha256.Sum256([]byte(payload))
		return base64.StdEncoding.EncodeToString(s[:])
	case "CRC64NVME":
		return b64u64(s3api.CRC64NVMEForTest([]byte(payload)))
	default:
		t.Fatalf("unknown algo %s", algo)
		return ""
	}
}

func TestChecksumRoundTripPerAlgo(t *testing.T) {
	algos := []string{"CRC32", "CRC32C", "SHA1", "SHA256", "CRC64NVME"}
	for _, algo := range algos {
		algo := algo
		t.Run(algo, func(t *testing.T) {
			h := newHarness(t)
			h.mustStatus(h.doString("PUT", checksumBucket, ""), 200)

			hdr := "x-amz-checksum-" + strings.ToLower(algo)
			expected := computeExpected(t, algo, checksumPayload)

			resp := h.doString("PUT", checksumBucket+"/k", checksumPayload, hdr, expected)
			h.mustStatus(resp, 200)
			_ = h.readBody(resp)

			get := h.doString("GET", checksumBucket+"/k", "", "x-amz-checksum-mode", "ENABLED")
			h.mustStatus(get, 200)
			if got := get.Header.Get(hdr); got != expected {
				t.Fatalf("GET %s header: got %q want %q", hdr, got, expected)
			}
			_ = h.readBody(get)

			head := h.doString("HEAD", checksumBucket+"/k", "", "x-amz-checksum-mode", "ENABLED")
			h.mustStatus(head, 200)
			if got := head.Header.Get(hdr); got != expected {
				t.Fatalf("HEAD %s header: got %q want %q", hdr, got, expected)
			}
			_ = h.readBody(head)
		})
	}
}

func TestChecksumMismatchPerAlgo(t *testing.T) {
	algos := []string{"CRC32", "CRC32C", "SHA1", "SHA256", "CRC64NVME"}
	for _, algo := range algos {
		algo := algo
		t.Run(algo, func(t *testing.T) {
			h := newHarness(t)
			h.mustStatus(h.doString("PUT", checksumBucket, ""), 200)

			hdr := "x-amz-checksum-" + strings.ToLower(algo)
			// Wrong digest: hash of "different bytes" instead of payload.
			wrong := computeExpected(t, algo, "different bytes")

			resp := h.doString("PUT", checksumBucket+"/k", checksumPayload, hdr, wrong)
			h.mustStatus(resp, 400)
			body := h.readBody(resp)
			if !strings.Contains(body, "BadDigest") {
				t.Fatalf("expected BadDigest in body, got %s", body)
			}

			// Object must not be visible after a rejected put.
			h.mustStatus(h.doString("GET", checksumBucket+"/k", ""), 404)
		})
	}
}

func TestChecksumMissingHeaderOptional(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", checksumBucket, ""), 200)

	resp := h.doString("PUT", checksumBucket+"/k", checksumPayload)
	h.mustStatus(resp, 200)
	_ = h.readBody(resp)

	get := h.doString("GET", checksumBucket+"/k", "")
	h.mustStatus(get, 200)
	for _, algo := range []string{"crc32", "crc32c", "sha1", "sha256", "crc64nvme"} {
		if v := get.Header.Get("x-amz-checksum-" + algo); v != "" {
			t.Fatalf("unexpected checksum header %s=%s on no-checksum put", algo, v)
		}
	}
	_ = h.readBody(get)
}

func TestChecksumInvalidBase64Rejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", checksumBucket, ""), 200)

	resp := h.doString("PUT", checksumBucket+"/k", checksumPayload,
		"x-amz-checksum-crc32", "not-base64!!")
	h.mustStatus(resp, 400)
}

func TestCRC64NVMEKnownVector(t *testing.T) {
	// Per CRC-64/NVME spec: check value of "123456789" is 0xae8b14860a799888.
	got := s3api.CRC64NVMEForTest([]byte("123456789"))
	const want uint64 = 0xae8b14860a799888
	if got != want {
		t.Fatalf("CRC64NVME(123456789): got %#016x want %#016x", got, want)
	}
}
