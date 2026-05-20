//go:build ceph

package rados

import (
	"fmt"
	"sync"
	"sync/atomic"

	goceph "github.com/ceph/go-ceph/rados"
)

// connPool serves a round-robin set of pre-connected *goceph.Conn instances
// for a single RADOS cluster. Size is fixed at construction; per-conn
// IOContexts are cached lazily by (pool, namespace).
//
// A single librados *Conn serialises ops through one cephx session and a
// per-conn thread pool. Multi-conn pooling lets PUT-heavy workloads
// spread the in-flight outbound RPC budget across N sessions — useful
// when the gateway is bottlenecked on per-conn lock contention rather
// than OSD throughput. See
// docs/site/content/architecture/benchmarks/rados-pool.md for the
// bench gate.
type connPool struct {
	conns   []*goceph.Conn
	counter atomic.Uint64

	mu      sync.Mutex
	ioctxes []map[string]*goceph.IOContext
}

// newConnPool dials `size` fresh *goceph.Conn instances against the same
// ClusterSpec. Every conn is Connect()'d eagerly; on the first failure
// the partially-dialed set is shut down and the error is returned — no
// half-connected pool is ever exposed to callers.
func newConnPool(spec ClusterSpec, size int) (*connPool, error) {
	if size < 1 {
		size = 1
	}
	conns := make([]*goceph.Conn, 0, size)
	for i := 0; i < size; i++ {
		c, err := dialCluster(spec)
		if err != nil {
			for _, existing := range conns {
				existing.Shutdown()
			}
			return nil, fmt.Errorf("conn[%d]: %w", i, err)
		}
		conns = append(conns, c)
	}
	ioctxes := make([]map[string]*goceph.IOContext, size)
	for i := range ioctxes {
		ioctxes[i] = make(map[string]*goceph.IOContext)
	}
	return &connPool{conns: conns, ioctxes: ioctxes}, nil
}

// Next returns the next conn in round-robin order. Surface kept for
// future writers that want a raw *Conn (e.g. mon_command pollers). No
// in-tree caller uses it today; PutChunks / GetChunks consume IOContext
// directly.
func (p *connPool) Next() *goceph.Conn {
	if len(p.conns) == 0 {
		return nil
	}
	return p.conns[nextRoundRobin(&p.counter, len(p.conns))]
}

// IOContext returns a cached *IOContext for (pool, namespace) tied to a
// round-robin-selected conn slot. Repeated calls for the same
// (pool, ns) keep cycling through slots; first call per (slot, pool, ns)
// opens the underlying ioctx lazily.
func (p *connPool) IOContext(pool, ns string) (*goceph.IOContext, error) {
	if len(p.conns) == 0 {
		return nil, fmt.Errorf("rados: empty conn pool")
	}
	idx := nextRoundRobin(&p.counter, len(p.conns))
	key := pool + "|" + ns
	p.mu.Lock()
	defer p.mu.Unlock()
	if cached, ok := p.ioctxes[idx][key]; ok {
		return cached, nil
	}
	x, err := p.conns[idx].OpenIOContext(pool)
	if err != nil {
		return nil, fmt.Errorf("rados: open ioctx %s: %w", pool, err)
	}
	if ns != "" {
		x.SetNamespace(ns)
	}
	p.ioctxes[idx][key] = x
	return x, nil
}

// Size returns the configured pool depth. Useful for observability.
func (p *connPool) Size() int {
	return len(p.conns)
}

// Close drops every cached ioctx + shuts down every conn. Safe to call
// multiple times.
func (p *connPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, m := range p.ioctxes {
		for _, x := range m {
			x.Destroy()
		}
		p.ioctxes[i] = nil
	}
	p.ioctxes = nil
	for i, c := range p.conns {
		if c != nil {
			c.Shutdown()
		}
		p.conns[i] = nil
	}
	p.conns = nil
}
