//go:build ceph

package rados

import (
	"context"
	"fmt"

	goceph "github.com/ceph/go-ceph/rados"

	"github.com/danchupin/strata/internal/rebalance"
)

// RebalanceClusters returns a librados-backed RadosCluster facade per
// configured cluster on b. Used by the cmd binary's rebalance-worker
// build (cmd/strata/workers/rebalance_movers_ceph.go) to feed a
// rebalance.RadosMover. The returned facades share b's ioctx cache so
// rebalance reads/writes hit the same goceph connection pool the PUT
// hot path already warms.
func RebalanceClusters(b *Backend) map[string]rebalance.RadosCluster {
	if b == nil {
		return nil
	}
	out := make(map[string]rebalance.RadosCluster, len(b.clusters))
	for id := range b.clusters {
		out[id] = &radosClusterFacade{backend: b, id: id}
	}
	return out
}

type radosClusterFacade struct {
	backend *Backend
	id      string
}

func (r *radosClusterFacade) ID() string { return r.id }

// Read pulls one chunk from RADOS. Stats the object first to size the
// buffer exactly so the reader does not allocate extra. ENOENT is
// surfaced as an error — rebalance treats it as a per-chunk failure
// (the source manifest is the source of truth, missing chunk == data
// loss already, can't recover by moving).
func (r *radosClusterFacade) Read(ctx context.Context, pool, namespace, oid string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ioctx, err := r.backend.ioctx(ctx, r.id, pool, namespace)
	if err != nil {
		return nil, fmt.Errorf("rados: open ioctx %s/%s: %w", r.id, pool, err)
	}
	stat, err := ioctx.Stat(oid)
	if err != nil {
		return nil, fmt.Errorf("rados: stat %s: %w", oid, err)
	}
	if stat.Size == 0 {
		return nil, nil
	}
	buf := make([]byte, stat.Size)
	n, err := ioctx.Read(oid, buf, 0)
	if err != nil {
		return nil, fmt.Errorf("rados: read %s: %w", oid, err)
	}
	return buf[:n], nil
}

// Write copies one chunk body into RADOS at oid. Uses the same
// WriteFull primitive PutChunks does so the operation is atomic from
// the cluster's perspective.
func (r *radosClusterFacade) Write(ctx context.Context, pool, namespace, oid string, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ioctx, err := r.backend.ioctx(ctx, r.id, pool, namespace)
	if err != nil {
		return fmt.Errorf("rados: open ioctx %s/%s: %w", r.id, pool, err)
	}
	op := goceph.CreateWriteOp()
	defer op.Release()
	op.WriteFull(body)
	if err := op.Operate(ioctx, oid, goceph.OperationNoFlag); err != nil {
		return fmt.Errorf("rados: write %s: %w", oid, err)
	}
	return nil
}
