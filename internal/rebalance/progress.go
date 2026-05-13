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
// BaseChunks / BaseBytes are the chunk count + bytes captured the first
// time the worker finalised a scan after the cluster transitioned to
// draining. The UI uses BaseChunks to render percentage filled
// (1 - chunks/BaseChunks). BaseBytes feeds the US-005 completion event's
// final_bytes_moved = BaseBytes - Bytes. Once set both stick until the
// cluster leaves the draining set (undrain or row deletion), at which
// point the tracker drops the snapshot entirely.
//
// CompletionFiredAt records the timestamp the rebalance worker emitted
// the most recent drain-complete event for this cluster (US-005). Zero
// means no event has fired since this snapshot row was created. The
// tracker clears the field on a `0 → >0` transition so refill-then-
// redrain shapes (`5 → 0 → 2 → 0`) re-fire on the second `→ 0`.
type ProgressSnapshot struct {
	Chunks            int64
	Bytes             int64
	LastScanAt        time.Time
	BaseChunks        int64
	BaseBytes         int64
	CompletionFiredAt time.Time
}

// CompletionEvent is the per-cluster transition signal returned by
// ProgressTracker.CommitScan when the latest scan observed
// `chunks_on_cluster: >0 → 0` while the cluster is still draining
// (US-005 drain-lifecycle). The rebalance worker consumes the slice and
// fans the event out to log + audit + metric + optional notify sinks.
// BytesMoved = BaseBytes - Bytes captured at scan-finish time. ScanFinish
// is the same UTC timestamp the tracker stamped onto LastScanAt.
type CompletionEvent struct {
	Cluster     string
	BaseChunks  int64
	BaseBytes   int64
	BytesMoved  int64
	ScanFinish  time.Time
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
//     fresh counts + LastScanAt=now. BaseChunks / BaseBytes are set on
//     the first commit for that cluster (i.e. the live → draining
//     transition; if the tracker already held a snapshot from a prior
//     session it is preserved).
//   - For each cluster present in the cache but NOT in draining (likely
//     undrained between ticks), the entry is dropped.
//   - Clusters in draining that did not appear in chunkCounts (no chunks
//     observed) get a snapshot with Chunks=0 / Bytes=0 so the
//     deregister_ready signal flips immediately on the next read.
//
// Returns the per-cluster completion events (US-005) detected during the
// commit: a cluster appears in the slice iff prior_chunks > 0 AND
// new_chunks == 0 AND CompletionFiredAt is zero. CompletionFiredAt is
// stamped at the same time so the event is idempotent — re-firing
// requires a subsequent 0 → >0 transition (the tracker resets the
// CompletionFiredAt slot on refill so a drain → fill → drain cycle fires
// again).
func (p *ProgressTracker) CommitScan(draining []string, chunkCounts map[string]int64, byteCounts map[string]int64, now time.Time) []CompletionEvent {
	if p == nil {
		return nil
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
	var completions []CompletionEvent
	for id := range want {
		chunks := chunkCounts[id]
		bytes := byteCounts[id]
		existing, hadSnap := p.snapshots[id]
		if !hadSnap {
			existing = &ProgressSnapshot{}
			p.snapshots[id] = existing
		}
		prevChunks := existing.Chunks
		if existing.BaseChunks == 0 && chunks > 0 {
			existing.BaseChunks = chunks
		}
		if existing.BaseBytes == 0 && bytes > 0 {
			existing.BaseBytes = bytes
		}
		// Re-arm completion firing when the cluster refills after a prior
		// completion. Without this reset a drain → fill → drain cycle
		// would silently skip the second event.
		if chunks > 0 && !existing.CompletionFiredAt.IsZero() {
			existing.CompletionFiredAt = time.Time{}
		}
		existing.Chunks = chunks
		existing.Bytes = bytes
		existing.LastScanAt = now
		// Fire only on >0 → 0 transitions we actually observed. A
		// first-ever commit with chunks=0 (legacy bucket with no chunks
		// on the cluster) must NOT fire — there was nothing to drain.
		if hadSnap && prevChunks > 0 && chunks == 0 && existing.CompletionFiredAt.IsZero() {
			existing.CompletionFiredAt = now
			completions = append(completions, CompletionEvent{
				Cluster:    id,
				BaseChunks: existing.BaseChunks,
				BaseBytes:  existing.BaseBytes,
				BytesMoved: existing.BaseBytes,
				ScanFinish: now,
			})
		}
	}
	return completions
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
