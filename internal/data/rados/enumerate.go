package rados

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/danchupin/strata/internal/data"
)

// EnumerateAllNamespaces selects every namespace in a pool when set as
// EnumerateOptions.Namespace. Its byte value matches librados'
// LIBRADOS_ALL_NSPACES ("\x01") so the cephimpl backend forwards it to
// goceph.AllNamespaces with no translation. Use it for a pool-wide walk
// that spans every per-class namespace; leave Namespace empty to scan only
// the default namespace, or set a concrete namespace for a single class.
const EnumerateAllNamespaces = "\x01"

// chunkOIDDigits is the zero-pad width PutChunks uses for the chunk index
// suffix (`fmt.Sprintf("%s.%05d", objID, idx)` in cephimpl backend.go). A
// chunk OID is therefore `<uuid>.<>=5 decimal digits>`.
const chunkOIDDigits = 5

// ChunkOID is the parsed shape of a Strata data-plane chunk object name.
// The PUT path stamps `<objID-uuid>.<5-wide chunk index>` per chunk; this
// is the inverse used by the reconcile/rebuild tooling to tell Strata
// chunks apart from foreign objects sharing a pool.
type ChunkOID struct {
	ObjID uuid.UUID
	Index int
}

// ParseChunkOID splits a RADOS object name into its owning-object UUID and
// chunk index, returning ok=false for anything that is not shaped like a
// Strata chunk OID (foreign objects, RGW index shards, omap blobs, …).
// The split is on the LAST '.', the left segment must parse as a UUID and
// the right must be all decimal digits at least chunkOIDDigits wide.
func ParseChunkOID(oid string) (ChunkOID, bool) {
	dot := strings.LastIndexByte(oid, '.')
	if dot < 0 {
		return ChunkOID{}, false
	}
	left, right := oid[:dot], oid[dot+1:]
	if len(right) < chunkOIDDigits {
		return ChunkOID{}, false
	}
	for i := 0; i < len(right); i++ {
		if right[i] < '0' || right[i] > '9' {
			return ChunkOID{}, false
		}
	}
	id, err := uuid.Parse(left)
	if err != nil {
		return ChunkOID{}, false
	}
	idx, err := strconv.Atoi(right)
	if err != nil {
		return ChunkOID{}, false
	}
	return ChunkOID{ObjID: id, Index: idx}, true
}

// IsChunkOID reports whether oid is shaped like a Strata data-plane chunk.
func IsChunkOID(oid string) bool {
	_, ok := ParseChunkOID(oid)
	return ok
}

// EnumerateCursor is a resumable position in a pool walk. It wraps the
// librados nobjects-list PG-hash position (uint32): a zero value starts at
// the beginning. Resume is placement-group granular — Seek lands on the PG
// boundary at or before the cursor, so a resumed walk may REPLAY objects
// from the current PG (at-least-once), never skip ahead of it (no drop).
// The reconcile/rebuild callers dedup by OID against the meta set, so the
// replay is harmless.
type EnumerateCursor uint32

// EnumerateOptions parameterises a single pool walk.
type EnumerateOptions struct {
	// Pool is the RADOS pool to enumerate (required).
	Pool string
	// Namespace selects the namespace: empty = default namespace only, a
	// concrete value = one class's namespace, EnumerateAllNamespaces = every
	// namespace in the pool.
	Namespace string
	// Start resumes the walk from a prior cursor; zero starts at the front.
	Start EnumerateCursor
	// RatePerSec caps objects emitted per second (token bucket) so a
	// live-cluster walk does not saturate OSDs. Zero disables the limit.
	RatePerSec int
	// ChunkOIDsOnly filters the stream to Strata chunk OIDs (IsChunkOID),
	// skipping foreign objects sharing the pool.
	ChunkOIDsOnly bool
}

// PoolObject is one enumerated object: its name plus the namespace it lives
// in (populated when walking EnumerateAllNamespaces).
type PoolObject struct {
	OID       string
	Namespace string
}

// PoolVisitor is invoked once per enumerated object with a resume cursor
// pointing AT-OR-AFTER that object. Returning a non-nil error stops the
// walk and is propagated out of EnumeratePool.
//
// Callers that checkpoint progress MUST persist the cursor handed to the
// visitor (the last successfully-processed object), NOT rely on
// EnumeratePool's return value: a mid-walk librados error surfaces only the
// bare error, so on a transient OSD failure the saved visitor cursor lets a
// retry resume from the last good PG instead of re-walking from Start.
type PoolVisitor func(obj PoolObject, resume EnumerateCursor) error

// PoolEnumerator is implemented by the librados-backed data.Backend
// (cephimpl, under the `ceph` build tag). Default-tag builds have no
// implementation, so EnumeratePool below returns the not-compiled sentinel
// — mirroring how rados.New stubs out to data.ErrRADOSNotCompiled.
type PoolEnumerator interface {
	EnumeratePool(ctx context.Context, cluster string, opts EnumerateOptions, visit PoolVisitor) error
}

// EnumeratePool dispatches a pool walk to a backend that implements
// PoolEnumerator, else returns data.ErrRADOSNotCompiled. This is the
// always-on entry point the reconcile/rebuild tooling calls: on a
// go-ceph-free build the backend is the rados.New stub (or nil), the type
// assertion fails, and callers get the sentinel — never a nil-pointer
// panic.
func EnumeratePool(ctx context.Context, b data.Backend, cluster string, opts EnumerateOptions, visit PoolVisitor) error {
	e, ok := b.(PoolEnumerator)
	if !ok {
		return data.ErrRADOSNotCompiled
	}
	return e.EnumeratePool(ctx, cluster, opts, visit)
}

// ScanLimiter is a token bucket gating object-emission rate on a pool walk.
// It wraps golang.org/x/time/rate the same way internal/rebalance.Throttle
// gates byte-rate on the mover; kept in the always-on main module so the
// cephimpl backend can reuse it without adding a go-ceph-side rate dep. A
// nil ScanLimiter is a valid no-op.
type ScanLimiter struct {
	lim *rate.Limiter
}

// NewScanLimiter returns a ScanLimiter enforcing perSec objects/second with
// a one-second burst. A non-positive perSec returns nil (unlimited).
func NewScanLimiter(perSec int) *ScanLimiter {
	if perSec <= 0 {
		return nil
	}
	return &ScanLimiter{lim: rate.NewLimiter(rate.Limit(perSec), perSec)}
}

// Wait blocks until one token is available or ctx is cancelled. A nil
// receiver is a no-op.
func (s *ScanLimiter) Wait(ctx context.Context) error {
	if s == nil || s.lim == nil {
		return nil
	}
	return s.lim.Wait(ctx)
}

// ScanRateFromEnv reads STRATA_RECONCILE_SCAN_RATE (objects/sec) for the
// reconcile/rebuild pool walk. Default 0 = unlimited; negative or
// unparseable values coerce to 0.
func ScanRateFromEnv() int {
	v := os.Getenv("STRATA_RECONCILE_SCAN_RATE")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
