package rebalance

import (
	"sort"
	"sync"
	"time"
)

// BucketScanCategory is one bucket's contribution to a draining
// cluster's progress snapshot (US-002 drain-transparency). Category is
// always one of "migratable" / "stuck_single_policy" / "stuck_no_policy"
// and is constant within a bucket per tick (it depends on policy +
// drain set, not on individual chunks). ChunkCount + BytesUsed sum over
// every chunk of this bucket that lives on the draining cluster.
type BucketScanCategory struct {
	Category   string
	ChunkCount int64
	BytesUsed  int64
}

// ScanResult is the per-cluster per-tick aggregate that the rebalance
// worker hands to ProgressTracker.CommitScan. It is the structured
// successor to the flat (chunks, bytes) pair used pre-US-002. ByBucket
// is keyed by bucket name and used by the /drain-impact endpoint
// (US-003) and the categorized progress UI (US-006).
type ScanResult struct {
	MigratableChunks        int64
	StuckSinglePolicyChunks int64
	StuckNoPolicyChunks     int64
	Bytes                   int64
	ByBucket                map[string]BucketScanCategory
}

// Chunks returns the sum of the three category counters — the total
// number of chunks that scan observed on this cluster.
func (r ScanResult) Chunks() int64 {
	return r.MigratableChunks + r.StuckSinglePolicyChunks + r.StuckNoPolicyChunks
}

// ProgressSnapshot is one cluster's draining-progress sample, refreshed
// by the rebalance worker at most once per tick (US-003 drain-lifecycle,
// extended in US-002 drain-transparency with categorized counters).
//
// MigratableChunks / StuckSinglePolicyChunks / StuckNoPolicyChunks
// replace the pre-US-002 flat Chunks field; Chunks() derives the total.
// Stuck counters never converge to zero through migration alone — only
// an operator policy edit (US-005 BulkPlacementFixDialog) can move them.
//
// ByBucket holds per-bucket breakdown used by the /drain-impact endpoint
// (US-003) and the categorized drain UI (US-006). nil/empty when the
// scan observed no draining-cluster chunks yet.
//
// BaseChunks / BaseBytes are captured on the first scan after the
// cluster transitioned to draining. BaseChunks feeds the UI's percent-
// filled rendering; BaseBytes feeds the US-005 completion event's
// final_bytes_moved.
//
// CompletionFiredAt records the timestamp the rebalance worker emitted
// the most recent drain-complete event for this cluster (US-005). Zero
// means no event has fired since this snapshot row was created. The
// tracker clears the field on a `0 → >0` transition so refill-then-
// redrain shapes (`5 → 0 → 2 → 0`) re-fire on the second `→ 0`.
type ProgressSnapshot struct {
	MigratableChunks        int64
	StuckSinglePolicyChunks int64
	StuckNoPolicyChunks     int64
	Bytes                   int64
	ByBucket                map[string]BucketScanCategory
	LastScanAt              time.Time
	BaseChunks              int64
	BaseBytes               int64
	CompletionFiredAt       time.Time
}

// Chunks returns the total chunk count on this cluster (sum of the
// three category counters). Replaces the pre-US-002 flat Chunks field.
func (s ProgressSnapshot) Chunks() int64 {
	return s.MigratableChunks + s.StuckSinglePolicyChunks + s.StuckNoPolicyChunks
}

// CompletionEvent is the per-cluster transition signal returned by
// ProgressTracker.CommitScan when the latest scan observed
// `total_chunks_on_cluster: >0 → 0` while the cluster is still in
// the draining set (US-005 drain-lifecycle).
type CompletionEvent struct {
	Cluster    string
	BaseChunks int64
	BaseBytes  int64
	BytesMoved int64
	ScanFinish time.Time
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
		return cloneSnapshot(s), true
	}
	return ProgressSnapshot{}, false
}

// CommitScan finalises one worker tick. draining is the set of cluster
// ids the worker selected for migration scan (state=evacuating in
// US-002). scans is keyed by cluster id with per-cluster aggregates.
// Clusters listed in draining but absent from scans get a zero-count
// snapshot — they may have been seeded without any draining-cluster
// chunks observed, in which case the deregister-ready signal should
// flip immediately.
//
// Behaviour:
//   - For each cluster in draining, the snapshot is upserted with the
//     fresh counters + LastScanAt=now. BaseChunks / BaseBytes are set on
//     the first commit for that cluster.
//   - For each cluster present in the cache but NOT in draining (likely
//     undrained between ticks), the entry is dropped.
//
// Returns the per-cluster completion events detected during the commit:
// a cluster appears in the slice iff prior_total > 0 AND new_total == 0
// AND CompletionFiredAt is zero. CompletionFiredAt is stamped at the
// same time so the event is idempotent — re-firing requires a
// subsequent 0 → >0 transition.
func (p *ProgressTracker) CommitScan(draining []string, scans map[string]ScanResult, now time.Time) []CompletionEvent {
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
	// Stable iteration so completion events come out in a deterministic
	// order — useful for assertions and downstream sinks.
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var completions []CompletionEvent
	for _, id := range ids {
		res := scans[id]
		total := res.Chunks()
		existing, hadSnap := p.snapshots[id]
		if !hadSnap {
			existing = &ProgressSnapshot{}
			p.snapshots[id] = existing
		}
		prevTotal := existing.Chunks()
		if existing.BaseChunks == 0 && total > 0 {
			existing.BaseChunks = total
		}
		if existing.BaseBytes == 0 && res.Bytes > 0 {
			existing.BaseBytes = res.Bytes
		}
		// Re-arm completion firing when the cluster refills after a prior
		// completion. Without this reset a drain → fill → drain cycle
		// would silently skip the second event.
		if total > 0 && !existing.CompletionFiredAt.IsZero() {
			existing.CompletionFiredAt = time.Time{}
		}
		existing.MigratableChunks = res.MigratableChunks
		existing.StuckSinglePolicyChunks = res.StuckSinglePolicyChunks
		existing.StuckNoPolicyChunks = res.StuckNoPolicyChunks
		existing.Bytes = res.Bytes
		existing.ByBucket = cloneByBucket(res.ByBucket)
		existing.LastScanAt = now
		// Fire only on >0 → 0 transitions we actually observed. A
		// first-ever commit with chunks=0 (legacy bucket with no chunks
		// on the cluster) must NOT fire — there was nothing to drain.
		if hadSnap && prevTotal > 0 && total == 0 && existing.CompletionFiredAt.IsZero() {
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

func cloneSnapshot(s *ProgressSnapshot) ProgressSnapshot {
	out := *s
	out.ByBucket = cloneByBucket(s.ByBucket)
	return out
}

func cloneByBucket(in map[string]BucketScanCategory) map[string]BucketScanCategory {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]BucketScanCategory, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
