package rebalance

import (
	"sync"
	"time"
)

// ProgressSnapshot is one cluster's draining-progress sample, refreshed by
// the rebalance worker at most once per tick. It is consumed by the
// adminapi GET /admin/v1/clusters/{id}/drain-progress handler (US-003
// drain-lifecycle). Zero-valued LastScanAt means the worker has not
// committed a scan for this cluster yet.
//
// BaseChunks is the chunk count captured the first time the worker
// finalised a scan after the cluster transitioned to draining. The UI
// uses it to render percentage filled (1 - chunks/BaseChunks). Once set
// it sticks until the cluster leaves the draining set (undrain or row
// deletion), at which point the tracker drops the snapshot entirely.
//
// CompletionFiredAt is reserved for US-005 (completion-detection). The
// US-003 scaffold leaves it zero — the tracker holds the slot so US-005
// can flip it without revisiting the wire shape.
type ProgressSnapshot struct {
	Chunks            int64
	Bytes             int64
	LastScanAt        time.Time
	BaseChunks        int64
	CompletionFiredAt time.Time
}

// ProgressTracker is the in-process draining-progress cache shared
// between the rebalance worker (writer) and the adminapi handler
// (reader). Goroutine-safe. nil is a valid receiver — every method
// short-circuits so callers do not need to nil-check.
type ProgressTracker struct {
	// Interval is the worker's tick cadence (clamped). The drain-progress
	// admin handler uses 2*Interval as the stale-cache threshold.
	Interval time.Duration

	mu        sync.RWMutex
	snapshots map[string]*ProgressSnapshot
}

// NewProgressTracker builds a tracker. interval defaults to one hour when
// <= 0 so the stale-cache threshold stays sane on bare unit-test rigs.
func NewProgressTracker(interval time.Duration) *ProgressTracker {
	if interval <= 0 {
		interval = time.Hour
	}
	return &ProgressTracker{
		Interval:  interval,
		snapshots: map[string]*ProgressSnapshot{},
	}
}

// Snapshot returns the most recent committed snapshot for clusterID plus
// an ok flag. ok=false means the tracker has no row — either the worker
// has never observed a draining cluster with that id, or the cluster
// just transitioned out of draining and the row was reaped.
func (p *ProgressTracker) Snapshot(clusterID string) (ProgressSnapshot, bool) {
	if p == nil {
		return ProgressSnapshot{}, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if s, ok := p.snapshots[clusterID]; ok {
		return *s, true
	}
	return ProgressSnapshot{}, false
}

// CommitScan finalises one worker tick. draining is the set of cluster
// ids in meta.ClusterStateDraining as seen by the worker at the start of
// the tick. chunkCounts / byteCounts are the per-cluster accumulators
// computed by walking every bucket manifest during the tick.
//
// Behaviour:
//   - For each cluster in draining, the snapshot is upserted with the
//     fresh counts + LastScanAt=now. BaseChunks is set on the first
//     commit for that cluster (i.e. the live → draining transition; if
//     the tracker already held a snapshot from a prior session it is
//     preserved).
//   - For each cluster present in the cache but NOT in draining (likely
//     undrained between ticks), the entry is dropped.
//   - Clusters in draining that did not appear in chunkCounts (no chunks
//     observed) get a snapshot with Chunks=0 / Bytes=0 so the
//     deregister_ready signal flips immediately on the next read.
func (p *ProgressTracker) CommitScan(draining []string, chunkCounts map[string]int64, byteCounts map[string]int64, now time.Time) {
	if p == nil {
		return
	}
	want := make(map[string]struct{}, len(draining))
	for _, id := range draining {
		want[id] = struct{}{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.snapshots {
		if _, keep := want[id]; !keep {
			delete(p.snapshots, id)
		}
	}
	for id := range want {
		chunks := chunkCounts[id]
		bytes := byteCounts[id]
		existing, hadSnap := p.snapshots[id]
		if !hadSnap {
			existing = &ProgressSnapshot{}
			p.snapshots[id] = existing
		}
		if existing.BaseChunks == 0 && chunks > 0 {
			existing.BaseChunks = chunks
		}
		existing.Chunks = chunks
		existing.Bytes = bytes
		existing.LastScanAt = now
	}
}

// Reset drops the snapshot for clusterID. Reserved for explicit
// resync flows (US-005 will call this on undrain to clear completion
// state). The next CommitScan re-creates a row if the cluster is still
// draining.
func (p *ProgressTracker) Reset(clusterID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.snapshots, clusterID)
}
