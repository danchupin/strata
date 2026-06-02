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

// countingStore wraps a shard-agnostic backend and counts the two calls a
// real-migration pass would make per batch — ListObjectVersions (the walk) and
// UpdateReshardJob (the watermark write). A backend that does NOT implement
// meta.ReshardMigrator must be completed AT ONCE, so neither call may fire.
// Deliberately does NOT implement MigrateReshardKey — that is the property
// under test.
type countingStore struct {
	meta.Store
	listVersionsCalls int
	updateJobCalls    int
}

func (c *countingStore) ListObjectVersions(ctx context.Context, bucketID uuid.UUID, opts meta.ListOptions) (*meta.ListVersionsResult, error) {
	c.listVersionsCalls++
	return c.Store.ListObjectVersions(ctx, bucketID, opts)
}

func (c *countingStore) UpdateReshardJob(ctx context.Context, job *meta.ReshardJob) error {
	c.updateJobCalls++
	return c.Store.UpdateReshardJob(ctx, job)
}

// TestReshardNoOpOnShardAgnosticBackend proves the US-004 immediate-complete
// no-op: against a backend with no meta.ReshardMigrator (memory == TiKV
// semantics) the worker completes the job moving zero rows AND without walking
// the object set — no ListObjectVersions, no watermark write. The full key set
// stays readable before, during, and after, and the bucket shard count flips.
func TestReshardNoOpOnShardAgnosticBackend(t *testing.T) {
	store := &countingStore{Store: memory.New()}
	ctx := context.Background()
	b, err := store.CreateBucket(ctx, "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	const objects = 500
	want := make(map[string]struct{}, objects)
	for i := 0; i < objects; i++ {
		key := fmt.Sprintf("k-%04d", i)
		want[key] = struct{}{}
		if err := store.PutObject(ctx, &meta.Object{
			BucketID: b.ID, Key: key, StorageClass: "STANDARD", ETag: "e",
			Mtime: time.Now().UTC(), Manifest: &data.Manifest{Class: "STANDARD"},
		}, false); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	keySet := func(phase string) {
		res, err := store.ListObjects(ctx, b.ID, meta.ListOptions{Limit: 5000})
		if err != nil {
			t.Fatalf("list %s: %v", phase, err)
		}
		got := make(map[string]struct{}, len(res.Objects))
		for _, o := range res.Objects {
			got[o.Key] = struct{}{}
		}
		if len(got) != len(want) {
			t.Fatalf("key set %s: got %d keys want %d", phase, len(got), len(want))
		}
		for k := range want {
			if _, ok := got[k]; !ok {
				t.Fatalf("key set %s: missing %q", phase, k)
			}
		}
	}

	keySet("before")

	if _, err := store.StartReshard(ctx, b.ID, 128); err != nil {
		t.Fatalf("start reshard: %v", err)
	}
	// In flight: TargetShardCount stamped, active count unchanged, key set intact.
	if got, _ := store.GetBucket(ctx, "bkt"); got.TargetShardCount != 128 || got.ShardCount != 64 {
		t.Fatalf("during reshard: shard=%d target=%d want 64/128", got.ShardCount, got.TargetShardCount)
	}
	keySet("during")

	worker, err := reshard.New(reshard.Config{Meta: store, BatchLimit: 100})
	if err != nil {
		t.Fatalf("worker new: %v", err)
	}
	stats, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("worker run: %v", err)
	}
	if stats.JobsCompleted != 1 {
		t.Fatalf("jobs completed: got %d want 1", stats.JobsCompleted)
	}
	if stats.ObjectsCopied != 0 {
		t.Fatalf("objects copied: got %d want 0 (shard-agnostic backend moves nothing)", stats.ObjectsCopied)
	}
	// Immediate-complete: the job is finished without walking the object set or
	// writing a watermark. A walk would fire these counters.
	if store.listVersionsCalls != 0 {
		t.Fatalf("ListObjectVersions called %d times — no-op reshard must not walk the object set", store.listVersionsCalls)
	}
	if store.updateJobCalls != 0 {
		t.Fatalf("UpdateReshardJob called %d times — no-op reshard writes no watermark", store.updateJobCalls)
	}

	if got, _ := store.GetBucket(ctx, "bkt"); got.ShardCount != 128 || got.TargetShardCount != 0 {
		t.Fatalf("post-reshard: shard=%d target=%d want 128/0", got.ShardCount, got.TargetShardCount)
	}
	if _, err := store.GetReshardJob(ctx, b.ID); err != meta.ErrReshardNotFound {
		t.Fatalf("post-reshard job lookup: %v want ErrReshardNotFound", err)
	}
	keySet("after")
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

// crashingStore is a meta.ReshardMigrator that fails the first time it is asked
// to migrate failKey, then succeeds on every subsequent call. It models a
// worker crash mid-job: the failed pass returns an error before CompleteReshard,
// so the job stays queued and the watermark holds at the last fully-migrated
// batch boundary. A restart resumes from that watermark and converges.
type crashingStore struct {
	meta.Store
	failKey  string
	failed   bool
	migrated []string
}

func (c *crashingStore) MigrateReshardKey(_ context.Context, _ uuid.UUID, key string) (int, error) {
	if key == c.failKey && !c.failed {
		c.failed = true
		return 0, fmt.Errorf("simulated crash migrating %q", key)
	}
	c.migrated = append(c.migrated, key)
	return 1, nil
}

// TestReshardCrashResumesFromWatermark proves the US-005 crash-resume leg: a
// worker that dies partway through a job must, on restart, resume from the
// persisted LastKey watermark and still converge to a correct key set. The
// first RunOnce errors mid-walk (job NOT completed, shard count NOT flipped,
// watermark pinned at the last clean batch); the second RunOnce finishes the
// job. MigrateReshardKey is idempotent, so re-walking the partial batch is safe.
func TestReshardCrashResumesFromWatermark(t *testing.T) {
	store := &crashingStore{Store: memory.New(), failKey: "k-25"}
	ctx := context.Background()
	b, err := store.CreateBucket(ctx, "bkt", "o", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	const objects = 50
	for i := 0; i < objects; i++ {
		if err := store.PutObject(ctx, &meta.Object{
			BucketID: b.ID, Key: fmt.Sprintf("k-%02d", i), StorageClass: "STANDARD",
			ETag: "e", Mtime: time.Now().UTC(), Manifest: &data.Manifest{Class: "STANDARD"},
		}, false); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if _, err := store.StartReshard(ctx, b.ID, 128); err != nil {
		t.Fatalf("start reshard: %v", err)
	}

	worker, err := reshard.New(reshard.Config{Meta: store, BatchLimit: 10})
	if err != nil {
		t.Fatalf("worker new: %v", err)
	}

	// First pass: must error at the simulated crash and leave the job queued.
	if _, err := worker.RunOnce(ctx); err == nil {
		t.Fatal("first RunOnce: expected error from simulated crash, got nil")
	}
	job, err := store.GetReshardJob(ctx, b.ID)
	if err != nil {
		t.Fatalf("job must survive a crash: %v", err)
	}
	// The ListObjectVersions marker is inclusive (key >= marker), so each batch
	// re-lists the prior batch's last key. With BatchLimit=10 the batches are
	// k-00..k-09, k-09..k-18, k-18..k-27 — the crash on k-25 lands in the third
	// batch, so the last persisted watermark is the second batch's tail, k-18.
	if job.LastKey != "k-18" {
		t.Fatalf("watermark after crash: got %q want k-18", job.LastKey)
	}
	if got, _ := store.GetBucket(ctx, "bkt"); got.ShardCount != 64 {
		t.Fatalf("shard count must NOT flip on a crashed job: got %d want 64", got.ShardCount)
	}

	// Second pass: the crash flag is spent, so the worker resumes from k-19 and
	// drives the job to completion.
	stats, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("resume RunOnce: %v", err)
	}
	if stats.JobsCompleted != 1 {
		t.Fatalf("resume jobs completed: got %d want 1", stats.JobsCompleted)
	}
	if got, _ := store.GetBucket(ctx, "bkt"); got.ShardCount != 128 || got.TargetShardCount != 0 {
		t.Fatalf("post-resume: shard=%d target=%d want 128/0", got.ShardCount, got.TargetShardCount)
	}
	if _, err := store.GetReshardJob(ctx, b.ID); err != meta.ErrReshardNotFound {
		t.Fatalf("post-resume job lookup: %v want ErrReshardNotFound", err)
	}

	// Convergence: every key must have been migrated at least once across the
	// two passes (the watermark-replayed prefix may appear twice — idempotent).
	seen := make(map[string]struct{}, objects)
	for _, k := range store.migrated {
		seen[k] = struct{}{}
	}
	for i := 0; i < objects; i++ {
		k := fmt.Sprintf("k-%02d", i)
		if _, ok := seen[k]; !ok {
			t.Fatalf("key %q never migrated — crash-resume did not converge", k)
		}
	}
}

func errInProgress(err error) bool {
	return err == meta.ErrReshardInProgress
}
