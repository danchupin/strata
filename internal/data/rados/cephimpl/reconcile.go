package cephimpl

import (
	"context"
	"errors"
	"fmt"

	goceph "github.com/ceph/go-ceph/rados"

	"github.com/danchupin/strata/internal/data"
)

// ChunkExists implements data.ChunkStater (US-003b): it answers whether the
// chunk OID still exists in the data tier by issuing a per-OID rados Stat on
// the chunk's (cluster, pool, namespace) ioctx. The reconcile worker's
// dangling-manifest pass (meta->data) probes every manifest-referenced chunk
// through it — a manifest whose chunk Stat returns ENOENT is dangling.
//
// ENOENT (the chunk is genuinely gone) returns (false, nil); a successful Stat
// returns (true, nil); any other error (transport/auth/timeout) returns it
// verbatim so the worker counts an error and never flags a healthy object on a
// probe it could not run. Stat runs on a goroutine guarded by ctx so a hung
// OSD cannot wedge the walk past a cancellation.
func (b *Backend) ChunkExists(ctx context.Context, ref data.ChunkRef) (bool, error) {
	if b == nil {
		return false, errors.New("rados backend closed")
	}
	if ref.OID == "" {
		return false, errors.New("rados chunk-exists: oid required")
	}
	ioctx, err := b.ioctx(ctx, ref.Cluster, ref.Pool, ref.Namespace)
	if err != nil {
		return false, err
	}
	done := make(chan error, 1)
	go func() { _, sErr := ioctx.Stat(ref.OID); done <- sErr }()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case sErr := <-done:
		if sErr == nil {
			return true, nil
		}
		if errors.Is(sErr, goceph.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("rados: stat %s: %w", ref.OID, sErr)
	}
}

var _ data.ChunkStater = (*Backend)(nil)
