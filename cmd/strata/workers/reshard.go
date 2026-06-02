package workers

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/reshard"
)

func init() {
	Register(Worker{
		Name:  "reshard",
		Build: buildReshard,
	})
}

func buildReshard(deps Dependencies) (Runner, error) {
	cfg := workerCfg(deps)
	rsCfg := cfg.Workers.Reshard
	interval := orDuration(rsCfg.Interval, 30*time.Second)
	w, err := reshard.New(reshard.Config{
		Meta:       deps.Meta,
		Logger:     deps.Logger,
		BatchLimit: orInt(rsCfg.BatchLimit, 500),
		Interval:   interval,
	})
	if err != nil {
		return nil, err
	}
	return &reshardRunner{
		worker:   w,
		interval: interval,
		logger:   deps.Logger,
		tracer:   deps.Tracer.Tracer("strata.worker.reshard"),
	}, nil
}

// reshardRunner drives the reshard worker in the supervisor's long-running
// loop: drain every queued reshard job (RunOnce), log stats, sleep interval,
// repeat. Leader-elected via the supervisor's `reshard-leader` lease so only
// one replica drives jobs at a time. RunOnce is idempotent + resumable from
// each job's LastKey watermark, so a crash mid-job is recovered on the next
// tick (or by the next leader after a lease handover).
type reshardRunner struct {
	worker   *reshard.Worker
	interval time.Duration
	logger   *slog.Logger
	tracer   trace.Tracer
}

func (r *reshardRunner) Run(ctx context.Context) error {
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "reshard: starting", "interval", r.interval)
	for {
		start := time.Now()
		iterCtx, span := StartIteration(ctx, r.tracer, "reshard")
		stats, err := r.worker.RunOnce(iterCtx)
		EndIteration(span, err)
		metrics.ObserveWorkerTick("reshard", err, time.Since(start))
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.WarnContext(ctx, "reshard tick failed", "error", err.Error())
		} else if stats.JobsCompleted > 0 {
			logger.InfoContext(ctx, "reshard tick",
				"jobs_scanned", stats.JobsScanned,
				"jobs_completed", stats.JobsCompleted,
				"objects_copied", stats.ObjectsCopied,
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
