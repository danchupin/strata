package placement

import (
	"context"
	"sync"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

// DefaultDrainCacheTTL is the in-process freshness window for the drain
// sentinel cache (US-006). PUT hot-path readers tolerate up to this much
// staleness; drain is a slow operator action so the eventual-consistency
// window is acceptable.
const DefaultDrainCacheTTL = 30 * time.Second

// DrainLoader is the upstream meta.Store fetch the cache wraps. Returns
// every persisted cluster_state row keyed on cluster id. The cache
// transforms the result into a set of cluster ids whose state blocks
// new writes — both draining_readonly and evacuating qualify
// (US-001 drain-transparency). live / removed pass through.
type DrainLoader func(ctx context.Context) (map[string]meta.ClusterStateRow, error)

// DrainCache caches the cluster_state snapshot with a fixed TTL.
// Two derived views are exposed: Get returns the draining-cluster set
// (PUT-routing exclusion) and States returns the full per-cluster row
// snapshot (default-routing weight synthesis, US-002 cluster-weights).
// Both share one underlying fetch so the PUT hot path consults at most
// one loader call per TTL window. Refresh is best-effort: an error on
// reload preserves the prior snapshot so routing keeps working even
// when the meta backend hiccups.
//
// Safe for concurrent use. Refresh is single-flighted by the mutex —
// concurrent Get / States calls during a refresh wait, but at most one
// fetch is in-flight at any time.
type DrainCache struct {
	loader DrainLoader
	ttl    time.Duration
	now    func() time.Time

	mu      sync.RWMutex
	snap    map[string]bool
	states  map[string]meta.ClusterStateRow
	fetched time.Time
}

// NewDrainCache builds a cache with the supplied loader + TTL. ttl == 0
// falls back to DefaultDrainCacheTTL. now defaults to time.Now.
func NewDrainCache(loader DrainLoader, ttl time.Duration) *DrainCache {
	if ttl <= 0 {
		ttl = DefaultDrainCacheTTL
	}
	return &DrainCache{
		loader: loader,
		ttl:    ttl,
		now:    time.Now,
	}
}

// SetClockForTest overrides the cache's clock. Tests use this to fake
// TTL advancement without sleeping.
func (c *DrainCache) SetClockForTest(now func() time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

// Get returns the draining-cluster set. Reloads on first call or when
// the snapshot has aged past TTL. Loader errors preserve the prior
// snapshot — the hot path never blocks on a meta hiccup. nil cache
// returns nil (no draining clusters).
func (c *DrainCache) Get(ctx context.Context) map[string]bool {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	snap := c.snap
	fetched := c.fetched
	c.mu.RUnlock()
	if snap != nil && c.now().Sub(fetched) < c.ttl {
		return snap
	}
	c.refresh(ctx)
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snap
}

// States returns the full cluster_state row snapshot keyed on cluster id.
// Used by placement.DefaultPolicy to synthesise the default-routing
// weight policy (US-002 cluster-weights). Same TTL / refresh semantics
// as Get — the underlying loader is single-flighted across both views.
// nil cache returns nil.
func (c *DrainCache) States(ctx context.Context) map[string]meta.ClusterStateRow {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	states := c.states
	fetched := c.fetched
	c.mu.RUnlock()
	if states != nil && c.now().Sub(fetched) < c.ttl {
		return states
	}
	c.refresh(ctx)
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.states
}

// Invalidate forces the next Get to reload. Wired into the drain/
// undrain admin handlers so a freshly-flipped state is visible to the
// PUT hot path without waiting out the TTL.
func (c *DrainCache) Invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.fetched = time.Time{}
	c.mu.Unlock()
}

func (c *DrainCache) refresh(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check under the write lock — a concurrent refresh may have
	// won the race.
	if c.snap != nil && c.now().Sub(c.fetched) < c.ttl {
		return
	}
	rows, err := c.loader(ctx)
	if err != nil {
		// Preserve the prior snapshot. Empty-but-non-nil avoids a hot
		// reload storm when meta is unavailable on the first call.
		if c.snap == nil {
			c.snap = map[string]bool{}
		}
		if c.states == nil {
			c.states = map[string]meta.ClusterStateRow{}
		}
		c.fetched = c.now()
		return
	}
	out := make(map[string]bool, len(rows))
	states := make(map[string]meta.ClusterStateRow, len(rows))
	for clusterID, row := range rows {
		states[clusterID] = row
		if meta.IsDrainingForWrite(row.State) {
			out[clusterID] = true
		}
	}
	c.snap = out
	c.states = states
	c.fetched = c.now()
}
