package workers

import (
	"context"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func TestReshardWorkerRegistered(t *testing.T) {
	w, ok := Lookup("reshard")
	if !ok {
		t.Fatal("reshard worker not registered (init() did not fire)")
	}
	if w.Name != "reshard" {
		t.Fatalf("name=%q want reshard", w.Name)
	}
	// Reshard does its CompleteReshard flip globally over queued jobs and does
	// NOT manage its own leader election, so it runs under the supervisor's
	// `reshard-leader` lease (SkipLease must stay false).
	if w.SkipLease {
		t.Fatal("reshard worker must run under the supervisor lease (SkipLease=false)")
	}
}

func TestBuildReshardReadsEnv(t *testing.T) {
	t.Setenv("STRATA_RESHARD_INTERVAL", "15s")
	t.Setenv("STRATA_RESHARD_BATCH_LIMIT", "250")

	r, err := buildReshard(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildReshard: %v", err)
	}
	rr, ok := r.(*reshardRunner)
	if !ok {
		t.Fatalf("buildReshard returned %T, want *reshardRunner", r)
	}
	if rr.interval != 15*time.Second {
		t.Errorf("interval = %v, want 15s", rr.interval)
	}
	if rr.worker == nil {
		t.Error("worker not constructed")
	}
}

func TestBuildReshardDefaultsWhenEnvUnset(t *testing.T) {
	r, err := buildReshard(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildReshard: %v", err)
	}
	rr := r.(*reshardRunner)
	if rr.interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s default", rr.interval)
	}
}

func TestBuildReshardRequiresMeta(t *testing.T) {
	if _, err := buildReshard(Dependencies{}); err == nil {
		t.Fatal("buildReshard: want error for missing meta, got nil")
	}
}

// TestReshardRunnerDrainsQueuedJobAndStops drives the runner end-to-end on a
// shard-agnostic backend: a queued reshard job must be drained (immediate
// complete no-op flips the shard count) on the first tick, and the runner must
// return cleanly on ctx cancel.
func TestReshardRunnerDrainsQueuedJobAndStops(t *testing.T) {
	store := metamem.New()
	ctx := context.Background()
	b, err := store.CreateBucket(ctx, "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := store.PutObject(ctx, &meta.Object{
		BucketID: b.ID, Key: "k", StorageClass: "STANDARD", ETag: "e",
		Mtime: time.Now().UTC(), Manifest: &data.Manifest{Class: "STANDARD"},
	}, false); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Snapshot the target BEFORE StartReshard: metamem.CreateBucket returns the
	// live map pointer and CompleteReshard flips b.ShardCount in place, so a poll
	// expression like `b.ShardCount*2` would drift from under us once the worker
	// completes. Compare against the constant instead.
	target := b.ShardCount * 2
	if _, err := store.StartReshard(ctx, b.ID, target); err != nil {
		t.Fatalf("start reshard: %v", err)
	}

	r, err := buildReshard(Dependencies{Meta: store, Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildReshard: %v", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(runCtx) }()

	// Poll until the first tick drains the queued job.
	deadline := time.After(time.Second)
	for {
		got, _ := store.GetBucket(ctx, "bkt")
		if got.ShardCount == target {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("reshard job not drained: shard count still %d", got.ShardCount)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, err := store.GetReshardJob(ctx, b.ID); err != meta.ErrReshardNotFound {
		t.Fatalf("job not removed after drain: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after cancel")
	}
}
