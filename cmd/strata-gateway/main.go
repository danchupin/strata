package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	strataconsole "github.com/danchupin/strata"
	"github.com/danchupin/strata/internal/adminapi"
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
	if mode == auth.ModeRequired && len(credMap) == 0 {
		log.Fatalf("auth: STRATA_AUTH_MODE=required but STRATA_STATIC_CREDENTIALS is empty")
	}
	mw := &auth.Middleware{
		Store: auth.NewStaticStore(credMap),
		Mode:  mode,
	}

	metrics.Register()
	apiHandler := s3api.New(dataBackend, metaStore)
	apiHandler.Region = cfg.RegionName

	jwtSecret, jwtSource := loadJWTSecret()
	log.Printf("admin: jwt secret source=%s", jwtSource)
	adminServer := adminapi.New(metaStore, mw.Store, buildVersion(), jwtSecret)

	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.Handle("/console/", strataconsole.ConsoleHandler())
	mux.Handle("/admin/v1/", adminServer.Handler())
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

// loadJWTSecret returns the HS256 key used to sign /admin/v1 session cookies.
// Production deployments MUST set STRATA_CONSOLE_JWT_SECRET (32 bytes hex,
// `openssl rand -hex 32`); when unset we generate an ephemeral random key
// and emit a WARN — sessions then invalidate on restart, which is fine for
// dev but unacceptable for prod.
func loadJWTSecret() ([]byte, string) {
	if v := os.Getenv("STRATA_CONSOLE_JWT_SECRET"); v != "" {
		return adminapi.DecodeSecret(v), "STRATA_CONSOLE_JWT_SECRET"
	}
	b, err := adminapi.GenerateSecret()
	if err != nil {
		log.Fatalf("admin: generate jwt secret: %v", err)
	}
	log.Printf("WARN admin: STRATA_CONSOLE_JWT_SECRET unset; generated ephemeral 32-byte secret. Sessions invalidate on restart. Set the env explicitly in production.")
	return b, "ephemeral"
}

// buildVersion returns the VCS revision baked in by `go build` (or "dev"
// when run without VCS metadata, e.g. in tests). Surfaced via
// /admin/v1/cluster/status::version so the console can display it.
func buildVersion() string {
	if v := os.Getenv("STRATA_VERSION"); v != "" {
		return v
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "dev"
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
