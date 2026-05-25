package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadMiscFromTOMLOnly proves the US-005 sweep sections drive every
// per-knob tunable end-to-end with zero STRATA_* env vars set.
func TestLoadMiscFromTOMLOnly(t *testing.T) {
	clearEnv(t)

	body := `
[bucket_stats]
interval = "30s"
top_n = 25

[cassandra]
slow_ms = 250

[cluster]
name = "strata-east"

[console]
jwt_secret = "deadbeefdeadbeefdeadbeefdeadbeef"
theme_default = "dark"

[jwt]
secret_file = "/var/run/strata/jwt"
shared_file = "/etc/strata/shared/jwt"

[manifest]
format = "json"

[mfa]
secrets = "arn:aws:iam::1:mfa/user1:JBSWY3DPEHPK3PXP"

[node]
id = "strata-a"

[prometheus]
url = "http://prom:9090"

[rados]
health_oid = "canary-east"
pool_size = 4
put_concurrency = 64
get_prefetch = 8
batch_ops = true

[sse]
master_key = "deadbeef"
master_key_id = "key-1"
master_key_file = "/etc/strata/sse/key"
master_key_vault = "https://vault:8200:transit/export/key"
master_keys = "key-1:hex,key-2:hex"

[vhost]
pattern = "*.s3.east.local"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "strata.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	t.Setenv("STRATA_CONFIG_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.BucketStats.Interval != 30*time.Second {
		t.Errorf("bucket_stats.interval = %v", cfg.BucketStats.Interval)
	}
	if cfg.BucketStats.TopN != 25 {
		t.Errorf("bucket_stats.top_n = %d", cfg.BucketStats.TopN)
	}
	if cfg.Cassandra.SlowMS != 250 {
		t.Errorf("cassandra.slow_ms = %d", cfg.Cassandra.SlowMS)
	}
	if cfg.Cluster.Name != "strata-east" {
		t.Errorf("cluster.name = %q", cfg.Cluster.Name)
	}
	if cfg.Console.JWTSecret != "deadbeefdeadbeefdeadbeefdeadbeef" {
		t.Errorf("console.jwt_secret = %q", cfg.Console.JWTSecret)
	}
	if cfg.Console.ThemeDefault != "dark" {
		t.Errorf("console.theme_default = %q", cfg.Console.ThemeDefault)
	}
	if cfg.JWT.SecretFile != "/var/run/strata/jwt" {
		t.Errorf("jwt.secret_file = %q", cfg.JWT.SecretFile)
	}
	if cfg.JWT.SharedFile != "/etc/strata/shared/jwt" {
		t.Errorf("jwt.shared_file = %q", cfg.JWT.SharedFile)
	}
	if cfg.Manifest.Format != "json" {
		t.Errorf("manifest.format = %q", cfg.Manifest.Format)
	}
	if cfg.MFA.Secrets == "" {
		t.Errorf("mfa.secrets empty")
	}
	if cfg.Node.ID != "strata-a" {
		t.Errorf("node.id = %q", cfg.Node.ID)
	}
	if cfg.Prometheus.URL != "http://prom:9090" {
		t.Errorf("prometheus.url = %q", cfg.Prometheus.URL)
	}
	if cfg.RADOS.HealthOID != "canary-east" {
		t.Errorf("rados.health_oid = %q", cfg.RADOS.HealthOID)
	}
	if cfg.RADOS.PoolSize != 4 {
		t.Errorf("rados.pool_size = %d", cfg.RADOS.PoolSize)
	}
	if cfg.RADOS.PutConcurrency != 64 {
		t.Errorf("rados.put_concurrency = %d", cfg.RADOS.PutConcurrency)
	}
	if cfg.RADOS.GetPrefetch != 8 {
		t.Errorf("rados.get_prefetch = %d", cfg.RADOS.GetPrefetch)
	}
	if !cfg.RADOS.BatchOps {
		t.Errorf("rados.batch_ops = %v want true", cfg.RADOS.BatchOps)
	}
	if cfg.SSE.MasterKey != "deadbeef" {
		t.Errorf("sse.master_key = %q", cfg.SSE.MasterKey)
	}
	if cfg.SSE.MasterKeyID != "key-1" {
		t.Errorf("sse.master_key_id = %q", cfg.SSE.MasterKeyID)
	}
	if cfg.SSE.MasterKeyFile != "/etc/strata/sse/key" {
		t.Errorf("sse.master_key_file = %q", cfg.SSE.MasterKeyFile)
	}
	if cfg.SSE.MasterKeyVault != "https://vault:8200:transit/export/key" {
		t.Errorf("sse.master_key_vault = %q", cfg.SSE.MasterKeyVault)
	}
	if cfg.SSE.MasterKeys != "key-1:hex,key-2:hex" {
		t.Errorf("sse.master_keys = %q", cfg.SSE.MasterKeys)
	}
	if cfg.VHost.Pattern != "*.s3.east.local" {
		t.Errorf("vhost.pattern = %q", cfg.VHost.Pattern)
	}
}

// TestEnvOverridesTOMLForMisc pins env > TOML precedence for the US-005
// sweep knobs.
func TestEnvOverridesTOMLForMisc(t *testing.T) {
	clearEnv(t)

	body := `
[manifest]
format = "json"

[node]
id = "from-toml"

[vhost]
pattern = "*.toml"

[rados]
pool_size = 4
`
	dir := t.TempDir()
	path := filepath.Join(dir, "strata.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	t.Setenv("STRATA_CONFIG_FILE", path)
	t.Setenv("STRATA_MANIFEST_FORMAT", "proto")
	t.Setenv("STRATA_NODE_ID", "from-env")
	t.Setenv("STRATA_VHOST_PATTERN", "*.env")
	t.Setenv("STRATA_RADOS_POOL_SIZE", "16")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Manifest.Format != "proto" {
		t.Errorf("manifest.format = %q want proto (env)", cfg.Manifest.Format)
	}
	if cfg.Node.ID != "from-env" {
		t.Errorf("node.id = %q want from-env", cfg.Node.ID)
	}
	if cfg.VHost.Pattern != "*.env" {
		t.Errorf("vhost.pattern = %q want *.env", cfg.VHost.Pattern)
	}
	if cfg.RADOS.PoolSize != 16 {
		t.Errorf("rados.pool_size = %d want 16", cfg.RADOS.PoolSize)
	}
}

// TestClampMiscOnLoad exercises the rados.* + bucket_stats.top_n + cassandra.slow_ms
// clamps defined in clampMisc / validateMisc.
func TestClampMiscOnLoad(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_RADOS_POOL_SIZE", "9999")
	t.Setenv("STRATA_RADOS_PUT_CONCURRENCY", "0")
	t.Setenv("STRATA_RADOS_GET_PREFETCH", "0")
	t.Setenv("STRATA_CASSANDRA_SLOW_MS", "-5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RADOS.PoolSize != 32 {
		t.Errorf("rados.pool_size clamp: %d want 32", cfg.RADOS.PoolSize)
	}
	// 0 (zero-valued) must pass through clamp untouched so consumers fall
	// back to the *FromEnv default.
	if cfg.RADOS.PutConcurrency != 0 {
		t.Errorf("rados.put_concurrency zero passthrough: %d", cfg.RADOS.PutConcurrency)
	}
	if cfg.RADOS.GetPrefetch != 0 {
		t.Errorf("rados.get_prefetch zero passthrough: %d", cfg.RADOS.GetPrefetch)
	}
	if cfg.Cassandra.SlowMS != 0 {
		t.Errorf("cassandra.slow_ms negative clamp: %d", cfg.Cassandra.SlowMS)
	}
}

// TestInvalidManifestFormatRejected pins the enum validation for
// manifest.format — only proto / json are accepted.
func TestInvalidManifestFormatRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_MANIFEST_FORMAT", "yaml")
	if _, err := Load(); err == nil {
		t.Fatal("invalid manifest.format must fail Load")
	}
}

// TestInvalidConsoleThemeRejected pins the enum validation for
// console.theme_default — only system / light / dark are accepted.
func TestInvalidConsoleThemeRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_CONSOLE_THEME_DEFAULT", "neon")
	if _, err := Load(); err == nil {
		t.Fatal("invalid console.theme_default must fail Load")
	}
}

// TestDefaultMiscKnobs pins the default values for the US-005 sweep.
func TestDefaultMiscKnobs(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Manifest.Format != "proto" {
		t.Errorf("manifest.format default = %q want proto", cfg.Manifest.Format)
	}
	if cfg.Console.ThemeDefault != "system" {
		t.Errorf("console.theme_default default = %q want system", cfg.Console.ThemeDefault)
	}
	if cfg.VHost.Pattern != "*.s3.local" {
		t.Errorf("vhost.pattern default = %q want *.s3.local", cfg.VHost.Pattern)
	}
	if cfg.BucketStats.TopN != 0 {
		t.Errorf("bucket_stats.top_n default = %d want 0 (consumer falls back)", cfg.BucketStats.TopN)
	}
	if cfg.Cassandra.SlowMS != 0 {
		t.Errorf("cassandra.slow_ms default = %d want 0 (consumer falls back)", cfg.Cassandra.SlowMS)
	}
}
