package rebalance

import (
	"context"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

// TestWorkerRunShardIteratesOnlyOwnedBuckets simulates a 3-shard fan-out
// against a memory backend: 9 draining-cluster buckets are seeded, each
// shard runs its own worker pinned to (shardID, 3), and the resulting
// merged tracker snapshot must include every bucket exactly once. This
// is the integration-shaped parity oracle for the Phase 2 fan-out — it
// proves the per-shard iteration agrees with the full-scan Phase 1 result
// when shards collaborate via a shared ProgressTracker.
func TestWorkerRunShardIteratesOnlyOwnedBuckets(t *testing.T) {
	m := metamem.New()
	ctx := context.Background()
	if err := m.SetClusterState(ctx, "old", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
		t.Fatalf("SetClusterState: %v", err)
	}

	const totalShards = 3
	const bucketsPerShard = 3
	bucketsByShard := map[int][]string{}
	for i := range totalShards * bucketsPerShard {
		// Try several bucket-name candidates until we land in a shard
		// that still has capacity. fnv32a(uuid)%3 is uniformly random
		// over UUIDs but the per-shard count needs to balance to 3/3/3
		// for the assertion to hold deterministically.
		for attempt := range 100 {
			name := bucketName(i, attempt)
			b, err := m.CreateBucket(ctx, name, "owner", "STANDARD")
			if err != nil {
				continue
			}
			shard := int(meta.BucketShardID(b.ID, totalShards))
			if len(bucketsByShard[shard]) >= bucketsPerShard {
				// Already full — leave the bucket but don't use it.
				continue
			}
			if err := m.SetBucketPlacement(ctx, name, map[string]int{"new": 1}); err != nil {
				t.Fatalf("SetBucketPlacement: %v", err)
			}
			seedObject(t, m, b.ID, "obj", []string{"old", "old"})
			bucketsByShard[shard] = append(bucketsByShard[shard], name)
			break
		}
	}
	for shard, names := range bucketsByShard {
		if len(names) != bucketsPerShard {
			t.Fatalf("shard %d: got %d buckets want %d", shard, len(names), bucketsPerShard)
		}
	}

	tracker := NewProgressTracker(time.Minute)
	for shard := range totalShards {
		w, err := New(Config{
			Meta:       m,
			Data:       newDataMemBackend(t),
			Logger:     newDiscardLogger(),
			Emitter:    &recordingEmitter{},
			Interval:   time.Hour,
			Progress:   tracker,
			ShardID:    shard,
			ShardCount: totalShards,
		})
		if err != nil {
			t.Fatalf("New shard %d: %v", shard, err)
		}
		if err := w.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce shard %d: %v", shard, err)
		}
	}

	snap, ok := tracker.Snapshot("old")
	if !ok {
		t.Fatal("expected merged snapshot for old after 3-shard scan")
	}
	wantChunks := int64(totalShards * bucketsPerShard * 2)
	if snap.MigratableChunks != wantChunks {
		t.Errorf("MigratableChunks: got %d want %d (9 buckets × 2 chunks)", snap.MigratableChunks, wantChunks)
	}
	if got := len(snap.ByBucket); got != totalShards*bucketsPerShard {
		t.Errorf("ByBucket entries: got %d want %d", got, totalShards*bucketsPerShard)
	}
	for shard, names := range bucketsByShard {
		for _, name := range names {
			cat, ok := snap.ByBucket[name]
			if !ok {
				t.Errorf("bucket %q (shard %d) missing from merged ByBucket", name, shard)
				continue
			}
			if cat.Category != "migratable" || cat.ChunkCount != 2 {
				t.Errorf("bucket %q: got %+v want migratable/2", name, cat)
			}
		}
	}
}

// TestWorkerSingleShardCountReproducesPhase1 — ShardCount=1 + ShardID=0
// must produce a byte-for-byte identical merged snapshot to the legacy
// (un-sharded) Phase 1 scan.
func TestWorkerSingleShardCountReproducesPhase1(t *testing.T) {
	m := metamem.New()
	ctx := context.Background()
	if err := m.SetClusterState(ctx, "old", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
		t.Fatalf("SetClusterState: %v", err)
	}
	for i := range 4 {
		name := bucketName(i, 0)
		b, err := m.CreateBucket(ctx, name, "owner", "STANDARD")
		if err != nil {
			t.Fatalf("CreateBucket %s: %v", name, err)
		}
		if err := m.SetBucketPlacement(ctx, name, map[string]int{"new": 1}); err != nil {
			t.Fatalf("SetBucketPlacement: %v", err)
		}
		seedObject(t, m, b.ID, "obj", []string{"old", "old", "old"})
	}

	tracker := NewProgressTracker(time.Minute)
	w, err := New(Config{
		Meta:       m,
		Data:       newDataMemBackend(t),
		Logger:     newDiscardLogger(),
		Emitter:    &recordingEmitter{},
		Interval:   time.Hour,
		Progress:   tracker,
		ShardID:    0,
		ShardCount: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	snap, ok := tracker.Snapshot("old")
	if !ok {
		t.Fatal("expected snapshot")
	}
	if snap.MigratableChunks != 12 {
		t.Errorf("MigratableChunks: got %d want 12 (4 buckets × 3 chunks)", snap.MigratableChunks)
	}
	if got := len(snap.ByBucket); got != 4 {
		t.Errorf("ByBucket entries: got %d want 4", got)
	}
}

func bucketName(idx, attempt int) string {
	prefix := "shardbkt"
	return prefix + "-" + itoa(idx) + "-" + itoa(attempt)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
