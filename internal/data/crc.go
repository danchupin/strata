package data

import (
	"fmt"
	"hash/crc32"
	"sync/atomic"
)

// crc32cTable is the Castagnoli CRC32C polynomial table, matching the
// algorithm AWS uses for x-amz-checksum-crc32c. Hardware-accelerated on
// amd64/arm64 via hash/crc32's SSE4.2 / NEON paths.
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// chunkCRCVerify gates read-path CRC32C verification. Default on; the
// gateway flips it from STRATA_CHUNK_CRC_VERIFY at startup. Stored as an
// atomic so the startup write never races the concurrent GET-path reads.
var chunkCRCVerify atomic.Bool

func init() { chunkCRCVerify.Store(true) }

// SetChunkCRCVerify enables/disables read-path chunk CRC32C verification.
// internal/serverapp reads STRATA_CHUNK_CRC_VERIFY once at startup and
// calls this; tests flip it as needed.
func SetChunkCRCVerify(on bool) { chunkCRCVerify.Store(on) }

// ChunkCRCVerifyEnabled reports whether read-path verification is on.
func ChunkCRCVerifyEnabled() bool { return chunkCRCVerify.Load() }

// ComputeChunkCRC returns the CRC32C (Castagnoli) of a chunk's bytes.
// Stamped onto ChunkRef.Checksum at PutChunks time.
func ComputeChunkCRC(b []byte) uint32 { return crc32.Checksum(b, crc32cTable) }

// VerifyChunk checks a chunk's read-back bytes against the CRC32C stored on
// its ChunkRef and returns ErrChecksumMismatch on a mismatch. It is a no-op
// (returns nil) when verification is disabled or the ref carries no checksum
// (ref.Checksum == 0, i.e. a pre-US-009 row). Backends MUST pass the FULL
// chunk bytes — partial (range-windowed) bytes would false-positive; the
// chunk-based backends read whole chunks and slice afterwards, so every
// chunk (including range-boundary chunks) is verified in full.
func VerifyChunk(ref ChunkRef, b []byte) error {
	if !chunkCRCVerify.Load() || ref.Checksum == 0 {
		return nil
	}
	if got := ComputeChunkCRC(b); got != ref.Checksum {
		return fmt.Errorf("%w: oid=%s want=%08x got=%08x", ErrChecksumMismatch, ref.OID, ref.Checksum, got)
	}
	return nil
}
