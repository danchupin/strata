package s3api

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"net/http"
	"strings"
	"sync"
)

// supportedChecksumAlgorithms enumerates the AWS x-amz-checksum-* algorithms
// Strata accepts on PutObject. Names match the SDK ChecksumAlgorithm enum and
// the suffix on the request/response headers.
var supportedChecksumAlgorithms = []string{"CRC32", "CRC32C", "SHA1", "SHA256", "CRC64NVME"}

// ErrBadDigest signals that a x-amz-checksum-<algo> header value did not match
// the digest computed over the streamed PutObject body.
var ErrBadDigest = APIError{
	Code:    "BadDigest",
	Message: "The Content-MD5 or checksum value that you specified did not match what the server received",
	Status:  http.StatusBadRequest,
}

func newChecksumHasher(algo string) (hash.Hash, error) {
	switch strings.ToUpper(algo) {
	case "CRC32":
		return adapt32(crc32.NewIEEE()), nil
	case "CRC32C":
		return adapt32(crc32.New(crc32.MakeTable(crc32.Castagnoli))), nil
	case "SHA1":
		return sha1.New(), nil
	case "SHA256":
		return sha256.New(), nil
	case "CRC64NVME":
		return adapt64(newCRC64NVME()), nil
	default:
		return nil, fmt.Errorf("unsupported checksum algorithm %q", algo)
	}
}

type hash32Adapter struct{ hash.Hash32 }

func adapt32(h hash.Hash32) hash.Hash { return hash32Adapter{h} }

type hash64Adapter struct{ hash.Hash64 }

func adapt64(h hash.Hash64) hash.Hash { return hash64Adapter{h} }

// checksumEntry pairs a request-supplied digest with a per-algorithm hasher
// that fans out body bytes via checksumWriter on the streaming put path.
type checksumEntry struct {
	Algo     string
	Expected string
	Hasher   hash.Hash
}

// parseRequestChecksums extracts every x-amz-checksum-<algo> header for an
// algorithm in supportedChecksumAlgorithms. Unknown algorithm names are
// surfaced as ErrInvalidArgument by the caller; malformed base64 returns an
// error so the caller can short-circuit before reading the body.
func parseRequestChecksums(r *http.Request) ([]*checksumEntry, error) {
	var out []*checksumEntry
	for _, algo := range supportedChecksumAlgorithms {
		hdr := "x-amz-checksum-" + strings.ToLower(algo)
		v := r.Header.Get(hdr)
		if v == "" {
			continue
		}
		if _, err := base64.StdEncoding.DecodeString(v); err != nil {
			return nil, fmt.Errorf("invalid checksum encoding for %s", algo)
		}
		h, err := newChecksumHasher(algo)
		if err != nil {
			return nil, err
		}
		out = append(out, &checksumEntry{Algo: algo, Expected: v, Hasher: h})
	}
	return out, nil
}

// checksumWriter is an io.Writer that fans bytes out to every entry's hasher
// in a single pass. Used by io.TeeReader on the put path.
type checksumWriter []*checksumEntry

func (w checksumWriter) Write(p []byte) (int, error) {
	for _, e := range w {
		_, _ = e.Hasher.Write(p)
	}
	return len(p), nil
}

// verifyChecksums finalises every hasher and compares against the supplied
// expected digest. Returns the computed map[algo]base64 on success.
func verifyChecksums(entries []*checksumEntry) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		got := base64.StdEncoding.EncodeToString(e.Hasher.Sum(nil))
		if got != e.Expected {
			return nil, errors.New("checksum mismatch")
		}
		out[e.Algo] = got
	}
	return out, nil
}

// writeChecksumHeaders emits any persisted x-amz-checksum-<algo> headers on a
// GetObject/HeadObject response. Empty map is a no-op.
func writeChecksumHeaders(h http.Header, sums map[string]string) {
	for algo, val := range sums {
		if val == "" {
			continue
		}
		h.Set("x-amz-checksum-"+strings.ToLower(algo), val)
	}
}

// CRC-64/NVME — polynomial 0xad93d23594c93659 (reflected 0x9a6c9329ac4bc9b5),
// init 0xFFFFFFFFFFFFFFFF, RefIn=true, RefOut=true, XorOut 0xFFFFFFFFFFFFFFFF.
// Check value of "123456789" is 0xae8b14860a799888.
const crc64NVMEPoly = 0x9a6c9329ac4bc9b5

var (
	crc64NVMETable     [256]uint64
	crc64NVMETableOnce sync.Once
)

func crc64NVMETableInit() {
	for i := range 256 {
		crc := uint64(i)
		for range 8 {
			if crc&1 == 1 {
				crc = (crc >> 1) ^ crc64NVMEPoly
			} else {
				crc >>= 1
			}
		}
		crc64NVMETable[i] = crc
	}
}

type crc64NVMEHash struct{ crc uint64 }

func newCRC64NVME() hash.Hash64 {
	crc64NVMETableOnce.Do(crc64NVMETableInit)
	return &crc64NVMEHash{crc: 0xFFFFFFFFFFFFFFFF}
}

func (h *crc64NVMEHash) Write(p []byte) (int, error) {
	crc := h.crc
	for _, b := range p {
		crc = crc64NVMETable[byte(crc)^b] ^ (crc >> 8)
	}
	h.crc = crc
	return len(p), nil
}

func (h *crc64NVMEHash) Sum(b []byte) []byte {
	s := h.Sum64()
	return append(b,
		byte(s>>56), byte(s>>48), byte(s>>40), byte(s>>32),
		byte(s>>24), byte(s>>16), byte(s>>8), byte(s),
	)
}

func (h *crc64NVMEHash) Sum64() uint64 { return h.crc ^ 0xFFFFFFFFFFFFFFFF }
func (h *crc64NVMEHash) Reset()         { h.crc = 0xFFFFFFFFFFFFFFFF }
func (h *crc64NVMEHash) Size() int      { return 8 }
func (h *crc64NVMEHash) BlockSize() int { return 1 }

// CRC64NVMEForTest exposes the CRC-64/NVME implementation for unit tests so
// the published AWS test vector can be validated without re-deriving the
// table here. Not part of the s3api public surface.
func CRC64NVMEForTest(p []byte) uint64 {
	h := newCRC64NVME()
	_, _ = h.Write(p)
	return h.Sum64()
}
