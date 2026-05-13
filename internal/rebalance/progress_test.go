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

func TestProgressTrackerNilReceiverNoopsAndReadsEmpty(t *testing.T) {
	var p *ProgressTracker
	p.CommitScan([]string{"c1"}, map[string]int64{"c1": 1}, map[string]int64{"c1": 1024}, time.Now())
	if _, ok := p.Snapshot("c1"); ok {
		t.Fatal("nil tracker must report no snapshot")
	}
	p.Reset("c1")
}

func TestProgressTrackerCommitSetsBaseAndUpserts(t *testing.T) {
	p := NewProgressTracker(10 * time.Minute)
	now := time.Unix(1_700_000_000, 0).UTC()

	p.CommitScan([]string{"c1"}, map[string]int64{"c1": 5}, map[string]int64{"c1": 5 * 1024}, now)
	snap, ok := p.Snapshot("c1")
	if !ok {
		t.Fatal("expected snapshot after first commit")
	}
	if snap.Chunks != 5 || snap.Bytes != 5*1024 {
		t.Fatalf("counts: got chunks=%d bytes=%d want 5/5120", snap.Chunks, snap.Bytes)
	}
	if snap.BaseChunks != 5 {
		t.Fatalf("BaseChunks: got %d want 5 (first commit captures base)", snap.BaseChunks)
	}
	if !snap.LastScanAt.Equal(now) {
		t.Fatalf("LastScanAt: got %v want %v", snap.LastScanAt, now)
	}

	// Second commit shrinks chunks → BaseChunks must NOT change.
	later := now.Add(time.Minute)
	p.CommitScan([]string{"c1"}, map[string]int64{"c1": 2}, map[string]int64{"c1": 2 * 1024}, later)
	snap, _ = p.Snapshot("c1")
	if snap.Chunks != 2 {
		t.Fatalf("Chunks after shrink: got %d want 2", snap.Chunks)
	}
	if snap.BaseChunks != 5 {
		t.Fatalf("BaseChunks must stick at first-seen value; got %d want 5", snap.BaseChunks)
	}
}

func TestProgressTrackerReapsUndrainedClusters(t *testing.T) {
	p := NewProgressTracker(time.Minute)
	now := time.Now().UTC()
	p.CommitScan([]string{"c1", "c2"}, map[string]int64{"c1": 3, "c2": 0}, map[string]int64{"c1": 3, "c2": 0}, now)
	if _, ok := p.Snapshot("c1"); !ok {
		t.Fatal("expected c1 snapshot after commit")
	}
	// Next tick only sees c2 as draining → c1 entry must be dropped.
	p.CommitScan([]string{"c2"}, map[string]int64{"c2": 0}, map[string]int64{"c2": 0}, now.Add(time.Second))
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
	p.CommitScan([]string{"c1"}, map[string]int64{}, map[string]int64{}, now)
	snap, ok := p.Snapshot("c1")
	if !ok {
		t.Fatal("expected zero-chunk snapshot")
	}
	if snap.Chunks != 0 || snap.BaseChunks != 0 {
		t.Fatalf("zero-chunk first commit: got chunks=%d base=%d want 0/0", snap.Chunks, snap.BaseChunks)
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
	if err := m.SetClusterState(context.Background(), "old", meta.ClusterStateDraining); err != nil {
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
	if snap.Chunks != 3 {
		t.Fatalf("Chunks: got %d want 3", snap.Chunks)
	}
	if snap.BaseChunks != 3 {
		t.Fatalf("BaseChunks: got %d want 3", snap.BaseChunks)
	}
	// "new" is not draining → no snapshot ever materialises.
	if _, ok := tracker.Snapshot("new"); ok {
		t.Fatal("non-draining cluster must not appear in tracker")
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
	if err := m.SetClusterState(context.Background(), "old", meta.ClusterStateDraining); err != nil {
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
	if snap.Chunks != 2 {
		t.Fatalf("Chunks: got %d want 2", snap.Chunks)
	}
}
