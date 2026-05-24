package cephimpl

import (
	"fmt"
	"sync"
	"sync/atomic"

	goceph "github.com/ceph/go-ceph/rados"

	"github.com/danchupin/strata/internal/data/rados"
)

// connPool serves a round-robin set of pre-connected *goceph.Conn
// instances for a single RADOS cluster. Size is fixed at construction;
// per-conn IOContexts are cached lazily by (pool, namespace).
type connPool struct {
	conns   []*goceph.Conn
	counter atomic.Uint64

	mu      sync.Mutex
	ioctxes []map[string]*goceph.IOContext
}

func newConnPool(spec rados.ClusterSpec, size int) (*connPool, error) {
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

func (p *connPool) Next() *goceph.Conn {
	if len(p.conns) == 0 {
		return nil
	}
	return p.conns[rados.NextRoundRobin(&p.counter, len(p.conns))]
}

func (p *connPool) IOContext(pool, ns string) (*goceph.IOContext, error) {
	if len(p.conns) == 0 {
		return nil, fmt.Errorf("rados: empty conn pool")
	}
	idx := rados.NextRoundRobin(&p.counter, len(p.conns))
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

func (p *connPool) Size() int { return len(p.conns) }

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
