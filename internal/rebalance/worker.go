// Package rebalance ships the strata-rebalance worker: a leader-elected
// background worker that ticks per-bucket, compares the actual
// chunk-to-cluster distribution against meta.Bucket.Placement, and emits
// a move plan that the per-backend movers (US-004 RADOS / US-005 S3)
// execute. This story (US-003) wires the scaffold: leader election,
// envelope read, distribution scan, plan emission, structured logs,
// per-iteration tracing spans, and a planned_moves_total counter. The
// executeMoves step is a stub that returns nil — the movers land in
// US-004 and US-005.
package rebalance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/data/placement"
	"github.com/danchupin/strata/internal/meta"
	strataotel "github.com/danchupin/strata/internal/otel"
)

// MoverMetrics is the sink for the mover-side counters
// (strata_rebalance_bytes_moved_total, _chunks_moved_total,
// _cas_conflicts_total). The cmd binary supplies
// metrics.RebalanceObserver{}; tests can plug a counting fake.
type MoverMetrics interface {
	IncBytesMoved(from, to string, bytes int64)
	IncChunksMoved(from, to, bucket string)
	IncCASConflict(bucket string)
}

type nopMoverMetrics struct{}

func (nopMoverMetrics) IncBytesMoved(string, string, int64)   {}
func (nopMoverMetrics) IncChunksMoved(string, string, string) {}
func (nopMoverMetrics) IncCASConflict(string)                 {}

// Move records one chunk that needs to migrate from FromCluster to
// ToCluster under the bucket's current placement policy. Emitted by
// scanDistribution per (object, chunkIdx). Mover implementations
// consume []Move per object and issue manifest CAS once the chunks are
// copied (US-004 / US-005). Version surfaces the object version so the
// mover hits the correct meta row when issuing SetObjectStorage.
type Move struct {
	Bucket      string
	BucketID    [16]byte
	ObjectKey   string
	VersionID   string
	ChunkIdx    int
	FromCluster string
	ToCluster   string
	// SrcRef is the source ChunkRef (cluster/pool/namespace/oid/size).
	// Movers Read from SrcRef + Write to ToCluster + the same
	// pool/namespace under a freshly-minted OID. Populated at scan time
	// so movers do not re-decode the manifest.
	SrcRef data.ChunkRef
	// Class is the object's storage class at scan time. Movers pass it
	// as both expectedClass and newClass when issuing the manifest CAS
	// — rebalance never changes class.
	Class string
}

// PlanEmitter receives the per-bucket move plan + actual/target chunk
// distribution. US-004/US-005 movers plug in here; for US-003 the
// worker uses planLogger which only logs the plan + bumps the counter.
type PlanEmitter interface {
	EmitPlan(ctx context.Context, bucket *meta.Bucket, actual map[string]int, target map[string]int, moves []Move) error
}

// Metrics is the per-counter sink the cmd binary plugs in via the
// MetricsObserver adapter. Keeps prometheus out of the rebalance
// package import set.
type Metrics interface {
	IncPlannedMove(bucket string)
}

type nopMetrics struct{}

func (nopMetrics) IncPlannedMove(string) {}

// Config wires a Worker. Defaults applied in New: Interval=1h,
// PollLimit=1000, Now=time.Now, Logger=slog.Default. Metrics defaults
// to nopMetrics. Emitter defaults to a logger-only emitter that bumps
// planned_moves_total and writes one INFO line per bucket.
type Config struct {
	Meta     meta.Store
	Data     data.Backend
	Logger   *slog.Logger
	Metrics  Metrics
	Emitter  PlanEmitter
	Interval time.Duration
	// PollLimit is the page size used when walking objects per bucket.
	PollLimit int
	// RateMBPerSec / Inflight are surfaced for US-004/US-005 movers;
	// the US-003 scaffold validates + logs them but does not enforce.
	RateMBPerSec int
	Inflight     int
	Now          func() time.Time
	Tracer       trace.Tracer
}

// Worker runs one rebalance pass per Interval.
type Worker struct {
	cfg Config

	iterErrMu sync.Mutex
	iterErr   error
}

// New builds a Worker. Returns error on missing required deps.
func New(cfg Config) (*Worker, error) {
	if cfg.Meta == nil {
		return nil, errors.New("rebalance: meta store required")
	}
	if cfg.Data == nil {
		return nil, errors.New("rebalance: data backend required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	if cfg.PollLimit <= 0 {
		cfg.PollLimit = 1000
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Metrics == nil {
		cfg.Metrics = nopMetrics{}
	}
	if cfg.Emitter == nil {
		cfg.Emitter = &planLogger{logger: cfg.Logger}
	}
	return &Worker{cfg: cfg}, nil
}

// Run loops until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.cfg.Logger.Info("rebalance: starting",
		"interval", w.cfg.Interval.String(),
		"rate_mb_s", w.cfg.RateMBPerSec,
		"inflight", w.cfg.Inflight,
	)
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.cfg.Logger.Warn("rebalance: tick failed", "error", err.Error())
			}
		}
	}
}

// RunOnce performs a single rebalance pass over every bucket with a
// non-nil Placement. Exposed for tests + the cmd binary's --once flag.
func (w *Worker) RunOnce(ctx context.Context) error {
	iterCtx, span := strataotel.StartIteration(ctx, w.tracerOrNoop(), "rebalance")
	err := w.runOnce(iterCtx)
	if sticky := w.takeIterErr(); err == nil {
		err = sticky
	}
	strataotel.EndIteration(span, err)
	return err
}

func (w *Worker) runOnce(ctx context.Context) error {
	buckets, err := w.cfg.Meta.ListBuckets(ctx, "")
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}
	for _, b := range buckets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		policy, perr := w.cfg.Meta.GetBucketPlacement(ctx, b.Name)
		if perr != nil {
			w.cfg.Logger.Warn("rebalance: load placement", "bucket", b.Name, "error", perr.Error())
			w.recordIterErr(perr)
			continue
		}
		if len(policy) == 0 {
			continue
		}
		w.scanAndEmit(ctx, b, policy)
	}
	return nil
}

func (w *Worker) scanAndEmit(ctx context.Context, b *meta.Bucket, policy map[string]int) {
	ctx, span := w.tracerOrNoop().Start(ctx, "rebalance.scan_bucket",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			strataotel.AttrComponentWorker,
			attribute.String(strataotel.WorkerKey, "rebalance"),
			attribute.String("strata.rebalance.bucket", b.Name),
			attribute.String("strata.rebalance.bucket_id", b.ID.String()),
		),
	)
	defer span.End()

	actual, moves, err := w.scanDistribution(ctx, b, policy)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		w.recordIterErr(err)
		w.cfg.Logger.Warn("rebalance: scan bucket", "bucket", b.Name, "error", err.Error())
		return
	}
	for range moves {
		w.cfg.Metrics.IncPlannedMove(b.Name)
	}
	if err := w.cfg.Emitter.EmitPlan(ctx, b, actual, policy, moves); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		w.recordIterErr(err)
		w.cfg.Logger.Warn("rebalance: emit plan", "bucket", b.Name, "error", err.Error())
	}
	span.SetAttributes(
		attribute.Int("strata.rebalance.moves", len(moves)),
	)
}

// scanDistribution walks every (latest) object in the bucket, computes
// the per-cluster chunk count (actual), compares each chunk's home
// against PickCluster's verdict, and emits a Move when they differ.
// Chunks with empty Cluster (legacy single-cluster pre-placement
// rows) are treated as living on the default cluster (resolved as ""
// here — the mover knows how to fall back). The picker uses the same
// stable hash the PUT path uses (US-002) so retries are convergent.
func (w *Worker) scanDistribution(ctx context.Context, b *meta.Bucket, policy map[string]int) (map[string]int, []Move, error) {
	actual := map[string]int{}
	var moves []Move
	opts := meta.ListOptions{Limit: w.cfg.PollLimit}
	for {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		res, err := w.cfg.Meta.ListObjects(ctx, b.ID, opts)
		if err != nil {
			return nil, nil, err
		}
		for _, o := range res.Objects {
			if o.Manifest == nil {
				continue
			}
			for idx, c := range o.Manifest.Chunks {
				actual[c.Cluster]++
				want := placement.PickCluster(b.ID, o.Key, idx, policy)
				if want == "" {
					continue
				}
				if c.Cluster == want {
					continue
				}
				moves = append(moves, Move{
					Bucket:      b.Name,
					BucketID:    b.ID,
					ObjectKey:   o.Key,
					VersionID:   o.VersionID,
					ChunkIdx:    idx,
					FromCluster: c.Cluster,
					ToCluster:   want,
					SrcRef:      c,
					Class:       o.StorageClass,
				})
			}
		}
		if !res.Truncated {
			break
		}
		opts.Marker = res.NextMarker
	}
	return actual, moves, nil
}

func (w *Worker) tracerOrNoop() trace.Tracer {
	if w.cfg.Tracer == nil {
		return strataotel.NoopTracer()
	}
	return w.cfg.Tracer
}

func (w *Worker) recordIterErr(err error) {
	if err == nil {
		return
	}
	w.iterErrMu.Lock()
	if w.iterErr == nil {
		w.iterErr = err
	}
	w.iterErrMu.Unlock()
}

func (w *Worker) takeIterErr() error {
	w.iterErrMu.Lock()
	defer w.iterErrMu.Unlock()
	err := w.iterErr
	w.iterErr = nil
	return err
}

// planLogger is the default Emitter for US-003: log the plan, no data
// movement. US-004 / US-005 will replace this with a real mover chain
// by injecting cfg.Emitter from the cmd binary. The planned-moves
// counter is bumped by the Worker itself before EmitPlan runs so the
// metric is independent of whichever emitter is wired.
type planLogger struct {
	logger *slog.Logger
}

func (p *planLogger) EmitPlan(_ context.Context, bucket *meta.Bucket, actual map[string]int, target map[string]int, moves []Move) error {
	p.logger.Info("rebalance plan",
		"bucket", bucket.Name,
		"moves", len(moves),
		"actual", actual,
		"target", target,
	)
	return nil
}
