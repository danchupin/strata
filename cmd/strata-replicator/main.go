// strata-replicator drains the replication_queue and copies each source
// object to a peer Strata gateway over HTTP. Leader-elected via
// internal/leader so only one replicator is active per Strata cluster.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	datarados "github.com/danchupin/strata/internal/data/rados"
	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/meta"
	metacassandra "github.com/danchupin/strata/internal/meta/cassandra"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/replication"
)

type promMetrics struct{}

func (promMetrics) ObserveLag(ruleID string, lag float64) {
	metrics.ReplicationLagSeconds.WithLabelValues(ruleID).Observe(lag)
}
func (promMetrics) IncCompleted(ruleID string) {
	metrics.ReplicationCompleted.WithLabelValues(ruleID).Inc()
}
func (promMetrics) IncFailed(ruleID string) {
	metrics.ReplicationFailed.WithLabelValues(ruleID).Inc()
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(os.Getenv("STRATA_LOG_LEVEL"))}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config", "error", err.Error())
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

	dispatcher := &replication.HTTPDispatcher{
		Client: &http.Client{Timeout: parseDuration("STRATA_REPLICATOR_HTTP_TIMEOUT", 30*time.Second)},
		Scheme: envOr("STRATA_REPLICATOR_PEER_SCHEME", "https"),
	}

	w, err := replication.New(replication.Config{
		Meta:        store,
		Data:        dataBackend,
		Dispatcher:  dispatcher,
		Logger:      logger,
		Metrics:     promMetrics{},
		Interval:    parseDuration("STRATA_REPLICATOR_INTERVAL", 5*time.Second),
		MaxRetries:  parseInt("STRATA_REPLICATOR_MAX_RETRIES", 6),
		BackoffBase: parseDuration("STRATA_REPLICATOR_BACKOFF_BASE", 1*time.Second),
		PollLimit:   parseInt("STRATA_REPLICATOR_POLL_LIMIT", 100),
	})
	if err != nil {
		logger.Error("worker", "error", err.Error())
		os.Exit(2)
	}

	metrics.Register()
	go func() {
		listen := envOr("STRATA_REPLICATOR_METRICS_LISTEN", ":9103")
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler())
		logger.Info("replicator: metrics", "listen", listen)
		_ = http.ListenAndServe(listen, mux)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	locker := buildLocker(cfg, store)
	if locker == nil {
		logger.Warn("replicator: leader election disabled (no distributed locker for this meta backend)",
			"meta_backend", cfg.MetaBackend)
		if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("worker", "error", err.Error())
			os.Exit(1)
		}
		return
	}
	session := &leader.Session{Locker: locker, Name: "replicator", Holder: leader.DefaultHolder()}
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
		return datarados.New(datarados.Config{
			ConfigFile: cfg.RADOS.ConfigFile,
			User:       cfg.RADOS.User,
			Keyring:    cfg.RADOS.Keyring,
			Pool:       cfg.RADOS.Pool,
			Namespace:  cfg.RADOS.Namespace,
			Classes:    classes,
			Logger:     logger,
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

func parseInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
