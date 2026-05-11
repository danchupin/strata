package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/gc"
	"github.com/danchupin/strata/internal/meta"
)

// cmdBenchGC pre-seeds N synthetic GC entries and times a single
// gc.Worker.RunOnce drain at the requested concurrency. Output is a single
// JSON line on stdout (machine-readable for the lab Makefile target) and a
// strata_gc_bench_throughput Prometheus gauge published to
// STRATA_PROM_PUSHGATEWAY when set.
func (a *app) cmdBenchGC(ctx context.Context, jsonOut bool, args []string) error {
	fs := flag.NewFlagSet("bench-gc", flag.ContinueOnError)
	fs.SetOutput(a.err)
	entries := fs.Int("entries", 10000, "number of synthetic GC entries to enqueue + drain")
	concurrency := fs.Int("concurrency", 1, "gc.Worker concurrency level (1..256)")
	shards := fs.Int("shards", 1, "STRATA_GC_SHARDS — Phase 2 multi-leader simulation; spawns N parallel workers each draining its own logical shard slice (1..1024)")
	region := fs.String("region", "", "GC region label; defaults to a unique bench-<uuid> label so seeded rows don't collide with real workers")
	pool := fs.String("pool", "bench", "ChunkRef.Pool stamped on every seeded entry")
	cluster := fs.String("cluster", "default", "ChunkRef.Cluster stamped on every seeded entry")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if *entries <= 0 {
		return fmt.Errorf("--entries must be > 0")
	}
	if *concurrency < 1 || *concurrency > 256 {
		return fmt.Errorf("--concurrency must be in [1, 256]")
	}
	if *shards < 1 || *shards > 1024 {
		return fmt.Errorf("--shards must be in [1, 1024]")
	}
	if *region == "" {
		*region = "bench-" + uuid.NewString()
	}
	_ = jsonOut // bench-gc always emits JSONL on stdout; --json kept for parity.

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
	defer backend.Close(context.Background())

	chunks := seedGCChunks(*entries, *cluster, *pool)
	if err := store.EnqueueChunkDeletion(ctx, *region, chunks); err != nil {
		return fmt.Errorf("seed enqueue: %w", err)
	}
	defer cleanupBenchGC(ctx, store, *region, logger)

	started := time.Now()
	var processed int
	if *shards == 1 {
		w := &gc.Worker{
			Meta:        store,
			Data:        backend,
			Region:      *region,
			Grace:       0,
			Batch:       *entries,
			Concurrency: *concurrency,
			Logger:      logger,
		}
		processed = w.RunOnce(ctx)
	} else {
		processed = runShardedDrain(ctx, store, backend, *region, *entries, *concurrency, *shards, logger)
	}
	elapsed := time.Since(started)
	if processed != *entries {
		logger.WarnContext(ctx, "bench-gc: processed != seeded",
			"processed", processed, "seeded", *entries)
	}

	res := newBenchResult("gc", *entries, *concurrency, elapsed, cfg, started)
	res.Shards = *shards
	if err := writeJSON(a.out, res); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	if err := pushBenchGauge(ctx, logger, "strata_gc_bench_throughput", "strata_bench_gc", res); err != nil {
		return err
	}
	return nil
}

// seedGCChunks builds N synthetic ChunkRefs with a unique OID prefix so two
// concurrent bench-gc runs don't collide on the same key in the meta backend.
func seedGCChunks(n int, cluster, pool string) []data.ChunkRef {
	prefix := uuid.NewString()
	chunks := make([]data.ChunkRef, 0, n)
	for i := range n {
		chunks = append(chunks, data.ChunkRef{
			Cluster: cluster,
			Pool:    pool,
			OID:     fmt.Sprintf("bench-gc-%s-%d", prefix, i),
			Size:    1,
		})
	}
	return chunks
}

// runShardedDrain spawns one gc.Worker per logical shard (Phase 2 multi-
// leader simulation) sharing the same meta + data backend. Each worker drains
// only entries with `entry.ShardID % shardCount == shardID`. Returns the sum
// of per-shard `RunOnce` returns, mirroring the production FanOut shape minus
// the leader-election lottery (the bench is single-process, so contention on
// `gc-leader-<i>` is moot — every shard goroutine is its own implicit leader).
func runShardedDrain(ctx context.Context, store meta.Store, backend data.Backend, region string, entries, concurrency, shards int, logger *slog.Logger) int {
	var processed atomic.Int64
	var wg sync.WaitGroup
	for shardID := range shards {
		wg.Go(func() {
			w := &gc.Worker{
				Meta:        store,
				Data:        backend,
				Region:      region,
				Grace:       0,
				Batch:       entries,
				Concurrency: concurrency,
				ShardID:     shardID,
				ShardCount:  shards,
				Logger:      logger.With("shard_id", shardID),
			}
			processed.Add(int64(w.RunOnce(ctx)))
		})
	}
	wg.Wait()
	return int(processed.Load())
}

// cleanupBenchGC drains any leftover GC entries in the bench region. Worst
// case (e.g. data-backend Delete failed mid-run) the queue still carries the
// rows after RunOnce; ack them with a synthetic worker so the same lab can
// re-run the bench without polluting future runs.
func cleanupBenchGC(ctx context.Context, store meta.Store, region string, logger *slog.Logger) {
	left, err := store.ListGCEntries(ctx, region, time.Now().Add(time.Hour), 100000)
	if err != nil {
		logger.WarnContext(ctx, "bench-gc cleanup list", "error", err.Error())
		return
	}
	for _, e := range left {
		if err := store.AckGCEntry(ctx, region, e); err != nil {
			logger.WarnContext(ctx, "bench-gc cleanup ack",
				"oid", e.Chunk.OID, "error", err.Error())
		}
	}
}
