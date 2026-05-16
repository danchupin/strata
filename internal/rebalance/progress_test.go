package rebalance

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
)

func newDataMemBackend(t *testing.T) data.Backend { t.Helper(); return datamem.New() }

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// migratableScan builds a ScanResult with `n` migratable chunks at
// `1024` bytes each — the canonical "all-on-one-cluster, fan out to
// live" shape used by most tracker tests.
func migratableScan(n int64) ScanResult {
	return ScanResult{MigratableChunks: n, Bytes: n * 1024}
}

func TestProgressTrackerNilReceiverNoopsAndReadsEmpty(t *testing.T) {
	var p *ProgressTracker
	p.CommitScan([]string{"c1"}, map[string]ScanResult{"c1": migratableScan(1)}, time.Now())
	if _, ok := p.Snapshot("c1"); ok {
		t.Fatal("nil tracker must report no snapshot")
	}
	p.Reset("c1")
}

func TestProgressTrackerCommitSetsBaseAndUpserts(t *testing.T) {
	p := NewProgressTracker(10 * time.Minute)
	now := time.Unix(1_700_000_000, 0).UTC()

	p.CommitScan([]string{"c1"}, map[string]ScanResult{"c1": migratableScan(5)}, now)
	snap, ok := p.Snapshot("c1")
	if !ok {
		t.Fatal("expected snapshot after first commit")
	}
	if snap.Chunks() != 5 || snap.Bytes != 5*1024 {
		t.Fatalf("counts: got chunks=%d bytes=%d want 5/5120", snap.Chunks(), snap.Bytes)
	}
	if snap.MigratableChunks != 5 {
		t.Fatalf("MigratableChunks: got %d want 5", snap.MigratableChunks)
	}
	if snap.BaseChunks != 5 {
		t.Fatalf("BaseChunks: got %d want 5 (first commit captures base)", snap.BaseChunks)
	}
	if !snap.LastScanAt.Equal(now) {
		t.Fatalf("LastScanAt: got %v want %v", snap.LastScanAt, now)
	}

	// Second commit shrinks chunks → BaseChunks must NOT change.
	later := now.Add(time.Minute)
	p.CommitScan([]string{"c1"}, map[string]ScanResult{"c1": migratableScan(2)}, later)
	snap, _ = p.Snapshot("c1")
	if snap.Chunks() != 2 {
		t.Fatalf("Chunks after shrink: got %d want 2", snap.Chunks())
	}
	if snap.BaseChunks != 5 {
		t.Fatalf("BaseChunks must stick at first-seen value; got %d want 5", snap.BaseChunks)
	}
}

func TestProgressTrackerReapsUndrainedClusters(t *testing.T) {
	p := NewProgressTracker(time.Minute)
	now := time.Now().UTC()
	p.CommitScan([]string{"c1", "c2"}, map[string]ScanResult{"c1": migratableScan(3), "c2": {}}, now)
	if _, ok := p.Snapshot("c1"); !ok {
		t.Fatal("expected c1 snapshot after commit")
	}
	// Next tick only sees c2 as draining → c1 entry must be dropped.
	p.CommitScan([]string{"c2"}, map[string]ScanResult{"c2": {}}, now.Add(time.Second))
	if _, ok := p.Snapshot("c1"); ok {
		t.Fatal("c1 snapshot must be reaped after it leaves draining set")
	}
	if _, ok := p.Snapshot("c2"); !ok {
		t.Fatal("c2 snapshot must persist")
	}
}

func TestProgressTrackerZeroChunksOnFirstCommit(t *testing.T) {
	// A draining cluster with no matching chunks must still produce a
	// snapshot so deregister_ready flips immediately. BaseChunks stays
	// zero in that case (no chunks were ever observed).
	p := NewProgressTracker(time.Minute)
	now := time.Now().UTC()
	completions := p.CommitScan([]string{"c1"}, map[string]ScanResult{}, now)
	snap, ok := p.Snapshot("c1")
	if !ok {
		t.Fatal("expected zero-chunk snapshot")
	}
	if snap.Chunks() != 0 || snap.BaseChunks != 0 {
		t.Fatalf("zero-chunk first commit: got chunks=%d base=%d want 0/0", snap.Chunks(), snap.BaseChunks)
	}
	if len(completions) != 0 {
		t.Fatalf("zero-chunk first commit must NOT fire completion: got %d events", len(completions))
	}
}

// TestProgressTrackerCompletionFiresOnceOnDrainToZero exercises the
// canonical US-005 transition: 5 → 3 → 0 → 0 fires exactly one
// completion event on the 3 → 0 boundary.
func TestProgressTrackerCompletionFiresOnceOnDrainToZero(t *testing.T) {
	p := NewProgressTracker(time.Minute)
	t0 := time.Unix(1_700_000_000, 0).UTC()

	if got := p.CommitScan([]string{"c1"}, map[string]ScanResult{"c1": migratableScan(5)}, t0); len(got) != 0 {
		t.Fatalf("first commit at chunks=5 must not fire: got %d events", len(got))
	}
	if got := p.CommitScan([]string{"c1"}, map[string]ScanResult{"c1": migratableScan(3)}, t0.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("intermediate chunks=3 must not fire: got %d events", len(got))
	}
	t3 := t0.Add(2 * time.Minute)
	events := p.CommitScan([]string{"c1"}, map[string]ScanResult{"c1": {}}, t3)
	if len(events) != 1 {
		t.Fatalf("transition 3 → 0 must fire one event: got %d", len(events))
	}
	ev := events[0]
	if ev.Cluster != "c1" {
		t.Errorf("Cluster: got %q want c1", ev.Cluster)
	}
	if ev.BaseChunks != 5 {
		t.Errorf("BaseChunks: got %d want 5", ev.BaseChunks)
	}
	if ev.BaseBytes != 5*1024 {
		t.Errorf("BaseBytes: got %d want %d", ev.BaseBytes, 5*1024)
	}
	if ev.BytesMoved != 5*1024 {
		t.Errorf("BytesMoved: got %d want %d", ev.BytesMoved, 5*1024)
	}
	if !ev.ScanFinish.Equal(t3) {
		t.Errorf("ScanFinish: got %v want %v", ev.ScanFinish, t3)
	}
	// A second commit at chunks=0 must NOT re-fire (idempotency).
	if again := p.CommitScan([]string{"c1"}, map[string]ScanResult{"c1": {}}, t3.Add(time.Minute)); len(again) != 0 {
		t.Fatalf("0 → 0 must not re-fire: got %d events", len(again))
	}
	snap, _ := p.Snapshot("c1")
	if snap.CompletionFiredAt.IsZero() {
		t.Fatal("CompletionFiredAt must be stamped after firing")
	}
}

// TestProgressTrackerCompletionRefiresAfterRefill covers the
// 5 → 3 → 0 → 2 → 0 cycle (drain → fill → drain) — the second 0
// transition must re-fire because the cluster was refilled between
// completions.
func TestProgressTrackerCompletionRefiresAfterRefill(t *testing.T) {
	p := NewProgressTracker(time.Minute)
	t0 := time.Unix(1_700_000_000, 0).UTC()
	steps := []struct {
		chunks   int64
		bytes    int64
		wantFire bool
	}{
		{5, 5 * 1024, false},
		{3, 3 * 1024, false},
		{0, 0, true}, // first completion
		{2, 2 * 1024, false},
		{0, 0, true}, // re-fire after refill
	}
	for i, s := range steps {
		events := p.CommitScan([]string{"c1"},
			map[string]ScanResult{"c1": {MigratableChunks: s.chunks, Bytes: s.bytes}},
			t0.Add(time.Duration(i)*time.Minute))
		if s.wantFire && len(events) != 1 {
			t.Fatalf("step %d (chunks=%d) expected 1 completion event, got %d", i, s.chunks, len(events))
		}
		if !s.wantFire && len(events) != 0 {
			t.Fatalf("step %d (chunks=%d) expected no completion, got %d", i, s.chunks, len(events))
		}
	}
}

// TestProgressTrackerCompletionResetsOnReap — an undrain that reaps the
// snapshot must not leak CompletionFiredAt into a subsequent re-drain.
func TestProgressTrackerCompletionResetsOnReap(t *testing.T) {
	p := NewProgressTracker(time.Minute)
	t0 := time.Unix(1_700_000_000, 0).UTC()
	p.CommitScan([]string{"c1"}, map[string]ScanResult{"c1": migratableScan(4)}, t0)
	p.CommitScan([]string{"c1"}, map[string]ScanResult{"c1": {}}, t0.Add(time.Minute))
	if _, ok := p.Snapshot("c1"); !ok {
		t.Fatal("snapshot must persist while c1 still draining")
	}
	// Undrain — c1 leaves the draining set, tracker reaps it.
	p.CommitScan(nil, nil, t0.Add(2*time.Minute))
	if _, ok := p.Snapshot("c1"); ok {
		t.Fatal("snapshot must be reaped after undrain")
	}
	// Re-drain: 7 → 0 fresh. Must fire because the prior FiredAt died with the row.
	p.CommitScan([]string{"c1"}, map[string]ScanResult{"c1": migratableScan(7)}, t0.Add(3*time.Minute))
	events := p.CommitScan([]string{"c1"}, map[string]ScanResult{"c1": {}}, t0.Add(4*time.Minute))
	if len(events) != 1 {
		t.Fatalf("fresh-drain → 0 after reap must fire: got %d events", len(events))
	}
}

func TestWorkerProgressScanPopulatesTracker(t *testing.T) {
	m, _, _, _ := newRebalanceFixture(t)
	b, err := m.CreateBucket(context.Background(), "drainbkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	// Mark cluster "old" as draining so the worker's progress accumulator
	// counts chunks living on it.
	if err := m.SetClusterState(context.Background(), "old", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
		t.Fatalf("SetClusterState: %v", err)
	}
	if err := m.SetBucketPlacement(context.Background(), b.Name, map[string]int{"new": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	seedObject(t, m, b.ID, "obj-a", []string{"old", "old", "old"})
	seedObject(t, m, b.ID, "obj-b", []string{"new"})

	tracker := NewProgressTracker(time.Minute)
	w, err := New(Config{
		Meta:     m,
		Data:     newDataMemBackend(t),
		Logger:   newDiscardLogger(),
		Emitter:  &recordingEmitter{},
		Interval: time.Hour,
		Progress: tracker,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	snap, ok := tracker.Snapshot("old")
	if !ok {
		t.Fatal("expected snapshot for draining cluster")
	}
	if snap.Chunks() != 3 {
		t.Fatalf("Chunks: got %d want 3", snap.Chunks())
	}
	if snap.MigratableChunks != 3 {
		t.Fatalf("MigratableChunks: got %d want 3 (policy={new:1}, old draining → migratable)", snap.MigratableChunks)
	}
	if snap.BaseChunks != 3 {
		t.Fatalf("BaseChunks: got %d want 3", snap.BaseChunks)
	}
	// "new" is not draining → no snapshot ever materialises.
	if _, ok := tracker.Snapshot("new"); ok {
		t.Fatal("non-draining cluster must not appear in tracker")
	}
}

// captureNotifier records every DrainCompleteEvent observed for assertion.
type captureNotifier struct{ events []DrainCompleteEvent }

func (c *captureNotifier) NotifyDrainComplete(_ context.Context, evt DrainCompleteEvent) {
	c.events = append(c.events, evt)
}

// TestWorkerFiresDrainCompleteEnd2End drives the rebalance worker
// through a `>0 → 0` transition and asserts the full US-005 fan-out:
// per-cluster metric bump, audit_log row, notifier callback. Re-running
// the worker at chunks=0 must NOT re-fire any of the sinks
// (idempotency at the worker layer).
func TestWorkerFiresDrainCompleteEnd2End(t *testing.T) {
	m, _, _, _ := newRebalanceFixture(t)
	ctx := context.Background()
	b, err := m.CreateBucket(ctx, "drainbkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := m.SetClusterState(ctx, "old", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
		t.Fatalf("SetClusterState: %v", err)
	}
	if err := m.SetBucketPlacement(ctx, b.Name, map[string]int{"new": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	// Seed three chunks on the draining cluster — first tick observes them
	// and stamps BaseChunks=3.
	seedObject(t, m, b.ID, "obj-a", []string{"old", "old", "old"})

	tracker := NewProgressTracker(time.Minute)
	cm := &countingMetrics{}
	notifier := &captureNotifier{}
	w, err := New(Config{
		Meta:     m,
		Data:     newDataMemBackend(t),
		Logger:   newDiscardLogger(),
		Metrics:  cm,
		Emitter:  &recordingEmitter{},
		Interval: time.Hour,
		Progress: tracker,
		Notifier: notifier,
		AuditTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce baseline: %v", err)
	}
	if cm.drainsDone["old"] != 0 {
		t.Fatalf("baseline tick must not fire completion: got %d", cm.drainsDone["old"])
	}

	// Wipe the seeded object so the next scan observes zero chunks for `old`.
	if _, err := m.DeleteObject(ctx, b.ID, "obj-a", "", false); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce zero: %v", err)
	}
	if cm.drainsDone["old"] != 1 {
		t.Fatalf("transition >0 → 0 must bump metric exactly once: got %d", cm.drainsDone["old"])
	}
	if len(notifier.events) != 1 {
		t.Fatalf("notifier must fire once: got %d events", len(notifier.events))
	}
	ev := notifier.events[0]
	if ev.Cluster != "old" {
		t.Errorf("notifier cluster: got %q want old", ev.Cluster)
	}
	if ev.BytesMoved != 3*1024 {
		t.Errorf("notifier BytesMoved: got %d want %d", ev.BytesMoved, 3*1024)
	}
	if ev.CompletedAt.IsZero() {
		t.Error("notifier CompletedAt must be set")
	}
	// Audit row visible to listing.
	rows, _, err := m.ListAuditFiltered(ctx, meta.AuditFilter{Limit: 16})
	if err != nil {
		t.Fatalf("ListAuditFiltered: %v", err)
	}
	var found *meta.AuditEvent
	for i, row := range rows {
		if row.Action == "drain.complete" && row.Resource == "cluster:old" {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("audit_log missing drain.complete row; rows=%v", rows)
	}
	if found.Principal != "system:rebalance-worker" {
		t.Errorf("audit Principal: got %q want system:rebalance-worker", found.Principal)
	}
	if found.Bucket != "-" {
		t.Errorf("audit Bucket: got %q want '-'", found.Bucket)
	}

	// Third tick: still zero chunks → completion must not re-fire.
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce idle: %v", err)
	}
	if cm.drainsDone["old"] != 1 {
		t.Fatalf("idle 0 → 0 must not re-fire: got %d", cm.drainsDone["old"])
	}
	if len(notifier.events) != 1 {
		t.Fatalf("idle 0 → 0 must not re-fire notifier: got %d events", len(notifier.events))
	}
}

func TestWorkerProgressScanCountsEmptyPolicyBuckets(t *testing.T) {
	// Buckets without a Placement policy still feed the progress
	// accumulator so legacy buckets do not hide chunks on a draining
	// cluster from the deregister-ready signal.
	m, _, _, _ := newRebalanceFixture(t)
	b, err := m.CreateBucket(context.Background(), "legacy", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := m.SetClusterState(context.Background(), "old", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
		t.Fatalf("SetClusterState: %v", err)
	}
	seedObject(t, m, b.ID, "obj-a", []string{"old", "old"})

	tracker := NewProgressTracker(time.Minute)
	w, err := New(Config{
		Meta:     m,
		Data:     newDataMemBackend(t),
		Logger:   newDiscardLogger(),
		Emitter:  &recordingEmitter{},
		Interval: time.Hour,
		Progress: tracker,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	snap, ok := tracker.Snapshot("old")
	if !ok {
		t.Fatal("expected snapshot from empty-policy bucket scan")
	}
	if snap.Chunks() != 2 {
		t.Fatalf("Chunks: got %d want 2", snap.Chunks())
	}
	if snap.StuckNoPolicyChunks != 2 {
		t.Fatalf("empty-policy chunks must land in stuck_no_policy: got %d want 2", snap.StuckNoPolicyChunks)
	}
}

// TestClassifyBucketCovers — exercises every branch of the per-bucket
// classifier the worker uses to split chunks into the three drain-
// transparency categories (US-003 effective-placement: classifier now
// takes the resolved effective policy + raw policy + mode).
func TestClassifyBucketCovers(t *testing.T) {
	cases := []struct {
		name      string
		raw       map[string]int
		effective map[string]int
		mode      string
		want      string
	}{
		{"effective non-empty → migratable", map[string]int{"old": 1, "new": 1}, map[string]int{"new": 1}, "", "migratable"},
		{"effective empty + strict + raw set → stuck_single_policy", map[string]int{"old": 1}, nil, meta.PlacementModeStrict, "stuck_single_policy"},
		{"effective empty + weighted + raw set → stuck_no_policy (fallback exhausted)", map[string]int{"old": 1}, nil, meta.PlacementModeWeighted, "stuck_no_policy"},
		{"effective empty + default mode → stuck_no_policy", map[string]int{"old": 1}, nil, "", "stuck_no_policy"},
		{"effective empty + strict + no raw policy → stuck_no_policy (strict needs explicit policy)", nil, nil, meta.PlacementModeStrict, "stuck_no_policy"},
		{"empty raw + empty effective → stuck_no_policy", nil, nil, "", "stuck_no_policy"},
	}
	for _, tc := range cases {
		if got := ClassifyBucket(tc.raw, tc.effective, tc.mode); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// TestWorkerCategorizesChunksAcrossBuckets — the rebalance worker
// tick must split chunks on an evacuating cluster into the three
// categories based on each bucket's policy. Per-bucket breakdown
// (ByBucket) must mirror the per-cluster aggregates.
func TestWorkerCategorizesChunksAcrossBuckets(t *testing.T) {
	m, _, _, _ := newRebalanceFixture(t)
	ctx := context.Background()
	if err := m.SetClusterState(ctx, "old", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
		t.Fatalf("SetClusterState: %v", err)
	}

	// Bucket A: policy {old:1, new:1} → migratable (live cluster left).
	bA, err := m.CreateBucket(ctx, "bkt-mig", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket A: %v", err)
	}
	if err := m.SetBucketPlacement(ctx, bA.Name, map[string]int{"old": 1, "new": 1}); err != nil {
		t.Fatalf("SetBucketPlacement A: %v", err)
	}
	seedObject(t, m, bA.ID, "a1", []string{"old", "old"})

	// Bucket B: policy {old:1} mode=strict → stuck_single_policy (only
	// target draining, strict opts out of cluster-weights fallback).
	bB, err := m.CreateBucket(ctx, "bkt-single", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket B: %v", err)
	}
	if err := m.SetBucketPlacement(ctx, bB.Name, map[string]int{"old": 1}); err != nil {
		t.Fatalf("SetBucketPlacement B: %v", err)
	}
	if err := m.SetBucketPlacementMode(ctx, bB.Name, meta.PlacementModeStrict); err != nil {
		t.Fatalf("SetBucketPlacementMode B: %v", err)
	}
	seedObject(t, m, bB.ID, "b1", []string{"old", "old", "old"})

	// Bucket C: no policy → stuck_no_policy.
	bC, err := m.CreateBucket(ctx, "bkt-nopol", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket C: %v", err)
	}
	seedObject(t, m, bC.ID, "c1", []string{"old"})

	tracker := NewProgressTracker(time.Minute)
	w, err := New(Config{
		Meta:     m,
		Data:     newDataMemBackend(t),
		Logger:   newDiscardLogger(),
		Emitter:  &recordingEmitter{},
		Interval: time.Hour,
		Progress: tracker,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	snap, ok := tracker.Snapshot("old")
	if !ok {
		t.Fatal("expected snapshot for evacuating cluster")
	}
	if snap.MigratableChunks != 2 {
		t.Errorf("MigratableChunks: got %d want 2", snap.MigratableChunks)
	}
	if snap.StuckSinglePolicyChunks != 3 {
		t.Errorf("StuckSinglePolicyChunks: got %d want 3", snap.StuckSinglePolicyChunks)
	}
	if snap.StuckNoPolicyChunks != 1 {
		t.Errorf("StuckNoPolicyChunks: got %d want 1", snap.StuckNoPolicyChunks)
	}
	if got := snap.Chunks(); got != 6 {
		t.Errorf("Chunks() total: got %d want 6", got)
	}
	if got := snap.ByBucket; len(got) != 3 {
		t.Fatalf("ByBucket: got %d buckets want 3 (%v)", len(got), got)
	}
	if cat := snap.ByBucket["bkt-mig"]; cat.Category != "migratable" || cat.ChunkCount != 2 {
		t.Errorf("bkt-mig: got %+v want migratable/2", cat)
	}
	if cat := snap.ByBucket["bkt-single"]; cat.Category != "stuck_single_policy" || cat.ChunkCount != 3 {
		t.Errorf("bkt-single: got %+v want stuck_single_policy/3", cat)
	}
	if cat := snap.ByBucket["bkt-nopol"]; cat.Category != "stuck_no_policy" || cat.ChunkCount != 1 {
		t.Errorf("bkt-nopol: got %+v want stuck_no_policy/1", cat)
	}
}

// TestWorkerSkipsReadonlyClusters — clusters in state=draining_readonly
// must NOT appear in the progress tracker because no migration runs;
// they're stop-write only.
func TestWorkerSkipsReadonlyClusters(t *testing.T) {
	m, _, _, _ := newRebalanceFixture(t)
	ctx := context.Background()
	if err := m.SetClusterState(ctx, "old", meta.ClusterStateDrainingReadonly, meta.ClusterModeReadonly, 0); err != nil {
		t.Fatalf("SetClusterState: %v", err)
	}
	b, err := m.CreateBucket(ctx, "ro", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := m.SetBucketPlacement(ctx, b.Name, map[string]int{"old": 1, "new": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	seedObject(t, m, b.ID, "k", []string{"old", "old"})

	tracker := NewProgressTracker(time.Minute)
	w, err := New(Config{
		Meta:     m,
		Data:     newDataMemBackend(t),
		Logger:   newDiscardLogger(),
		Emitter:  &recordingEmitter{},
		Interval: time.Hour,
		Progress: tracker,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, ok := tracker.Snapshot("old"); ok {
		t.Fatal("readonly cluster must not produce a progress snapshot")
	}
}
