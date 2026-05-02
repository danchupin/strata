// strata-rewrap walks every bucket and rewraps each per-object DEK from a
// historical master key to the active master key in STRATA_SSE_MASTER_KEYS.
// Idempotent + resumable: rows whose persisted key id already matches the
// active id are skipped, and per-bucket completion is recorded so a re-run
// only revisits buckets that haven't been processed for the current target.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/crypto/master"
	"github.com/danchupin/strata/internal/meta"
	metacassandra "github.com/danchupin/strata/internal/meta/cassandra"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/rewrap"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "scan only; do not rewrap")
	batch := flag.Int("batch", 500, "page size for ListObjectVersions / ListMultipartUploads")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config", "error", err.Error())
		os.Exit(2)
	}

	provider, err := master.NewRotationProviderFromEnv()
	if err != nil {
		logger.Error("rotation provider", "error", err.Error(),
			"hint", "set STRATA_SSE_MASTER_KEYS=<active-id>:<hex64>[,<old-id>:<hex64>...]")
		os.Exit(2)
	}
	logger.Info("rewrap starting", "active_id", provider.ActiveID(), "all_ids", provider.IDs(), "dry_run", *dryRun)

	store, err := buildMetaStore(cfg, logger)
	if err != nil {
		logger.Error("meta store", "error", err.Error())
		os.Exit(2)
	}
	defer store.Close()

	if *dryRun {
		logger.Info("dry-run requested; counting only (no UPDATE issued)")
	}

	w, err := rewrap.New(rewrap.Config{
		Meta:       store,
		Provider:   provider,
		Logger:     logger,
		BatchLimit: *batch,
	})
	if err != nil {
		logger.Error("worker", "error", err.Error())
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stats, err := w.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("rewrap run failed",
			"error", err.Error(),
			"buckets_scanned", stats.BucketsScanned,
			"objects_rewrapped", stats.ObjectsRewrapped,
			"uploads_rewrapped", stats.UploadsRewrapped,
		)
		os.Exit(1)
	}
	logger.Info("rewrap complete",
		"buckets_scanned", stats.BucketsScanned,
		"buckets_skipped", stats.BucketsSkipped,
		"objects_scanned", stats.ObjectsScanned,
		"objects_rewrapped", stats.ObjectsRewrapped,
		"uploads_scanned", stats.UploadsScanned,
		"uploads_rewrapped", stats.UploadsRewrapped,
	)
}

func buildMetaStore(cfg *config.Config, logger *slog.Logger) (meta.Store, error) {
	switch cfg.MetaBackend {
	case "memory":
		return metamem.New(), nil
	case "cassandra":
		return metacassandra.Open(
			metacassandra.SessionConfig{
				Hosts:       cfg.Cassandra.Hosts,
				Keyspace:    cfg.Cassandra.Keyspace,
				LocalDC:     cfg.Cassandra.LocalDC,
				Replication: cfg.Cassandra.Replication,
				Username:    cfg.Cassandra.Username,
				Password:    cfg.Cassandra.Password,
				Timeout:     cfg.Cassandra.Timeout,
				Logger:      logger,
				SlowMS:      metacassandra.SlowMSFromEnv(),
			},
			metacassandra.Options{DefaultShardCount: cfg.DefaultBucketShards},
		)
	default:
		return nil, errors.New("unknown meta backend: " + cfg.MetaBackend)
	}
}
