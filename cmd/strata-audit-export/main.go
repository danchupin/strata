// strata-audit-export drains aged audit_log partitions, writes one
// gzipped JSON-lines object per (bucket, day) partition into the
// configured export bucket, then deletes the source partition.
// Leader-elected so only one worker is active per Strata cluster (US-046).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/danchupin/strata/internal/auditexport"
	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	datarados "github.com/danchupin/strata/internal/data/rados"
	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/meta"
	metacassandra "github.com/danchupin/strata/internal/meta/cassandra"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/metrics"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(os.Getenv("STRATA_LOG_LEVEL"))}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config", "error", err.Error())
		os.Exit(2)
	}

	bucket := strings.TrimSpace(os.Getenv("STRATA_AUDIT_EXPORT_BUCKET"))
	if bucket == "" {
		logger.Error("audit-export: STRATA_AUDIT_EXPORT_BUCKET is required")
		os.Exit(2)
	}

	dataBackend, err := buildDataBackend(cfg, logger)
	if err != nil {
		logger.Error("data backend", "error", err.Error())
		os.Exit(2)
	}
	defer dataBackend.Close()

	store, err := buildMetaStore(cfg, logger)
	if err != nil {
		logger.Error("meta store", "error", err.Error())
		os.Exit(2)
	}
	defer store.Close()

	w, err := auditexport.New(auditexport.Config{
		Meta:     store,
		Data:     dataBackend,
		Bucket:   bucket,
		Prefix:   os.Getenv("STRATA_AUDIT_EXPORT_PREFIX"),
		After:    parseDuration("STRATA_AUDIT_EXPORT_AFTER", 30*24*time.Hour),
		Interval: parseDuration("STRATA_AUDIT_EXPORT_INTERVAL", 24*time.Hour),
		Logger:   logger,
	})
	if err != nil {
		logger.Error("worker", "error", err.Error())
		os.Exit(2)
	}

	metrics.Register()
	go func() {
		listen := envOr("STRATA_AUDIT_EXPORT_METRICS_LISTEN", ":9106")
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler())
		logger.Info("audit-export: metrics", "listen", listen)
		_ = http.ListenAndServe(listen, mux)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	locker := buildLocker(cfg, store)
	if locker == nil {
		logger.Warn("audit-export: leader election disabled (no distributed locker for this meta backend)",
			"meta_backend", cfg.MetaBackend)
		if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("worker", "error", err.Error())
			os.Exit(1)
		}
		return
	}
	session := &leader.Session{Locker: locker, Name: "audit-export", Holder: leader.DefaultHolder()}
	for ctx.Err() == nil {
		if err := session.AwaitAcquire(ctx); err != nil {
			return
		}
		workCtx := session.Supervise(ctx)
		if err := w.Run(workCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("worker", "error", err.Error())
		}
		session.Release(context.Background())
	}
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

func buildDataBackend(cfg *config.Config, logger *slog.Logger) (data.Backend, error) {
	switch cfg.DataBackend {
	case "memory":
		return datamem.New(), nil
	case "rados":
		classes, err := datarados.ParseClasses(cfg.RADOS.Classes)
		if err != nil {
			return nil, err
		}
		clusters, err := datarados.ParseClusters(cfg.RADOS.Clusters)
		if err != nil {
			return nil, err
		}
		return datarados.New(datarados.Config{
			ConfigFile: cfg.RADOS.ConfigFile,
			User:       cfg.RADOS.User,
			Keyring:    cfg.RADOS.Keyring,
			Pool:       cfg.RADOS.Pool,
			Namespace:  cfg.RADOS.Namespace,
			Classes:    classes,
			Clusters:   clusters,
			Logger:     logger,
			Metrics:    metrics.RADOSObserver{},
		})
	default:
		return nil, errors.New("unknown data backend: " + cfg.DataBackend)
	}
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
				Metrics:     metrics.CassandraObserver{},
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

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func parseDuration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
