package admin

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/config"
	datarados "github.com/danchupin/strata/internal/data/rados"
	"github.com/danchupin/strata/internal/rebuild"
	"github.com/danchupin/strata/internal/reconcile"
)

// cmdRebuildIndex runs the last-resort manifest-index rebuild (US-004
// metadata-data-reconcile). It scans a data-tier pool via the US-000
// enumeration primitive, groups chunks by their US-001 back-reference, and
// writes reconstructed meta.Object rows so intact bytes are not permanently
// lost when the metadata backup is gone.
//
// Single-binary invariant: this is a subcommand of `strata admin`, NOT a new
// binary. It connects directly to the configured meta + data backends
// (mirroring `strata admin rewrap`), not over the gateway HTTP surface.
//
// PLAINTEXT-ONLY: SSE-S3/KMS objects are reported unrecoverable (the wrapped
// DEK lived in the lost meta); the meta backup remains the primary recovery
// path. Run --dry-run first to review the recovery report before writing.
func (a *app) cmdRebuildIndex(ctx context.Context, jsonOut bool, args []string) error {
	fs := flag.NewFlagSet("rebuild-index", flag.ContinueOnError)
	fs.SetOutput(a.err)
	cluster := fs.String("cluster", "", "data-tier cluster id to scan (default cluster when empty)")
	pool := fs.String("pool", "", "RADOS pool to scan (default backend pool when empty)")
	namespace := fs.String("namespace", "", "RADOS namespace to scan (empty = default ns)")
	bucketID := fs.String("bucket-id", "", "restrict rebuild to this bucket UUID (empty = every bucket in the scope)")
	force := fs.Bool("force", false, "overwrite manifest rows that already exist in meta (default: live meta wins)")
	dryRun := fs.Bool("dry-run", false, "scan + classify + report only; write no rows")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}

	var filter uuid.UUID
	if *bucketID != "" {
		id, err := uuid.Parse(*bucketID)
		if err != nil {
			return fmt.Errorf("--bucket-id %q: %w", *bucketID, err)
		}
		filter = id
	}

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

	scanner := &reconcile.RADOSScanner{
		Backend:    backend,
		RatePerSec: datarados.ScanRateFromEnv(),
	}
	rb, err := rebuild.New(rebuild.Config{
		Meta:         store,
		Data:         backend,
		Scanner:      scanner,
		Logger:       logger,
		Force:        *force,
		DryRun:       *dryRun,
		BucketFilter: filter,
	})
	if err != nil {
		return fmt.Errorf("rebuilder: %w", err)
	}

	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("rebuild-index starting",
		"cluster", *cluster, "pool", *pool, "namespace", *namespace,
		"bucket_filter", *bucketID, "force", *force, "dry_run", *dryRun)

	stats, err := rb.Run(runCtx, reconcile.ScanScope{Cluster: *cluster, Pool: *pool, Namespace: *namespace})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("rebuild-index run failed: %w", err)
	}

	if jsonOut {
		return writeJSON(a.out, stats)
	}
	fmt.Fprintf(a.out, "rebuild-index: cluster=%s pool=%s namespace=%s\n", *cluster, *pool, *namespace)
	fmt.Fprintf(a.out, "  chunks:   scanned=%d absent_backref=%d\n", stats.ChunksScanned, stats.AbsentBackref)
	fmt.Fprintf(a.out, "  versions: groups=%d rebuilt=%d skipped_existing=%d\n", stats.GroupsSeen, stats.Rebuilt, stats.SkippedExist)
	fmt.Fprintf(a.out, "  rejected: gapped=%d unrecoverable_sse=%d errors=%d\n", stats.Gapped, stats.Unrecoverable, stats.Errors)
	if stats.Unrecoverable > 0 {
		fmt.Fprintf(a.out, "  NOTE: SSE objects are unrecoverable from data alone (wrapped DEK was in the lost meta) — restore the meta backup for those.\n")
	}
	if *dryRun {
		fmt.Fprintf(a.out, "  (dry-run: no rows written)\n")
	}
	return nil
}
