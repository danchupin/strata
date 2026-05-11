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
	S3        S3Config        `koanf:"s3"`
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
	"STRATA_RADOS_CLUSTERS":           "rados.clusters",
	"STRATA_S3_CLUSTERS":              "s3.clusters",
	"STRATA_S3_CLASSES":               "s3.classes",
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
	return nil
}
