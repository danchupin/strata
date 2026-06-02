package data

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Chunk back-reference (US-001 metadata-data-reconcile).
//
// Strata splits the S3-key→chunk map (meta tier) from the chunk bytes
// (data tier). That split is the scale win, but it drops the safety net
// RGW gets by colocating its index with the data: a RADOS chunk is an
// opaque random-OID blob with NO pointer back to its owning object, so the
// map is one-directional (meta→data). A restore skew then silently leaks
// orphan chunks (GC walks meta→data and can't see them) or leaves dangling
// manifests, and a lost meta backup makes the intact bytes unreadable.
//
// The back-reference makes every chunk self-describing: a small,
// versioned, key-material-free pointer to {bucket, key, version, chunk
// index, mtime} stamped on the chunk at PUT. It is what the reconcile
// worker (US-002/003) and `strata admin rebuild-index` (US-004) read to
// realign the two tiers from data alone.

const (
	// BackrefXattrName is the librados xattr key under which the RADOS
	// backend stores the per-chunk back-reference payload, written in the
	// SAME WriteOp as the chunk body (see cephimpl.writeChunkBatched).
	BackrefXattrName = "user.strata.backref"

	// BackrefMetaKey is the S3 user-metadata key (sent on the wire as
	// x-amz-meta-strata-backref) the S3-passthrough backend stamps with a
	// base64 of the same payload — that backend reconciles via its native
	// ListObjects, so the back-reference rides the backing object's
	// metadata rather than an xattr.
	BackrefMetaKey = "strata-backref"

	// BackrefSchemaV1 is the leading schema byte of the v1 wire form.
	// Values > 1 reserve room for a future refcount-aware / multi-owner
	// shape: content-addressed dedup (ROADMAP P2) would make one chunk
	// owned by N objects, incompatible with this single-owner pointer.
	// Today CopyObject re-writes chunks to fresh OIDs, so every chunk has
	// exactly one owner and v1 suffices.
	BackrefSchemaV1 byte = 1

	// backrefHeaderLen is the fixed-width prefix of the v1 wire form:
	// schema(1) + bucketID(16) + mtimeUnixNano(8) + chunkIdx(4) +
	// versionIDLen(2).
	backrefHeaderLen = 1 + 16 + 8 + 4 + 2
)

// ErrBackrefMalformed is returned by DecodeBackref when the payload is too
// short or its length fields don't fit the buffer.
var ErrBackrefMalformed = errors.New("data: malformed chunk back-reference")

// ErrBackrefSchema is returned by DecodeBackref when the leading schema
// byte is one this build does not understand (forward-compat guard).
var ErrBackrefSchema = errors.New("data: unsupported chunk back-reference schema")

// Backref is the self-describing owner pointer stamped on each data-tier
// chunk. It carries NO key material (no plaintext, no wrapped DEK) so it
// is safe under SSE-S3/KMS and preserves the meta/data security
// separation — rebuild-index (US-004) therefore reports SSE objects as
// unrecoverable rather than serving ciphertext it cannot decrypt.
//
// Mtime is REQUIRED, not optional: VersionID orders the version chain but
// cannot derive IsLatest when a Suspended-mode null version (ts=0)
// coexists with TimeUUID versions — the exact ListObjectVersions bug fixed
// in ralph/architecture-hardening. Rebuild needs Mtime to pick the latest
// correctly.
type Backref struct {
	BucketID  uuid.UUID
	Key       string
	VersionID string
	ChunkIdx  int
	Mtime     time.Time
}

// BackrefAttrs is the object-level identity carried on the data-plane ctx
// (the per-chunk index is added by the backend at write time). See
// WithBackref / BackrefFromContext in context.go.
type BackrefAttrs struct {
	BucketID  uuid.UUID
	Key       string
	VersionID string
	Mtime     time.Time
}

// EncodeBackref returns the compact versioned wire form:
//
//	schema(1) | bucketID(16) | mtimeUnixNano(8 BE) | chunkIdx(4 BE) |
//	versionIDLen(2 BE) | versionID | key
//
// key is the trailing remainder (no length prefix needed — it's last).
// Both VersionID and Key may be empty. A zero Mtime encodes as nanos=0.
func EncodeBackref(b Backref) []byte {
	ver := []byte(b.VersionID)
	key := []byte(b.Key)
	out := make([]byte, backrefHeaderLen+len(ver)+len(key))
	out[0] = BackrefSchemaV1
	copy(out[1:17], b.BucketID[:])
	var nano int64
	if !b.Mtime.IsZero() {
		nano = b.Mtime.UnixNano()
	}
	binary.BigEndian.PutUint64(out[17:25], uint64(nano))
	binary.BigEndian.PutUint32(out[25:29], uint32(b.ChunkIdx))
	binary.BigEndian.PutUint16(out[29:31], uint16(len(ver)))
	n := backrefHeaderLen
	n += copy(out[n:], ver)
	copy(out[n:], key)
	return out
}

// DecodeBackref parses the wire form produced by EncodeBackref. It returns
// ErrBackrefSchema for an unrecognised leading byte and ErrBackrefMalformed
// for a truncated / inconsistent buffer — never a partially-filled Backref
// a caller might mistake for whole.
func DecodeBackref(p []byte) (Backref, error) {
	if len(p) < backrefHeaderLen {
		return Backref{}, ErrBackrefMalformed
	}
	if p[0] != BackrefSchemaV1 {
		return Backref{}, fmt.Errorf("%w: %d", ErrBackrefSchema, p[0])
	}
	var b Backref
	copy(b.BucketID[:], p[1:17])
	if nano := int64(binary.BigEndian.Uint64(p[17:25])); nano != 0 {
		b.Mtime = time.Unix(0, nano).UTC()
	}
	b.ChunkIdx = int(binary.BigEndian.Uint32(p[25:29]))
	verLen := int(binary.BigEndian.Uint16(p[29:31]))
	if backrefHeaderLen+verLen > len(p) {
		return Backref{}, ErrBackrefMalformed
	}
	b.VersionID = string(p[backrefHeaderLen : backrefHeaderLen+verLen])
	b.Key = string(p[backrefHeaderLen+verLen:])
	return b, nil
}

// BackrefEnabledFromEnv reports whether chunk back-references are stamped
// at PUT. Default ON; operators opt out with STRATA_CHUNK_BACKREF=false to
// get legacy no-xattr behaviour — reconcile / rebuild then degrade
// gracefully and log that back-references are absent. Read once at backend
// New time (mirrors BatchOpsFromEnv).
func BackrefEnabledFromEnv() bool {
	if v := os.Getenv("STRATA_CHUNK_BACKREF"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return true
}
