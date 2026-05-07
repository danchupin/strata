package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/lifecycle"
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
	bucket := fs.String("bucket", "", "bench bucket name; defaults to lcbench-<uuid> (auto-deleted at exit)")
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

	b, err := store.CreateBucket(ctx, *bucket, *owner, "STANDARD")
	if err != nil {
		return fmt.Errorf("create bench bucket: %w", err)
	}
	defer cleanupBenchLifecycle(ctx, store, b, *region, logger)

	if err := seedLifecycleObjects(ctx, store, b, *objects); err != nil {
		return fmt.Errorf("seed objects: %w", err)
	}
	rule := []byte(`<LifecycleConfiguration><Rule><ID>bench-expire</ID><Status>Enabled</Status>
		<Filter><Prefix></Prefix></Filter>
		<Expiration><Days>1</Days></Expiration>
	</Rule></LifecycleConfiguration>`)
	if err := store.SetBucketLifecycle(ctx, b.ID, rule); err != nil {
		return fmt.Errorf("set lifecycle: %w", err)
	}

	w := &lifecycle.Worker{
		Meta:        store,
		Data:        backend,
		Region:      *region,
		AgeUnit:     time.Hour,
		Concurrency: *concurrency,
		Logger:      logger,
	}

	started := time.Now()
	if err := w.RunOnce(ctx); err != nil {
		return fmt.Errorf("lifecycle run: %w", err)
	}
	elapsed := time.Since(started)

	res := newBenchResult("lifecycle", *objects, *concurrency, elapsed, cfg, started)
	if err := writeJSON(a.out, res); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	if err := pushBenchGauge(ctx, logger, "strata_lifecycle_bench_throughput", "strata_bench_lifecycle", res); err != nil {
		return err
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
