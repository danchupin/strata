package workers

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/data/rados"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/reconcile"
)

func init() {
	Register(Worker{
		Name:  "reconcile",
		Build: buildReconcile,
	})
}

// chunkProber returns the data backend as a reconcile.ChunkProber when it can
// answer chunk-existence (data.ChunkStater), else nil. nil disables the
// dangling pass for that backend (US-003b wires the RADOS leg).
func chunkProber(d data.Backend) reconcile.ChunkProber {
	if st, ok := d.(data.ChunkStater); ok {
		return st
	}
	return nil
}

func buildReconcile(deps Dependencies) (Runner, error) {
	cfg := workerCfg(deps)
	rcCfg := cfg.Workers.Reconcile
	interval := orDuration(rcCfg.Interval, 30*time.Second)
	w, err := reconcile.New(reconcile.Config{
		Meta: deps.Meta,
		// RADOS pool walk via the US-000 primitive (rados.EnumeratePool).
		// On a go-ceph-free build the backend is the rados.New stub, so a
		// queued job records data.ErrRADOSNotCompiled and stops — never a
		// nil-pointer. The S3-passthrough native-ListObjects scanner is the
		// trailing US-002b split.
		Scanner: &reconcile.RADOSScanner{
			Backend:    deps.Data,
			RatePerSec: rados.ScanRateFromEnv(),
		},
		// Prober drives the US-003 dangling-manifest pass (meta->data). The
		// memory backend implements data.ChunkStater directly; a default-tag
		// RADOS build does not (the per-OID stat is the US-003b split), so a
		// dangling job there records an error and quarantines nothing — it
		// never flags a healthy object on a probe it could not run.
		Prober:          chunkProber(deps.Data),
		Region:          deps.Region,
		Logger:          deps.Logger,
		Obs:             metrics.ReconcileObserver{},
		CheckpointEvery: orInt(rcCfg.CheckpointEvery, 500),
	})
	if err != nil {
		return nil, err
	}
	return &reconcileRunner{
		worker:   w,
		interval: interval,
		logger:   deps.Logger,
		tracer:   deps.Tracer.Tracer("strata.worker.reconcile"),
	}, nil
}

// reconcileRunner drives the reconcile worker in the supervisor's long-running
// loop: drain every queued reconcile job (RunOnce), log stats, sleep interval,
// repeat. Leader-elected via the supervisor's `reconcile-leader` lease so only
// one replica drives jobs at a time. RunOnce is idempotent + resumable from
// each job's Cursor watermark, so a crash mid-job is recovered on the next
// tick (or by the next leader after a lease handover).
type reconcileRunner struct {
	worker   *reconcile.Worker
	interval time.Duration
	logger   *slog.Logger
	tracer   trace.Tracer
}

func (r *reconcileRunner) Run(ctx context.Context) error {
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "reconcile: starting", "interval", r.interval)
	for {
		start := time.Now()
		iterCtx, span := StartIteration(ctx, r.tracer, "reconcile")
		stats, err := r.worker.RunOnce(iterCtx)
		EndIteration(span, err)
		metrics.ObserveWorkerTick("reconcile", err, time.Since(start))
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.WarnContext(ctx, "reconcile tick failed", "error", err.Error())
		} else if stats.JobsCompleted > 0 {
			logger.InfoContext(ctx, "reconcile tick",
				"jobs_scanned", stats.JobsScanned,
				"jobs_completed", stats.JobsCompleted,
				"chunks_scanned", stats.Scanned,
				"orphans_found", stats.OrphansFound,
			)
		}
		if ctx.Err() != nil {
			return nil
		}
		t := time.NewTimer(r.interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}
