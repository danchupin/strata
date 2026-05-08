package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/lifecycle"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/meta"
)

// cmdBenchLifecycle pre-seeds a bench bucket with N objects each carrying an
// Mtime in the past, attaches a 1-day expiration rule, and times a single
// lifecycle.Worker.RunOnce at the requested concurrency. Output mirrors
// bench-gc: one JSON line on stdout + a strata_lifecycle_bench_throughput
// gauge pushed to STRATA_PROM_PUSHGATEWAY when set.
func (a *app) cmdBenchLifecycle(ctx context.Context, jsonOut bool, args []string) error {
	fs := flag.NewFlagSet("bench-lifecycle", flag.ContinueOnError)
	fs.SetOutput(a.err)
	objects := fs.Int("objects", 10000, "number of synthetic objects to seed + expire in one tick")
	concurrency := fs.Int("concurrency", 1, "lifecycle.Worker concurrency level (1..256)")
	replicas := fs.Int("replicas", 1, "Phase 2 multi-replica simulation; spawns N parallel workers each pinned to ReplicaInfo=(N, i) racing for per-bucket leases (1..16)")
	buckets := fs.Int("buckets", 0, "number of bench buckets to seed; 0 means one bucket (legacy single-replica shape). Recommended >= replicas*3 so the distribution gate has work to spread")
	bucket := fs.String("bucket", "", "bench bucket name; defaults to lcbench-<uuid> (auto-deleted at exit). Only honoured when --buckets<=1")
	region := fs.String("region", "default", "lifecycle worker region label")
	owner := fs.String("owner", "bench", "bucket owner label")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if *objects <= 0 {
		return fmt.Errorf("--objects must be > 0")
	}
	if *concurrency < 1 || *concurrency > 256 {
		return fmt.Errorf("--concurrency must be in [1, 256]")
	}
	if *replicas < 1 || *replicas > 16 {
		return fmt.Errorf("--replicas must be in [1, 16]")
	}
	if *buckets < 0 {
		return fmt.Errorf("--buckets must be >= 0")
	}
	if *bucket == "" {
		*bucket = "lcbench-" + strings.ReplaceAll(uuid.NewString()[:12], "-", "")
	}
	_ = jsonOut

	logger := slog.New(slog.NewJSONHandler(a.err, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	store, err := buildRewrapMetaStore(cfg, logger)
	if err != nil {
		return fmt.Errorf("meta store: %w", err)
	}
	defer store.Close()
	backend, err := buildBenchDataBackend(cfg, logger)
	if err != nil {
		return fmt.Errorf("data backend: %w", err)
	}
	defer backend.Close()

	bucketCount := *buckets
	if bucketCount <= 0 {
		bucketCount = 1
	}
	bs := make([]*meta.Bucket, 0, bucketCount)
	for i := range bucketCount {
		name := *bucket
		if bucketCount > 1 || name == "" {
			name = fmt.Sprintf("lcbench-%s-%d",
				strings.ReplaceAll(uuid.NewString()[:8], "-", ""), i)
		}
		b, err := store.CreateBucket(ctx, name, *owner, "STANDARD")
		if err != nil {
			return fmt.Errorf("create bench bucket %d: %w", i, err)
		}
		bs = append(bs, b)
	}
	defer func() {
		for _, b := range bs {
			cleanupBenchLifecycle(ctx, store, b, *region, logger)
		}
	}()

	perBucket := max(*objects/bucketCount, 1)
	totalSeeded := 0
	rule := []byte(`<LifecycleConfiguration><Rule><ID>bench-expire</ID><Status>Enabled</Status>
		<Filter><Prefix></Prefix></Filter>
		<Expiration><Days>1</Days></Expiration>
	</Rule></LifecycleConfiguration>`)
	for _, b := range bs {
		if err := seedLifecycleObjects(ctx, store, b, perBucket); err != nil {
			return fmt.Errorf("seed objects: %w", err)
		}
		if err := store.SetBucketLifecycle(ctx, b.ID, rule); err != nil {
			return fmt.Errorf("set lifecycle: %w", err)
		}
		totalSeeded += perBucket
	}

	started := time.Now()
	if *replicas == 1 {
		w := &lifecycle.Worker{
			Meta:        store,
			Data:        backend,
			Region:      *region,
			AgeUnit:     time.Hour,
			Concurrency: *concurrency,
			Logger:      logger,
		}
		if err := w.RunOnce(ctx); err != nil {
			return fmt.Errorf("lifecycle run: %w", err)
		}
	} else {
		if err := runMultiReplicaLifecycle(ctx, store, backend, *region, *concurrency, *replicas, logger); err != nil {
			return fmt.Errorf("lifecycle multi-replica run: %w", err)
		}
	}
	elapsed := time.Since(started)

	res := newBenchResult("lifecycle", totalSeeded, *concurrency, elapsed, cfg, started)
	res.Shards = *replicas
	if err := writeJSON(a.out, res); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	if err := pushBenchGauge(ctx, logger, "strata_lifecycle_bench_throughput", "strata_bench_lifecycle", res); err != nil {
		return err
	}
	return nil
}

// runMultiReplicaLifecycle simulates the Phase 2 multi-replica deploy in one
// process: spawns N parallel lifecycle.Worker goroutines, each pinned to
// ReplicaInfo=(N, i) racing for `lifecycle-leader-<bucketID>` leases on a
// shared in-process locker. Returns once all replicas have completed one
// pass over their bucket subset. Mirrors the production STRATA_GC_SHARDS=N
// shape minus the cross-host lease lottery (in-process Acquire is FIFO).
func runMultiReplicaLifecycle(ctx context.Context, store meta.Store, backend data.Backend, region string, concurrency, replicas int, logger *slog.Logger) error {
	locker := metamem.NewLocker()
	var wg sync.WaitGroup
	errCh := make(chan error, replicas)
	for i := range replicas {
		wg.Go(func() {
			w := &lifecycle.Worker{
				Meta:        store,
				Data:        backend,
				Region:      region,
				AgeUnit:     time.Hour,
				Concurrency: concurrency,
				Logger:      logger.With("replica_id", i),
				Locker:      locker,
				ReplicaInfo: func() (int, int) { return replicas, i },
			}
			if err := w.RunOnce(ctx); err != nil {
				errCh <- err
			}
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// seedLifecycleObjects creates N meta.Object rows with Mtime ~2h in the past
// so the 1-day expiration rule (with AgeUnit=Hour) triggers on every entry
// in a single worker tick. Manifests carry a single synthetic ChunkRef so
// the lifecycle expire path enqueues a GC entry per object — exercising the
// worker's full per-object work, not a degenerate no-op.
func seedLifecycleObjects(ctx context.Context, store meta.Store, b *meta.Bucket, n int) error {
	now := time.Now()
	prefix := uuid.NewString()
	for i := range n {
		oid := fmt.Sprintf("bench-lc-%s-%d", prefix, i)
		obj := &meta.Object{
			BucketID:     b.ID,
			Key:          fmt.Sprintf("k-%07d", i),
			Size:         1,
			ETag:         fmt.Sprintf("%032x", i),
			StorageClass: "STANDARD",
			Mtime:        now.Add(-2 * time.Hour),
			Manifest: &data.Manifest{
				Class: "STANDARD",
				Size:  1,
				Chunks: []data.ChunkRef{{
					Cluster: "bench",
					Pool:    "bench",
					OID:     oid,
					Size:    1,
				}},
			},
		}
		if err := store.PutObject(ctx, obj, false); err != nil {
			return fmt.Errorf("seed PutObject[%d]: %w", i, err)
		}
	}
	return nil
}

// cleanupBenchLifecycle drains the bench bucket: drops the lifecycle rule,
// deletes any survivor objects, then deletes the bucket itself, then drains
// the bench-region GC queue (lifecycle expire enqueues per-object). Best-
// effort — failures log WARN so the same lab can re-run the bench cleanly.
func cleanupBenchLifecycle(ctx context.Context, store meta.Store, b *meta.Bucket, region string, logger *slog.Logger) {
	if err := store.DeleteBucketLifecycle(ctx, b.ID); err != nil {
		logger.WarnContext(ctx, "bench-lifecycle cleanup: delete lifecycle", "error", err.Error())
	}
	for {
		res, err := store.ListObjects(ctx, b.ID, meta.ListOptions{Limit: 1000})
		if err != nil {
			logger.WarnContext(ctx, "bench-lifecycle cleanup: list", "error", err.Error())
			break
		}
		if len(res.Objects) == 0 {
			break
		}
		for _, o := range res.Objects {
			if _, err := store.DeleteObject(ctx, b.ID, o.Key, "", false); err != nil {
				logger.WarnContext(ctx, "bench-lifecycle cleanup: delete object",
					"key", o.Key, "error", err.Error())
			}
		}
		if !res.Truncated {
			break
		}
	}
	if err := store.DeleteBucket(ctx, b.Name); err != nil {
		logger.WarnContext(ctx, "bench-lifecycle cleanup: delete bucket",
			"bucket", b.Name, "error", err.Error())
	}
	left, err := store.ListGCEntries(ctx, region, time.Now().Add(time.Hour), 100000)
	if err != nil {
		logger.WarnContext(ctx, "bench-lifecycle cleanup: list gc", "error", err.Error())
		return
	}
	for _, e := range left {
		if !strings.HasPrefix(e.Chunk.OID, "bench-lc-") {
			continue
		}
		if err := store.AckGCEntry(ctx, region, e); err != nil {
			logger.WarnContext(ctx, "bench-lifecycle cleanup: ack gc",
				"oid", e.Chunk.OID, "error", err.Error())
		}
	}
}
