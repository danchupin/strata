package config

import (
	"fmt"
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
	S3Backend S3BackendConfig `koanf:"s3_backend"`
	Auth      AuthConfig      `koanf:"auth"`
	Lifecycle LifecycleConfig `koanf:"lifecycle"`
	GC        GCConfig        `koanf:"gc"`

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
}

// S3BackendConfig wires the STRATA_S3_BACKEND_* env vars used by the
// internal/data/s3 backend (US-005). Endpoint empty falls back to AWS
// region-based resolution; AccessKey/SecretKey both empty falls through
// to the SDK default credential chain (env / ~/.aws / IRSA / IMDS).
type S3BackendConfig struct {
	Endpoint          string `koanf:"endpoint"`
	Region            string `koanf:"region"`
	Bucket            string `koanf:"bucket"`
	AccessKey         string `koanf:"access_key"`
	SecretKey         string `koanf:"secret_key"`
	ForcePathStyle    bool   `koanf:"force_path_style"`
	PartSize          int64  `koanf:"part_size"`
	UploadConcurrency int    `koanf:"upload_concurrency"`
	// MaxRetries caps total SDK attempts per request (US-006). Zero
	// applies the s3 package default (5).
	MaxRetries int `koanf:"max_retries"`
	// OpTimeoutSecs is the small-op deadline in whole seconds (US-006).
	// Zero applies the s3 package default (30 s). Multipart Put gets a
	// separate 10-min ceiling that is not operator-tunable today —
	// bump OpTimeoutSecs only when small-op latency runs hot.
	OpTimeoutSecs int `koanf:"op_timeout_secs"`
	// SSEMode (US-013) selects the encryption disposition for backend
	// writes. One of {passthrough, strata, both}; empty resolves to
	// passthrough at s3.Open. Recorded per-object on Manifest.SSE so
	// the GET path branches per-object regardless of current backend
	// config. See internal/data/s3.Config.SSEMode for semantics.
	SSEMode string `koanf:"sse_mode"`
	// SSEKMSKeyID, when set, selects aws:kms over AES256 for the backend
	// SSE header in passthrough/both. Empty falls back to AES256
	// (SSE-S3). Ignored in strata mode.
	SSEKMSKeyID string `koanf:"sse_kms_key_id"`
}

type AuthConfig struct {
	Mode              string `koanf:"mode"`
	StaticCredentials string `koanf:"static_credentials"`
}

type LifecycleConfig struct {
	Interval      time.Duration `koanf:"interval"`
	Unit          string        `koanf:"unit"`
	MetricsListen string        `koanf:"metrics_listen"`
}

type GCConfig struct {
	Interval      time.Duration `koanf:"interval"`
	Grace         time.Duration `koanf:"grace"`
	MetricsListen string        `koanf:"metrics_listen"`
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
		Lifecycle: LifecycleConfig{
			Interval:      60 * time.Second,
			Unit:          "day",
			MetricsListen: ":9101",
		},
		GC: GCConfig{
			Interval:      30 * time.Second,
			Grace:         5 * time.Minute,
			MetricsListen: ":9100",
		},
	}
}

// envMap declares the explicit mapping from environment variables to koanf paths.
// Keeping it explicit avoids surprises from auto-mangling underscores into dots.
var envMap = map[string]string{
	"STRATA_LISTEN":                   "listen",
	"STRATA_REGION":                   "region",
	"STRATA_DATA_BACKEND":             "data_backend",
	"STRATA_META_BACKEND":             "meta_backend",
	"STRATA_BUCKET_SHARDS":            "default_bucket_shards",
	"STRATA_SHUTDOWN_WAIT":            "shutdown_wait",
	"STRATA_CASSANDRA_HOSTS":          "cassandra.hosts",
	"STRATA_CASSANDRA_KEYSPACE":       "cassandra.keyspace",
	"STRATA_CASSANDRA_DC":             "cassandra.local_dc",
	"STRATA_CASSANDRA_REPLICATION":    "cassandra.replication",
	"STRATA_CASSANDRA_USER":           "cassandra.username",
	"STRATA_CASSANDRA_PASSWORD":       "cassandra.password",
	"STRATA_CASSANDRA_TIMEOUT":        "cassandra.timeout",
	"STRATA_TIKV_PD_ENDPOINTS":        "tikv.pd_endpoints",
	"STRATA_RADOS_CONF":               "rados.config_file",
	"STRATA_RADOS_USER":               "rados.user",
	"STRATA_RADOS_KEYRING":            "rados.keyring",
	"STRATA_RADOS_POOL":               "rados.pool",
	"STRATA_RADOS_NAMESPACE":          "rados.namespace",
	"STRATA_RADOS_CLASSES":            "rados.classes",
	"STRATA_S3_BACKEND_ENDPOINT":           "s3_backend.endpoint",
	"STRATA_S3_BACKEND_REGION":             "s3_backend.region",
	"STRATA_S3_BACKEND_BUCKET":             "s3_backend.bucket",
	"STRATA_S3_BACKEND_ACCESS_KEY":         "s3_backend.access_key",
	"STRATA_S3_BACKEND_SECRET_KEY":         "s3_backend.secret_key",
	"STRATA_S3_BACKEND_FORCE_PATH_STYLE":   "s3_backend.force_path_style",
	"STRATA_S3_BACKEND_PART_SIZE":          "s3_backend.part_size",
	"STRATA_S3_BACKEND_UPLOAD_CONCURRENCY": "s3_backend.upload_concurrency",
	"STRATA_S3_BACKEND_MAX_RETRIES":        "s3_backend.max_retries",
	"STRATA_S3_BACKEND_OP_TIMEOUT_SECS":    "s3_backend.op_timeout_secs",
	"STRATA_S3_BACKEND_SSE_MODE":           "s3_backend.sse_mode",
	"STRATA_S3_BACKEND_SSE_KMS_KEY_ID":     "s3_backend.sse_kms_key_id",
	"STRATA_AUTH_MODE":                "auth.mode",
	"STRATA_STATIC_CREDENTIALS":       "auth.static_credentials",
	"STRATA_LIFECYCLE_INTERVAL":       "lifecycle.interval",
	"STRATA_LIFECYCLE_UNIT":           "lifecycle.unit",
	"STRATA_LIFECYCLE_METRICS_LISTEN": "lifecycle.metrics_listen",
	"STRATA_GC_INTERVAL":              "gc.interval",
	"STRATA_GC_GRACE":                 "gc.grace",
	"STRATA_GC_METRICS_LISTEN":        "gc.metrics_listen",
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
		if err := c.S3Backend.validate(); err != nil {
			return err
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
	return nil
}

// validate enforces fail-fast on misconfiguration of the S3 data backend.
// Only invoked when DataBackend == "s3" — leaves the struct unvalidated
// otherwise so operators using rados/memory don't need to set
// STRATA_S3_BACKEND_*.
func (c *S3BackendConfig) validate() error {
	if c.Bucket == "" {
		return fmt.Errorf("STRATA_S3_BACKEND_BUCKET is required when data_backend=s3")
	}
	if c.Region == "" {
		return fmt.Errorf("STRATA_S3_BACKEND_REGION is required when data_backend=s3")
	}
	if (c.AccessKey == "") != (c.SecretKey == "") {
		return fmt.Errorf("STRATA_S3_BACKEND_ACCESS_KEY and STRATA_S3_BACKEND_SECRET_KEY must be set together (or both empty for SDK default chain)")
	}
	if c.PartSize < 0 {
		return fmt.Errorf("STRATA_S3_BACKEND_PART_SIZE must be non-negative (got %d)", c.PartSize)
	}
	if c.UploadConcurrency < 0 {
		return fmt.Errorf("STRATA_S3_BACKEND_UPLOAD_CONCURRENCY must be non-negative (got %d)", c.UploadConcurrency)
	}
	if c.MaxRetries < 0 {
		return fmt.Errorf("STRATA_S3_BACKEND_MAX_RETRIES must be non-negative (got %d)", c.MaxRetries)
	}
	if c.OpTimeoutSecs < 0 {
		return fmt.Errorf("STRATA_S3_BACKEND_OP_TIMEOUT_SECS must be non-negative (got %d)", c.OpTimeoutSecs)
	}
	switch c.SSEMode {
	case "", "passthrough", "strata", "both":
	default:
		return fmt.Errorf("STRATA_S3_BACKEND_SSE_MODE %q is not one of {passthrough, strata, both}", c.SSEMode)
	}
	return nil
}
