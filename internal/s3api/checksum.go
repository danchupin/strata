package s3api

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
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

// checksumModeEnabled returns true when the request's x-amz-checksum-mode
// header is "ENABLED" (case-insensitive). AWS gates checksum echo on
// HeadObject/GetObject responses behind this header — without it, no
// x-amz-checksum-* / x-amz-checksum-type headers should appear, even when
// the object has stored checksums.
func checksumModeEnabled(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), "ENABLED")
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

// composeMultipartChecksums computes the per-algorithm checksum echoed on
// CompleteMultipartUpload. checksumType selects between AWS's two shapes:
//   - "" / "COMPOSITE": BASE64(HASH(concat(rawDigest_i)))-N — concat raw
//     per-part digests, hash the concat, append "-N".
//   - "FULL_OBJECT": for the CRC family (CRC32 / CRC32C / CRC64NVME) compute
//     the CRC of the concatenated bodies via standard CRC-combine math from
//     each part's stored CRC + size; emit the raw base64 with no "-N" suffix.
//     SHA1 / SHA256 fall back to COMPOSITE — AWS rejects SHA + FULL_OBJECT on
//     Initiate, so this branch is unreachable in practice and we keep the
//     response well-formed if a misbehaving client gets here.
//
// Algorithms missing on any part are skipped. Returns nil when no algorithm is
// universally present.
func composeMultipartChecksums(parts []*checksumPart, checksumType string) map[string]string {
	if len(parts) == 0 {
		return nil
	}
	out := map[string]string{}
	fullObject := strings.EqualFold(strings.TrimSpace(checksumType), "FULL_OBJECT")
	for _, algo := range supportedChecksumAlgorithms {
		raws := make([][]byte, 0, len(parts))
		ok := true
		for _, p := range parts {
			v, has := p.Checksums[algo]
			if !has || v == "" {
				ok = false
				break
			}
			raw, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				ok = false
				break
			}
			raws = append(raws, raw)
		}
		if !ok {
			continue
		}
		if fullObject && isCRCAlgo(algo) {
			combined, err := combineCRCParts(algo, raws, parts)
			if err != nil {
				continue
			}
			out[algo] = combined
			continue
		}
		h, err := newChecksumHasher(algo)
		if err != nil {
			continue
		}
		for _, r := range raws {
			_, _ = h.Write(r)
		}
		out[algo] = fmt.Sprintf("%s-%d", base64.StdEncoding.EncodeToString(h.Sum(nil)), len(parts))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// checksumPart is the per-part input to composeMultipartChecksums; just the
// fields we need so the helper does not depend on the meta package. Size is
// the plaintext byte length of the part — required by the FULL_OBJECT CRC
// combine path.
type checksumPart struct {
	Checksums map[string]string
	Size      int64
}

// isCRCAlgo reports whether algo is one of the three CRC variants AWS supports
// for the FULL_OBJECT checksum shape on multipart uploads.
func isCRCAlgo(algo string) bool {
	switch algo {
	case "CRC32", "CRC32C", "CRC64NVME":
		return true
	}
	return false
}

// CRC polynomials in zlib's reversed/right-shift convention used by
// hash/crc32 and our CRC-64/NVME implementation. crc32_combine math operates
// in this representation.
const (
	crc32IEEEPolyRev   uint32 = 0xedb88320
	crc32CastagnoliRev uint32 = 0x82f63b78
)

// combineCRCParts produces the FULL_OBJECT CRC by chaining each part's stored
// CRC via the standard zlib crc32_combine / crc64_combine algorithm. The
// returned value is the base64 of the combined CRC's big-endian bytes — no
// "-N" suffix (AWS's FULL_OBJECT shape).
func combineCRCParts(algo string, raws [][]byte, parts []*checksumPart) (string, error) {
	if len(raws) != len(parts) {
		return "", fmt.Errorf("composeMultipartChecksums: parts/raws length mismatch")
	}
	switch algo {
	case "CRC32":
		var combined uint32
		for i, r := range raws {
			if len(r) != 4 {
				return "", fmt.Errorf("CRC32: per-part digest must be 4 bytes, got %d", len(r))
			}
			crc := binary.BigEndian.Uint32(r)
			if i == 0 {
				combined = crc
			} else {
				combined = crc32Combine(crc32IEEEPolyRev, combined, crc, parts[i].Size)
			}
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], combined)
		return base64.StdEncoding.EncodeToString(b[:]), nil
	case "CRC32C":
		var combined uint32
		for i, r := range raws {
			if len(r) != 4 {
				return "", fmt.Errorf("CRC32C: per-part digest must be 4 bytes, got %d", len(r))
			}
			crc := binary.BigEndian.Uint32(r)
			if i == 0 {
				combined = crc
			} else {
				combined = crc32Combine(crc32CastagnoliRev, combined, crc, parts[i].Size)
			}
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], combined)
		return base64.StdEncoding.EncodeToString(b[:]), nil
	case "CRC64NVME":
		var combined uint64
		for i, r := range raws {
			if len(r) != 8 {
				return "", fmt.Errorf("CRC64NVME: per-part digest must be 8 bytes, got %d", len(r))
			}
			crc := binary.BigEndian.Uint64(r)
			if i == 0 {
				combined = crc
			} else {
				combined = crc64Combine(crc64NVMEPoly, combined, crc, parts[i].Size)
			}
		}
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], combined)
		return base64.StdEncoding.EncodeToString(b[:]), nil
	}
	return "", fmt.Errorf("combineCRCParts: unsupported algo %q", algo)
}

// crc32Combine implements zlib's crc32_combine: given two finalized CRC values
// crc1, crc2 and the byte length of the second segment len2, returns the CRC
// of the concatenation. Operates in the reversed-polynomial representation
// (the right-shift convention used by hash/crc32). Math is GF(2)[x] / poly.
func crc32Combine(poly, crc1, crc2 uint32, len2 int64) uint32 {
	if len2 <= 0 {
		return crc1
	}
	var even, odd [32]uint32
	odd[0] = poly
	row := uint32(1)
	for n := 1; n < 32; n++ {
		odd[n] = row
		row <<= 1
	}
	gf2MatrixSquare32(&even, &odd)
	gf2MatrixSquare32(&odd, &even)
	for {
		gf2MatrixSquare32(&even, &odd)
		if len2&1 != 0 {
			crc1 = gf2MatrixTimes32(&even, crc1)
		}
		len2 >>= 1
		if len2 == 0 {
			break
		}
		gf2MatrixSquare32(&odd, &even)
		if len2&1 != 0 {
			crc1 = gf2MatrixTimes32(&odd, crc1)
		}
		len2 >>= 1
		if len2 == 0 {
			break
		}
	}
	return crc1 ^ crc2
}

func gf2MatrixTimes32(mat *[32]uint32, vec uint32) uint32 {
	var sum uint32
	i := 0
	for vec != 0 {
		if vec&1 != 0 {
			sum ^= mat[i]
		}
		vec >>= 1
		i++
	}
	return sum
}

func gf2MatrixSquare32(square, mat *[32]uint32) {
	for n := 0; n < 32; n++ {
		square[n] = gf2MatrixTimes32(mat, mat[n])
	}
}

// crc64Combine is the 64-bit twin of crc32Combine. Used to compose CRC-64/NVME
// per-part values into the whole-stream CRC.
func crc64Combine(poly, crc1, crc2 uint64, len2 int64) uint64 {
	if len2 <= 0 {
		return crc1
	}
	var even, odd [64]uint64
	odd[0] = poly
	row := uint64(1)
	for n := 1; n < 64; n++ {
		odd[n] = row
		row <<= 1
	}
	gf2MatrixSquare64(&even, &odd)
	gf2MatrixSquare64(&odd, &even)
	for {
		gf2MatrixSquare64(&even, &odd)
		if len2&1 != 0 {
			crc1 = gf2MatrixTimes64(&even, crc1)
		}
		len2 >>= 1
		if len2 == 0 {
			break
		}
		gf2MatrixSquare64(&odd, &even)
		if len2&1 != 0 {
			crc1 = gf2MatrixTimes64(&odd, crc1)
		}
		len2 >>= 1
		if len2 == 0 {
			break
		}
	}
	return crc1 ^ crc2
}

func gf2MatrixTimes64(mat *[64]uint64, vec uint64) uint64 {
	var sum uint64
	i := 0
	for vec != 0 {
		if vec&1 != 0 {
			sum ^= mat[i]
		}
		vec >>= 1
		i++
	}
	return sum
}

func gf2MatrixSquare64(square, mat *[64]uint64) {
	for n := 0; n < 64; n++ {
		square[n] = gf2MatrixTimes64(mat, mat[n])
	}
}

// CombineCRCPartsForTest exposes combineCRCParts so unit tests can lock in
// the s3-tests-published vectors without going through the full multipart
// HTTP harness. partSizes[i] is the byte length of partB64[i]; both slices
// must be the same length.
func CombineCRCPartsForTest(algo string, partB64 []string, partSizes []int64) (string, error) {
	if len(partB64) != len(partSizes) {
		return "", fmt.Errorf("partB64/partSizes length mismatch")
	}
	raws := make([][]byte, len(partB64))
	parts := make([]*checksumPart, len(partB64))
	for i, b := range partB64 {
		r, err := base64.StdEncoding.DecodeString(b)
		if err != nil {
			return "", fmt.Errorf("decode part %d: %w", i, err)
		}
		raws[i] = r
		parts[i] = &checksumPart{Size: partSizes[i]}
	}
	return combineCRCParts(algo, raws, parts)
}

// CRC64NVMEForTest exposes the CRC-64/NVME implementation for unit tests so
// the published AWS test vector can be validated without re-deriving the
// table here. Not part of the s3api public surface.
func CRC64NVMEForTest(p []byte) uint64 {
	h := newCRC64NVME()
	_, _ = h.Write(p)
	return h.Sum64()
}
