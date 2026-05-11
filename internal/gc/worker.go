package gc

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
	strataotel "github.com/danchupin/strata/internal/otel"
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
	// Tracer emits per-iteration parent spans (`worker.gc.tick`) plus the
	// `gc.scan_partition` / `gc.delete_chunk` sub-op children. Nil falls
	// back to a process-shared no-op tracer so the worker stays usable in
	// tests + bench harnesses without OTel wiring.
	Tracer trace.Tracer
}

func (w *Worker) tracerOrNoop() trace.Tracer {
	if w.Tracer == nil {
		return strataotel.NoopTracer()
	}
	return w.Tracer
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
	iterCtx, span := strataotel.StartIteration(ctx, w.tracerOrNoop(), "gc")
	span.SetAttributes(attribute.Int("strata.gc.shard_id", w.ShardID))
	n, err := w.drainCount(iterCtx)
	strataotel.EndIteration(span, err)
	return n
}

func (w *Worker) drain(ctx context.Context) {
	iterCtx, span := strataotel.StartIteration(ctx, w.tracerOrNoop(), "gc")
	span.SetAttributes(attribute.Int("strata.gc.shard_id", w.ShardID))
	_, err := w.drainCount(iterCtx)
	strataotel.EndIteration(span, err)
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

func (w *Worker) drainCount(ctx context.Context) (int, error) {
	before := time.Now().Add(-w.Grace)
	first := true
	var processed atomic.Int64
	var (
		errMu     sync.Mutex
		stickyErr error
	)
	recordErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if stickyErr == nil {
			stickyErr = err
		}
		errMu.Unlock()
	}
	limit := w.effectiveConcurrency()
	shardID := w.ShardID
	shardCount := max(w.ShardCount, 1)
	if shardID < 0 || shardID >= shardCount {
		shardID = 0
	}
	tracer := w.tracerOrNoop()
	for {
		scanCtx, scanSpan := tracer.Start(ctx, "gc.scan_partition",
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(
				strataotel.AttrComponentWorker,
				attribute.String("strata.worker", "gc"),
				attribute.Int("strata.gc.shard_id", shardID),
				attribute.Int("strata.gc.shard_count", shardCount),
			),
		)
		entries, err := w.Meta.ListGCEntriesShard(scanCtx, w.Region, shardID, shardCount, before, w.Batch)
		if err != nil {
			scanSpan.RecordError(err)
			scanSpan.SetStatus(codes.Error, err.Error())
			scanSpan.End()
			w.Logger.WarnContext(ctx, "gc list", "error", err.Error(), "shard_id", shardID, "shard_count", shardCount)
			recordErr(err)
			return int(processed.Load()), stickyErr
		}
		scanSpan.SetAttributes(attribute.Int("strata.gc.batch_size", len(entries)))
		scanSpan.End()
		if first && w.Metrics != nil {
			w.Metrics.SetQueueDepth(w.Region, len(entries))
		}
		first = false
		if len(entries) == 0 {
			return int(processed.Load()), stickyErr
		}
		eg := new(errgroup.Group)
		eg.SetLimit(limit)
		for _, e := range entries {
			eg.Go(func() error {
				delCtx, delSpan := tracer.Start(ctx, "gc.delete_chunk",
					trace.WithSpanKind(trace.SpanKindInternal),
					trace.WithAttributes(
						strataotel.AttrComponentWorker,
						attribute.String("strata.worker", "gc"),
						attribute.String("strata.gc.cluster", e.Chunk.Cluster),
						attribute.String("strata.gc.pool", e.Chunk.Pool),
						attribute.String("strata.gc.oid", e.Chunk.OID),
					),
				)
				var subErr error
				defer func() {
					if r := recover(); r != nil {
						w.Logger.WarnContext(ctx, "gc panic",
							"pool", e.Chunk.Pool, "oid", e.Chunk.OID, "panic", r)
						delSpan.SetStatus(codes.Error, "panic")
					} else if subErr != nil {
						delSpan.RecordError(subErr)
						delSpan.SetStatus(codes.Error, subErr.Error())
					}
					delSpan.End()
				}()
				manifest := &data.Manifest{Chunks: []data.ChunkRef{e.Chunk}}
				if err := w.Data.Delete(delCtx, manifest); err != nil {
					w.Logger.WarnContext(ctx, "gc delete", "pool", e.Chunk.Pool, "oid", e.Chunk.OID, "error", err.Error())
					subErr = err
					recordErr(err)
					return nil
				}
				if err := w.Meta.AckGCEntry(delCtx, w.Region, e); err != nil {
					w.Logger.WarnContext(ctx, "gc ack", "pool", e.Chunk.Pool, "oid", e.Chunk.OID, "error", err.Error())
					subErr = err
					recordErr(err)
					return nil
				}
				metrics.GCProcessed.Inc()
				processed.Add(1)
				return nil
			})
		}
		_ = eg.Wait()
		if len(entries) < w.Batch {
			return int(processed.Load()), stickyErr
		}
	}
}
