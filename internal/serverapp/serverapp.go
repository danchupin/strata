// Package serverapp hosts the gateway entrypoint shared by the unified
// `strata server` subcommand. Builds backends, wires the HTTP handler
// chain, listens on cfg.Listen, and spawns the worker Supervisor.
package serverapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"

	strataconsole "github.com/danchupin/strata"
	"github.com/danchupin/strata/cmd/strata/workers"
	"github.com/danchupin/strata/internal/adminapi"
	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/bucketstats"
	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/crypto/kms"
	"github.com/danchupin/strata/internal/crypto/master"
	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	datarados "github.com/danchupin/strata/internal/data/rados"
	"github.com/danchupin/strata/internal/health"
	"github.com/danchupin/strata/internal/heartbeat"
	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/logging"
	"github.com/danchupin/strata/internal/meta"
	metacassandra "github.com/danchupin/strata/internal/meta/cassandra"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	metatikv "github.com/danchupin/strata/internal/meta/tikv"
	"github.com/danchupin/strata/internal/metrics"
	strataotel "github.com/danchupin/strata/internal/otel"
	"github.com/danchupin/strata/internal/promclient"
	"github.com/danchupin/strata/internal/s3api"
)

// Run starts the S3 gateway: builds the data + meta backends, wires the
// HTTP handler chain, listens on cfg.Listen, and spawns the worker
// Supervisor with the resolved worker set. Blocks until ctx is cancelled
// or the listener fails. Returns nil on a clean ctx-driven shutdown.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger, selected []workers.Worker) error {
	if v := os.Getenv("STRATA_MANIFEST_FORMAT"); v != "" {
		if err := data.SetManifestFormat(v); err != nil {
			return fmt.Errorf("manifest format: %w", err)
		}
	}
	logger.Info("manifest encoder", "format", data.ManifestFormat())

	tracerProvider, err := strataotel.Init(ctx)
	if err != nil {
		return fmt.Errorf("otel init: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracerProvider.Shutdown(shutdownCtx); err != nil {
			logger.Warn("otel shutdown", "error", err.Error())
		}
	}()

	dataBackend, err := buildDataBackend(cfg, logger, tracerProvider)
	if err != nil {
		return fmt.Errorf("data backend: %w", err)
	}
	defer dataBackend.Close()

	metaStore, err := buildMetaStore(cfg, logger, tracerProvider)
	if err != nil {
		return fmt.Errorf("meta store: %w", err)
	}
	defer metaStore.Close()

	mode, err := auth.ParseMode(cfg.Auth.Mode)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	credMap, err := auth.ParseStaticCredentials(cfg.Auth.StaticCredentials)
	if err != nil {
		return fmt.Errorf("auth credentials: %w", err)
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
		return errors.New("auth: STRATA_AUTH_MODE=required but no credential stores are configured")
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
		return fmt.Errorf("mfa secrets: %w", err)
	}
	apiHandler.MFASecrets = mfaSecrets
	masterProvider, err := master.FromEnv()
	if err != nil && !errors.Is(err, master.ErrNoConfig) {
		return fmt.Errorf("sse master key: %w", err)
	}
	if masterProvider != nil {
		apiHandler.Master = masterProvider
	}
	kmsProvider, err := kms.FromEnv(kms.WithAWSKMSClientFactory(awsKMSClientFactory))
	if err != nil && !errors.Is(err, kms.ErrNoConfig) {
		return fmt.Errorf("sse-kms provider: %w", err)
	}
	if kmsProvider != nil {
		apiHandler.KMS = kmsProvider
	}
	apiHandler.VHostPatterns = vhostPatterns()

	healthHandler := buildHealthHandler(metaStore, dataBackend)

	jwtSecret, jwtSource := loadJWTSecret(logger)
	logger.Info("admin jwt secret", "source", jwtSource)
	clusterName := os.Getenv("STRATA_CLUSTER_NAME")
	hbStore := buildHeartbeatStore(cfg, metaStore)
	version := buildVersion()
	prom := promclient.New(os.Getenv("STRATA_PROMETHEUS_URL"))
	if !prom.Available() {
		logger.Info("admin: STRATA_PROMETHEUS_URL unset; top-buckets/consumers + metrics dashboard will report metrics_available=false")
	}
	adminLocker := buildLocker(cfg, metaStore)
	adminServer := adminapi.New(adminapi.Config{
		Meta:        metaStore,
		Creds:       multi,
		Heartbeat:   hbStore,
		Prom:        prom,
		Locker:      adminLocker,
		Version:     version,
		ClusterName: clusterName,
		Region:      cfg.RegionName,
		MetaBackend: cfg.MetaBackend,
		DataBackend: cfg.DataBackend,
		JWTSecret:   jwtSecret,
	})

	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", healthHandler.Healthz)
	mux.HandleFunc("/readyz", healthHandler.Readyz)
	mux.Handle("/console/", strataconsole.ConsoleHandler())
	auditTTL := auditRetention(logger)
	mux.Handle("/admin/v1/", s3api.NewAuditMiddleware(metaStore, auditTTL, adminServer.Handler()))
	auditHandler := s3api.NewAuditMiddleware(metaStore, auditTTL, apiHandler)
	mux.Handle("/", strataotel.NewMiddleware(tracerProvider, logging.NewMiddleware(logger, metrics.ObserveHTTP(mw.Wrap(s3api.NewAccessLogMiddleware(metaStore, auditHandler), s3api.WriteAuthDenied)))))

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("strata server listening",
			"listen", cfg.Listen,
			"data", cfg.DataBackend,
			"meta", cfg.MetaBackend,
			"region", cfg.RegionName,
			"auth", cfg.Auth.Mode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
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

	if hbStore != nil {
		hb := &heartbeat.Heartbeater{
			Store: hbStore,
			Node: heartbeat.Node{
				ID:        heartbeat.DefaultNodeID(),
				Address:   cfg.Listen,
				Version:   version,
				StartedAt: time.Now().UTC(),
				Workers:   workerNamesFromList(selected),
			},
		}
		go hb.Run(ctx)
	}

	workerErr := make(chan error, 1)
	if len(selected) > 0 {
		if adminLocker == nil {
			return fmt.Errorf("workers selected (%s) but meta backend %q exposes no leader-election locker",
				workerNames(selected), cfg.MetaBackend)
		}
		supervisor := &workers.Supervisor{
			Deps: workers.Dependencies{
				Logger: logger,
				Meta:   metaStore,
				Data:   dataBackend,
				Tracer: tracerProvider,
				Locker: adminLocker,
				Region: cfg.RegionName,
			},
		}
		go func() {
			err := supervisor.Run(ctx, selected)
			if err != nil && !errors.Is(err, context.Canceled) {
				workerErr <- err
				return
			}
			workerErr <- nil
		}()
	} else {
		workerErr <- nil
	}

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownWait)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		<-serverErr
		<-workerErr
		return nil
	case err := <-serverErr:
		return err
	case err := <-workerErr:
		return err
	}
}

// buildLocker returns the leader-election locker exposed by the meta
// backend. Cassandra uses LWT-backed leases; TiKV uses pessimistic-txn
// leases (US-011); the in-memory backend ships a process-local locker.
// Backends that lack a locker return nil.
func buildLocker(cfg *config.Config, store meta.Store) leader.Locker {
	if cfg.MetaBackend == "cassandra" {
		if cs, ok := store.(*metacassandra.Store); ok {
			return &metacassandra.Locker{S: cs.Session()}
		}
	}
	if cfg.MetaBackend == "tikv" {
		if ts, ok := store.(*metatikv.Store); ok {
			return metatikv.NewLocker(ts)
		}
	}
	if cfg.MetaBackend == "memory" {
		if ms, ok := store.(*metamem.Store); ok {
			return ms.Locker()
		}
	}
	return nil
}

func workerNames(ws []workers.Worker) string {
	names := make([]string, len(ws))
	for i, w := range ws {
		names[i] = w.Name
	}
	return strings.Join(names, ",")
}

func buildDataBackend(cfg *config.Config, logger *slog.Logger, tp *strataotel.Provider) (data.Backend, error) {
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
			Tracer:     tp.Tracer("strata.data.rados"),
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

// awsKMSClientFactory builds an AWS SDK KMS client wrapped behind the narrow
// kms.KMSAPI interface. Resolves credentials via the standard AWS credential
// chain (env vars, shared config, IRSA, EC2/ECS instance roles). Empty region
// lets the chain pick from AWS_REGION / EC2 metadata.
func awsKMSClientFactory(region string) (kms.KMSAPI, error) {
	ctx := context.Background()
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return awskms.NewFromConfig(cfg), nil
}

// auditRetention reads STRATA_AUDIT_RETENTION (Go duration or "<N>d") and
// returns the row TTL applied to audit_log writes. Falls back to
// s3api.DefaultAuditRetention on parse error and logs a WARN.
func auditRetention(logger *slog.Logger) time.Duration {
	v := os.Getenv("STRATA_AUDIT_RETENTION")
	d, err := s3api.ParseAuditRetention(v)
	if err != nil {
		logger.Warn("audit retention parse failed; using default",
			"value", v, "default", s3api.DefaultAuditRetention.String(), "error", err.Error())
		return s3api.DefaultAuditRetention
	}
	return d
}

// vhostPatterns returns the configured virtual-hosted-style host patterns.
// Reads STRATA_VHOST_PATTERN as a comma-separated list of "*.<suffix>"
// entries; defaults to "*.s3.local" so a fresh deployment supports
// virtual-hosted-style URLs out of the box. Set the env var to "-" to
// disable vhost extraction entirely.
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

func buildMetaStore(cfg *config.Config, logger *slog.Logger, tp *strataotel.Provider) (meta.Store, error) {
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
				Tracer:      tp.Tracer("strata.meta.cassandra"),
			},
			metacassandra.Options{DefaultShardCount: cfg.DefaultBucketShards},
		)
	case "tikv":
		eps := parseTiKVEndpoints(cfg.TiKV.Endpoints)
		if len(eps) == 0 {
			return nil, errors.New("tikv: STRATA_TIKV_PD_ENDPOINTS is empty")
		}
		return metatikv.Open(metatikv.Config{PDEndpoints: eps})
	default:
		return nil, errors.New("unknown meta backend")
	}
}

// parseTiKVEndpoints splits a comma-separated PD endpoint list, trims
// whitespace, and drops empty entries. The koanf env provider does not
// auto-split slice values (it stores the raw string), so we do the split
// at use-site instead of fighting mapstructure decode hooks.
func parseTiKVEndpoints(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// loadJWTSecret returns the HS256 key used to sign /admin/v1 session cookies.
// Production deployments MUST set STRATA_CONSOLE_JWT_SECRET (32 bytes hex);
// when unset we generate an ephemeral random key and emit a WARN.
func loadJWTSecret(logger *slog.Logger) ([]byte, string) {
	if v := os.Getenv("STRATA_CONSOLE_JWT_SECRET"); v != "" {
		return adminapi.DecodeSecret(v), "STRATA_CONSOLE_JWT_SECRET"
	}
	b, err := adminapi.GenerateSecret()
	if err != nil {
		logger.Warn("admin: generate jwt secret", "error", err.Error())
		return nil, "ephemeral-error"
	}
	logger.Warn("admin: STRATA_CONSOLE_JWT_SECRET unset; generated ephemeral 32-byte secret. Sessions invalidate on restart. Set the env explicitly in production.")
	return b, "ephemeral"
}

// buildVersion returns the VCS revision baked in by `go build` (or "dev"
// when run without VCS metadata, e.g. in tests).
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

// buildHeartbeatStore returns a heartbeat.Store backed by the same backend
// as metaStore. Returns nil for backends without a heartbeat implementation
// (currently TiKV) — the admin handlers degrade gracefully.
func buildHeartbeatStore(cfg *config.Config, metaStore meta.Store) heartbeat.Store {
	switch cfg.MetaBackend {
	case "memory":
		return heartbeat.NewMemoryStore()
	case "cassandra":
		if cas, ok := metaStore.(*metacassandra.Store); ok {
			return &heartbeat.CassandraStore{S: cas.Session()}
		}
	}
	return nil
}

// workerNamesFromList extracts worker names for the heartbeat row.
func workerNamesFromList(ws []workers.Worker) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Name)
	}
	return out
}
