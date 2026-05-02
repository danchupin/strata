// strata-manifest-rewriter walks every bucket and converts any JSON
// manifest blob to protobuf in place. Leader-elected so only one worker is
// active per Strata cluster (US-049). Idempotent + resumable: rows already
// in proto format are skipped, so a crash mid-pass is safe to re-run.
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
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/manifestrewriter"
	"github.com/danchupin/strata/internal/meta"
	metacassandra "github.com/danchupin/strata/internal/meta/cassandra"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "scan only; do not rewrite blobs")
	batch := flag.Int("batch", 500, "page size for ListObjectVersions")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(os.Getenv("STRATA_LOG_LEVEL"))}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config", "error", err.Error())
		os.Exit(2)
	}

	// The rewriter forces proto when it (re)writes a row; respect any
	// operator-set encoder for new PutObject paths in the same process if
	// somebody runs the binary as a long-lived daemon.
	if v := os.Getenv("STRATA_MANIFEST_FORMAT"); v != "" {
		if err := data.SetManifestFormat(v); err != nil {
			logger.Error("manifest format", "error", err.Error())
			os.Exit(2)
		}
	}

	store, err := buildMetaStore(cfg, logger)
	if err != nil {
		logger.Error("meta store", "error", err.Error())
		os.Exit(2)
	}
	defer store.Close()

	w, err := manifestrewriter.New(manifestrewriter.Config{
		Meta:       store,
		Logger:     logger,
		BatchLimit: *batch,
		DryRun:     *dryRun,
	})
	if err != nil {
		logger.Error("worker", "error", err.Error())
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	run := func(runCtx context.Context) error {
		stats, err := w.Run(runCtx)
		logger.Info("manifestrewriter run",
			"buckets_scanned", stats.BucketsScanned,
			"objects_scanned", stats.ObjectsScanned,
			"objects_rewritten", stats.ObjectsRewritten,
			"already_proto", stats.ObjectsSkippedProto,
			"dry_run", *dryRun,
		)
		return err
	}

	locker := buildLocker(cfg, store)
	if locker == nil {
		logger.Warn("manifestrewriter: leader election disabled (no distributed locker for this meta backend)",
			"meta_backend", cfg.MetaBackend)
		if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("run", "error", err.Error())
			os.Exit(1)
		}
		return
	}
	session := &leader.Session{Locker: locker, Name: "manifest-rewriter", Holder: leader.DefaultHolder()}
	if err := session.AwaitAcquire(ctx); err != nil {
		return
	}
	workCtx := session.Supervise(ctx)
	if err := run(workCtx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("run", "error", err.Error())
		session.Release(context.Background())
		os.Exit(1)
	}
	session.Release(context.Background())
}

func buildLocker(cfg *config.Config, store meta.Store) leader.Locker {
	if cfg.MetaBackend == "cassandra" {
		if cs, ok := store.(*metacassandra.Store); ok {
			return &metacassandra.Locker{S: cs.Session()}
		}
	}
	if cfg.MetaBackend == "memory" {
		if ms, ok := store.(*metamem.Store); ok {
			return ms.Locker()
		}
	}
	return nil
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

func parseLogLevel(s string) slog.Level {
	switch s {
	case "DEBUG", "debug":
		return slog.LevelDebug
	case "WARN", "warn":
		return slog.LevelWarn
	case "ERROR", "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
