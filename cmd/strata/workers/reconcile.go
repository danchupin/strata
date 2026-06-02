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
// dangling pass for that backend. The cephimpl RADOS backend (per-OID stat) and
// the S3-passthrough backend (HEAD) implement data.ChunkStater (US-003b); the
// probe is rate-limited (reuse STRATA_RECONCILE_SCAN_RATE) so a live-cluster
// dangling walk does not saturate OSDs.
func chunkProber(d data.Backend, ratePerSec int) reconcile.ChunkProber {
	st, ok := d.(data.ChunkStater)
	if !ok {
		return nil
	}
	if ratePerSec <= 0 {
		return st
	}
	return &rateLimitedProber{inner: st, lim: rados.NewScanLimiter(ratePerSec)}
}

// rateLimitedProber gates each chunk-existence probe through a token bucket so
// the dangling-manifest walk's per-OID stat/HEAD load stays bounded (mirrors
// the orphan-pass RADOSScanner's scan rate limit).
type rateLimitedProber struct {
	inner data.ChunkStater
	lim   *rados.ScanLimiter
}

func (p *rateLimitedProber) ChunkExists(ctx context.Context, ref data.ChunkRef) (bool, error) {
	if err := p.lim.Wait(ctx); err != nil {
		return false, err
	}
	return p.inner.ChunkExists(ctx, ref)
}

// chunkScanner picks the orphan-pass scanner for the live backend. The
// S3-passthrough backend enumerates natively via ListObjects (data.ChunkLister
// -> reconcile.S3Scanner, US-002b); every other backend uses the US-000 pool
// walk (reconcile.RADOSScanner), which returns data.ErrRADOSNotCompiled on a
// go-ceph-free build so a queued job records that and stops.
func chunkScanner(d data.Backend, ratePerSec int) reconcile.ChunkScanner {
	if l, ok := d.(data.ChunkLister); ok {
		return &reconcile.S3Scanner{Lister: l}
	}
	return &reconcile.RADOSScanner{Backend: d, RatePerSec: ratePerSec}
}

func buildReconcile(deps Dependencies) (Runner, error) {
	cfg := workerCfg(deps)
	rcCfg := cfg.Workers.Reconcile
	interval := orDuration(rcCfg.Interval, 30*time.Second)
	w, err := reconcile.New(reconcile.Config{
		Meta: deps.Meta,
		// Orphan-pass scanner: native ListObjects for the S3-passthrough backend
		// (US-002b), else the US-000 RADOS pool walk. On a go-ceph-free build the
		// RADOS leg records data.ErrRADOSNotCompiled and stops — never a
		// nil-pointer.
		Scanner: chunkScanner(deps.Data, rados.ScanRateFromEnv()),
		// Data backs the restore policy (US-002b): it reads chunk bytes to
		// recompute a rebuilt manifest's single-part ETag.
		Data: deps.Data,
		// Prober drives the US-003 dangling-manifest pass (meta->data). The
		// memory backend, the cephimpl RADOS backend (per-OID stat), and the
		// S3-passthrough backend (HEAD) implement data.ChunkStater (US-003b);
		// a default-tag (go-ceph-free) RADOS build does not, so a dangling job
		// there records an error and quarantines/deletes nothing — it never
		// flags a healthy object on a probe it could not run. The probe is
		// rate-limited via STRATA_RECONCILE_SCAN_RATE.
		Prober:          chunkProber(deps.Data, rados.ScanRateFromEnv()),
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
