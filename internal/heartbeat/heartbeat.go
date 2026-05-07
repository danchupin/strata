// Package heartbeat tracks live Strata replicas across the cluster.
//
// Each replica periodically writes a Node row keyed by its NodeID; rows are
// evicted automatically when their TTL expires (Cassandra default_time_to_live;
// in-process timestamp filter for the memory backend). The /admin/v1/cluster/*
// endpoints assemble the cluster overview by listing the surviving rows.
//
// Phase 1 (US-006) wires the gateway as the only writer. Worker daemons
// (cmd/strata-gc, cmd/strata-lifecycle) can adopt the same Heartbeater in a
// follow-up to surface their workers + leader_for chips.
package heartbeat

import (
	"context"
	"log"
	"os"
	"sort"
	"sync"
	"time"
)

const (
	// DefaultInterval is how often a replica writes its heartbeat.
	DefaultInterval = 10 * time.Second
	// DefaultTTL is how long a row survives without a refresh. Cassandra
	// applies this via USING TTL; the memory store filters at read time.
	DefaultTTL = 30 * time.Second
)

// Node is one row in the cluster_nodes table.
type Node struct {
	ID            string
	Address       string
	Version       string
	StartedAt     time.Time
	Workers       []string
	LeaderFor     []string
	LastHeartbeat time.Time
}

// Store is the storage shape backed by memory + cassandra.
type Store interface {
	WriteHeartbeat(ctx context.Context, n Node) error
	ListNodes(ctx context.Context) ([]Node, error)
}

// Heartbeater writes Node into Store on every Interval tick. The first write
// is synchronous on Run() so the local replica appears immediately.
type Heartbeater struct {
	Store    Store
	Node     Node
	Interval time.Duration
	Logger   *log.Logger

	mu sync.Mutex
}

// Run blocks until ctx is cancelled.
func (h *Heartbeater) Run(ctx context.Context) {
	if h.Interval == 0 {
		h.Interval = DefaultInterval
	}
	if h.Logger == nil {
		h.Logger = log.Default()
	}
	h.write(ctx)
	t := time.NewTicker(h.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.write(ctx)
		}
	}
}

func (h *Heartbeater) write(ctx context.Context) {
	h.mu.Lock()
	n := h.Node
	if len(h.Node.LeaderFor) > 0 {
		n.LeaderFor = append([]string(nil), h.Node.LeaderFor...)
	} else {
		n.LeaderFor = nil
	}
	h.mu.Unlock()
	n.LastHeartbeat = time.Now().UTC()
	if err := h.Store.WriteHeartbeat(ctx, n); err != nil {
		h.Logger.Printf("heartbeat: write %s: %v", n.ID, err)
	}
}

// SetLeaderFor mutates Heartbeater.Node.LeaderFor under a mutex; the next
// write tick picks up the new slice. Idempotent: re-acquiring an already
// owned worker, or releasing one that isn't owned, is a no-op. The slice
// stays sorted for deterministic comparison in tests.
func (h *Heartbeater) SetLeaderFor(worker string, owned bool) {
	if worker == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.Node.LeaderFor
	idx := -1
	for i, w := range cur {
		if w == worker {
			idx = i
			break
		}
	}
	if owned {
		if idx >= 0 {
			return
		}
		next := append(append([]string(nil), cur...), worker)
		sort.Strings(next)
		h.Node.LeaderFor = next
		return
	}
	if idx < 0 {
		return
	}
	next := make([]string, 0, len(cur)-1)
	for i, w := range cur {
		if i == idx {
			continue
		}
		next = append(next, w)
	}
	h.Node.LeaderFor = next
}

// IsAlive reports whether the node's last heartbeat falls within ttl.
func IsAlive(n Node, ttl time.Duration) bool {
	return !n.LastHeartbeat.IsZero() && time.Since(n.LastHeartbeat) <= ttl
}

// DefaultNodeID returns the node identifier used when STRATA_NODE_ID is unset:
// the OS hostname, falling back to "strata" if the lookup fails.
func DefaultNodeID() string {
	if v := os.Getenv("STRATA_NODE_ID"); v != "" {
		return v
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "strata"
}
