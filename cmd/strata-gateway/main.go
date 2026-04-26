package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/bucketstats"
	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/crypto/master"
	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	datarados "github.com/danchupin/strata/internal/data/rados"
	"github.com/danchupin/strata/internal/health"
	"github.com/danchupin/strata/internal/logging"
	"github.com/danchupin/strata/internal/meta"
	metacassandra "github.com/danchupin/strata/internal/meta/cassandra"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/s3api"
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

	mode, err := auth.ParseMode(cfg.Auth.Mode)
	if err != nil {
		logger.Error("auth", "error", err.Error())
		os.Exit(2)
	}
	credMap, err := auth.ParseStaticCredentials(cfg.Auth.StaticCredentials)
	if err != nil {
		logger.Error("auth credentials", "error", err.Error())
		os.Exit(2)
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
		logger.Error("auth: STRATA_AUTH_MODE=required but no credential stores are configured")
		os.Exit(2)
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
	mfaSecrets, err := s3api.ParseMFASecrets(os.Getenv("STRATA_MFA_SECRETS"))
	if err != nil {
		logger.Error("mfa secrets", "error", err.Error())
		os.Exit(2)
	}
	apiHandler.MFASecrets = mfaSecrets
	masterProvider, err := master.FromEnv()
	if err != nil && !errors.Is(err, master.ErrNoConfig) {
		logger.Error("sse master key", "error", err.Error())
		os.Exit(2)
	}
	if masterProvider != nil {
		apiHandler.Master = masterProvider
	}
	apiHandler.VHostPatterns = vhostPatterns()

	healthHandler := buildHealthHandler(metaStore, dataBackend)

	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", healthHandler.Healthz)
	mux.HandleFunc("/readyz", healthHandler.Readyz)
	mux.Handle("/", logging.NewMiddleware(logger, metrics.ObserveHTTP(mw.Wrap(s3api.NewAccessLogMiddleware(metaStore, apiHandler), s3api.WriteAuthDenied))))

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("strata-gateway listening",
			"listen", cfg.Listen,
			"data", cfg.DataBackend,
			"meta", cfg.MetaBackend,
			"region", cfg.RegionName,
			"auth", cfg.Auth.Mode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("http", "error", err.Error())
			os.Exit(1)
		}
	}()

	go func() {
		sampler := &bucketstats.Sampler{
			Meta:   metaStore,
			Sink:   metrics.BucketStatsObserver{},
			Logger: logger,
		}
		if err := sampler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("bucketstats", "error", err.Error())
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownWait)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
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
			Metrics:    metrics.RADOSObserver{},
		})
	default:
		return nil, errors.New("unknown data backend")
	}
}

// healthCanaryOID returns the RADOS OID stat'd by /readyz to confirm
// connectivity. Defaults to a fixed canary; override via env to point at a
// known-existing OID (operator-installed).
func healthCanaryOID() string {
	if v := os.Getenv("STRATA_RADOS_HEALTH_OID"); v != "" {
		return v
	}
	return "strata-readyz-canary"
}

// vhostPatterns returns the configured virtual-hosted-style host patterns.
// Reads STRATA_VHOST_PATTERN as a comma-separated list of "*.<suffix>"
// entries; defaults to "*.s3.local" so a fresh deployment supports
// virtual-hosted-style URLs out of the box. Set the env var to an empty
// non-empty placeholder ("-") to disable vhost extraction entirely.
func vhostPatterns() []string {
	v, ok := os.LookupEnv("STRATA_VHOST_PATTERN")
	if !ok {
		return []string{"*.s3.local"}
	}
	if v == "-" {
		return nil
	}
	return s3api.ParseVHostPatterns(v)
}

type cassandraProber interface {
	Probe(ctx context.Context) error
}

type radosProber interface {
	Probe(ctx context.Context, oid string) error
}

func buildHealthHandler(metaStore meta.Store, dataBackend data.Backend) *health.Handler {
	probes := map[string]health.Probe{}
	if cp, ok := metaStore.(cassandraProber); ok {
		probes["cassandra"] = cp.Probe
	}
	if rp, ok := dataBackend.(radosProber); ok {
		oid := healthCanaryOID()
		probes["rados"] = func(ctx context.Context) error { return rp.Probe(ctx, oid) }
	}
	return &health.Handler{Probes: probes}
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
