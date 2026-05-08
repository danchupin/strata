package gc

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
)

// Metrics is the narrow observer the worker uses to publish queue depth.
// Cmd-layer plugs metrics.GCObserver{}.
type Metrics interface {
	SetQueueDepth(region string, depth int)
}

type Worker struct {
	Meta        meta.Store
	Data        data.Backend
	Region      string
	Interval    time.Duration
	Grace       time.Duration
	Batch       int
	Concurrency int
	// ShardID / ShardCount select the runtime shard slice this worker drains.
	// ShardCount=1, ShardID=0 (the zero-value default) reproduces the
	// single-leader Phase 1 shape: one worker drains the entire queue. With
	// ShardCount=N>1 the worker only fetches entries belonging to its slot
	// (entry.ShardID % ShardCount == ShardID); FanOut spawns one Worker per
	// slot.
	ShardID    int
	ShardCount int
	Logger     *slog.Logger
	Metrics    Metrics
}

func (w *Worker) Run(ctx context.Context) error {
	if w.Interval == 0 {
		w.Interval = 30 * time.Second
	}
	if w.Grace < 0 {
		w.Grace = 0
	}
	if w.Batch == 0 {
		w.Batch = 500
	}
	if w.ShardCount < 1 {
		w.ShardCount = 1
	}
	if w.ShardID < 0 || w.ShardID >= w.ShardCount {
		w.ShardID = 0
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	w.Logger.InfoContext(ctx, "gc: starting",
		"region", w.Region,
		"interval", w.Interval.String(),
		"grace", w.Grace.String(),
		"concurrency", w.effectiveConcurrency(),
		"shard_id", w.ShardID,
		"shard_count", w.ShardCount)

	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.drain(ctx)
		}
	}
}

// RunOnce performs a single drain pass. Exposed for tests + the admin
// /admin/gc/drain endpoint so an operator can trigger out-of-band cleanup.
// Returns the count of chunks successfully ack'd this pass.
func (w *Worker) RunOnce(ctx context.Context) int {
	if w.Grace < 0 {
		w.Grace = 0
	}
	if w.Batch == 0 {
		w.Batch = 500
	}
	if w.ShardCount < 1 {
		w.ShardCount = 1
	}
	if w.ShardID < 0 || w.ShardID >= w.ShardCount {
		w.ShardID = 0
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	return w.drainCount(ctx)
}

func (w *Worker) drain(ctx context.Context) {
	w.drainCount(ctx)
}

// effectiveConcurrency clamps Concurrency to [1, 256]; zero/negative -> 1.
func (w *Worker) effectiveConcurrency() int {
	c := w.Concurrency
	if c < 1 {
		return 1
	}
	if c > 256 {
		return 256
	}
	return c
}

func (w *Worker) drainCount(ctx context.Context) int {
	before := time.Now().Add(-w.Grace)
	first := true
	var processed atomic.Int64
	limit := w.effectiveConcurrency()
	shardID := w.ShardID
	shardCount := max(w.ShardCount, 1)
	if shardID < 0 || shardID >= shardCount {
		shardID = 0
	}
	for {
		entries, err := w.Meta.ListGCEntriesShard(ctx, w.Region, shardID, shardCount, before, w.Batch)
		if err != nil {
			w.Logger.WarnContext(ctx, "gc list", "error", err.Error(), "shard_id", shardID, "shard_count", shardCount)
			return int(processed.Load())
		}
		if first && w.Metrics != nil {
			w.Metrics.SetQueueDepth(w.Region, len(entries))
		}
		first = false
		if len(entries) == 0 {
			return int(processed.Load())
		}
		eg := new(errgroup.Group)
		eg.SetLimit(limit)
		for _, e := range entries {
			eg.Go(func() error {
				defer func() {
					if r := recover(); r != nil {
						w.Logger.WarnContext(ctx, "gc panic",
							"pool", e.Chunk.Pool, "oid", e.Chunk.OID, "panic", r)
					}
				}()
				manifest := &data.Manifest{Chunks: []data.ChunkRef{e.Chunk}}
				if err := w.Data.Delete(ctx, manifest); err != nil {
					w.Logger.WarnContext(ctx, "gc delete", "pool", e.Chunk.Pool, "oid", e.Chunk.OID, "error", err.Error())
					return nil
				}
				if err := w.Meta.AckGCEntry(ctx, w.Region, e); err != nil {
					w.Logger.WarnContext(ctx, "gc ack", "pool", e.Chunk.Pool, "oid", e.Chunk.OID, "error", err.Error())
					return nil
				}
				metrics.GCProcessed.Inc()
				processed.Add(1)
				return nil
			})
		}
		_ = eg.Wait()
		if len(entries) < w.Batch {
			return int(processed.Load())
		}
	}
}
