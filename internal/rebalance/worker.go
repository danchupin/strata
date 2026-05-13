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
	// IncRefused bumps the per-iteration safety-rail counter (US-006).
	// reason is one of "target_full" / "target_draining"; target is the
	// cluster id the move would have written to.
	IncRefused(reason, target string)
	// IncDrainComplete fires once per `>0 → 0` chunks_on_cluster
	// transition observed by the rebalance worker (US-005 drain-
	// lifecycle). Idempotent at the tracker layer — the counter mirrors
	// the observed transitions, not the underlying audit log.
	IncDrainComplete(cluster string)
}

type nopMetrics struct{}

func (nopMetrics) IncPlannedMove(string)       {}
func (nopMetrics) IncRefused(string, string)   {}
func (nopMetrics) IncDrainComplete(string)     {}

// DrainCompleteEvent is the wire payload handed to a DrainNotifier when
// the rebalance worker detects a cluster's drain has reached zero
// chunks (US-005). CompletedAt is UTC. BytesMoved is BaseBytes − Bytes
// captured at scan-finish time (i.e. the total bytes that left the
// cluster between the live → draining transition and completion).
type DrainCompleteEvent struct {
	Cluster     string
	BytesMoved  int64
	CompletedAt time.Time
}

// DrainNotifier is the optional best-effort sink for drain-complete
// events (US-005). The notify-worker pipeline wires an adapter when
// STRATA_NOTIFY_TARGETS is set; nil disables the fan-out. The worker
// swallows every error returned from NotifyDrainComplete — notify
// failure must never block the rebalance tick.
type DrainNotifier interface {
	NotifyDrainComplete(ctx context.Context, evt DrainCompleteEvent)
}

// DefaultDrainAuditRetention mirrors s3api.DefaultAuditRetention (30 days)
// and is the row TTL applied to drain.complete audit_log entries when
// Config.AuditTTL is zero. Hardcoded to keep the rebalance package free
// of the s3api import.
const DefaultDrainAuditRetention = 30 * 24 * time.Hour

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
	// StatsProbe is the optional cluster-fill probe consulted before
	// dispatching a move (US-006). nil disables the target_full safety
	// rail — moves are allowed without a fill check. RADOS-backed
	// gateways inject *rados.Backend (which implements
	// data.ClusterStatsProbe); S3-only deployments leave nil.
	StatsProbe data.ClusterStatsProbe
	// FillCeiling is the target-cluster used/total ratio above which the
	// rebalance worker refuses to dispatch a move (US-006). Zero / <0
	// falls back to DefaultFillCeiling (0.90).
	FillCeiling float64
	// Progress is the in-process draining-progress cache shared with the
	// adminapi GET /admin/v1/clusters/{id}/drain-progress handler
	// (US-003 drain-lifecycle). nil disables the per-tick scan
	// accumulator — the move-planning side of the loop is unaffected.
	Progress *ProgressTracker
	// Notifier is the optional sink for drain-complete events (US-005).
	// nil disables the fan-out — log + audit + metric still fire on
	// every transition.
	Notifier DrainNotifier
	// AuditTTL is the row TTL applied to drain.complete audit_log
	// entries. Zero falls back to DefaultDrainAuditRetention so the
	// gateway and the worker keep the same retention shape without
	// crossing an import boundary into s3api.
	AuditTTL time.Duration
}

// DefaultFillCeiling is the conservative target_full threshold from the
// US-006 PRD: 90% utilisation. Operators may override via
// Config.FillCeiling; out-of-range values are clamped to (0, 1].
const DefaultFillCeiling = 0.90

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
	tickStart := w.cfg.Now()
	buckets, err := w.cfg.Meta.ListBuckets(ctx, "")
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}
	draining := w.loadDrainingClusters(ctx)
	fillCache := w.newFillCache(ctx)
	prog := newProgressAcc(w.cfg.Progress, draining)
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
			// Empty-policy buckets still need a progress scan so chunks
			// on a draining cluster are counted toward the deregister-
			// ready signal. The move plan is a no-op (no policy → no
			// PickCluster verdicts).
			w.scanBucketForProgress(ctx, b, draining, prog)
			continue
		}
		w.scanAndEmit(ctx, b, policy, draining, fillCache, prog)
	}
	tickEnd := w.cfg.Now()
	completions := prog.commit(w.cfg.Progress, draining, tickEnd)
	for _, ev := range completions {
		w.handleDrainComplete(ctx, ev, tickEnd.Sub(tickStart))
	}
	return nil
}

// handleDrainComplete fans the per-cluster `>0 → 0` transition out to
// log + counter + audit + (optional) notify (US-005 drain-lifecycle).
// Every sink is best-effort: a failed audit write logs WARN and falls
// through; a notify failure is swallowed by the adapter. The metric is
// bumped before the side-effects so the operator dashboards reflect
// completion even when audit/notify wiring degrades.
func (w *Worker) handleDrainComplete(ctx context.Context, ev CompletionEvent, scanDuration time.Duration) {
	w.cfg.Logger.Info("drain complete",
		"cluster", ev.Cluster,
		"scan_seconds", scanDuration.Seconds(),
		"final_bytes_moved", ev.BytesMoved,
	)
	w.cfg.Metrics.IncDrainComplete(ev.Cluster)
	w.recordDrainCompleteAudit(ctx, ev)
	if w.cfg.Notifier != nil {
		w.cfg.Notifier.NotifyDrainComplete(ctx, DrainCompleteEvent{
			Cluster:     ev.Cluster,
			BytesMoved:  ev.BytesMoved,
			CompletedAt: ev.ScanFinish,
		})
	}
}

// recordDrainCompleteAudit appends one audit_log row for the drain
// completion. Failure is logged WARN — never propagated.
func (w *Worker) recordDrainCompleteAudit(ctx context.Context, ev CompletionEvent) {
	if w.cfg.Meta == nil {
		return
	}
	ttl := w.cfg.AuditTTL
	if ttl == 0 {
		ttl = DefaultDrainAuditRetention
	}
	row := &meta.AuditEvent{
		Bucket:    "-",
		Time:      ev.ScanFinish.UTC(),
		Principal: "system:rebalance-worker",
		Action:    "drain.complete",
		Resource:  "cluster:" + ev.Cluster,
		Result:    "200",
	}
	if err := w.cfg.Meta.EnqueueAudit(ctx, row, ttl); err != nil {
		w.cfg.Logger.Warn("drain complete: audit enqueue failed",
			"cluster", ev.Cluster, "error", err.Error())
	}
}

// progressAcc accumulates per-draining-cluster {chunks, bytes} for one
// rebalance tick. The worker increments it from scanDistribution +
// scanBucketForProgress and CommitScan-flushes at iteration end. nil
// receiver short-circuits every method so callers do not need a
// nil-check on the hot path.
type progressAcc struct {
	enabled bool
	chunks  map[string]int64
	bytes   map[string]int64
}

func newProgressAcc(p *ProgressTracker, draining map[string]bool) *progressAcc {
	if p == nil || len(draining) == 0 {
		return &progressAcc{}
	}
	chunks := make(map[string]int64, len(draining))
	bytes := make(map[string]int64, len(draining))
	for id := range draining {
		chunks[id] = 0
		bytes[id] = 0
	}
	return &progressAcc{enabled: true, chunks: chunks, bytes: bytes}
}

func (a *progressAcc) observe(clusterID string, draining map[string]bool, size int64) {
	if a == nil || !a.enabled || clusterID == "" {
		return
	}
	if !draining[clusterID] {
		return
	}
	a.chunks[clusterID]++
	a.bytes[clusterID] += size
}

func (a *progressAcc) commit(p *ProgressTracker, draining map[string]bool, now time.Time) []CompletionEvent {
	if a == nil || p == nil {
		return nil
	}
	ids := make([]string, 0, len(draining))
	for id := range draining {
		ids = append(ids, id)
	}
	if !a.enabled {
		// No draining clusters this tick (or tracker disabled). Still
		// flush so previously-cached entries for clusters that
		// transitioned out of draining are reaped.
		return p.CommitScan(ids, nil, nil, now)
	}
	return p.CommitScan(ids, a.chunks, a.bytes, now)
}

// scanBucketForProgress walks every (latest) object in the bucket and
// accumulates per-draining-cluster chunk / byte counts. This runs even
// for buckets without a Placement policy so cluster decommissioning is
// observable on legacy buckets (the move plan stays a no-op for those
// because policy is empty; the count is independent).
func (w *Worker) scanBucketForProgress(ctx context.Context, b *meta.Bucket, draining map[string]bool, prog *progressAcc) {
	if prog == nil || !prog.enabled {
		return
	}
	opts := meta.ListOptions{Limit: w.cfg.PollLimit}
	for {
		if ctx.Err() != nil {
			return
		}
		res, err := w.cfg.Meta.ListObjects(ctx, b.ID, opts)
		if err != nil {
			w.cfg.Logger.Warn("rebalance: progress scan", "bucket", b.Name, "error", err.Error())
			return
		}
		for _, o := range res.Objects {
			if o.Manifest == nil {
				continue
			}
			for _, c := range o.Manifest.Chunks {
				prog.observe(c.Cluster, draining, c.Size)
			}
			if br := o.Manifest.BackendRef; br != nil && br.Cluster != "" {
				prog.observe(br.Cluster, draining, br.Size)
			}
		}
		if !res.Truncated {
			return
		}
		opts.Marker = res.NextMarker
	}
}

// loadDrainingClusters reads cluster_state once per tick and returns
// the set of cluster ids whose state is meta.ClusterStateDraining.
// Errors are logged + treated as empty so a meta hiccup never breaks
// the rebalance tick.
func (w *Worker) loadDrainingClusters(ctx context.Context) map[string]bool {
	rows, err := w.cfg.Meta.ListClusterStates(ctx)
	if err != nil {
		w.cfg.Logger.Warn("rebalance: load cluster states", "error", err.Error())
		w.recordIterErr(err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	out := make(map[string]bool, len(rows))
	for id, state := range rows {
		if state == meta.ClusterStateDraining {
			out[id] = true
		}
	}
	return out
}

func (w *Worker) scanAndEmit(ctx context.Context, b *meta.Bucket, policy map[string]int, draining map[string]bool, fills *fillCache, prog *progressAcc) {
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

	actual, moves, err := w.scanDistribution(ctx, b, policy, draining, prog)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		w.recordIterErr(err)
		w.cfg.Logger.Warn("rebalance: scan bucket", "bucket", b.Name, "error", err.Error())
		return
	}
	moves = w.applySafetyRails(ctx, b, moves, draining, fills)
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

// applySafetyRails post-filters the move plan against the two operator
// safety rails (US-006):
//
//   - target_draining: refuses any move whose ToCluster is in `draining`.
//     The picker already skips draining clusters via
//     placement.PickClusterExcluding, so this branch only fires in a race
//     between scan emission and a drain flip — defense-in-depth.
//   - target_full: when the optional ClusterStatsProbe is wired and the
//     target's used/total exceeds FillCeiling, refuses the move and bumps
//     the metric. S3-backed deployments (no probe) skip the check —
//     equivalent to ErrClusterStatsNotSupported per PRD.
func (w *Worker) applySafetyRails(ctx context.Context, b *meta.Bucket, moves []Move, draining map[string]bool, fills *fillCache) []Move {
	if len(moves) == 0 {
		return moves
	}
	out := moves[:0]
	for _, mv := range moves {
		if draining[mv.ToCluster] {
			w.cfg.Logger.Warn("rebalance refused: target draining",
				"bucket", b.Name, "target", mv.ToCluster, "object_key", mv.ObjectKey)
			w.cfg.Metrics.IncRefused("target_draining", mv.ToCluster)
			continue
		}
		if full, ok := fills.isFull(ctx, mv.ToCluster, w.cfg.Logger); ok && full {
			w.cfg.Logger.Warn("rebalance refused: target full",
				"bucket", b.Name, "target", mv.ToCluster, "object_key", mv.ObjectKey)
			w.cfg.Metrics.IncRefused("target_full", mv.ToCluster)
			continue
		}
		out = append(out, mv)
	}
	return out
}

// scanDistribution walks every (latest) object in the bucket, computes
// the per-cluster chunk count (actual), compares each chunk's home
// against PickCluster's verdict, and emits a Move when they differ.
// Chunks with empty Cluster (legacy single-cluster pre-placement
// rows) are treated as living on the default cluster (resolved as ""
// here — the mover knows how to fall back). The picker uses the same
// stable hash the PUT path uses (US-002) so retries are convergent.
func (w *Worker) scanDistribution(ctx context.Context, b *meta.Bucket, policy map[string]int, draining map[string]bool, prog *progressAcc) (map[string]int, []Move, error) {
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
				prog.observe(c.Cluster, draining, c.Size)
				want := placement.PickClusterExcluding(b.ID, o.Key, idx, policy, draining)
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
			if br := o.Manifest.BackendRef; br != nil && br.Cluster != "" {
				actual[br.Cluster]++
				prog.observe(br.Cluster, draining, br.Size)
				want := placement.PickClusterExcluding(b.ID, o.Key, 0, policy, draining)
				if want == "" || br.Cluster == want {
					continue
				}
				moves = append(moves, Move{
					Bucket:      b.Name,
					BucketID:    b.ID,
					ObjectKey:   o.Key,
					VersionID:   o.VersionID,
					ChunkIdx:    0,
					FromCluster: br.Cluster,
					ToCluster:   want,
					SrcRef: data.ChunkRef{
						Cluster: br.Cluster,
						OID:     br.Key,
						Size:    br.Size,
					},
					Class: o.StorageClass,
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

// fillCache memoises per-iteration ClusterStats probes so a fan-out of
// N moves into K target clusters costs at most K probes per tick. The
// per-probe result is cached as full / not-full; transient probe errors
// are logged once per cluster and treated as "ok to proceed" so a
// flaky probe never freezes the rebalance.
type fillCache struct {
	probe   data.ClusterStatsProbe
	ceiling float64
	results map[string]fillResult
}

type fillResult struct {
	known bool
	full  bool
}

func (w *Worker) newFillCache(ctx context.Context) *fillCache {
	ceiling := w.cfg.FillCeiling
	if ceiling <= 0 || ceiling > 1 {
		ceiling = DefaultFillCeiling
	}
	return &fillCache{
		probe:   w.cfg.StatsProbe,
		ceiling: ceiling,
		results: map[string]fillResult{},
	}
}

// isFull reports whether the target cluster is above the configured
// fill ceiling. ok=false signals "no probe wired or probe unsupported";
// the worker treats that as "OK to proceed".
func (c *fillCache) isFull(ctx context.Context, target string, logger *slog.Logger) (full bool, ok bool) {
	if c == nil || c.probe == nil || target == "" {
		return false, false
	}
	if r, cached := c.results[target]; cached {
		return r.full, r.known
	}
	used, total, err := c.probe.ClusterStats(ctx, target)
	if err != nil {
		if errors.Is(err, data.ErrClusterStatsNotSupported) {
			// One WARN per iteration per cluster, then short-circuit.
			if logger != nil {
				logger.Warn("rebalance: cluster stats not supported; skipping target_full check",
					"target", target)
			}
			c.results[target] = fillResult{known: false}
			return false, false
		}
		if logger != nil {
			logger.Warn("rebalance: cluster stats probe failed; treating as not full",
				"target", target, "error", err.Error())
		}
		c.results[target] = fillResult{known: false}
		return false, false
	}
	if total <= 0 {
		c.results[target] = fillResult{known: true, full: false}
		return false, true
	}
	ratio := float64(used) / float64(total)
	full = ratio > c.ceiling
	c.results[target] = fillResult{known: true, full: full}
	return full, true
}
