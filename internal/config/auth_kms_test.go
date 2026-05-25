package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadAuthKMSFromTOMLOnly proves the auth.* + kms.* TOML sections
// drive every per-knob tunable end-to-end with zero STRATA_* env vars
// set. Parity AC for US-003.
func TestLoadAuthKMSFromTOMLOnly(t *testing.T) {
	clearEnv(t)

	body := `
[auth]
mode = "required"
static_credentials = "AKID:SECRET"
sts_duration = "2h"
key_max_age = "240h"

[kms]
adapter = "vault"
dek_cache_ttl = "10m"
default_key_id = "alias/strata-prod"

[kms.aws]
region = "us-west-2"
endpoint = "https://kms.us-west-2.amazonaws.com"
role_arn = "arn:aws:iam::123:role/kms"

[kms.vault]
address = "https://vault.example.com"
mount = "transit"
token = "s.deadbeef"
role_id = "role-uuid"
secret_id = "secret-uuid"

[kms.local_hsm]
seed = "deadbeef"
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

	if cfg.Auth.Mode != "required" {
		t.Errorf("auth.mode = %q", cfg.Auth.Mode)
	}
	if cfg.Auth.StaticCredentials != "AKID:SECRET" {
		t.Errorf("auth.static_credentials = %q", cfg.Auth.StaticCredentials)
	}
	if cfg.Auth.STSDuration != 2*time.Hour {
		t.Errorf("auth.sts_duration = %v", cfg.Auth.STSDuration)
	}
	if cfg.Auth.KeyMaxAge != 240*time.Hour {
		t.Errorf("auth.key_max_age = %v", cfg.Auth.KeyMaxAge)
	}

	if cfg.KMS.Adapter != "vault" {
		t.Errorf("kms.adapter = %q", cfg.KMS.Adapter)
	}
	if cfg.KMS.DEKCacheTTL != 10*time.Minute {
		t.Errorf("kms.dek_cache_ttl = %v", cfg.KMS.DEKCacheTTL)
	}
	if cfg.KMS.DefaultKeyID != "alias/strata-prod" {
		t.Errorf("kms.default_key_id = %q", cfg.KMS.DefaultKeyID)
	}
	if cfg.KMS.AWS.Region != "us-west-2" {
		t.Errorf("kms.aws.region = %q", cfg.KMS.AWS.Region)
	}
	if cfg.KMS.AWS.Endpoint != "https://kms.us-west-2.amazonaws.com" {
		t.Errorf("kms.aws.endpoint = %q", cfg.KMS.AWS.Endpoint)
	}
	if cfg.KMS.AWS.RoleARN != "arn:aws:iam::123:role/kms" {
		t.Errorf("kms.aws.role_arn = %q", cfg.KMS.AWS.RoleARN)
	}
	if cfg.KMS.Vault.Address != "https://vault.example.com" {
		t.Errorf("kms.vault.address = %q", cfg.KMS.Vault.Address)
	}
	if cfg.KMS.Vault.Mount != "transit" {
		t.Errorf("kms.vault.mount = %q", cfg.KMS.Vault.Mount)
	}
	if cfg.KMS.Vault.Token != "s.deadbeef" {
		t.Errorf("kms.vault.token = %q", cfg.KMS.Vault.Token)
	}
	if cfg.KMS.Vault.RoleID != "role-uuid" {
		t.Errorf("kms.vault.role_id = %q", cfg.KMS.Vault.RoleID)
	}
	if cfg.KMS.Vault.SecretID != "secret-uuid" {
		t.Errorf("kms.vault.secret_id = %q", cfg.KMS.Vault.SecretID)
	}
	if cfg.KMS.LocalHSM.Seed != "deadbeef" {
		t.Errorf("kms.local_hsm.seed = %q", cfg.KMS.LocalHSM.Seed)
	}
}

// TestEnvOverridesTOMLForAuthKMS pins env > TOML precedence for the auth
// + kms knobs.
func TestEnvOverridesTOMLForAuthKMS(t *testing.T) {
	clearEnv(t)

	body := `
[auth]
sts_duration = "2h"
key_max_age = "240h"

[kms]
dek_cache_ttl = "10m"
default_key_id = "alias/from-toml"

[kms.vault]
address = "https://vault.toml"
mount = "transit-toml"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "strata.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	t.Setenv("STRATA_CONFIG_FILE", path)
	t.Setenv("STRATA_STS_DURATION", "30m")
	t.Setenv("STRATA_KEY_MAX_AGE", "168h")
	t.Setenv("STRATA_DEK_CACHE_TTL", "45s")
	t.Setenv("STRATA_KMS_DEFAULT_KEY_ID", "alias/from-env")
	t.Setenv("STRATA_KMS_VAULT_ADDR", "https://vault.env")
	t.Setenv("STRATA_KMS_VAULT_PATH", "transit-env")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.STSDuration != 30*time.Minute {
		t.Errorf("auth.sts_duration env override: %v", cfg.Auth.STSDuration)
	}
	if cfg.Auth.KeyMaxAge != 168*time.Hour {
		t.Errorf("auth.key_max_age env override: %v", cfg.Auth.KeyMaxAge)
	}
	if cfg.KMS.DEKCacheTTL != 45*time.Second {
		t.Errorf("kms.dek_cache_ttl env override: %v", cfg.KMS.DEKCacheTTL)
	}
	if cfg.KMS.DefaultKeyID != "alias/from-env" {
		t.Errorf("kms.default_key_id env override: %q", cfg.KMS.DefaultKeyID)
	}
	if cfg.KMS.Vault.Address != "https://vault.env" {
		t.Errorf("kms.vault.address env override: %q", cfg.KMS.Vault.Address)
	}
	if cfg.KMS.Vault.Mount != "transit-env" {
		t.Errorf("kms.vault.mount env override: %q", cfg.KMS.Vault.Mount)
	}
}

// TestClampAuthKMSOnLoad enforces range clamps for both env and TOML
// sources, matching the legacy env-only clamps in serverapp.
func TestClampAuthKMSOnLoad(t *testing.T) {
	clearEnv(t)

	body := `
[auth]
sts_duration = "100h"
key_max_age = "9000h"

[kms]
dek_cache_ttl = "10m"
`
	// dek_cache_ttl in body is in-range; override via env to exercise the
	// above-max clamp branch.
	dir := t.TempDir()
	path := filepath.Join(dir, "strata.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	t.Setenv("STRATA_CONFIG_FILE", path)
	t.Setenv("STRATA_DEK_CACHE_TTL", "6h")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.STSDuration != 12*time.Hour {
		t.Errorf("sts_duration clamp: %v want 12h", cfg.Auth.STSDuration)
	}
	if cfg.Auth.KeyMaxAge != 365*24*time.Hour {
		t.Errorf("key_max_age clamp: %v want 365d", cfg.Auth.KeyMaxAge)
	}
	if cfg.KMS.DEKCacheTTL != time.Hour {
		t.Errorf("dek_cache_ttl clamp: %v want 1h", cfg.KMS.DEKCacheTTL)
	}
}

// TestDefaultAuthKMSKnobs pins the default values that the drift-lint
// in US-006 will hash against.
func TestDefaultAuthKMSKnobs(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.STSDuration != time.Hour {
		t.Errorf("auth.sts_duration default: %v want 1h", cfg.Auth.STSDuration)
	}
	if cfg.Auth.KeyMaxAge != 90*24*time.Hour {
		t.Errorf("auth.key_max_age default: %v want 2160h", cfg.Auth.KeyMaxAge)
	}
	if cfg.KMS.DEKCacheTTL != 5*time.Minute {
		t.Errorf("kms.dek_cache_ttl default: %v want 5m", cfg.KMS.DEKCacheTTL)
	}
	if cfg.KMS.Adapter != "" {
		t.Errorf("kms.adapter default: %q want empty (auto-precedence)", cfg.KMS.Adapter)
	}
}
