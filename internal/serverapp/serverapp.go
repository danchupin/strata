// Package serverapp hosts the gateway entrypoint shared by the unified
// `strata server` subcommand. Builds backends, wires the HTTP handler
// chain, listens on cfg.Listen, and spawns the worker Supervisor.
package serverapp

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"

	strataconsole "github.com/danchupin/strata"
	"github.com/danchupin/strata/cmd/strata/workers"
	"github.com/danchupin/strata/internal/adminapi"
	"github.com/danchupin/strata/internal/auditstream"
	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/bucketstats"
	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/crypto/kms"
	"github.com/danchupin/strata/internal/crypto/master"
	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/data/placement"
	datarados "github.com/danchupin/strata/internal/data/rados"
	datas3 "github.com/danchupin/strata/internal/data/s3"
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
	"github.com/danchupin/strata/internal/rebalance"
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

	tracerProvider, err := strataotel.Init(ctx, strataotel.InitOptions{
		Logger:         logger,
		RingbufMetrics: metrics.OTelRingbufObserver{},
	})
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
	// Wire access_key extraction for the strata_http_requests_total counter
	// (kept here to avoid an internal/metrics → internal/auth import cycle).
	metrics.HTTPMetricsLabeler = func(r *http.Request) string {
		ai := auth.FromContext(r.Context())
		if ai == nil || ai.IsAnonymous || ai.AccessKey == "" {
			return "_anon"
		}
		return ai.AccessKey
	}
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
	drainCache := placement.NewDrainCache(metaStore.ListClusterStates, 0)
	apiHandler.DrainCache = drainCache

	// Boot reconcile: materialise cluster_state for every configured
	// cluster id without a row. Existing-live (referenced by class env
	// or any bucket Placement, with at least one bucket carrying data)
	// → live + weight=100; otherwise → pending + weight=0. Idempotent
	// re-runs leave existing rows alone (US-001 cluster-weights).
	envClusters := make([]string, 0, len(knownDataClusters(cfg)))
	for id := range knownDataClusters(cfg) {
		envClusters = append(envClusters, id)
	}
	if _, _, rerr := ReconcileClusters(ctx, metaStore, ReconcileInput{
		EnvClusters:   envClusters,
		ClassDefaults: classDefaultClusters(cfg),
		HasData:       reconcileHasData(ctx, metaStore),
	}, logger); rerr != nil {
		logger.Warn("cluster reconcile", "error", rerr.Error())
	}

	// One-shot lookup-table reconcile (US-005 ALLOW FILTERING denormalize):
	// backfill the `_by_cluster` denormalised lookup tables for every row in
	// `gc_entries_v2` and `multipart_uploads` that carries a cluster id.
	// Cassandra-only — memory + TiKV backends do not suffer ALLOW FILTERING.
	// Runs once per process before the listener accepts requests; idempotent
	// re-runs are cheap (every write is an upsert with the same payload).
	if cs, ok := metaStore.(*metacassandra.Store); ok {
		if _, rerr := cs.ReconcileLookupTables(ctx, logger); rerr != nil {
			logger.Warn("cluster reconcile lookup tables", "error", rerr.Error())
		}
	}

	healthHandler := buildHealthHandler(metaStore, dataBackend)

	jwtSecret, jwtSource, jwtFile := loadJWTSecret(logger)
	logger.Info("admin jwt secret", "source", jwtSource, "file", jwtFile)
	jwtEphemeral := strings.HasPrefix(jwtSource, "ephemeral")
	clusterName := os.Getenv("STRATA_CLUSTER_NAME")
	hbStore := buildHeartbeatStore(cfg, metaStore)
	version := buildVersion()
	prom := promclient.New(os.Getenv("STRATA_PROMETHEUS_URL"))
	if !prom.Available() {
		logger.Info("admin: STRATA_PROMETHEUS_URL unset; top-buckets/consumers + metrics dashboard will report metrics_available=false")
	}
	adminLocker := buildLocker(cfg, metaStore)
	auditTTL := auditRetention(logger)
	auditBroadcaster := auditstream.New(logger, metrics.AuditStreamObserver{})
	storageClassSnapshot := bucketstats.NewSnapshot(poolsByClass(cfg, logger))
	rebalanceProgress := rebalance.NewProgressTracker(rebalanceInterval(logger))
	adminServer := adminapi.New(adminapi.Config{
		Meta:                 metaStore,
		Data:                 dataBackend,
		Creds:                multi,
		Heartbeat:            hbStore,
		Prom:                 prom,
		Locker:               adminLocker,
		Version:              version,
		ClusterName:          clusterName,
		Region:               cfg.RegionName,
		MetaBackend:          cfg.MetaBackend,
		DataBackend:          cfg.DataBackend,
		JWTSecret:            jwtSecret,
		JWTEphemeral:         jwtEphemeral,
		JWTSecretFile:        jwtFile,
		PrometheusURL:        os.Getenv("STRATA_PROMETHEUS_URL"),
		OtelEndpoint:         os.Getenv(strataotel.EnvEndpoint),
		HeartbeatInterval:    heartbeat.DefaultInterval,
		ConsoleThemeDefault:  consoleThemeDefault(),
		CassandraSettings:    cassandraSettings(cfg),
		RADOSSettings:        radosSettings(cfg),
		TiKVSettings:         tikvSettings(cfg),
		S3Backend:            s3BackendSettings(cfg),
		AuditTTL:             auditTTL,
		InvalidateCredential: multi.Invalidate,
		S3Handler:            apiHandler,
		AuditStream:          auditBroadcaster,
		TraceRingbuf:         tracerProvider.Ringbuf(),
		StorageClasses:       storageClassSnapshot,
		KnownClusters:        knownDataClusters(cfg),
		ClusterBackends:      clusterBackends(cfg),
		DrainCache:           drainCache,
		RebalanceProgress:    rebalanceProgress,
	})

	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", healthHandler.Healthz)
	mux.HandleFunc("/readyz", healthHandler.Readyz)
	mux.Handle("/console/", strataconsole.ConsoleHandler())
	adminAudit := s3api.NewAuditMiddleware(metaStore, auditTTL, adminServer.Handler())
	adminAudit.Publisher = auditBroadcaster
	mux.Handle("/admin/v1/", adminAudit)
	auditHandler := s3api.NewAuditMiddleware(metaStore, auditTTL, apiHandler)
	auditHandler.Publisher = auditBroadcaster
	mux.Handle("/", strataotel.NewMiddleware(tracerProvider, logging.NewMiddleware(logger, metrics.ObserveHTTP(mw.Wrap(s3api.NewAccessLogMiddleware(metaStore, auditHandler), s3api.NewAuthDenyHandler(metaStore))))))

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
			Meta:      metaStore,
			Sink:      metrics.BucketStatsObserver{},
			ShardSink: metrics.BucketStatsObserver{},
			ClassSink: metrics.BucketStatsObserver{},
			Snapshot:  storageClassSnapshot,
			Logger:    logger,
			TopN:      bucketStatsTopN(logger),
			Interval:  bucketStatsInterval(logger),
		}
		if err := sampler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("bucketstats", "error", err.Error())
		}
	}()

	var supervisor *workers.Supervisor
	if len(selected) > 0 {
		if adminLocker == nil {
			return fmt.Errorf("workers selected (%s) but meta backend %q exposes no leader-election locker",
				workerNames(selected), cfg.MetaBackend)
		}
		supervisor = &workers.Supervisor{
			Deps: workers.Dependencies{
				Logger:            logger,
				Meta:              metaStore,
				Data:              dataBackend,
				Tracer:            tracerProvider,
				Locker:            adminLocker,
				Region:            cfg.RegionName,
				RebalanceProgress: rebalanceProgress,
			},
		}
	}

	var hb *heartbeat.Heartbeater
	if hbStore != nil {
		hb = &heartbeat.Heartbeater{
			Store: hbStore,
			Node: heartbeat.Node{
				ID:        heartbeat.DefaultNodeID(),
				Address:   cfg.Listen,
				Version:   version,
				StartedAt: time.Now().UTC(),
				Workers:   workerNamesFromList(selected),
			},
		}
		if supervisor != nil {
			events := supervisor.LeaderEvents()
			go func() {
				for ev := range events {
					hb.SetLeaderFor(ev.Worker, ev.Acquired)
				}
			}()
		}
		go hb.Run(ctx)
	}

	workerErr := make(chan error, 1)
	if supervisor != nil {
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
	case "s3":
		s3Clusters, err := datas3.ParseClusters(cfg.S3.Clusters)
		if err != nil {
			return nil, fmt.Errorf("STRATA_S3_CLUSTERS: %w", err)
		}
		s3Classes, err := datas3.ParseClasses(cfg.S3.Classes)
		if err != nil {
			return nil, fmt.Errorf("STRATA_S3_CLASSES: %w", err)
		}
		return datas3.New(datas3.Config{
			Clusters:       s3Clusters,
			Classes:        s3Classes,
			Tracer:         tp.Tracer("strata.data.s3"),
			TracerProvider: tp.TracerProvider(),
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

// poolsByClass parses cfg.RADOS.Classes into a class -> pool name map for
// the storage-classes admin endpoint (US-003 storage cycle). Empty for
// non-RADOS backends or when the env var is unset / malformed (a parse
// error is logged and an empty map returned so the gateway still boots).
func poolsByClass(cfg *config.Config, logger *slog.Logger) map[string]string {
	if cfg.DataBackend != "rados" || cfg.RADOS.Classes == "" {
		return map[string]string{}
	}
	classes, err := datarados.ParseClasses(cfg.RADOS.Classes)
	if err != nil {
		logger.Warn("rados classes parse failed; storage classes pool map empty",
			"error", err.Error())
		return map[string]string{}
	}
	out := make(map[string]string, len(classes))
	for class, spec := range classes {
		out[class] = spec.Pool
	}
	return out
}

// rebalanceInterval reads STRATA_REBALANCE_INTERVAL (Go duration). Out-of-
// range values are clamped to [1m, 24h]; unparseable falls back to 1h. Used
// by serverapp to size the drain-progress stale-cache threshold (US-003
// drain-lifecycle) — the rebalance worker re-reads the env independently
// in its own Build constructor so the two values stay in lock-step.
func rebalanceInterval(logger *slog.Logger) time.Duration {
	const (
		fallback = time.Hour
		lo       = time.Minute
		hi       = 24 * time.Hour
	)
	v := strings.TrimSpace(os.Getenv("STRATA_REBALANCE_INTERVAL"))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		logger.Warn("rebalance interval parse failed; using default",
			"value", v, "default", fallback.String(), "error", err.Error())
		return fallback
	}
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}

// bucketStatsInterval reads STRATA_BUCKETSTATS_INTERVAL (Go duration). Empty
// or unparseable falls back to the sampler default (1h via Sampler.Run).
// Surfaced primarily so e2e specs can drive the sampler at sub-second cadence
// without waiting the production-shape default.
func bucketStatsInterval(logger *slog.Logger) time.Duration {
	v := strings.TrimSpace(os.Getenv("STRATA_BUCKETSTATS_INTERVAL"))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		logger.Warn("bucketstats interval parse failed; using default",
			"value", v, "error", errString(err))
		return 0
	}
	return d
}

// bucketStatsTopN reads STRATA_BUCKETSTATS_TOPN and returns the cap for the
// per-shard distribution sampling pass (US-012). Falls back to
// bucketstats.DefaultTopN on unset / parse error / non-positive value.
func bucketStatsTopN(logger *slog.Logger) int {
	v := strings.TrimSpace(os.Getenv("STRATA_BUCKETSTATS_TOPN"))
	if v == "" {
		return bucketstats.DefaultTopN
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		logger.Warn("bucketstats topN parse failed; using default",
			"value", v, "default", bucketstats.DefaultTopN, "error", errString(err))
		return bucketstats.DefaultTopN
	}
	return n
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
		return metatikv.Open(metatikv.Config{
			PDEndpoints: eps,
			Tracer:      tp.Tracer("strata.meta.tikv"),
		})
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

// jwtSharedSecretFile is the on-disk path consulted as the third stage of
// loadJWTSecret. Mounted via the `strata-jwt-shared` named volume in the
// `lab-tikv` compose profile so multi-replica deployments converge on the
// same HS256 key without operator coordination. Variable (not const) so
// tests can substitute a tempdir.
var jwtSharedSecretFile = "/etc/strata/jwt-shared/secret"

// loadJWTSecret returns the HS256 key used to sign /admin/v1 session cookies
// plus the file path used by the rotate-secret endpoint (US-019).
//
// Resolution order:
//  1. STRATA_CONSOLE_JWT_SECRET (32 bytes hex)            — env wins
//  2. STRATA_JWT_SECRET_FILE (default /etc/strata/jwt-secret) — read at boot
//     so rotated keys persist across restarts
//  3. /etc/strata/jwt-shared/secret — file-based atomic bootstrap shared
//     across replicas via a docker volume; first writer wins per POSIX
//     O_EXCL, losers re-read with backoff
//  4. generate ephemeral 32-byte secret + WARN
//
// The returned file path is what handleRotateJWTSecret writes to; an empty
// string means rotation falls back to adminapi.DefaultJWTSecretFile.
func loadJWTSecret(logger *slog.Logger) ([]byte, string, string) {
	target := os.Getenv("STRATA_JWT_SECRET_FILE")
	if target == "" {
		target = adminapi.DefaultJWTSecretFile
	}
	return loadJWTSecretFrom(os.Getenv("STRATA_CONSOLE_JWT_SECRET"), target, jwtSharedSecretFile, logger)
}

func loadJWTSecretFrom(envSecret, secretFile, sharedFile string, logger *slog.Logger) ([]byte, string, string) {
	if envSecret != "" {
		return adminapi.DecodeSecret(envSecret), "STRATA_CONSOLE_JWT_SECRET", secretFile
	}
	if b, ok := readJWTSecretFile(secretFile, logger); ok {
		return b, "STRATA_JWT_SECRET_FILE", secretFile
	}
	if b, ok := bootstrapSharedJWTSecret(sharedFile, logger); ok {
		return b, "STRATA_JWT_SHARED", secretFile
	}
	b, err := adminapi.GenerateSecret()
	if err != nil {
		logger.Warn("admin: generate jwt secret", "error", err.Error())
		return nil, "ephemeral-error", secretFile
	}
	logger.Warn("admin: STRATA_CONSOLE_JWT_SECRET unset; generated ephemeral 32-byte secret. Sessions invalidate on restart. Set the env explicitly in production.")
	return b, "ephemeral", secretFile
}

// bootstrapSharedJWTSecret implements the file-based atomic bootstrap used
// by the multi-replica lab profile. Fast path: read an existing file. Cold
// path: try O_EXCL write, on success persist a fresh hex-encoded 32-byte
// secret, on EEXIST re-read with up to 3 × 100 ms retries to absorb the
// fsync race window between the writer's create and its WriteString. Any
// other error (parent dir missing, permission denied) returns ok=false so
// the caller falls through to the ephemeral generator.
func bootstrapSharedJWTSecret(path string, logger *slog.Logger) ([]byte, bool) {
	if path == "" {
		return nil, false
	}
	if b, ok := readJWTSecretFile(path, logger); ok {
		return b, true
	}
	secret, err := adminapi.GenerateSecret()
	if err != nil {
		logger.Warn("admin: shared jwt: generate", "path", path, "error", err.Error())
		return nil, false
	}
	encoded := hex.EncodeToString(secret)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case err == nil:
		if _, werr := f.WriteString(encoded); werr != nil {
			_ = f.Close()
			_ = os.Remove(path)
			logger.Warn("admin: shared jwt: write", "path", path, "error", werr.Error())
			return nil, false
		}
		if cerr := f.Close(); cerr != nil {
			logger.Warn("admin: shared jwt: close", "path", path, "error", cerr.Error())
			return nil, false
		}
		return secret, true
	case errors.Is(err, os.ErrExist):
		for range 3 {
			time.Sleep(100 * time.Millisecond)
			if b, ok := readJWTSecretFile(path, logger); ok {
				return b, true
			}
		}
		logger.Warn("admin: shared jwt: lost create race but peer file never readable", "path", path)
		return nil, false
	default:
		logger.Warn("admin: shared jwt: open", "path", path, "error", err.Error())
		return nil, false
	}
}

// readJWTSecretFile reads a hex-encoded secret previously written by
// handleRotateJWTSecret. Missing path returns ok=false (not an error —
// fresh deployments fall through to ephemeral generation).
func readJWTSecretFile(path string, logger *slog.Logger) ([]byte, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("admin: read jwt secret file", "path", path, "error", err.Error())
		}
		return nil, false
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, false
	}
	b := adminapi.DecodeSecret(trimmed)
	if len(b) < 16 {
		logger.Warn("admin: jwt secret file too short; ignoring", "path", path, "len", len(b))
		return nil, false
	}
	return b, true
}

// consoleThemeDefault reads STRATA_CONSOLE_THEME_DEFAULT (system|light|dark);
// empty / invalid resolves to "system".
func consoleThemeDefault() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("STRATA_CONSOLE_THEME_DEFAULT")))
	switch v {
	case "system", "light", "dark":
		return v
	default:
		return "system"
	}
}

func cassandraSettings(cfg *config.Config) adminapi.CassandraSettings {
	if cfg.MetaBackend != "cassandra" {
		return adminapi.CassandraSettings{}
	}
	hosts := append([]string(nil), cfg.Cassandra.Hosts...)
	return adminapi.CassandraSettings{
		Hosts:       hosts,
		Keyspace:    cfg.Cassandra.Keyspace,
		LocalDC:     cfg.Cassandra.LocalDC,
		Replication: cfg.Cassandra.Replication,
		Username:    cfg.Cassandra.Username,
	}
}

func radosSettings(cfg *config.Config) adminapi.RADOSSettings {
	if cfg.DataBackend != "rados" {
		return adminapi.RADOSSettings{}
	}
	return adminapi.RADOSSettings{
		ConfigFile: cfg.RADOS.ConfigFile,
		User:       cfg.RADOS.User,
		Pool:       cfg.RADOS.Pool,
		Namespace:  cfg.RADOS.Namespace,
		Classes:    cfg.RADOS.Classes,
		Clusters:   cfg.RADOS.Clusters,
	}
}

func tikvSettings(cfg *config.Config) adminapi.TiKVSettings {
	if cfg.MetaBackend != "tikv" {
		return adminapi.TiKVSettings{}
	}
	return adminapi.TiKVSettings{Endpoints: parseTiKVEndpoints(cfg.TiKV.Endpoints)}
}

// knownDataClusters returns the set of cluster ids the data backend was
// configured with (STRATA_RADOS_CLUSTERS / STRATA_S3_CLUSTERS). Used by the
// admin placement handler to reject policies that reference unconfigured
// cluster ids (US-001 placement-rebalance). Returns nil when the backend
// has no enumerable cluster set (memory backend) so the handler skips the
// check.
func knownDataClusters(cfg *config.Config) map[string]struct{} {
	switch cfg.DataBackend {
	case "rados":
		clusters, err := datarados.ParseClusters(cfg.RADOS.Clusters)
		if err != nil || len(clusters) == 0 {
			return nil
		}
		out := make(map[string]struct{}, len(clusters))
		for id := range clusters {
			out[id] = struct{}{}
		}
		return out
	case "s3":
		clusters, err := datas3.ParseClusters(cfg.S3.Clusters)
		if err != nil || len(clusters) == 0 {
			return nil
		}
		out := make(map[string]struct{}, len(clusters))
		for id := range clusters {
			out[id] = struct{}{}
		}
		return out
	}
	return nil
}

// clusterBackends returns clusterID → backend label ("rados" / "s3")
// for the GET /admin/v1/clusters response (US-006). Memory backend
// returns nil.
func clusterBackends(cfg *config.Config) map[string]string {
	out := map[string]string{}
	switch cfg.DataBackend {
	case "rados":
		clusters, err := datarados.ParseClusters(cfg.RADOS.Clusters)
		if err != nil {
			return nil
		}
		for id := range clusters {
			out[id] = "rados"
		}
	case "s3":
		clusters, err := datas3.ParseClusters(cfg.S3.Clusters)
		if err != nil {
			return nil
		}
		for id := range clusters {
			out[id] = "s3"
		}
	default:
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func s3BackendSettings(cfg *config.Config) adminapi.S3BackendSettings {
	if cfg.DataBackend != "s3" {
		return adminapi.S3BackendSettings{}
	}
	return adminapi.S3BackendSettings{
		Kind:     "s3",
		Clusters: cfg.S3.Clusters,
		Classes:  cfg.S3.Classes,
	}
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
// as metaStore. The store is per-backend because heartbeat semantics differ
// at the storage layer (Cassandra uses USING TTL; TiKV encodes ExpiresAt in
// the row payload; memory filters at read time). Returns nil only when the
// backend type-assertion fails — admin handlers degrade gracefully.
func buildHeartbeatStore(cfg *config.Config, metaStore meta.Store) heartbeat.Store {
	switch cfg.MetaBackend {
	case "memory":
		return heartbeat.NewMemoryStore()
	case "cassandra":
		if cas, ok := metaStore.(*metacassandra.Store); ok {
			return &heartbeat.CassandraStore{S: cas.Session()}
		}
	case "tikv":
		if ts, ok := metaStore.(*metatikv.Store); ok {
			return metatikv.NewHeartbeatStore(ts)
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
