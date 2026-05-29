package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"

	"github.com/danchupin/strata/internal/trustedproxies"
)

type Config struct {
	Listen       string        `koanf:"listen"`
	RegionName   string        `koanf:"region"`
	DataBackend  string        `koanf:"data_backend"`
	MetaBackend  string        `koanf:"meta_backend"`
	ShutdownWait time.Duration `koanf:"shutdown_wait"`

	HTTP        HTTPConfig        `koanf:"http"`
	TLS         TLSConfig         `koanf:"tls"`
	AdminListen AdminListenConfig `koanf:"admin_listen"`
	RateLimit   RateLimitConfig   `koanf:"rate_limit"`
	Cassandra   CassandraConfig   `koanf:"cassandra"`
	TiKV        TiKVConfig        `koanf:"tikv"`
	RADOS       RADOSConfig       `koanf:"rados"`
	S3          S3Config          `koanf:"s3"`
	Auth        AuthConfig        `koanf:"auth"`
	KMS         KMSConfig         `koanf:"kms"`
	Workers     WorkersConfig     `koanf:"workers"`
	OTel        OTelConfig        `koanf:"otel"`
	Logging     LoggingConfig     `koanf:"logging"`
	AuditLog    AuditLogConfig    `koanf:"audit_log"`
	BucketStats BucketStatsConfig `koanf:"bucket_stats"`
	Cluster     ClusterConfig     `koanf:"cluster"`
	Console     ConsoleConfig     `koanf:"console"`
	JWT         JWTConfig         `koanf:"jwt"`
	Manifest    ManifestConfig    `koanf:"manifest"`
	MFA         MFAConfig         `koanf:"mfa"`
	Node        NodeConfig        `koanf:"node"`
	Pprof       PprofConfig       `koanf:"pprof"`
	Prometheus  PrometheusConfig  `koanf:"prometheus"`
	SSE         SSEConfig         `koanf:"sse"`
	VHost       VHostConfig       `koanf:"vhost"`

	TrustedProxies string `koanf:"trusted_proxies"`

	DefaultBucketShards int `koanf:"default_bucket_shards"`
}

// HTTPConfig carries per-connection timeout knobs applied to the gateway
// listener (US-001 harden-gateway). Defaults are slowloris-safe:
// ReadHeaderTimeout=10s, ReadTimeout=60s, WriteTimeout=30m (5 GiB ÷ 30m ≈
// 2.8 MB/s minimum throughput — cellular safe), IdleTimeout=120s,
// MaxHeaderBytes=1 MiB. A zero value is honored by net/http as "disabled".
type HTTPConfig struct {
	ReadHeaderTimeout time.Duration `koanf:"read_header_timeout"`
	ReadTimeout       time.Duration `koanf:"read_timeout"`
	WriteTimeout      time.Duration `koanf:"write_timeout"`
	IdleTimeout       time.Duration `koanf:"idle_timeout"`
	MaxHeaderBytes    int           `koanf:"max_header_bytes"`
}

// AdminListenConfig wires the optional admin/console/metrics/healthz listener
// onto a separate port (US-008 harden-gateway). Empty Listen → backwards-
// compat single-port shape preserved (S3 + admin share cfg.Listen). Set Listen
// to e.g. "127.0.0.1:9001" to bind admin endpoints to loopback or RFC1918
// while keeping the S3 surface on cfg.Listen.
//
// When split, the admin listener owns: /admin/v1/, /console/, /metrics,
// /healthz, /readyz. The S3 catch-all (/*) stays on the primary listener.
//
// HTTP timeouts apply to the admin listener only; defaults match the main
// HTTP family except WriteTimeout=2m (no large multipart on admin).
//
// TLS is optional: empty CertFile/KeyFile → plain HTTP (typical loopback
// shape). When set, the listener terminates TLS. ClientCAFile flips to
// RequireAndVerifyClientCert (mTLS); empty leaves client auth disabled.
type AdminListenConfig struct {
	Listen string         `koanf:"listen"`
	HTTP   HTTPConfig     `koanf:"http"`
	TLS    AdminTLSConfig `koanf:"tls"`
}

// AdminTLSConfig wires the optional admin-listener TLS terminator (US-008).
// Empty CertFile + KeyFile → plain HTTP on the admin listener. When set, the
// admin listener uses a fresh tls.Config built from these PEMs (no SNI / no
// hot-reload — kept simple). ClientCAFile (PEM) enables mTLS via
// RequireAndVerifyClientCert.
type AdminTLSConfig struct {
	CertFile     string `koanf:"cert_file"`
	KeyFile      string `koanf:"key_file"`
	ClientCAFile string `koanf:"client_ca_file"`
}

// TLSConfig wires the built-in TLS listener (US-002 + US-003
// harden-gateway). Empty CertFile + KeyFile + CertDir → plain HTTP.
// MinVersion ∈ {"", "TLS1.2", "TLS1.3"}; default "TLS1.2". CipherProfile ∈
// {"", "mozilla-modern", "mozilla-intermediate", "go-default"}; default
// "mozilla-modern". Profile is informational only when MinVersion=TLS1.3
// (Go's tls package picks TLS 1.3 ciphers regardless per RFC 8446).
//
// CertDir is mutually exclusive with CertFile/KeyFile. When set, the
// directory is walked for *.crt + matching *.key pairs and an SNI-driven
// GetCertificate callback dispatches per-handshake. Cert store is backed
// by an atomic.Pointer so hot-reload via fsnotify (parent-dir watch for
// k8s symlink-swap semantics) plus periodic reconciliation never blocks
// the read path. ReloadInterval controls the periodic reconciler fallback
// (range [10s, 1h]; 0 disables).
//
// ClientCAFile (optional, PEM) enables RequireAndVerifyClientCert for the
// gateway listener — every TLS handshake must present a cert signed by
// the configured CA.
type TLSConfig struct {
	CertFile       string        `koanf:"cert_file"`
	KeyFile        string        `koanf:"key_file"`
	MinVersion     string        `koanf:"min_version"`
	CipherProfile  string        `koanf:"cipher_profile"`
	CertDir        string        `koanf:"cert_dir"`
	ClientCAFile   string        `koanf:"client_ca_file"`
	ReloadInterval time.Duration `koanf:"reload_interval"`
}

// RateLimitConfig wires the per-IP + per-key ingress rate limiter (US-009
// harden-gateway). Both layers default off (0 = disabled) — opt-in to
// preserve backwards-compat. Limits apply to the S3 hot path only; admin /
// console / metrics / healthz / readyz bypass.
//
// PerKey limits requests per `auth.AuthInfo.AccessKey`; empty access key
// (anonymous mode) skips this layer. PerIP limits by remote IP, resolved
// via `trustedproxies.ClientIP` when a trusted proxy CIDR matches.
//
// Burst is the token-bucket burst capacity (peak above the sustained
// rate). Zero → max(PerKey, PerIP, 1) × 2 (≈ 2× the larger limit) so a
// short spike absorbs cleanly. CacheSize bounds the LRU of per-(key|IP)
// limiters; eviction grants the evicted client a fresh bucket on next
// hit (conservative).
type RateLimitConfig struct {
	PerKey    int `koanf:"per_key"`
	PerIP     int `koanf:"per_ip"`
	Burst     int `koanf:"burst"`
	CacheSize int `koanf:"cache_size"`
}

type CassandraConfig struct {
	Hosts       []string      `koanf:"hosts"`
	Keyspace    string        `koanf:"keyspace"`
	LocalDC     string        `koanf:"local_dc"`
	Replication string        `koanf:"replication"`
	Username    string        `koanf:"username"`
	Password    string        `koanf:"password"`
	Timeout     time.Duration `koanf:"timeout"`
	// SlowMS is the WARN threshold (in milliseconds) applied by the
	// gocql QueryObserver. 0 disables; unset falls back to the
	// observer's DefaultSlowQueryMS (100ms). Wired via STRATA_CASSANDRA_SLOW_MS.
	SlowMS int                `koanf:"slow_ms"`
	TLS    CassandraTLSConfig `koanf:"tls"`
}

// CassandraTLSConfig wires gocql.SslOptions for the Cassandra meta backend
// (US-004 harden-gateway). Empty CAFile + CertFile + KeyFile → plain-TCP
// (no SslOptions on the cluster config; backwards-compat preserved).
//
// CAFile (optional, PEM) populates a root pool used to verify the server
// certificate. CertFile + KeyFile (PEM, both or neither) supply the client
// certificate for mutual TLS. SkipVerify=true sets tls.Config.InsecureSkipVerify
// and disables gocql's EnableHostVerification — operators get a single
// WARN at boot plus a `strata_backend_tls_skip_verify{backend="cassandra"}=1`
// gauge bump so dashboards can flag the unsafe shape.
type CassandraTLSConfig struct {
	CAFile     string `koanf:"ca_file"`
	CertFile   string `koanf:"cert_file"`
	KeyFile    string `koanf:"key_file"`
	SkipVerify bool   `koanf:"skip_verify"`
}

// TiKVConfig holds connection parameters for the TiKV-backed meta store
// (US-015). Endpoints is a comma-separated PD address list; serverapp
// splits + trims it before dialling. Empty until STRATA_META_BACKEND=tikv
// is in play.
type TiKVConfig struct {
	Endpoints string        `koanf:"pd_endpoints"`
	TLS       TiKVTLSConfig `koanf:"tls"`
}

// TiKVTLSConfig wires tikv-client-go config.Security for the TiKV meta
// backend (US-005 harden-gateway). Empty CAFile + CertFile + KeyFile →
// plain-gRPC = current backwards-compat behavior. SkipVerify=true logs a
// single WARN at boot + bumps strata_backend_tls_skip_verify{backend="tikv"}=1.
//
// Note: tikv-client-go's Security.ToTLSConfig() requires CAFile to enable
// TLS; the serverapp layer routes SkipVerify-only configs through a custom
// dial path. CertFile + KeyFile must come as a pair.
type TiKVTLSConfig struct {
	CAFile     string `koanf:"ca_file"`
	CertFile   string `koanf:"cert_file"`
	KeyFile    string `koanf:"key_file"`
	SkipVerify bool   `koanf:"skip_verify"`
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
	// HealthOID is the canary OID stat'd by /readyz against RADOS. Empty
	// falls back to the cephimpl default ("strata-readyz-canary"). Wired
	// via STRATA_RADOS_HEALTH_OID.
	HealthOID string `koanf:"health_oid"`
	// PoolSize is the per-cluster connection-pool depth. 0 → cephimpl
	// reads STRATA_RADOS_POOL_SIZE (default 1). Range [1, 32].
	PoolSize int `koanf:"pool_size"`
	// PutConcurrency caps the per-PutChunks worker fan-out. 0 → cephimpl
	// reads STRATA_RADOS_PUT_CONCURRENCY (default 32). Range [1, 256].
	PutConcurrency int `koanf:"put_concurrency"`
	// GetPrefetch caps the per-GetChunks in-flight read prefetch. 0 →
	// cephimpl reads STRATA_RADOS_GET_PREFETCH (default 4). Range [1, 64].
	GetPrefetch int `koanf:"get_prefetch"`
	// BatchOps toggles the WriteOp/ReadOp batched helpers. Default off.
	// Wired via STRATA_RADOS_BATCH_OPS.
	BatchOps bool `koanf:"batch_ops"`
}

// S3Config carries the multi-cluster S3 data-backend wiring (US-004).
// Both fields are raw JSON blobs sourced from STRATA_S3_CLUSTERS (a JSON
// array of S3ClusterSpec) and STRATA_S3_CLASSES (a JSON object of
// ClassSpec). Parsed + validated by `internal/data/s3.ParseClusters` /
// `ParseClasses`; cross-validated (every class.Cluster references a known
// cluster) by `s3.New`. Both required when DataBackend=s3.
//
// TLS carries the global default mTLS bundle applied to every S3-upstream
// SDK client unless a per-cluster `tls` block in STRATA_S3_CLUSTERS overrides
// it. Per-cluster override replaces the global block ENTIRELY for that
// cluster — no merge, to avoid surprise semantics when one knob is omitted.
type S3Config struct {
	Clusters string      `koanf:"clusters"`
	Classes  string      `koanf:"classes"`
	TLS      S3TLSConfig `koanf:"tls"`
}

// S3TLSConfig wires net/http.Transport TLS for the S3-upstream data backend
// (US-006 harden-gateway). Empty CAFile + CertFile + KeyFile → plain HTTP
// (or TLS without client cert + system roots — Go default). SkipVerify=true
// logs a single WARN at boot + bumps
// strata_backend_tls_skip_verify{backend="s3",cluster=<id>}=1.
//
// Per-cluster overrides via the `tls` field on each S3ClusterSpec JSON
// entry win outright when set; this global block is the fallback for every
// cluster that has no per-cluster TLS shape.
type S3TLSConfig struct {
	CAFile     string `koanf:"ca_file"`
	CertFile   string `koanf:"cert_file"`
	KeyFile    string `koanf:"key_file"`
	SkipVerify bool   `koanf:"skip_verify"`
}

type AuthConfig struct {
	Mode              string        `koanf:"mode"`
	StaticCredentials string        `koanf:"static_credentials"`
	STSDuration       time.Duration `koanf:"sts_duration"`
	KeyMaxAge         time.Duration `koanf:"key_max_age"`
}

// KMSConfig collects every SSE-KMS knob under a single nested TOML section.
// Adapter selects the provider explicitly ("vault"|"aws"|"local_hsm");
// empty falls back to the FromEnv precedence (vault > aws > local_hsm).
// DEKCacheTTL drives the wall-clock TTL on the auth-side DEK cache; range
// [30s, 1h] (clamped in validate). DefaultKeyID feeds the admin
// SigningKeyConfig as the default CMK id when the operator does not pass
// one per Rotate call.
type KMSConfig struct {
	Adapter      string           `koanf:"adapter"`
	DEKCacheTTL  time.Duration    `koanf:"dek_cache_ttl"`
	DefaultKeyID string           `koanf:"default_key_id"`
	AWS          KMSAWSConfig     `koanf:"aws"`
	Vault        KMSVaultConfig   `koanf:"vault"`
	LocalHSM     KMSLocalHSMConfig `koanf:"local_hsm"`
}

type KMSAWSConfig struct {
	Region   string `koanf:"region"`
	Endpoint string `koanf:"endpoint"`
	RoleARN  string `koanf:"role_arn"`
}

type KMSVaultConfig struct {
	Address  string `koanf:"address"`
	Mount    string `koanf:"mount"`
	Token    string `koanf:"token"`
	RoleID   string `koanf:"role_id"`
	SecretID string `koanf:"secret_id"`
}

type KMSLocalHSMConfig struct {
	Seed string `koanf:"seed"`
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

// OTelConfig collects the OpenTelemetry tracing knobs. Endpoint defaults
// to the W3C-spec OTEL_EXPORTER_OTLP_ENDPOINT env var (read at Load time
// when STRATA_OTEL_EXPORTER_ENDPOINT is unset). Empty Endpoint + Ringbuf
// false yields a no-op tracer provider. SampleRatio drives tail-based
// sampling — failing spans are exported regardless.
type OTelConfig struct {
	Endpoint     string  `koanf:"endpoint"`
	SampleRatio  float64 `koanf:"sample_ratio"`
	Ringbuf      bool    `koanf:"ringbuf"`
	RingbufBytes int     `koanf:"ringbuf_bytes"`
}

// LoggingConfig drives the gateway slog setup. Level ∈ {DEBUG,INFO,WARN,
// ERROR}; Format ∈ {json,text} — only json is supported today, the field
// exists for forward-compat with a text handler.
type LoggingConfig struct {
	Level  string `koanf:"level"`
	Format string `koanf:"format"`
}

// AuditLogConfig carries audit_log table TTL knobs. Retention is the row
// TTL applied to every audit row written via s3api.AuditMiddleware.
// Accepts standard Go durations.
type AuditLogConfig struct {
	Retention time.Duration `koanf:"retention"`
}

// BucketStatsConfig drives the per-process bucket-stats sampler. Interval
// 0 falls back to the sampler's internal default (1h); TopN 0 falls back
// to bucketstats.DefaultTopN. Wired via STRATA_BUCKETSTATS_INTERVAL /
// STRATA_BUCKETSTATS_TOPN (legacy env names without underscore between
// "bucket" and "stats" — TOML key remains [bucket_stats]).
type BucketStatsConfig struct {
	Interval time.Duration `koanf:"interval"`
	TopN     int           `koanf:"top_n"`
}

// ClusterConfig collects single-knob cluster-identity fields. Name feeds
// the /admin/v1/cluster/status response. Wired via STRATA_CLUSTER_NAME.
type ClusterConfig struct {
	Name string `koanf:"name"`
}

// ConsoleConfig carries the admin-console knobs the gateway consumes at
// boot. JWTSecret is the hex-encoded HS256 key for session cookies (env
// wins via STRATA_CONSOLE_JWT_SECRET); ThemeDefault picks the console UI
// default theme.
type ConsoleConfig struct {
	JWTSecret    string `koanf:"jwt_secret"`
	ThemeDefault string `koanf:"theme_default"`
}

// JWTConfig collects JWT-secret persistence knobs. SecretFile is the
// on-disk path read at boot and written by handleRotateJWTSecret;
// SharedFile is the multi-replica bootstrap file (mounted from a docker
// volume in the lab compose profile).
type JWTConfig struct {
	SecretFile string `koanf:"secret_file"`
	SharedFile string `koanf:"shared_file"`
}

// ManifestConfig drives the data.Manifest blob encoder. Format ∈
// {"proto","json"}; default proto. Wired via STRATA_MANIFEST_FORMAT.
type ManifestConfig struct {
	Format string `koanf:"format"`
}

// MFAConfig wires multi-factor delete secrets. Secrets is the raw
// STRATA_MFA_SECRETS spec (see s3api.ParseMFASecrets).
type MFAConfig struct {
	Secrets string `koanf:"secrets"`
}

// NodeConfig pins the heartbeat node identifier. Empty falls back to the
// OS hostname via heartbeat.DefaultNodeID().
type NodeConfig struct {
	ID string `koanf:"id"`
}

// PprofConfig wires the opt-in pprof endpoint (US-004 prod-observability).
// Enabled=false (default) → /debug/pprof/* never registered on any mux.
// Enabled=true + Listen empty → handlers attach to the admin listener
// (cfg.AdminListen.Listen must be set; boot fails otherwise — pprof MUST
// NOT share the S3 hot path). Listen non-empty → dedicated pprof listener
// (e.g. "127.0.0.1:9002"). BlockRate / MutexRate drive
// runtime.SetBlockProfileRate / SetMutexProfileFraction; 0 leaves them
// disabled (block + mutex profiles return empty without the rates).
type PprofConfig struct {
	Enabled   bool   `koanf:"enabled"`
	Listen    string `koanf:"listen"`
	BlockRate int    `koanf:"block_rate"`
	MutexRate int    `koanf:"mutex_rate"`
}

// PrometheusConfig points the admin API at an upstream PromQL endpoint
// for the metrics-aware admin handlers. Empty disables (admin handlers
// degrade with metrics_available=false).
type PrometheusConfig struct {
	URL string `koanf:"url"`
}

// SSEConfig collects the SSE-S3 master-key sourcing knobs. Precedence
// (highest first): Keys (rotation list) > KeyVault > KeyFile > Key.
// See internal/crypto/master.FromConfig for resolution.
type SSEConfig struct {
	MasterKey      string `koanf:"master_key"`
	MasterKeyID    string `koanf:"master_key_id"`
	MasterKeyFile  string `koanf:"master_key_file"`
	MasterKeyVault string `koanf:"master_key_vault"`
	MasterKeys     string `koanf:"master_keys"`
}

// VHostConfig pins the virtual-hosted-style S3 host suffixes. Pattern is
// a comma-separated list of "*.<suffix>" entries; "-" disables vhost
// extraction. Wired via STRATA_VHOST_PATTERN.
type VHostConfig struct {
	Pattern string `koanf:"pattern"`
}

func defaults() Config {
	return Config{
		Listen:              ":9000",
		RegionName:          "strata-local",
		DataBackend:         "memory",
		MetaBackend:         "memory",
		ShutdownWait:        10 * time.Second,
		DefaultBucketShards: 64,
		HTTP: HTTPConfig{
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      30 * time.Minute,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20,
		},
		TLS: TLSConfig{
			MinVersion:     "TLS1.2",
			CipherProfile:  "mozilla-modern",
			ReloadInterval: 60 * time.Second,
		},
		AdminListen: AdminListenConfig{
			HTTP: HTTPConfig{
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       60 * time.Second,
				WriteTimeout:      2 * time.Minute,
				IdleTimeout:       120 * time.Second,
				MaxHeaderBytes:    1 << 20,
			},
		},
		RateLimit: RateLimitConfig{
			PerKey:    0,
			PerIP:     0,
			Burst:     0,
			CacheSize: 100_000,
		},
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
		Auth: AuthConfig{
			// Secure-by-default: an unconfigured deployment demands valid
			// SigV4. Dev/test paths opt into STRATA_AUTH_MODE=off explicitly.
			Mode:        "required",
			STSDuration: time.Hour,
			KeyMaxAge:   90 * 24 * time.Hour,
		},
		KMS: KMSConfig{
			DEKCacheTTL: 5 * time.Minute,
		},
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
		OTel: OTelConfig{
			SampleRatio:  0.01,
			Ringbuf:      true,
			RingbufBytes: 4 << 20,
		},
		Logging: LoggingConfig{
			Level:  "INFO",
			Format: "json",
		},
		AuditLog: AuditLogConfig{
			Retention: 30 * 24 * time.Hour,
		},
		Manifest: ManifestConfig{
			Format: "proto",
		},
		Console: ConsoleConfig{
			ThemeDefault: "system",
		},
		VHost: VHostConfig{
			Pattern: "*.s3.local",
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
	"STRATA_HTTP_READ_HEADER_TIMEOUT":        "http.read_header_timeout",
	"STRATA_HTTP_READ_TIMEOUT":               "http.read_timeout",
	"STRATA_HTTP_WRITE_TIMEOUT":              "http.write_timeout",
	"STRATA_HTTP_IDLE_TIMEOUT":               "http.idle_timeout",
	"STRATA_HTTP_MAX_HEADER_BYTES":           "http.max_header_bytes",
	"STRATA_TLS_CERT_FILE":                   "tls.cert_file",
	"STRATA_TLS_KEY_FILE":                    "tls.key_file",
	"STRATA_TLS_MIN_VERSION":                 "tls.min_version",
	"STRATA_TLS_CIPHER_PROFILE":              "tls.cipher_profile",
	"STRATA_TLS_CERT_DIR":                    "tls.cert_dir",
	"STRATA_TLS_CLIENT_CA_FILE":              "tls.client_ca_file",
	"STRATA_TLS_RELOAD_INTERVAL":             "tls.reload_interval",
	"STRATA_ADMIN_LISTEN":                    "admin_listen.listen",
	"STRATA_ADMIN_HTTP_READ_HEADER_TIMEOUT":  "admin_listen.http.read_header_timeout",
	"STRATA_ADMIN_HTTP_READ_TIMEOUT":         "admin_listen.http.read_timeout",
	"STRATA_ADMIN_HTTP_WRITE_TIMEOUT":        "admin_listen.http.write_timeout",
	"STRATA_ADMIN_HTTP_IDLE_TIMEOUT":         "admin_listen.http.idle_timeout",
	"STRATA_ADMIN_HTTP_MAX_HEADER_BYTES":     "admin_listen.http.max_header_bytes",
	"STRATA_ADMIN_TLS_CERT_FILE":             "admin_listen.tls.cert_file",
	"STRATA_ADMIN_TLS_KEY_FILE":              "admin_listen.tls.key_file",
	"STRATA_ADMIN_TLS_CLIENT_CA_FILE":        "admin_listen.tls.client_ca_file",
	"STRATA_RATE_LIMIT_PER_KEY":              "rate_limit.per_key",
	"STRATA_RATE_LIMIT_PER_IP":               "rate_limit.per_ip",
	"STRATA_RATE_LIMIT_BURST":                "rate_limit.burst",
	"STRATA_RATE_LIMIT_CACHE_SIZE":           "rate_limit.cache_size",
	"STRATA_CASSANDRA_HOSTS":                 "cassandra.hosts",
	"STRATA_CASSANDRA_KEYSPACE":              "cassandra.keyspace",
	"STRATA_CASSANDRA_DC":                    "cassandra.local_dc",
	"STRATA_CASSANDRA_REPLICATION":           "cassandra.replication",
	"STRATA_CASSANDRA_USER":                  "cassandra.username",
	"STRATA_CASSANDRA_PASSWORD":              "cassandra.password",
	"STRATA_CASSANDRA_TIMEOUT":               "cassandra.timeout",
	"STRATA_CASSANDRA_TLS_CA_FILE":           "cassandra.tls.ca_file",
	"STRATA_CASSANDRA_TLS_CERT_FILE":         "cassandra.tls.cert_file",
	"STRATA_CASSANDRA_TLS_KEY_FILE":          "cassandra.tls.key_file",
	"STRATA_CASSANDRA_TLS_SKIP_VERIFY":       "cassandra.tls.skip_verify",
	"STRATA_TIKV_PD_ENDPOINTS":               "tikv.pd_endpoints",
	"STRATA_TIKV_TLS_CA_FILE":                "tikv.tls.ca_file",
	"STRATA_TIKV_TLS_CERT_FILE":              "tikv.tls.cert_file",
	"STRATA_TIKV_TLS_KEY_FILE":               "tikv.tls.key_file",
	"STRATA_TIKV_TLS_SKIP_VERIFY":            "tikv.tls.skip_verify",
	"STRATA_RADOS_CONF":                      "rados.config_file",
	"STRATA_RADOS_USER":                      "rados.user",
	"STRATA_RADOS_KEYRING":                   "rados.keyring",
	"STRATA_RADOS_POOL":                      "rados.pool",
	"STRATA_RADOS_NAMESPACE":                 "rados.namespace",
	"STRATA_RADOS_CLASSES":                   "rados.classes",
	"STRATA_RADOS_CLUSTERS":                  "rados.clusters",
	"STRATA_S3_CLUSTERS":                     "s3.clusters",
	"STRATA_S3_CLASSES":                      "s3.classes",
	"STRATA_S3_TLS_CA_FILE":                  "s3.tls.ca_file",
	"STRATA_S3_TLS_CERT_FILE":                "s3.tls.cert_file",
	"STRATA_S3_TLS_KEY_FILE":                 "s3.tls.key_file",
	"STRATA_S3_TLS_SKIP_VERIFY":              "s3.tls.skip_verify",
	"STRATA_AUTH_MODE":                       "auth.mode",
	"STRATA_STATIC_CREDENTIALS":              "auth.static_credentials",
	"STRATA_STS_DURATION":                    "auth.sts_duration",
	"STRATA_KEY_MAX_AGE":                     "auth.key_max_age",
	"STRATA_KMS_ADAPTER":                     "kms.adapter",
	"STRATA_DEK_CACHE_TTL":                   "kms.dek_cache_ttl",
	"STRATA_KMS_DEFAULT_KEY_ID":              "kms.default_key_id",
	"STRATA_KMS_AWS_REGION":                  "kms.aws.region",
	"STRATA_KMS_AWS_ENDPOINT":                "kms.aws.endpoint",
	"STRATA_KMS_AWS_ROLE_ARN":                "kms.aws.role_arn",
	"STRATA_KMS_VAULT_ADDR":                  "kms.vault.address",
	"STRATA_KMS_VAULT_PATH":                  "kms.vault.mount",
	"STRATA_KMS_VAULT_TOKEN":                 "kms.vault.token",
	"STRATA_SSE_VAULT_ROLE_ID":               "kms.vault.role_id",
	"STRATA_SSE_VAULT_SECRET_ID":             "kms.vault.secret_id",
	"STRATA_KMS_LOCAL_HSM_SEED":              "kms.local_hsm.seed",
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
	"STRATA_OTEL_EXPORTER_ENDPOINT":          "otel.endpoint",
	"STRATA_OTEL_SAMPLE_RATIO":               "otel.sample_ratio",
	"STRATA_OTEL_RINGBUF":                    "otel.ringbuf",
	"STRATA_OTEL_RINGBUF_BYTES":              "otel.ringbuf_bytes",
	"STRATA_LOG_LEVEL":                       "logging.level",
	"STRATA_LOG_FORMAT":                      "logging.format",
	"STRATA_AUDIT_RETENTION":                 "audit_log.retention",
	"STRATA_BUCKETSTATS_INTERVAL":            "bucket_stats.interval",
	"STRATA_BUCKETSTATS_TOPN":                "bucket_stats.top_n",
	"STRATA_CASSANDRA_SLOW_MS":               "cassandra.slow_ms",
	"STRATA_CLUSTER_NAME":                    "cluster.name",
	"STRATA_CONSOLE_JWT_SECRET":              "console.jwt_secret",
	"STRATA_CONSOLE_THEME_DEFAULT":           "console.theme_default",
	"STRATA_JWT_SECRET_FILE":                 "jwt.secret_file",
	"STRATA_JWT_SHARED":                      "jwt.shared_file",
	"STRATA_MANIFEST_FORMAT":                 "manifest.format",
	"STRATA_MFA_SECRETS":                     "mfa.secrets",
	"STRATA_NODE_ID":                         "node.id",
	"STRATA_PPROF_ENABLED":                   "pprof.enabled",
	"STRATA_PPROF_LISTEN":                    "pprof.listen",
	"STRATA_PPROF_BLOCK_RATE":                "pprof.block_rate",
	"STRATA_PPROF_MUTEX_RATE":                "pprof.mutex_rate",
	"STRATA_PROMETHEUS_URL":                  "prometheus.url",
	"STRATA_RADOS_HEALTH_OID":                "rados.health_oid",
	"STRATA_RADOS_POOL_SIZE":                 "rados.pool_size",
	"STRATA_RADOS_PUT_CONCURRENCY":           "rados.put_concurrency",
	"STRATA_RADOS_GET_PREFETCH":              "rados.get_prefetch",
	"STRATA_RADOS_BATCH_OPS":                 "rados.batch_ops",
	"STRATA_SSE_MASTER_KEY":                  "sse.master_key",
	"STRATA_SSE_MASTER_KEY_ID":               "sse.master_key_id",
	"STRATA_SSE_MASTER_KEY_FILE":             "sse.master_key_file",
	"STRATA_SSE_MASTER_KEY_VAULT":            "sse.master_key_vault",
	"STRATA_SSE_MASTER_KEYS":                 "sse.master_keys",
	"STRATA_VHOST_PATTERN":                   "vhost.pattern",
	"STRATA_TRUSTED_PROXIES":                 "trusted_proxies",
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
		key, ok := envMap[s]
		if !ok {
			return "", nil
		}
		if key == "audit_log.retention" {
			if d, err := ParseAuditRetention(v); err == nil {
				v = d.String()
			}
		}
		return key, v
	}), nil); err != nil {
		return nil, fmt.Errorf("config env: %w", err)
	}

	var out Config
	if err := k.Unmarshal("", &out); err != nil {
		return nil, fmt.Errorf("config unmarshal: %w", err)
	}

	if out.OTel.Endpoint == "" {
		out.OTel.Endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
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
	if err := c.validateHTTP(); err != nil {
		return err
	}
	c.clampHTTP()
	if err := c.validateAdminListen(); err != nil {
		return err
	}
	c.clampAdminListen()
	if err := c.validateTLS(); err != nil {
		return err
	}
	if err := c.validateBackendTLS(); err != nil {
		return err
	}
	if err := c.validateTrustedProxies(); err != nil {
		return err
	}
	if err := c.validateRateLimit(); err != nil {
		return err
	}
	c.clampRateLimit()
	c.clampWorkers()
	c.clampAuthKMS()
	c.clampObservability()
	if err := c.validateMisc(); err != nil {
		return err
	}
	c.clampMisc()
	if err := c.validatePprof(); err != nil {
		return err
	}
	c.clampPprof()
	warnLegacyDrainStrict()
	return nil
}

// validatePprof enforces non-negative profiling rates + fails fast when
// pprof is enabled without any listener (admin or dedicated). Half-paired
// listener config catches operator typos at boot rather than silently
// running without admin auth.
func (c *Config) validatePprof() error {
	if c.Pprof.BlockRate < 0 {
		return fmt.Errorf("pprof.block_rate %d: must be >= 0", c.Pprof.BlockRate)
	}
	if c.Pprof.MutexRate < 0 {
		return fmt.Errorf("pprof.mutex_rate %d: must be >= 0", c.Pprof.MutexRate)
	}
	if c.Pprof.Enabled && c.Pprof.Listen == "" && c.AdminListen.Listen == "" {
		return fmt.Errorf("pprof.enabled=true requires pprof.listen or admin_listen.listen to be set " +
			"(pprof MUST NOT share the S3 hot path)")
	}
	return nil
}

// clampPprof clamps BlockRate + MutexRate to operator-sane ranges.
// BlockRate is a nanosecond threshold; values in [1, 1e9] cover the
// "sample every Nns" idiom. MutexRate is "sample 1/N mutex events";
// values in [1, 1e6] cover the spectrum from "sample every event" to
// "rare sample".
func (c *Config) clampPprof() {
	if c.Pprof.BlockRate > 0 {
		c.Pprof.BlockRate = clampInt("pprof.block_rate", c.Pprof.BlockRate, 1, 1_000_000_000)
	}
	if c.Pprof.MutexRate > 0 {
		c.Pprof.MutexRate = clampInt("pprof.mutex_rate", c.Pprof.MutexRate, 1, 1_000_000)
	}
}

// validateHTTP rejects negative timeouts / max-header-bytes. Zero is a
// valid value (net/http treats zero as "disabled"); negative values fail
// fast at boot.
func (c *Config) validateHTTP() error {
	return validateHTTPSection("http", c.HTTP)
}

// validateAdminListen rejects negative admin-listener timeouts +
// half-paired admin TLS cert/key (US-008 harden-gateway). Empty Listen
// passes through (single-port shape; admin endpoints stay on the main
// listener).
func (c *Config) validateAdminListen() error {
	if err := validateHTTPSection("admin_listen.http", c.AdminListen.HTTP); err != nil {
		return err
	}
	if (c.AdminListen.TLS.CertFile == "") != (c.AdminListen.TLS.KeyFile == "") {
		return fmt.Errorf("admin_listen.tls.cert_file and admin_listen.tls.key_file must both be set or both unset")
	}
	if c.AdminListen.TLS.ClientCAFile != "" && c.AdminListen.TLS.CertFile == "" {
		return fmt.Errorf("admin_listen.tls.client_ca_file requires admin_listen.tls.cert_file + admin_listen.tls.key_file (mTLS needs a server cert)")
	}
	return nil
}

func validateHTTPSection(prefix string, h HTTPConfig) error {
	if h.ReadHeaderTimeout < 0 {
		return fmt.Errorf("%s.read_header_timeout %s: must be >= 0", prefix, h.ReadHeaderTimeout)
	}
	if h.ReadTimeout < 0 {
		return fmt.Errorf("%s.read_timeout %s: must be >= 0", prefix, h.ReadTimeout)
	}
	if h.WriteTimeout < 0 {
		return fmt.Errorf("%s.write_timeout %s: must be >= 0", prefix, h.WriteTimeout)
	}
	if h.IdleTimeout < 0 {
		return fmt.Errorf("%s.idle_timeout %s: must be >= 0", prefix, h.IdleTimeout)
	}
	if h.MaxHeaderBytes < 0 {
		return fmt.Errorf("%s.max_header_bytes %d: must be >= 0", prefix, h.MaxHeaderBytes)
	}
	return nil
}

// validateTLS rejects invalid TLS knob values at boot. Empty CertFile +
// KeyFile is valid (plain HTTP). Setting one without the other is rejected
// — fail fast rather than silently fall back to plain HTTP and surprise the
// operator. MinVersion / CipherProfile are enum-checked; empty passes
// through to the default ("TLS1.2" / "mozilla-modern").
func (c *Config) validateTLS() error {
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return fmt.Errorf("tls.cert_file and tls.key_file must both be set or both unset")
	}
	if c.TLS.CertDir != "" && c.TLS.CertFile != "" {
		return fmt.Errorf("tls.cert_dir and tls.cert_file are mutually exclusive — set only one")
	}
	switch c.TLS.MinVersion {
	case "", "TLS1.2", "TLS1.3":
	default:
		return fmt.Errorf("tls.min_version %q is not one of {TLS1.2, TLS1.3}", c.TLS.MinVersion)
	}
	switch c.TLS.CipherProfile {
	case "", "mozilla-modern", "mozilla-intermediate", "go-default":
	default:
		return fmt.Errorf("tls.cipher_profile %q is not one of {mozilla-modern, mozilla-intermediate, go-default}", c.TLS.CipherProfile)
	}
	if c.TLS.ReloadInterval < 0 {
		return fmt.Errorf("tls.reload_interval %s: must be >= 0", c.TLS.ReloadInterval)
	}
	if c.TLS.ReloadInterval != 0 {
		c.TLS.ReloadInterval = clampDuration("tls.reload_interval", c.TLS.ReloadInterval, 10*time.Second, time.Hour)
	}
	return nil
}

// validateBackendTLS rejects half-paired client cert/key envs on every
// backend that supports mTLS (Cassandra + TiKV + S3-upstream global default).
// Empty all-three (CA + cert + key) = plain backend, the backwards-compat
// default. Per-cluster TLS overrides on STRATA_S3_CLUSTERS entries get their
// own half-pair guard in internal/data/s3.ParseClusters.
func (c *Config) validateBackendTLS() error {
	if (c.Cassandra.TLS.CertFile == "") != (c.Cassandra.TLS.KeyFile == "") {
		return fmt.Errorf("cassandra.tls.cert_file and cassandra.tls.key_file must both be set or both unset")
	}
	if (c.TiKV.TLS.CertFile == "") != (c.TiKV.TLS.KeyFile == "") {
		return fmt.Errorf("tikv.tls.cert_file and tikv.tls.key_file must both be set or both unset")
	}
	if (c.S3.TLS.CertFile == "") != (c.S3.TLS.KeyFile == "") {
		return fmt.Errorf("s3.tls.cert_file and s3.tls.key_file must both be set or both unset")
	}
	return nil
}

// validateTrustedProxies rejects malformed CIDR entries in
// STRATA_TRUSTED_PROXIES at boot (US-007 harden-gateway). Empty input is
// valid — default is "never trust forwarded headers".
func (c *Config) validateTrustedProxies() error {
	if _, err := trustedproxies.Parse(c.TrustedProxies); err != nil {
		return err
	}
	return nil
}

// validateRateLimit rejects negative rate-limit knobs at boot (US-009
// harden-gateway). Zero is valid (= disabled per layer). Ranges:
//
//	PerKey, PerIP ∈ [0, 100000] req/s
//	Burst         ∈ [0, 1000000] (token-bucket capacity)
//	CacheSize     ∈ [1000, 10000000] when non-zero (zero falls through to
//	                default at consume site)
func (c *Config) validateRateLimit() error {
	if c.RateLimit.PerKey < 0 {
		return fmt.Errorf("rate_limit.per_key %d: must be >= 0", c.RateLimit.PerKey)
	}
	if c.RateLimit.PerIP < 0 {
		return fmt.Errorf("rate_limit.per_ip %d: must be >= 0", c.RateLimit.PerIP)
	}
	if c.RateLimit.Burst < 0 {
		return fmt.Errorf("rate_limit.burst %d: must be >= 0", c.RateLimit.Burst)
	}
	if c.RateLimit.CacheSize < 0 {
		return fmt.Errorf("rate_limit.cache_size %d: must be >= 0", c.RateLimit.CacheSize)
	}
	return nil
}

// clampRateLimit pins the rate-limit knobs to operator-sane ranges. Zero
// passes through (= disabled / use default).
func (c *Config) clampRateLimit() {
	if c.RateLimit.PerKey > 0 {
		c.RateLimit.PerKey = clampInt("rate_limit.per_key", c.RateLimit.PerKey, 1, 100_000)
	}
	if c.RateLimit.PerIP > 0 {
		c.RateLimit.PerIP = clampInt("rate_limit.per_ip", c.RateLimit.PerIP, 1, 100_000)
	}
	if c.RateLimit.Burst > 0 {
		c.RateLimit.Burst = clampInt("rate_limit.burst", c.RateLimit.Burst, 1, 1_000_000)
	}
	if c.RateLimit.CacheSize > 0 {
		c.RateLimit.CacheSize = clampInt("rate_limit.cache_size", c.RateLimit.CacheSize, 1_000, 10_000_000)
	}
}

// clampHTTP enforces upper bounds on WriteTimeout + MaxHeaderBytes so a
// fat-finger 999h timeout doesn't silently disable slowloris protection.
// Zero passes through (consumers treat zero as "disabled" per net/http).
func (c *Config) clampHTTP() {
	clampHTTPSection("http", &c.HTTP)
}

// clampAdminListen mirrors clampHTTP for the admin listener (US-008).
func (c *Config) clampAdminListen() {
	clampHTTPSection("admin_listen.http", &c.AdminListen.HTTP)
}

func clampHTTPSection(prefix string, h *HTTPConfig) {
	if h.WriteTimeout > 24*time.Hour {
		slog.Warn("clamping config value", "key", prefix+".write_timeout", "value", h.WriteTimeout.String(), "max", (24 * time.Hour).String())
		h.WriteTimeout = 24 * time.Hour
	}
	if h.MaxHeaderBytes > 16<<20 {
		slog.Warn("clamping config value", "key", prefix+".max_header_bytes", "value", h.MaxHeaderBytes, "max", 16<<20)
		h.MaxHeaderBytes = 16 << 20
	}
}

// validateMisc validates the US-005 sweep knobs that require a finite
// set of values. Zero-valued enums pass through (treated as "default" by
// consumers).
func (c *Config) validateMisc() error {
	switch strings.ToLower(c.Manifest.Format) {
	case "", "proto", "json":
	default:
		return fmt.Errorf("manifest.format %q is not one of {proto, json}", c.Manifest.Format)
	}
	switch strings.ToLower(c.Console.ThemeDefault) {
	case "", "system", "light", "dark":
	default:
		return fmt.Errorf("console.theme_default %q is not one of {system, light, dark}", c.Console.ThemeDefault)
	}
	return nil
}

// clampMisc enforces the historical env-side ranges on the US-005 sweep
// fields. Zero values pass through (consumers treat them as "use default").
func (c *Config) clampMisc() {
	if c.RADOS.PoolSize != 0 {
		c.RADOS.PoolSize = clampInt("rados.pool_size", c.RADOS.PoolSize, 1, 32)
	}
	if c.RADOS.PutConcurrency != 0 {
		c.RADOS.PutConcurrency = clampInt("rados.put_concurrency", c.RADOS.PutConcurrency, 1, 256)
	}
	if c.RADOS.GetPrefetch != 0 {
		c.RADOS.GetPrefetch = clampInt("rados.get_prefetch", c.RADOS.GetPrefetch, 1, 64)
	}
	if c.Cassandra.SlowMS < 0 {
		slog.Warn("clamping config value", "key", "cassandra.slow_ms", "value", c.Cassandra.SlowMS, "min", 0)
		c.Cassandra.SlowMS = 0
	}
	if c.BucketStats.TopN < 0 {
		slog.Warn("clamping config value", "key", "bucket_stats.top_n", "value", c.BucketStats.TopN, "min", 0)
		c.BucketStats.TopN = 0
	}
}

// clampObservability enforces the historical env-side ranges on the new
// otel + audit_log TOML fields. Zero values pass through (consumers treat
// them as "use default").
func (c *Config) clampObservability() {
	if c.OTel.SampleRatio < 0 {
		slog.Warn("clamping config value", "key", "otel.sample_ratio", "value", c.OTel.SampleRatio, "min", 0.0)
		c.OTel.SampleRatio = 0
	}
	if c.OTel.SampleRatio > 1 {
		slog.Warn("clamping config value", "key", "otel.sample_ratio", "value", c.OTel.SampleRatio, "max", 1.0)
		c.OTel.SampleRatio = 1
	}
	if c.OTel.RingbufBytes != 0 {
		c.OTel.RingbufBytes = clampInt("otel.ringbuf_bytes", c.OTel.RingbufBytes, 1<<20, 1<<30)
	}
	if c.AuditLog.Retention != 0 {
		c.AuditLog.Retention = clampDuration("audit_log.retention", c.AuditLog.Retention, time.Minute, 10*365*24*time.Hour)
	}
}

// ParseAuditRetention parses an audit_log retention string. Accepts plain
// Go durations and a bare "<N>d" days suffix (historical operator UX).
// Empty input returns 0 — callers fall back to defaults() (30d).
func ParseAuditRetention(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if trimmed, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(trimmed)
		if err != nil {
			return 0, fmt.Errorf("audit_log.retention: %q: %w", s, err)
		}
		if n < 0 {
			return 0, fmt.Errorf("audit_log.retention: negative value %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// clampAuthKMS enforces the historical env-side ranges on the new
// auth + kms TOML fields so TOML-loaded and env-loaded values get the
// same WARN-and-clamp treatment. Zero values pass through (treated as
// "default" by downstream consumers).
func (c *Config) clampAuthKMS() {
	if c.Auth.STSDuration != 0 {
		c.Auth.STSDuration = clampDuration("auth.sts_duration", c.Auth.STSDuration, 15*time.Minute, 12*time.Hour)
	}
	if c.Auth.KeyMaxAge != 0 {
		c.Auth.KeyMaxAge = clampDuration("auth.key_max_age", c.Auth.KeyMaxAge, 24*time.Hour, 365*24*time.Hour)
	}
	if c.KMS.DEKCacheTTL != 0 {
		c.KMS.DEKCacheTTL = clampDuration("kms.dek_cache_ttl", c.KMS.DEKCacheTTL, 30*time.Second, time.Hour)
	}
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
