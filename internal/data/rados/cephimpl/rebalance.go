package cephimpl

import (
	"context"
	"fmt"

	goceph "github.com/ceph/go-ceph/rados"
)

// RadosCluster is the per-cluster facade cephimpl exposes for the
// rebalance worker. It is structurally compatible with
// internal/rebalance.RadosCluster — the worker (in main module) does the
// implicit interface conversion at the call site. Keeping the interface
// here means cephimpl never imports internal/rebalance and the main
// module is the sole place where rebalance's transitive dependencies
// (tikv pulls in old google.golang.org/genproto) get loaded — otherwise
// the workspace MVS conflates new + old genproto.
type RadosCluster interface {
	ID() string
	Read(ctx context.Context, pool, namespace, oid string) ([]byte, error)
	Write(ctx context.Context, pool, namespace, oid string, body []byte) error
}

// RebalanceClusters returns a librados-backed RadosCluster facade per
// configured cluster on b.
func RebalanceClusters(b *Backend) map[string]RadosCluster {
	if b == nil {
		return nil
	}
	out := make(map[string]RadosCluster, len(b.clusters))
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
