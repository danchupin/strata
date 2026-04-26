package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	datarados "github.com/danchupin/strata/internal/data/rados"
	"github.com/danchupin/strata/internal/gc"
	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/logging"
	"github.com/danchupin/strata/internal/meta"
	metacassandra "github.com/danchupin/strata/internal/meta/cassandra"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/metrics"
)

func main() {
	logger := logging.Setup()

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

	metaStore, err := buildMetaStore(cfg, logger)
	if err != nil {
		logger.Error("meta store", "error", err.Error())
		os.Exit(2)
	}
	defer metaStore.Close()

	metrics.Register()
	w := &gc.Worker{
		Meta:     metaStore,
		Data:     dataBackend,
		Region:   cfg.RegionName,
		Interval: cfg.GC.Interval,
		Grace:    cfg.GC.Grace,
		Logger:   logger,
	}

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler())
		logger.Info("gc: metrics", "listen", cfg.GC.MetricsListen)
		_ = http.ListenAndServe(cfg.GC.MetricsListen, mux)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	locker := buildLocker(cfg, metaStore)
	if locker == nil {
		logger.Warn("gc: leader election disabled (no distributed locker)", "meta_backend", cfg.MetaBackend)
		if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("worker", "error", err.Error())
			os.Exit(1)
		}
		return
	}
	session := &leader.Session{
		Locker: locker,
		Name:   "gc",
		Holder: leader.DefaultHolder(),
		Logger: logger,
	}
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
		return nil, errors.New("unknown data backend")
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
		return nil, errors.New("unknown meta backend")
	}
}
