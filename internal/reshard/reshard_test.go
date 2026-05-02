package reshard_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/reshard"
)

func TestReshardWorkerCompletes1000Objects(t *testing.T) {
	store := memory.New()
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
	if stats.ObjectsCopied < 1000 {
		t.Fatalf("objects copied: %d", stats.ObjectsCopied)
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
	store := memory.New()
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
	if stats.ObjectsCopied >= 50 {
		t.Fatalf("resume should skip already-copied keys: copied=%d", stats.ObjectsCopied)
	}
}

func errInProgress(err error) bool {
	return err == meta.ErrReshardInProgress
}
