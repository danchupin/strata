package reconcile

import (
	"context"
	"strconv"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/data/rados"
)

// RADOSScanner adapts the always-on rados.EnumeratePool primitive (US-000) to
// the ChunkScanner interface. It walks a pool with ChunkOIDsOnly+WithBackref so
// each visited chunk arrives already filtered to Strata chunk OIDs and carrying
// its raw back-reference xattr — the worker decodes + classifies without a
// second per-OID round trip.
//
// On a go-ceph-free build (default tag) the wrapped data.Backend is the
// rados.New stub, so EnumeratePool returns data.ErrRADOSNotCompiled and the
// worker records that on the job — the scanner stays CGO-free.
type RADOSScanner struct {
	Backend data.Backend
	// RatePerSec caps objects emitted per second (token bucket) so a
	// live-cluster walk does not saturate OSDs. Zero = unlimited.
	RatePerSec int
}

// Scan enumerates the scope's pool, decoding each chunk's back-reference. The
// opaque cursor is the librados PG-hash position rendered as decimal so the
// worker can persist + resume it as a plain string.
func (s *RADOSScanner) Scan(ctx context.Context, scope ScanScope, startCursor string, visit func(ScannedChunk, string) error) error {
	var start rados.EnumerateCursor
	if startCursor != "" {
		if n, err := strconv.ParseUint(startCursor, 10, 32); err == nil {
			start = rados.EnumerateCursor(uint32(n))
		}
	}
	opts := rados.EnumerateOptions{
		Pool:          scope.Pool,
		Namespace:     scope.Namespace,
		Start:         start,
		RatePerSec:    s.RatePerSec,
		ChunkOIDsOnly: true,
		WithBackref:   true,
	}
	return rados.EnumeratePool(ctx, s.Backend, scope.Cluster, opts, func(obj rados.PoolObject, resume rados.EnumerateCursor) error {
		c := ScannedChunk{
			Cluster:   scope.Cluster,
			Pool:      scope.Pool,
			Namespace: obj.Namespace,
			OID:       obj.OID,
		}
		if len(obj.Backref) > 0 {
			// A malformed back-reference is treated as absent (HasBackref
			// stays false) — reported, never used to delete. We never destroy
			// a chunk on a payload we could not decode.
			if br, err := data.DecodeBackref(obj.Backref); err == nil {
				c.Backref = br
				c.HasBackref = true
			}
		}
		return visit(c, strconv.FormatUint(uint64(resume), 10))
	})
}
