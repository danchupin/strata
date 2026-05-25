package config

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

type Config struct {
	Listen       string        `koanf:"listen"`
	RegionName   string        `koanf:"region"`
	DataBackend  string        `koanf:"data_backend"`
	MetaBackend  string        `koanf:"meta_backend"`
	ShutdownWait time.Duration `koanf:"shutdown_wait"`

	Cassandra CassandraConfig `koanf:"cassandra"`
	TiKV      TiKVConfig      `koanf:"tikv"`
	RADOS     RADOSConfig     `koanf:"rados"`
	S3        S3Config        `koanf:"s3"`
	Auth      AuthConfig      `koanf:"auth"`
	Workers   WorkersConfig   `koanf:"workers"`

	DefaultBucketShards int `koanf:"default_bucket_shards"`
}

type CassandraConfig struct {
	Hosts       []string      `koanf:"hosts"`
	Keyspace    string        `koanf:"keyspace"`
	LocalDC     string        `koanf:"local_dc"`
	Replication string        `koanf:"replication"`
	Username    string        `koanf:"username"`
	Password    string        `koanf:"password"`
	Timeout     time.Duration `koanf:"timeout"`
}

// TiKVConfig holds connection parameters for the TiKV-backed meta store
// (US-015). Endpoints is a comma-separated PD address list; serverapp
// splits + trims it before dialling. Empty until STRATA_META_BACKEND=tikv
// is in play.
type TiKVConfig struct {
	Endpoints string `koanf:"pd_endpoints"`
}

type RADOSConfig struct {
	ConfigFile string `koanf:"config_file"`
	User       string `koanf:"user"`
	Keyring    string `koanf:"keyring"`
	Pool       string `koanf:"pool"`
	Namespace  string `koanf:"namespace"`
	Classes    string `koanf:"classes"`
	// Clusters lists per-cluster connection specs as a comma-separated
	// "<id>:<conf-path>:<keyring-path>" list. See rados.ParseClusters for
	// format details. Existing single-cluster fields above coexist as the
	// implicit "default" cluster.
	Clusters string `koanf:"clusters"`
}

// S3Config carries the multi-cluster S3 data-backend wiring (US-004).
// Both fields are raw JSON blobs sourced from STRATA_S3_CLUSTERS (a JSON
// array of S3ClusterSpec) and STRATA_S3_CLASSES (a JSON object of
// ClassSpec). Parsed + validated by `internal/data/s3.ParseClusters` /
// `ParseClasses`; cross-validated (every class.Cluster references a known
// cluster) by `s3.New`. Both required when DataBackend=s3.
type S3Config struct {
	Clusters string `koanf:"clusters"`
	Classes  string `koanf:"classes"`
}

type AuthConfig struct {
	Mode              string `koanf:"mode"`
	StaticCredentials string `koanf:"static_credentials"`
}

// WorkersConfig collects every worker knob under a single nested TOML
// section. Enabled mirrors STRATA_WORKERS (comma-separated worker names);
// each substruct collects per-worker tunables.
type WorkersConfig struct {
	Enabled          string                 `koanf:"enabled"`
	GC               WorkerGCConfig         `koanf:"gc"`
	Lifecycle        WorkerLifecycleConfig  `koanf:"lifecycle"`
	Rebalance        RebalanceConfig        `koanf:"rebalance"`
	UsageRollup      UsageRollupConfig      `koanf:"usage_rollup"`
	ManifestRewriter ManifestRewriterConfig `koanf:"manifest_rewriter"`
	AuditExport      AuditExportConfig      `koanf:"audit_export"`
	QuotaReconcile   QuotaReconcileConfig   `koanf:"quota_reconcile"`
	Notify           NotifyConfig           `koanf:"notify"`
	Replicator       ReplicatorConfig       `koanf:"replicator"`
	AccessLog        AccessLogConfig        `koanf:"access_log"`
	Inventory        InventoryConfig        `koanf:"inventory"`
}

type WorkerGCConfig struct {
	Interval      time.Duration `koanf:"interval"`
	Grace         time.Duration `koanf:"grace"`
	BatchSize     int           `koanf:"batch_size"`
	Concurrency   int           `koanf:"concurrency"`
	Shards        int           `koanf:"shards"`
	DualWrite     bool          `koanf:"dual_write"`
	MetricsListen string        `koanf:"metrics_listen"`
}

type WorkerLifecycleConfig struct {
	Interval      time.Duration `koanf:"interval"`
	Unit          string        `koanf:"unit"`
	Concurrency   int           `koanf:"concurrency"`
	MetricsListen string        `koanf:"metrics_listen"`
}

type RebalanceConfig struct {
	Interval   time.Duration `koanf:"interval"`
	RateMBPerS int           `koanf:"rate_mb_s"`
	Inflight   int           `koanf:"inflight"`
	Shards     int           `koanf:"shards"`
}

type UsageRollupConfig struct {
	At            string        `koanf:"at"`
	Interval      time.Duration `koanf:"interval"`
	SamplesPerDay int           `koanf:"samples_per_day"`
}

type ManifestRewriterConfig struct {
	Interval   time.Duration `koanf:"interval"`
	BatchLimit int           `koanf:"batch_limit"`
	DryRun     bool          `koanf:"dry_run"`
}

type AuditExportConfig struct {
	Bucket   string        `koanf:"bucket"`
	Prefix   string        `koanf:"prefix"`
	After    time.Duration `koanf:"after"`
	Interval time.Duration `koanf:"interval"`
}

type QuotaReconcileConfig struct {
	Interval time.Duration `koanf:"interval"`
}

type NotifyConfig struct {
	Targets     string        `koanf:"targets"`
	Interval    time.Duration `koanf:"interval"`
	MaxRetries  int           `koanf:"max_retries"`
	BackoffBase time.Duration `koanf:"backoff_base"`
	PollLimit   int           `koanf:"poll_limit"`
}

type ReplicatorConfig struct {
	Interval    time.Duration `koanf:"interval"`
	MaxRetries  int           `koanf:"max_retries"`
	BackoffBase time.Duration `koanf:"backoff_base"`
	PollLimit   int           `koanf:"poll_limit"`
	HTTPTimeout time.Duration `koanf:"http_timeout"`
	PeerScheme  string        `koanf:"peer_scheme"`
}

type AccessLogConfig struct {
	Interval      time.Duration `koanf:"interval"`
	MaxFlushBytes int64         `koanf:"max_flush_bytes"`
	PollLimit     int           `koanf:"poll_limit"`
}

type InventoryConfig struct {
	Interval time.Duration `koanf:"interval"`
	Region   string        `koanf:"region"`
}

func defaults() Config {
	return Config{
		Listen:              ":9000",
		RegionName:          "strata-local",
		DataBackend:         "memory",
		MetaBackend:         "memory",
		ShutdownWait:        10 * time.Second,
		DefaultBucketShards: 64,
		Cassandra: CassandraConfig{
			Hosts:       []string{"127.0.0.1"},
			Keyspace:    "strata",
			LocalDC:     "datacenter1",
			Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
			Timeout:     10 * time.Second,
		},
		RADOS: RADOSConfig{
			ConfigFile: "/etc/ceph/ceph.conf",
			User:       "admin",
			Pool:       "strata.rgw.buckets.data",
		},
		Auth: AuthConfig{Mode: "off"},
		Workers: WorkersConfig{
			GC: WorkerGCConfig{
				Interval:      30 * time.Second,
				Grace:         5 * time.Minute,
				BatchSize:     0,
				Concurrency:   1,
				Shards:        1,
				DualWrite:     false,
				MetricsListen: ":9100",
			},
			Lifecycle: WorkerLifecycleConfig{
				Interval:      60 * time.Second,
				Unit:          "day",
				Concurrency:   1,
				MetricsListen: ":9101",
			},
			Rebalance: RebalanceConfig{
				Interval:   5 * time.Minute,
				RateMBPerS: 100,
				Inflight:   4,
				Shards:     1,
			},
			UsageRollup: UsageRollupConfig{
				At:            "00:00",
				Interval:      24 * time.Hour,
				SamplesPerDay: 24,
			},
			ManifestRewriter: ManifestRewriterConfig{
				Interval:   24 * time.Hour,
				BatchLimit: 500,
				DryRun:     false,
			},
			AuditExport: AuditExportConfig{
				After:    30 * 24 * time.Hour,
				Interval: 24 * time.Hour,
			},
			QuotaReconcile: QuotaReconcileConfig{
				Interval: 6 * time.Hour,
			},
			Notify: NotifyConfig{
				Interval:    5 * time.Second,
				MaxRetries:  6,
				BackoffBase: 1 * time.Second,
				PollLimit:   100,
			},
			Replicator: ReplicatorConfig{
				Interval:    5 * time.Second,
				MaxRetries:  6,
				BackoffBase: 1 * time.Second,
				PollLimit:   100,
				HTTPTimeout: 30 * time.Second,
				PeerScheme:  "https",
			},
			AccessLog: AccessLogConfig{
				Interval:      5 * time.Minute,
				MaxFlushBytes: 5 * 1024 * 1024,
				PollLimit:     10000,
			},
			Inventory: InventoryConfig{
				Interval: 5 * time.Minute,
			},
		},
	}
}

// envMap declares the explicit mapping from environment variables to koanf paths.
// Keeping it explicit avoids surprises from auto-mangling underscores into dots.
var envMap = map[string]string{
	"STRATA_LISTEN":                          "listen",
	"STRATA_REGION":                          "region",
	"STRATA_DATA_BACKEND":                    "data_backend",
	"STRATA_META_BACKEND":                    "meta_backend",
	"STRATA_BUCKET_SHARDS":                   "default_bucket_shards",
	"STRATA_SHUTDOWN_WAIT":                   "shutdown_wait",
	"STRATA_CASSANDRA_HOSTS":                 "cassandra.hosts",
	"STRATA_CASSANDRA_KEYSPACE":              "cassandra.keyspace",
	"STRATA_CASSANDRA_DC":                    "cassandra.local_dc",
	"STRATA_CASSANDRA_REPLICATION":           "cassandra.replication",
	"STRATA_CASSANDRA_USER":                  "cassandra.username",
	"STRATA_CASSANDRA_PASSWORD":              "cassandra.password",
	"STRATA_CASSANDRA_TIMEOUT":               "cassandra.timeout",
	"STRATA_TIKV_PD_ENDPOINTS":               "tikv.pd_endpoints",
	"STRATA_RADOS_CONF":                      "rados.config_file",
	"STRATA_RADOS_USER":                      "rados.user",
	"STRATA_RADOS_KEYRING":                   "rados.keyring",
	"STRATA_RADOS_POOL":                      "rados.pool",
	"STRATA_RADOS_NAMESPACE":                 "rados.namespace",
	"STRATA_RADOS_CLASSES":                   "rados.classes",
	"STRATA_RADOS_CLUSTERS":                  "rados.clusters",
	"STRATA_S3_CLUSTERS":                     "s3.clusters",
	"STRATA_S3_CLASSES":                      "s3.classes",
	"STRATA_AUTH_MODE":                       "auth.mode",
	"STRATA_STATIC_CREDENTIALS":              "auth.static_credentials",
	"STRATA_WORKERS":                         "workers.enabled",
	"STRATA_GC_INTERVAL":                     "workers.gc.interval",
	"STRATA_GC_GRACE":                        "workers.gc.grace",
	"STRATA_GC_BATCH_SIZE":                   "workers.gc.batch_size",
	"STRATA_GC_CONCURRENCY":                  "workers.gc.concurrency",
	"STRATA_GC_SHARDS":                       "workers.gc.shards",
	"STRATA_GC_DUAL_WRITE":                   "workers.gc.dual_write",
	"STRATA_GC_METRICS_LISTEN":               "workers.gc.metrics_listen",
	"STRATA_LIFECYCLE_INTERVAL":              "workers.lifecycle.interval",
	"STRATA_LIFECYCLE_UNIT":                  "workers.lifecycle.unit",
	"STRATA_LIFECYCLE_CONCURRENCY":           "workers.lifecycle.concurrency",
	"STRATA_LIFECYCLE_METRICS_LISTEN":        "workers.lifecycle.metrics_listen",
	"STRATA_REBALANCE_INTERVAL":              "workers.rebalance.interval",
	"STRATA_REBALANCE_RATE_MB_S":             "workers.rebalance.rate_mb_s",
	"STRATA_REBALANCE_INFLIGHT":              "workers.rebalance.inflight",
	"STRATA_REBALANCE_SHARDS":                "workers.rebalance.shards",
	"STRATA_USAGE_ROLLUP_AT":                 "workers.usage_rollup.at",
	"STRATA_USAGE_ROLLUP_INTERVAL":           "workers.usage_rollup.interval",
	"STRATA_USAGE_ROLLUP_SAMPLES_PER_DAY":    "workers.usage_rollup.samples_per_day",
	"STRATA_MANIFEST_REWRITER_INTERVAL":      "workers.manifest_rewriter.interval",
	"STRATA_MANIFEST_REWRITER_BATCH_LIMIT":   "workers.manifest_rewriter.batch_limit",
	"STRATA_MANIFEST_REWRITER_DRY_RUN":       "workers.manifest_rewriter.dry_run",
	"STRATA_AUDIT_EXPORT_BUCKET":             "workers.audit_export.bucket",
	"STRATA_AUDIT_EXPORT_PREFIX":             "workers.audit_export.prefix",
	"STRATA_AUDIT_EXPORT_AFTER":              "workers.audit_export.after",
	"STRATA_AUDIT_EXPORT_INTERVAL":           "workers.audit_export.interval",
	"STRATA_QUOTA_RECONCILE_INTERVAL":        "workers.quota_reconcile.interval",
	"STRATA_NOTIFY_TARGETS":                  "workers.notify.targets",
	"STRATA_NOTIFY_INTERVAL":                 "workers.notify.interval",
	"STRATA_NOTIFY_MAX_RETRIES":              "workers.notify.max_retries",
	"STRATA_NOTIFY_BACKOFF_BASE":             "workers.notify.backoff_base",
	"STRATA_NOTIFY_POLL_LIMIT":               "workers.notify.poll_limit",
	"STRATA_REPLICATOR_INTERVAL":             "workers.replicator.interval",
	"STRATA_REPLICATOR_MAX_RETRIES":          "workers.replicator.max_retries",
	"STRATA_REPLICATOR_BACKOFF_BASE":         "workers.replicator.backoff_base",
	"STRATA_REPLICATOR_POLL_LIMIT":           "workers.replicator.poll_limit",
	"STRATA_REPLICATOR_HTTP_TIMEOUT":         "workers.replicator.http_timeout",
	"STRATA_REPLICATOR_PEER_SCHEME":          "workers.replicator.peer_scheme",
	"STRATA_ACCESS_LOG_INTERVAL":             "workers.access_log.interval",
	"STRATA_ACCESS_LOG_MAX_FLUSH_BYTES":      "workers.access_log.max_flush_bytes",
	"STRATA_ACCESS_LOG_POLL_LIMIT":           "workers.access_log.poll_limit",
	"STRATA_INVENTORY_INTERVAL":              "workers.inventory.interval",
	"STRATA_INVENTORY_REGION":                "workers.inventory.region",
}

func Load() (*Config, error) {
	k := koanf.New(".")
	cfg := defaults()

	if err := k.Load(structs.Provider(cfg, "koanf"), nil); err != nil {
		return nil, fmt.Errorf("config defaults: %w", err)
	}

	if path := os.Getenv("STRATA_CONFIG_FILE"); path != "" {
		if err := k.Load(file.Provider(path), toml.Parser()); err != nil {
			return nil, fmt.Errorf("config file %s: %w", path, err)
		}
	}

	if err := k.Load(env.ProviderWithValue("STRATA_", ".", func(s, v string) (string, any) {
		if v == "" {
			return "", nil
		}
		return envMap[s], v
	}), nil); err != nil {
		return nil, fmt.Errorf("config env: %w", err)
	}

	var out Config
	if err := k.Unmarshal("", &out); err != nil {
		return nil, fmt.Errorf("config unmarshal: %w", err)
	}

	if err := out.validate(); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Config) validate() error {
	switch c.DataBackend {
	case "memory", "rados":
	case "s3":
		if c.S3.Clusters == "" {
			return fmt.Errorf("STRATA_S3_CLUSTERS is required when data_backend=s3")
		}
		if c.S3.Classes == "" {
			return fmt.Errorf("STRATA_S3_CLASSES is required when data_backend=s3")
		}
	default:
		return fmt.Errorf("data_backend %q is not one of {memory, rados, s3}", c.DataBackend)
	}
	switch c.MetaBackend {
	case "memory", "cassandra":
	case "tikv":
		if c.TiKV.Endpoints == "" {
			return fmt.Errorf("meta_backend=tikv requires STRATA_TIKV_PD_ENDPOINTS (or [tikv].pd_endpoints) to be set")
		}
	default:
		return fmt.Errorf("meta_backend %q is not one of {memory, cassandra, tikv}", c.MetaBackend)
	}
	switch c.Auth.Mode {
	case "", "off", "disabled", "required", "optional":
	default:
		return fmt.Errorf("auth.mode %q is not one of {off, disabled, required, optional}", c.Auth.Mode)
	}
	if c.DefaultBucketShards <= 0 {
		return fmt.Errorf("default_bucket_shards must be positive (got %d)", c.DefaultBucketShards)
	}
	c.clampWorkers()
	warnLegacyDrainStrict()
	return nil
}

// clampWorkers enforces the same numeric ranges that the legacy per-worker
// env-read helpers used, so TOML-loaded values get the same clamp + WARN
// log as STRATA_*-loaded values. Mirrors the per-worker constructor
// invariants documented in CLAUDE.md.
func (c *Config) clampWorkers() {
	c.Workers.GC.Concurrency = clampInt("workers.gc.concurrency", c.Workers.GC.Concurrency, 1, 256)
	c.Workers.GC.Shards = clampInt("workers.gc.shards", c.Workers.GC.Shards, 1, 1024)
	c.Workers.Lifecycle.Concurrency = clampInt("workers.lifecycle.concurrency", c.Workers.Lifecycle.Concurrency, 1, 256)
	c.Workers.Rebalance.Interval = clampDuration("workers.rebalance.interval", c.Workers.Rebalance.Interval, time.Minute, 24*time.Hour)
	c.Workers.Rebalance.RateMBPerS = clampInt("workers.rebalance.rate_mb_s", c.Workers.Rebalance.RateMBPerS, 1, 10000)
	c.Workers.Rebalance.Inflight = clampInt("workers.rebalance.inflight", c.Workers.Rebalance.Inflight, 1, 64)
	c.Workers.Rebalance.Shards = clampInt("workers.rebalance.shards", c.Workers.Rebalance.Shards, 1, 1024)
}

func clampInt(key string, v, lo, hi int) int {
	if v < lo {
		slog.Warn("clamping config value", "key", key, "value", v, "min", lo)
		return lo
	}
	if v > hi {
		slog.Warn("clamping config value", "key", key, "value", v, "max", hi)
		return hi
	}
	return v
}

func clampDuration(key string, v, lo, hi time.Duration) time.Duration {
	if v < lo {
		slog.Warn("clamping config value", "key", key, "value", v.String(), "min", lo.String())
		return lo
	}
	if v > hi {
		slog.Warn("clamping config value", "key", key, "value", v.String(), "max", hi.String())
		return hi
	}
	return v
}

// warnLegacyDrainStrict logs a WARN when the retired STRATA_DRAIN_STRICT
// env is still set in the environment (US-007 drain-transparency).
// Drain is now unconditionally strict, so the env value is ignored —
// the WARN nudges operators to remove it from their deploy descriptors.
func warnLegacyDrainStrict() {
	if v := os.Getenv("STRATA_DRAIN_STRICT"); v != "" {
		slog.Warn("STRATA_DRAIN_STRICT is retired and ignored — drain is unconditionally strict (US-007 drain-transparency)",
			"legacy_value", v)
	}
}
