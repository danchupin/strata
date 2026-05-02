package heartbeat

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryStore is an in-process heartbeat table for single-replica dev stacks
// (STRATA_META_BACKEND=memory) and unit tests. Rows older than TTL are
// filtered out at read time and removed on the next write — no background
// sweeper required.
type MemoryStore struct {
	mu    sync.Mutex
	nodes map[string]Node
	TTL   time.Duration
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{nodes: make(map[string]Node), TTL: DefaultTTL}
}

func (s *MemoryStore) WriteHeartbeat(_ context.Context, n Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n.LastHeartbeat.IsZero() {
		n.LastHeartbeat = time.Now().UTC()
	}
	s.nodes[n.ID] = n
	s.gcLocked(time.Now().UTC())
	return nil
}

func (s *MemoryStore) ListNodes(_ context.Context) ([]Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(time.Now().UTC())
	out := make([]Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemoryStore) gcLocked(now time.Time) {
	ttl := s.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	for id, n := range s.nodes {
		if now.Sub(n.LastHeartbeat) > ttl {
			delete(s.nodes, id)
		}
	}
}
