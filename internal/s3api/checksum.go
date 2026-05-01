package s3api

import (
	"encoding/base64"
	"strconv"
	"strings"

	"crypto/sha1"
	"crypto/sha256"
	"hash"
	"hash/crc32"
)

// normalizeChecksumAlgo upper-cases and validates a FlexibleChecksum
// algorithm name. Returns "" when the input is empty or unknown.
func normalizeChecksumAlgo(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "CRC32":
		return "CRC32"
	case "CRC32C":
		return "CRC32C"
	case "SHA1":
		return "SHA1"
	case "SHA256":
		return "SHA256"
	}
	return ""
}

// newChecksumHasher returns a fresh hash.Hash for the given canonical
// algorithm name (CRC32 / CRC32C / SHA1 / SHA256) and the corresponding
// `x-amz-checksum-<algo>` request/response header name (lowercased
// algorithm). Returns (nil, "") when the algo is unrecognised.
func newChecksumHasher(algo string) (hash.Hash, string) {
	switch algo {
	case "CRC32":
		return crc32.NewIEEE(), "x-amz-checksum-crc32"
	case "CRC32C":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli)), "x-amz-checksum-crc32c"
	case "SHA1":
		return sha1.New(), "x-amz-checksum-sha1"
	case "SHA256":
		return sha256.New(), "x-amz-checksum-sha256"
	}
	return nil, ""
}

// checksumHeader returns the response/request header name for an algo,
// or "" when unknown.
func checksumHeader(algo string) string {
	_, hdr := newChecksumHasher(algo)
	return hdr
}

// compositeChecksum computes the COMPOSITE multipart checksum the AWS SDK
// expects on CompleteMultipartUploadResult. Each part's raw digest (the
// base64-decoded `x-amz-checksum-<algo>` value supplied on UploadPart) is
// concatenated in part-number order; the named hash function is applied
// to the concatenation; the resulting digest is base64-encoded and
// suffixed with `-<numparts>`. Returns ("", false) when any part is
// missing a checksum or carries a different algorithm — caller should
// treat that as "no composite available" and skip the response shape.
func compositeChecksum(algo string, parts []partChecksum) (string, bool) {
	h, _ := newChecksumHasher(algo)
	if h == nil {
		return "", false
	}
	for _, p := range parts {
		if p.algo != algo || p.value == "" {
			return "", false
		}
		raw, err := base64.StdEncoding.DecodeString(p.value)
		if err != nil {
			return "", false
		}
		h.Write(raw)
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)) + "-" + strconv.Itoa(len(parts)), true
}

// partChecksum is the (algorithm, base64-digest) pair captured per part on
// UploadPart and consumed by the composite computation at Complete time.
type partChecksum struct {
	algo  string
	value string
}
