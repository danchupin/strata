package placement

import (
	"sync"
	"time"
)

// DefaultClusterStatsCacheTTL is the in-process freshness window for the
// per-cluster fill + object-count snapshot served by GET /admin/v1/
// clusters/{id}/drain-progress (US-001 drain-progress-physical).
//
// 10 s is short enough that operator-paced UI polls feel live (drain bar
// refreshes every 5 s) while still absorbing the per-poll MonCommand +
// GetPoolStats cost when multiple drain dashboards refresh in lockstep.
const DefaultClusterStatsCacheTTL = 10 * time.Second

// ClusterStatsCache is a per-cluster TTL cache holding the (usedBytes,
// objectCount) pair last returned by data.ClusterStatsProbe +
// data.ClusterObjectCountProbe. Concurrency-safe; readers see a snapshot
// or ok=false (expired / never set). Invalidation is TTL-only — drain /
// undrain admin handlers do NOT invalidate because the underlying
// `ceph df` payload doesn't change at those events.
type ClusterStatsCache struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.RWMutex
	entries map[string]clusterStatsEntry
}

type clusterStatsEntry struct {
	bytes   int64
	objects int64
	fetched time.Time
}

// NewClusterStatsCache builds a cache with the supplied TTL. ttl <= 0
// falls back to DefaultClusterStatsCacheTTL. now defaults to time.Now.
func NewClusterStatsCache(ttl time.Duration) *ClusterStatsCache {
	if ttl <= 0 {
		ttl = DefaultClusterStatsCacheTTL
	}
	return &ClusterStatsCache{
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]clusterStatsEntry),
	}
}

// SetClockForTest overrides the cache's clock so tests can fake TTL
// advancement without sleeping.
func (c *ClusterStatsCache) SetClockForTest(now func() time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

// Get returns the cached (bytes, objects) for clusterID. ok=false when
// the entry is missing or older than TTL. nil cache returns (0, 0, false).
func (c *ClusterStatsCache) Get(clusterID string) (bytes, objects int64, ok bool) {
	if c == nil {
		return 0, 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, found := c.entries[clusterID]
	if !found {
		return 0, 0, false
	}
	if c.now().Sub(e.fetched) >= c.ttl {
		return 0, 0, false
	}
	return e.bytes, e.objects, true
}

// Set records a fresh (bytes, objects) snapshot for clusterID. nil cache
// is a no-op.
func (c *ClusterStatsCache) Set(clusterID string, bytes, objects int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries[clusterID] = clusterStatsEntry{
		bytes:   bytes,
		objects: objects,
		fetched: c.now(),
	}
	c.mu.Unlock()
}
