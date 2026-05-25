package workers

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/danchupin/strata/internal/manifestrewriter"
)

func init() {
	Register(Worker{
		Name:  "manifest-rewriter",
		Build: buildManifestRewriter,
	})
}

func buildManifestRewriter(deps Dependencies) (Runner, error) {
	cfg := workerCfg(deps)
	mrCfg := cfg.Workers.ManifestRewriter
	dryRun := mrCfg.DryRun
	w, err := manifestrewriter.New(manifestrewriter.Config{
		Meta:       deps.Meta,
		Logger:     deps.Logger,
		BatchLimit: orInt(mrCfg.BatchLimit, 500),
		DryRun:     dryRun,
		Tracer:     deps.Tracer.Tracer("strata.worker.manifest-rewriter"),
	})
	if err != nil {
		return nil, err
	}
	return &manifestRewriterRunner{
		worker:   w,
		interval: orDuration(mrCfg.Interval, 24*time.Hour),
		dryRun:   dryRun,
		logger:   deps.Logger,
	}, nil
}

// manifestRewriterRunner drives manifestrewriter.Worker in the
// supervisor's long-running loop: one full pass, log stats, sleep
// interval, repeat. Re-runs are idempotent (already-proto rows are
// skipped) so the loop is safe even when the migration is complete.
type manifestRewriterRunner struct {
	worker   *manifestrewriter.Worker
	interval time.Duration
	dryRun   bool
	logger   *slog.Logger
}

func (r *manifestRewriterRunner) Run(ctx context.Context) error {
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "manifestrewriter: starting",
		"interval", r.interval,
		"dry_run", r.dryRun,
	)
	for {
		stats, err := r.worker.Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		logger.InfoContext(ctx, "manifestrewriter run",
			"buckets_scanned", stats.BucketsScanned,
			"objects_scanned", stats.ObjectsScanned,
			"objects_rewritten", stats.ObjectsRewritten,
			"already_proto", stats.ObjectsSkippedProto,
			"dry_run", r.dryRun,
		)
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
