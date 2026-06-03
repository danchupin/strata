package cephimpl

import (
	"context"
	"fmt"

	goceph "github.com/ceph/go-ceph/rados"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/data/rados"
)

// Linux errno values returned by go-ceph's GetXattr via the
// ErrorCode()-exposing cephError. ENODATA = the xattr is absent (legacy /
// STRATA_CHUNK_BACKREF=false chunk); ENOENT = the object vanished between the
// Iter step and the GetXattr (a benign restore/GC race). Both mean "no
// back-reference here", not a hard IO failure. cephimpl only builds on Linux
// (the ceph tag), so these constants are fixed.
const (
	errnoENOENT  = -2
	errnoENODATA = -61
)

// errCoder is the structural view of go-ceph's internal cephError, which
// exposes the raw errno via ErrorCode(). Asserting the interface avoids
// importing go-ceph's internal/errutil package.
type errCoder interface{ ErrorCode() int }

// readBackrefXattr reads the back-reference xattr (BackrefXattrName) off oid
// using the supplied (already namespace-bound) ioctx. Returns (nil, nil) when
// the xattr is absent or the object vanished — both are benign "no
// back-reference" signals the reconcile worker reports rather than fails on. A
// real IO error propagates.
func readBackrefXattr(ioctx *goceph.IOContext, oid string) ([]byte, error) {
	// 4 KiB comfortably exceeds the max back-reference payload (schema(1) +
	// bucketID(16) + mtime(8) + chunkIdx(4) + verLen(2) + version(~36) +
	// key(<=1024) ~= 1.1 KiB), so ERANGE-truncation cannot happen.
	buf := make([]byte, 4096)
	n, err := ioctx.GetXattr(oid, data.BackrefXattrName, buf)
	if err != nil {
		if ec, ok := err.(errCoder); ok {
			switch ec.ErrorCode() {
			case errnoENODATA, errnoENOENT:
				return nil, nil
			}
		}
		return nil, err
	}
	out := make([]byte, n)
	copy(out, buf[:n])
	return out, nil
}

// readChunkSize stats oid on the supplied (already namespace-bound) ioctx and
// returns its byte length. A vanished object (ENOENT — the chunk was deleted
// between the Iter step and the Stat, a benign restore/GC race) returns
// (0, nil): rebuild treats a zero-size chunk as degraded rather than failing
// the walk. A real IO error propagates.
func readChunkSize(ioctx *goceph.IOContext, oid string) (int64, error) {
	st, err := ioctx.Stat(oid)
	if err != nil {
		if ec, ok := err.(errCoder); ok && ec.ErrorCode() == errnoENOENT {
			return 0, nil
		}
		return 0, err
	}
	return int64(st.Size), nil
}

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
		if opts.WithBackref || opts.WithSize {
			// Per-object xattr/stat ops CANNOT run on an ALL_NSPACES
			// enumeration ioctx — LIBRADOS_ALL_NSPACES is enumerate-only, so a
			// GetXattr/Stat against it errors and (a per-object error aborts
			// the walk) silently truncates the scan. Route per-object reads
			// through a namespace-correct ioctx for the object's actual
			// namespace (connPool caches one per pool|ns, so it's a map hit
			// after the first object of each namespace). When a CONCRETE
			// namespace was requested the enumeration ioctx already matches, so
			// reuse it directly.
			readIoctx := ioctx
			if opts.Namespace == rados.EnumerateAllNamespaces {
				nsIoctx, nerr := b.ioctx(ctx, cluster, opts.Pool, iter.Namespace())
				if nerr != nil {
					return fmt.Errorf("rados: enumerate ns ioctx %s/%s ns=%q: %w", cluster, opts.Pool, iter.Namespace(), nerr)
				}
				readIoctx = nsIoctx
			}
			if opts.WithBackref {
				// A missing xattr (legacy / STRATA_CHUNK_BACKREF=false chunk)
				// yields a nil Backref, not an error — reconcile reports it absent.
				br, gerr := readBackrefXattr(readIoctx, oid)
				if gerr != nil {
					return fmt.Errorf("rados: enumerate read backref %s: %w", oid, gerr)
				}
				obj.Backref = br
			}
			if opts.WithSize {
				// A vanished object (restore/GC race between Iter and Stat)
				// yields size 0, not an error — reconcile/rebuild treats a
				// zero-size read as a degraded chunk rather than failing the walk.
				sz, serr := readChunkSize(readIoctx, oid)
				if serr != nil {
					return fmt.Errorf("rados: enumerate stat %s: %w", oid, serr)
				}
				obj.Size = sz
			}
		}
		if err := visit(obj, rados.EnumerateCursor(uint32(iter.Token()))); err != nil {
			return err
		}
	}
	return iter.Err()
}
