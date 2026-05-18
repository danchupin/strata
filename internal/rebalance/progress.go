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
// extended in US-002 drain-transparency with categorized counters,
// US-003 rebalance-scale-phase-2 with per-shard aggregation).
//
// On the read side this struct is the MERGED view across every shard
// that committed a scan for this cluster: counters / Bytes / BaseChunks
// / BaseBytes sum across shards, LastScanAt is the latest, ByBucket
// merges per-bucket maps (each bucket lives in exactly one shard so
// keys do not collide). Operator-facing wire shape is unchanged.
//
// Stuck counters never converge to zero through migration alone — only
// an operator policy edit (US-005 BulkPlacementFixDialog) can move them.
//
// BaseChunks / BaseBytes are captured per-shard on the first scan after
// the cluster transitioned to draining; the merged BaseChunks feeds the
// UI's percent-filled rendering, the merged BaseBytes feeds the US-005
// completion event's final_bytes_moved.
//
// CompletionFiredAt records the timestamp the rebalance worker emitted
// the most recent drain-complete event for this cluster (US-005). Zero
// means no event has fired since this snapshot row was created. The
// tracker clears the field on a `merged-total: 0 → >0` transition so
// refill-then-redrain shapes (`5 → 0 → 2 → 0`) re-fire on the second
// `→ 0`. Tracked at the per-cluster level (not per-shard) so multiple
// shards hitting zero simultaneously fire exactly once.
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

// shardContribution is one shard's per-tick aggregate for a single
// draining cluster (US-003 rebalance-scale-phase-2). The tracker keeps
// one of these per (cluster, shardID) so the per-shard BaseChunks
// capture-on-first-non-zero semantic survives the fan-out — multiple
// goroutines on the same replica each independently observe their
// shard's first non-zero scan and stamp the per-shard base.
type shardContribution struct {
	MigratableChunks        int64
	StuckSinglePolicyChunks int64
	StuckNoPolicyChunks     int64
	Bytes                   int64
	ByBucket                map[string]BucketScanCategory
	LastScanAt              time.Time
	BaseChunks              int64
	BaseBytes               int64
}

func (s *shardContribution) chunks() int64 {
	return s.MigratableChunks + s.StuckSinglePolicyChunks + s.StuckNoPolicyChunks
}

// clusterEntry holds every shard's contribution for one cluster plus the
// cluster-level CompletionFiredAt (US-003 rebalance-scale-phase-2). The
// fire-once gate lives at the cluster level so a `merged-total: >0 → 0`
// transition that spans multiple shards still fires exactly one event.
type clusterEntry struct {
	shards            map[int]*shardContribution
	completionFiredAt time.Time
}

// ProgressTracker is the in-process draining-progress cache shared
// between the rebalance worker (writer) and the adminapi handler
// (reader). Goroutine-safe. nil is a valid receiver — every method
// short-circuits so callers do not need to nil-check.
//
// Per-shard storage (US-003 rebalance-scale-phase-2): CommitScan takes
// a shardID and writes to that shard's slot under the cluster entry;
// Snapshot returns the merged view across every shard.
type ProgressTracker struct {
	// Interval is the worker's tick cadence (clamped). The drain-progress
	// admin handler uses 2*Interval as the stale-cache threshold.
	Interval time.Duration

	mu        sync.RWMutex
	snapshots map[string]*clusterEntry
}

// NewProgressTracker builds a tracker. interval defaults to one hour when
// <= 0 so the stale-cache threshold stays sane on bare unit-test rigs.
func NewProgressTracker(interval time.Duration) *ProgressTracker {
	if interval <= 0 {
		interval = time.Hour
	}
	return &ProgressTracker{
		Interval:  interval,
		snapshots: map[string]*clusterEntry{},
	}
}

// Snapshot returns the most recent committed snapshot for clusterID plus
// an ok flag. ok=false means the tracker has no row — either the worker
// has never observed a draining cluster with that id, or the cluster
// just transitioned out of draining and the row was reaped. Counters /
// Bytes / BaseChunks / BaseBytes sum across every shard that has
// committed for this cluster; LastScanAt is the latest across shards;
// ByBucket merges per-bucket maps (each bucket lives in exactly one
// shard so keys never collide — duplicates sum defensively).
func (p *ProgressTracker) Snapshot(clusterID string) (ProgressSnapshot, bool) {
	if p == nil {
		return ProgressSnapshot{}, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry, ok := p.snapshots[clusterID]
	if !ok || len(entry.shards) == 0 {
		return ProgressSnapshot{}, false
	}
	return mergeShardSnapshots(entry), true
}

// CommitScan finalises one worker tick for `shardID`. draining is the
// set of cluster ids the worker selected for migration scan
// (state=evacuating in US-002). scans is keyed by cluster id with this
// shard's per-cluster aggregates. Clusters listed in draining but absent
// from scans get a zero-count slot — they may have been seeded without
// any draining-cluster chunks observed on this shard's bucket subset.
//
// Behaviour:
//   - For each cluster in draining, this shard's slot is upserted with
//     the fresh counters + LastScanAt=now. BaseChunks / BaseBytes are
//     stamped on the slot's first non-zero observation (per-shard).
//   - For each cluster present in the cache but NOT in draining
//     (operator undrain or shard-set change), this shard's slot is
//     dropped; the cluster entry is reaped only after every shard slot
//     is gone.
//
// Returns the per-cluster completion events detected during the commit:
// a cluster appears in the slice iff the cluster's merged total
// transitioned `>0 → 0` AND CompletionFiredAt was previously zero. The
// FiredAt stamp lives at the cluster level (not per-shard) so multiple
// shards reaching zero in the same tick produce exactly one event.
// Re-firing requires a subsequent merged 0 → >0 transition.
func (p *ProgressTracker) CommitScan(shardID int, draining []string, scans map[string]ScanResult, now time.Time) []CompletionEvent {
	if p == nil {
		return nil
	}
	want := make(map[string]struct{}, len(draining))
	for _, id := range draining {
		want[id] = struct{}{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, entry := range p.snapshots {
		if _, keep := want[id]; keep {
			continue
		}
		delete(entry.shards, shardID)
		if len(entry.shards) == 0 {
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
		entry, ok := p.snapshots[id]
		if !ok {
			entry = &clusterEntry{shards: map[int]*shardContribution{}}
			p.snapshots[id] = entry
		}
		// Snapshot prev merged total BEFORE mutating the shard slot so
		// the >0 → 0 transition guard sees the true previous-tick state.
		prevMerged := int64(0)
		for _, s := range entry.shards {
			prevMerged += s.chunks()
		}
		slot, hadSlot := entry.shards[shardID]
		if !hadSlot {
			slot = &shardContribution{}
			entry.shards[shardID] = slot
		}
		prevSlot := slot.chunks()
		total := res.Chunks()
		if slot.BaseChunks == 0 && total > 0 {
			slot.BaseChunks = total
		}
		if slot.BaseBytes == 0 && res.Bytes > 0 {
			slot.BaseBytes = res.Bytes
		}
		slot.MigratableChunks = res.MigratableChunks
		slot.StuckSinglePolicyChunks = res.StuckSinglePolicyChunks
		slot.StuckNoPolicyChunks = res.StuckNoPolicyChunks
		slot.Bytes = res.Bytes
		slot.ByBucket = cloneByBucket(res.ByBucket)
		slot.LastScanAt = now

		newMerged := prevMerged - prevSlot + total
		// Re-arm completion firing when the cluster refills after a prior
		// completion. Without this reset a drain → fill → drain cycle
		// would silently skip the second event.
		if newMerged > 0 && !entry.completionFiredAt.IsZero() {
			entry.completionFiredAt = time.Time{}
		}
		// Fire only on `>0 → 0` transitions of the merged total. A
		// first-ever commit with chunks=0 (legacy bucket with no chunks
		// on the cluster) must NOT fire — there was nothing to drain.
		if prevMerged > 0 && newMerged == 0 && entry.completionFiredAt.IsZero() {
			entry.completionFiredAt = now
			var baseChunks, baseBytes int64
			for _, s := range entry.shards {
				baseChunks += s.BaseChunks
				baseBytes += s.BaseBytes
			}
			completions = append(completions, CompletionEvent{
				Cluster:    id,
				BaseChunks: baseChunks,
				BaseBytes:  baseBytes,
				BytesMoved: baseBytes,
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

// mergeShardSnapshots projects a clusterEntry's per-shard slots into the
// caller-facing ProgressSnapshot. Caller holds the lock.
func mergeShardSnapshots(entry *clusterEntry) ProgressSnapshot {
	out := ProgressSnapshot{CompletionFiredAt: entry.completionFiredAt}
	for _, s := range entry.shards {
		out.MigratableChunks += s.MigratableChunks
		out.StuckSinglePolicyChunks += s.StuckSinglePolicyChunks
		out.StuckNoPolicyChunks += s.StuckNoPolicyChunks
		out.Bytes += s.Bytes
		out.BaseChunks += s.BaseChunks
		out.BaseBytes += s.BaseBytes
		if s.LastScanAt.After(out.LastScanAt) {
			out.LastScanAt = s.LastScanAt
		}
		for name, cat := range s.ByBucket {
			if out.ByBucket == nil {
				out.ByBucket = map[string]BucketScanCategory{}
			}
			if existing, ok := out.ByBucket[name]; ok {
				existing.ChunkCount += cat.ChunkCount
				existing.BytesUsed += cat.BytesUsed
				out.ByBucket[name] = existing
			} else {
				out.ByBucket[name] = cat
			}
		}
	}
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
