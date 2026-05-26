package config

import (
	"os"
	"path/filepath"
	"strings"
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

// TestCassandraTLSHalfPairRejected mirrors validateTLS's half-pair guard for
// the Cassandra backend mTLS pair (US-004): setting cert_file without
// key_file (or vice versa) must fail at boot rather than silently fall back
// to server-auth-only TLS.
func TestCassandraTLSHalfPairRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_CASSANDRA_TLS_CERT_FILE", "/tmp/client.crt")
	if _, err := Load(); err == nil {
		t.Fatal("cert_file without key_file must fail Load")
	}
	clearEnv(t)
	t.Setenv("STRATA_CASSANDRA_TLS_KEY_FILE", "/tmp/client.key")
	if _, err := Load(); err == nil {
		t.Fatal("key_file without cert_file must fail Load")
	}
}

// TestCassandraTLSEnvWiresThrough confirms the four envs land on the
// Config.Cassandra.TLS substruct verbatim. Boot-time skip_verify=true is
// the operator's signal to bump the strata_backend_tls_skip_verify gauge —
// the gauge wiring itself lives in serverapp.buildMetaStore.
func TestCassandraTLSEnvWiresThrough(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_CASSANDRA_TLS_CA_FILE", "/tmp/ca.pem")
	t.Setenv("STRATA_CASSANDRA_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("STRATA_CASSANDRA_TLS_KEY_FILE", "/tmp/k.pem")
	t.Setenv("STRATA_CASSANDRA_TLS_SKIP_VERIFY", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cassandra.TLS.CAFile != "/tmp/ca.pem" {
		t.Errorf("ca_file=%q", cfg.Cassandra.TLS.CAFile)
	}
	if cfg.Cassandra.TLS.CertFile != "/tmp/c.pem" {
		t.Errorf("cert_file=%q", cfg.Cassandra.TLS.CertFile)
	}
	if cfg.Cassandra.TLS.KeyFile != "/tmp/k.pem" {
		t.Errorf("key_file=%q", cfg.Cassandra.TLS.KeyFile)
	}
	if !cfg.Cassandra.TLS.SkipVerify {
		t.Errorf("skip_verify=%v want true", cfg.Cassandra.TLS.SkipVerify)
	}
}

// TestCassandraTLSDefaultPlainTCP confirms the empty default leaves every
// TLS field zero-valued, so the gocql cluster builder keeps the historical
// plain-TCP shape.
func TestCassandraTLSDefaultPlainTCP(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cassandra.TLS.CAFile != "" || cfg.Cassandra.TLS.CertFile != "" ||
		cfg.Cassandra.TLS.KeyFile != "" || cfg.Cassandra.TLS.SkipVerify {
		t.Errorf("default cassandra.tls non-empty: %+v", cfg.Cassandra.TLS)
	}
}

// TestTiKVTLSHalfPairRejected mirrors validateBackendTLS's half-pair guard
// for the TiKV backend mTLS pair (US-005): setting cert_file without
// key_file (or vice versa) must fail at boot.
func TestTiKVTLSHalfPairRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_META_BACKEND", "tikv")
	t.Setenv("STRATA_TIKV_PD_ENDPOINTS", "pd0:2379")
	t.Setenv("STRATA_TIKV_TLS_CERT_FILE", "/tmp/client.crt")
	if _, err := Load(); err == nil {
		t.Fatal("cert_file without key_file must fail Load")
	}
	clearEnv(t)
	t.Setenv("STRATA_META_BACKEND", "tikv")
	t.Setenv("STRATA_TIKV_PD_ENDPOINTS", "pd0:2379")
	t.Setenv("STRATA_TIKV_TLS_KEY_FILE", "/tmp/client.key")
	if _, err := Load(); err == nil {
		t.Fatal("key_file without cert_file must fail Load")
	}
}

// TestTiKVTLSEnvWiresThrough confirms the four envs land on the
// Config.TiKV.TLS substruct verbatim. The gauge bump + WARN log live in
// serverapp.buildMetaStore.
func TestTiKVTLSEnvWiresThrough(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_META_BACKEND", "tikv")
	t.Setenv("STRATA_TIKV_PD_ENDPOINTS", "pd0:2379")
	t.Setenv("STRATA_TIKV_TLS_CA_FILE", "/tmp/ca.pem")
	t.Setenv("STRATA_TIKV_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("STRATA_TIKV_TLS_KEY_FILE", "/tmp/k.pem")
	t.Setenv("STRATA_TIKV_TLS_SKIP_VERIFY", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TiKV.TLS.CAFile != "/tmp/ca.pem" {
		t.Errorf("ca_file=%q", cfg.TiKV.TLS.CAFile)
	}
	if cfg.TiKV.TLS.CertFile != "/tmp/c.pem" {
		t.Errorf("cert_file=%q", cfg.TiKV.TLS.CertFile)
	}
	if cfg.TiKV.TLS.KeyFile != "/tmp/k.pem" {
		t.Errorf("key_file=%q", cfg.TiKV.TLS.KeyFile)
	}
	if !cfg.TiKV.TLS.SkipVerify {
		t.Errorf("skip_verify=%v want true", cfg.TiKV.TLS.SkipVerify)
	}
}

// TestTiKVTLSDefaultPlainGRPC confirms the empty default leaves every TLS
// field zero-valued so tikv-client-go keeps the historical plain-gRPC
// shape.
func TestTiKVTLSDefaultPlainGRPC(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TiKV.TLS.CAFile != "" || cfg.TiKV.TLS.CertFile != "" ||
		cfg.TiKV.TLS.KeyFile != "" || cfg.TiKV.TLS.SkipVerify {
		t.Errorf("default tikv.tls non-empty: %+v", cfg.TiKV.TLS)
	}
}

// TestS3TLSHalfPairRejected mirrors validateBackendTLS's half-pair guard for
// the S3-upstream backend mTLS pair (US-006): setting cert_file without
// key_file (or vice versa) must fail at boot.
func TestS3TLSHalfPairRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_S3_TLS_CERT_FILE", "/tmp/client.crt")
	if _, err := Load(); err == nil {
		t.Fatal("cert_file without key_file must fail Load")
	}
	clearEnv(t)
	t.Setenv("STRATA_S3_TLS_KEY_FILE", "/tmp/client.key")
	if _, err := Load(); err == nil {
		t.Fatal("key_file without cert_file must fail Load")
	}
}

// TestS3TLSEnvWiresThrough confirms the four envs land on the Config.S3.TLS
// substruct verbatim. The gauge bump + WARN log live in
// serverapp.buildDataBackend.
func TestS3TLSEnvWiresThrough(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_S3_TLS_CA_FILE", "/tmp/ca.pem")
	t.Setenv("STRATA_S3_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("STRATA_S3_TLS_KEY_FILE", "/tmp/k.pem")
	t.Setenv("STRATA_S3_TLS_SKIP_VERIFY", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.S3.TLS.CAFile != "/tmp/ca.pem" {
		t.Errorf("ca_file=%q", cfg.S3.TLS.CAFile)
	}
	if cfg.S3.TLS.CertFile != "/tmp/c.pem" {
		t.Errorf("cert_file=%q", cfg.S3.TLS.CertFile)
	}
	if cfg.S3.TLS.KeyFile != "/tmp/k.pem" {
		t.Errorf("key_file=%q", cfg.S3.TLS.KeyFile)
	}
	if !cfg.S3.TLS.SkipVerify {
		t.Errorf("skip_verify=%v want true", cfg.S3.TLS.SkipVerify)
	}
}

// TestS3TLSDefaultPlainHTTP confirms the empty default leaves every TLS
// field zero-valued so the s3 backend keeps the Go-default HTTP-client
// shape (system roots, no client cert).
func TestS3TLSDefaultPlainHTTP(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.S3.TLS.CAFile != "" || cfg.S3.TLS.CertFile != "" ||
		cfg.S3.TLS.KeyFile != "" || cfg.S3.TLS.SkipVerify {
		t.Errorf("default s3.tls non-empty: %+v", cfg.S3.TLS)
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

// TestTrustedProxiesEnvWiresThrough confirms STRATA_TRUSTED_PROXIES lands
// on the top-level Config field verbatim (US-007 harden-gateway).
func TestTrustedProxiesEnvWiresThrough(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_TRUSTED_PROXIES", "10.0.0.0/8, fd00::/8")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TrustedProxies != "10.0.0.0/8, fd00::/8" {
		t.Errorf("trusted_proxies=%q", cfg.TrustedProxies)
	}
}

// TestTrustedProxiesMalformedRejected confirms a bad CIDR fails at boot
// (US-007 harden-gateway). The error message must name the offending entry
// so operators can fix it without grepping the source.
func TestTrustedProxiesMalformedRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_TRUSTED_PROXIES", "10.0.0.0/8,not-a-cidr")
	_, err := Load()
	if err == nil {
		t.Fatal("malformed CIDR must fail Load")
	}
	if !strings.Contains(err.Error(), "not-a-cidr") {
		t.Errorf("error should name bad entry: %v", err)
	}
}

// TestTrustedProxiesDefaultEmpty confirms the secure default (forwarded
// headers ignored — no trusted proxies).
func TestTrustedProxiesDefaultEmpty(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TrustedProxies != "" {
		t.Errorf("default trusted_proxies non-empty: %q", cfg.TrustedProxies)
	}
}

// TestRateLimitEnvWiresThrough confirms the four rate-limit envs land on
// Config.RateLimit verbatim (US-009 harden-gateway).
func TestRateLimitEnvWiresThrough(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_RATE_LIMIT_PER_KEY", "50")
	t.Setenv("STRATA_RATE_LIMIT_PER_IP", "100")
	t.Setenv("STRATA_RATE_LIMIT_BURST", "250")
	t.Setenv("STRATA_RATE_LIMIT_CACHE_SIZE", "5000")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RateLimit.PerKey != 50 || cfg.RateLimit.PerIP != 100 ||
		cfg.RateLimit.Burst != 250 || cfg.RateLimit.CacheSize != 5000 {
		t.Errorf("rate_limit wired wrong: %+v", cfg.RateLimit)
	}
}

// TestRateLimitDefaultsDisabled — both per-key + per-IP default to 0
// (disabled); CacheSize defaults to 100000.
func TestRateLimitDefaultsDisabled(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RateLimit.PerKey != 0 || cfg.RateLimit.PerIP != 0 || cfg.RateLimit.Burst != 0 {
		t.Errorf("default rate_limit not zero: %+v", cfg.RateLimit)
	}
	if cfg.RateLimit.CacheSize != 100_000 {
		t.Errorf("default cache_size=%d want 100000", cfg.RateLimit.CacheSize)
	}
}

// TestRateLimitNegativeRejected — every rate-limit knob fails fast at boot
// when set negative (mirrors the US-001 timeout discipline).
func TestRateLimitNegativeRejected(t *testing.T) {
	for _, env := range []string{
		"STRATA_RATE_LIMIT_PER_KEY",
		"STRATA_RATE_LIMIT_PER_IP",
		"STRATA_RATE_LIMIT_BURST",
		"STRATA_RATE_LIMIT_CACHE_SIZE",
	} {
		t.Run(env, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(env, "-1")
			if _, err := Load(); err == nil {
				t.Fatalf("%s=-1 must fail Load", env)
			}
		})
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
