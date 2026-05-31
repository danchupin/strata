package reshard_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/reshard"
)

// recordingStore wraps a shard-agnostic backend (memory) with a
// meta.ReshardMigrator that records which keys the worker asks to migrate and
// reports one row moved per key. Memory itself does NOT implement
// ReshardMigrator (a reshard moves nothing on a flat map), so this fake is the
// in-package stand-in for a partition-rewriting backend (Cassandra) — it lets
// the worker tests assert the orchestration (walk every key, dedup a key's
// versions, resume from the watermark) deterministically without a container.
// The discriminating physical-move proof lives in the Cassandra integration
// suite (TestCassandraReshardWorkerMovesRows).
type recordingStore struct {
	meta.Store
	mu       sync.Mutex
	migrated []string
}

func (r *recordingStore) MigrateReshardKey(_ context.Context, _ uuid.UUID, key string) (int, error) {
	r.mu.Lock()
	r.migrated = append(r.migrated, key)
	r.mu.Unlock()
	return 1, nil
}

func (r *recordingStore) keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.migrated))
	copy(out, r.migrated)
	return out
}

func TestReshardWorkerCompletes1000Objects(t *testing.T) {
	store := &recordingStore{Store: memory.New()}
	ctx := context.Background()

	b, err := store.CreateBucket(ctx, "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	for i := 0; i < 1000; i++ {
		o := &meta.Object{
			BucketID:     b.ID,
			Key:          fmt.Sprintf("k-%04d", i),
			StorageClass: "STANDARD",
			ETag:         "etag",
			Mtime:        time.Now().UTC(),
			Manifest:     &data.Manifest{Class: "STANDARD"},
		}
		if err := store.PutObject(ctx, o, false); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	if got, _ := store.GetBucket(ctx, "bkt"); got.ShardCount != 64 {
		t.Fatalf("default shard count: got %d want 64", got.ShardCount)
	}

	if _, err := store.StartReshard(ctx, b.ID, 128); err != nil {
		t.Fatalf("start reshard: %v", err)
	}
	if _, err := store.StartReshard(ctx, b.ID, 256); !errInProgress(err) {
		t.Fatalf("second StartReshard: got %v want ErrReshardInProgress", err)
	}

	got, _ := store.GetBucket(ctx, "bkt")
	if got.TargetShardCount != 128 {
		t.Fatalf("target shard count after start: %d", got.TargetShardCount)
	}
	if got.ShardCount != 64 {
		t.Fatalf("active shard count must stay until cutover: %d", got.ShardCount)
	}

	res, err := store.ListObjects(ctx, b.ID, meta.ListOptions{Limit: 5000})
	if err != nil {
		t.Fatalf("list during reshard: %v", err)
	}
	if len(res.Objects) != 1000 {
		t.Fatalf("list during reshard: got %d want 1000", len(res.Objects))
	}

	worker, err := reshard.New(reshard.Config{Meta: store, BatchLimit: 200})
	if err != nil {
		t.Fatalf("worker new: %v", err)
	}
	stats, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("worker run: %v", err)
	}
	if stats.JobsCompleted != 1 {
		t.Fatalf("jobs completed: %d", stats.JobsCompleted)
	}
	// The worker must drive MigrateReshardKey for every distinct key exactly
	// once (versions of a key dedup to a single call), so all 1000 keys move.
	if stats.ObjectsCopied != 1000 {
		t.Fatalf("objects copied: got %d want 1000", stats.ObjectsCopied)
	}
	if migrated := store.keys(); len(migrated) != 1000 {
		t.Fatalf("migrate calls: got %d want 1000 (a key's versions must dedup to one call)", len(migrated))
	}

	got, _ = store.GetBucket(ctx, "bkt")
	if got.ShardCount != 128 {
		t.Fatalf("post-reshard shard count: %d", got.ShardCount)
	}
	if got.TargetShardCount != 0 {
		t.Fatalf("post-reshard target: %d", got.TargetShardCount)
	}

	if _, err := store.GetReshardJob(ctx, b.ID); err != meta.ErrReshardNotFound {
		t.Fatalf("post-reshard job lookup: %v", err)
	}

	res, err = store.ListObjects(ctx, b.ID, meta.ListOptions{Limit: 5000})
	if err != nil {
		t.Fatalf("list after reshard: %v", err)
	}
	if len(res.Objects) != 1000 {
		t.Fatalf("list after reshard: got %d want 1000", len(res.Objects))
	}
}

func TestReshardRejectsInvalidTarget(t *testing.T) {
	store := memory.New()
	ctx := context.Background()
	b, _ := store.CreateBucket(ctx, "bkt", "o", "STANDARD")

	for _, target := range []int{0, -1, 3, 64, 32} {
		if _, err := store.StartReshard(ctx, b.ID, target); err != meta.ErrReshardInvalidTarget {
			t.Fatalf("target=%d: got %v want ErrReshardInvalidTarget", target, err)
		}
	}
}

func TestReshardResumesFromWatermark(t *testing.T) {
	store := &recordingStore{Store: memory.New()}
	ctx := context.Background()
	b, _ := store.CreateBucket(ctx, "bkt", "o", "STANDARD")
	for i := 0; i < 50; i++ {
		_ = store.PutObject(ctx, &meta.Object{
			BucketID: b.ID, Key: fmt.Sprintf("k-%02d", i),
			Mtime: time.Now().UTC(), Manifest: &data.Manifest{},
		}, false)
	}
	if _, err := store.StartReshard(ctx, b.ID, 128); err != nil {
		t.Fatalf("start: %v", err)
	}
	job, _ := store.GetReshardJob(ctx, b.ID)
	job.LastKey = "k-25"
	if err := store.UpdateReshardJob(ctx, job); err != nil {
		t.Fatalf("update: %v", err)
	}

	worker, _ := reshard.New(reshard.Config{Meta: store, BatchLimit: 100})
	stats, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.JobsCompleted != 1 {
		t.Fatalf("jobs: %d", stats.JobsCompleted)
	}
	// Resume from the "k-25" watermark must skip the already-migrated prefix
	// k-00..k-24 — the worker only hands keys at or after the watermark to the
	// migrator, so fewer than half move.
	if stats.ObjectsCopied >= 50 {
		t.Fatalf("resume should skip already-copied keys: copied=%d", stats.ObjectsCopied)
	}
	for _, k := range store.keys() {
		if k < "k-25" {
			t.Fatalf("resume migrated key %q before the watermark k-25 — watermark not honoured", k)
		}
	}
}

func errInProgress(err error) bool {
	return err == meta.ErrReshardInProgress
}
