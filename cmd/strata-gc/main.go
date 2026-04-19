package main

import (
	"context"
	"errors"
	"log"
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
	"github.com/danchupin/strata/internal/meta"
	metacassandra "github.com/danchupin/strata/internal/meta/cassandra"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/metrics"
)

func main() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	dataBackend, err := buildDataBackend(cfg)
	if err != nil {
		log.Fatalf("data backend: %v", err)
	}
	defer dataBackend.Close()

	metaStore, err := buildMetaStore(cfg)
	if err != nil {
		log.Fatalf("meta store: %v", err)
	}
	defer metaStore.Close()

	metrics.Register()
	w := &gc.Worker{
		Meta:     metaStore,
		Data:     dataBackend,
		Region:   cfg.RegionName,
		Interval: cfg.GC.Interval,
		Grace:    cfg.GC.Grace,
	}

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler())
		log.Printf("gc: metrics on %s", cfg.GC.MetricsListen)
		_ = http.ListenAndServe(cfg.GC.MetricsListen, mux)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	locker := buildLocker(cfg, metaStore)
	if locker == nil {
		log.Printf("gc: leader election disabled (no distributed locker for meta=%s)", cfg.MetaBackend)
		if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatalf("worker: %v", err)
		}
		return
	}
	session := &leader.Session{
		Locker: locker,
		Name:   "gc",
		Holder: leader.DefaultHolder(),
	}
	for ctx.Err() == nil {
		if err := session.AwaitAcquire(ctx); err != nil {
			return
		}
		workCtx := session.Supervise(ctx)
		if err := w.Run(workCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("worker: %v", err)
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

func buildDataBackend(cfg *config.Config) (data.Backend, error) {
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
		})
	default:
		return nil, errors.New("unknown data backend")
	}
}

func buildMetaStore(cfg *config.Config) (meta.Store, error) {
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
			},
			metacassandra.Options{DefaultShardCount: cfg.DefaultBucketShards},
		)
	default:
		return nil, errors.New("unknown meta backend")
	}
}

