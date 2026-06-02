package cephimpl

import (
	"context"
	"fmt"

	goceph "github.com/ceph/go-ceph/rados"

	"github.com/danchupin/strata/internal/data/rados"
)

// EnumeratePool walks every object in (cluster, opts.Pool, opts.Namespace)
// via librados' rados_nobjects_list (go-ceph ioctx.Iter), streaming object
// names to visit. It implements rados.PoolEnumerator so the always-on
// rados.EnumeratePool dispatcher reaches the real librados backend under
// the `ceph` build tag.
//
// Resumable: opts.Start seeks the iterator to a prior PG-hash cursor before
// the first read; each visit gets the post-advance cursor. Cancellable: the
// loop checks ctx between objects. Rate-limited: opts.RatePerSec drives a
// rados.ScanLimiter token bucket so a live-pool walk does not saturate
// OSDs. Filtered: opts.ChunkOIDsOnly drops foreign (non-Strata-chunk) OIDs.
func (b *Backend) EnumeratePool(ctx context.Context, cluster string, opts rados.EnumerateOptions, visit rados.PoolVisitor) error {
	if opts.Pool == "" {
		return fmt.Errorf("rados: enumerate requires a pool")
	}
	if visit == nil {
		return fmt.Errorf("rados: enumerate requires a visitor")
	}
	// opts.Namespace forwards verbatim — EnumerateAllNamespaces ("\x01")
	// shares librados' LIBRADOS_ALL_NSPACES byte value, so the cached ioctx
	// SetNamespace picks up the all-namespaces sentinel with no translation.
	ioctx, err := b.ioctx(ctx, cluster, opts.Pool, opts.Namespace)
	if err != nil {
		return fmt.Errorf("rados: enumerate open ioctx %s/%s: %w", cluster, opts.Pool, err)
	}
	iter, err := ioctx.Iter()
	if err != nil {
		return fmt.Errorf("rados: enumerate iter %s/%s: %w", cluster, opts.Pool, err)
	}
	defer iter.Close()
	if opts.Start != 0 {
		iter.Seek(goceph.IterToken(uint32(opts.Start)))
	}
	limiter := rados.NewScanLimiter(opts.RatePerSec)
	for iter.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := limiter.Wait(ctx); err != nil {
			return err
		}
		oid := iter.Value()
		if opts.ChunkOIDsOnly && !rados.IsChunkOID(oid) {
			continue
		}
		obj := rados.PoolObject{OID: oid, Namespace: iter.Namespace()}
		if err := visit(obj, rados.EnumerateCursor(uint32(iter.Token()))); err != nil {
			return err
		}
	}
	return iter.Err()
}
