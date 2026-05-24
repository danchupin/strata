package auth

import (
	"crypto/sha1"
	"crypto/sha256"
	"hash"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// goldenChunks reproduces the deterministic chunk set used by
// testdata/chunked-trailer-*.bin fixtures. Both the sha256 fixture (US-009)
// and the sha1 / crc32 / crc32c fixtures (US-004) use this exact byte layout
// so the decoder-vs-fixture cross-check stays aligned across algos.
func goldenChunks() [][]byte {
	return [][]byte{
		[]byte(repeat("Hello, US-009 trailer-aware streaming. ", 1024)),
		[]byte(repeat("Second chunk ", 64)),
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// TestRegenerateTrailerFixtures regenerates testdata/chunked-trailer-<algo>.bin
// for sha256 / sha1 / crc32 / crc32c from the deterministic streamSecret /
// streamSeedSig vector. Skipped unless STRATA_REGEN_TRAILER_FIXTURES=1 is set
// so CI never touches the binaries.
func TestRegenerateTrailerFixtures(t *testing.T) {
	if os.Getenv("STRATA_REGEN_TRAILER_FIXTURES") != "1" {
		t.Skip("set STRATA_REGEN_TRAILER_FIXTURES=1 to regenerate trailer fixtures")
	}
	chunks := goldenChunks()
	specs := []struct {
		path   string
		header string
		newH   func() hash.Hash
	}{
		{"testdata/chunked-trailer-sha256.bin", trailerHeaderChecksumSha256, sha256.New},
		{"testdata/chunked-trailer-sha1.bin", trailerHeaderChecksumSha1, sha1.New},
		{"testdata/chunked-trailer-crc32.bin", trailerHeaderChecksumCRC32, func() hash.Hash { return crc32.NewIEEE() }},
		{"testdata/chunked-trailer-crc32c.bin", trailerHeaderChecksumCRC32C, func() hash.Hash { return crc32.New(crc32CastagnoliTable) }},
	}
	for _, s := range specs {
		body, _ := awsStreamingTrailerBodyAlgo(chunks, s.header, s.newH, "", "")
		if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(s.path, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", s.path, err)
		}
		t.Logf("wrote %s (%d bytes)", s.path, len(body))
	}
}
