package bucketstats

import "sync"

// Snapshot holds the latest cluster-wide per-storage-class totals plus the
// (mostly static) class -> backend pool mapping. Sampler updates Classes at
// the end of each pass; the adminapi /storage/classes handler reads via
// Classes() / Pools(). nil-safe — methods on a nil receiver no-op.
type Snapshot struct {
	mu      sync.RWMutex
	classes map[string]ClassStat
	pools   map[string]string
}

// NewSnapshot constructs a Snapshot seeded with poolsByClass. The map is
// copied so the caller can mutate the source without disturbing the snapshot.
func NewSnapshot(poolsByClass map[string]string) *Snapshot {
	pools := make(map[string]string, len(poolsByClass))
	for k, v := range poolsByClass {
		pools[k] = v
	}
	return &Snapshot{classes: map[string]ClassStat{}, pools: pools}
}

// SetClasses replaces the per-class totals atomically.
func (s *Snapshot) SetClasses(totals map[string]ClassStat) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]ClassStat, len(totals))
	for k, v := range totals {
		next[k] = v
	}
	s.classes = next
}

// Classes returns a copy of the current per-class totals.
func (s *Snapshot) Classes() map[string]ClassStat {
	if s == nil {
		return map[string]ClassStat{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]ClassStat, len(s.classes))
	for k, v := range s.classes {
		out[k] = v
	}
	return out
}

// Pools returns a copy of the class -> pool name map. Empty for backends that
// don't expose a pool dimension (memory, s3-over-s3).
func (s *Snapshot) Pools() map[string]string {
	if s == nil {
		return map[string]string{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.pools))
	for k, v := range s.pools {
		out[k] = v
	}
	return out
}
