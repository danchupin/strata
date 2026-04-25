package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	datarados "github.com/danchupin/strata/internal/data/rados"
	"github.com/danchupin/strata/internal/meta"
	metacassandra "github.com/danchupin/strata/internal/meta/cassandra"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/s3api"
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

	mode, err := auth.ParseMode(cfg.Auth.Mode)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	credMap, err := auth.ParseStaticCredentials(cfg.Auth.StaticCredentials)
	if err != nil {
		log.Fatalf("auth credentials: %v", err)
	}
	sts := auth.NewSTSStore()
	stores := []auth.CredentialsStore{sts, auth.NewStaticStore(credMap)}
	if cs, ok := metaStore.(*metacassandra.Store); ok {
		stores = append(stores, metacassandra.NewCredentialStore(cs.Session()))
	}
	if ms, ok := metaStore.(*metamem.Store); ok {
		stores = append(stores, metamem.NewCredentialStore(ms))
	}
	if mode == auth.ModeRequired && len(credMap) == 0 && len(stores) == 2 {
		log.Fatalf("auth: STRATA_AUTH_MODE=required but no credential stores are configured")
	}
	multi := auth.NewMultiStore(auth.DefaultCacheTTL, stores...)
	mw := &auth.Middleware{
		Store: multi,
		Mode:  mode,
	}

	metrics.Register()
	apiHandler := s3api.New(dataBackend, metaStore)
	apiHandler.Region = cfg.RegionName
	apiHandler.InvalidateCredential = multi.Invalidate
	apiHandler.STS = sts

	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.Handle("/", metrics.ObserveHTTP(mw.Wrap(apiHandler, s3api.WriteAuthDenied)))

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("strata-gateway listening on %s (data=%s meta=%s region=%s auth=%s)",
			cfg.Listen, cfg.DataBackend, cfg.MetaBackend, cfg.RegionName, cfg.Auth.Mode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownWait)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
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
