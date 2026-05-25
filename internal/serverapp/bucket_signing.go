package serverapp

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/danchupin/strata/internal/adminapi"
	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/crypto/kms"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
)

const (
	defaultDEKCacheTTL = 5 * time.Minute
	minDEKCacheTTL     = 30 * time.Second
	maxDEKCacheTTL     = 1 * time.Hour

	defaultKeyMaxAge = 90 * 24 * time.Hour
	minKeyMaxAge     = 24 * time.Hour
	maxKeyMaxAge     = 365 * 24 * time.Hour
)

// dekCacheTTL resolves the DEK cache TTL from the loaded Config. cfg.KMS.
// DEKCacheTTL is already range-clamped to [30s, 1h] by config.validate;
// zero indicates "operator left it unset" and falls back to defaultDEKCacheTTL.
// The legacy env-side STRATA_DEK_CACHE_TTL still flows through cfg via
// the koanf env provider, so explicit env overrides keep working.
func dekCacheTTL(cfg *config.Config, logger *slog.Logger) time.Duration {
	if cfg == nil || cfg.KMS.DEKCacheTTL == 0 {
		return defaultDEKCacheTTL
	}
	d := cfg.KMS.DEKCacheTTL
	switch {
	case d < minDEKCacheTTL:
		logger.Warn("kms.dek_cache_ttl below minimum; clamping",
			"value", d.String(), "min", minDEKCacheTTL.String())
		return minDEKCacheTTL
	case d > maxDEKCacheTTL:
		logger.Warn("kms.dek_cache_ttl above maximum; clamping",
			"value", d.String(), "max", maxDEKCacheTTL.String())
		return maxDEKCacheTTL
	}
	return d
}

// kmsProviderLabel maps the live kms.Provider concrete type to the
// strata_kms_decrypt_total{provider="..."} label. Unknown providers
// fall back to "unknown" so the metric keeps emitting.
func kmsProviderLabel(p kms.Provider) string {
	switch p.(type) {
	case *kms.AWSKMSProvider:
		return "aws_kms"
	case *kms.VaultProvider:
		return "vault"
	case *kms.LocalHSMProvider:
		return "local_hsm"
	default:
		return "unknown"
	}
}

// keyMaxAge resolves the per-bucket signing key rotation window from the
// loaded Config. cfg.Auth.KeyMaxAge is already range-clamped to
// [24h, 8760h] by config.validate; zero falls back to defaultKeyMaxAge.
func keyMaxAge(cfg *config.Config, logger *slog.Logger) time.Duration {
	if cfg == nil || cfg.Auth.KeyMaxAge == 0 {
		return defaultKeyMaxAge
	}
	d := cfg.Auth.KeyMaxAge
	switch {
	case d < minKeyMaxAge:
		logger.Warn("auth.key_max_age below minimum; clamping",
			"value", d.String(), "min", minKeyMaxAge.String())
		return minKeyMaxAge
	case d > maxKeyMaxAge:
		logger.Warn("auth.key_max_age above maximum; clamping",
			"value", d.String(), "max", maxKeyMaxAge.String())
		return maxKeyMaxAge
	}
	return d
}

// buildBucketSigningResolver wires the meta-store signing-key surface,
// the configured KMS provider, a wall-clock TTL cache and the
// strata_kms_decrypt_total counter into a single resolver consumed by
// the SigV4 middleware.
func buildBucketSigningResolver(cfg *config.Config, store meta.Store, provider kms.Provider, logger *slog.Logger) *auth.BucketSigningResolver {
	ttl := dekCacheTTL(cfg, logger)
	cache := auth.NewDEKCache(ttl)
	label := kmsProviderLabel(provider)
	return &auth.BucketSigningResolver{
		Store:    store,
		KMS:      provider,
		Provider: label,
		Cache:    cache,
		MaxAge:   keyMaxAge(cfg, logger),
		CounterInc: func(prov, outcome string) {
			metrics.KMSDecryptTotal.WithLabelValues(prov, outcome).Inc()
		},
		ClassifyUnwrap: classifyKMSUnwrapErr,
	}
}

// buildSigningKeyAdminConfig wires the operator-facing admin surface
// (US-002): the live KMS provider for Rotate, the auth resolver's DEK
// cache so Rotate / Delete drop the cached entry synchronously, the
// default CMK id from cfg.KMS.DefaultKeyID (empty falls back to the
// bucket name per resolveCMKID), and the max-age window for the Status
// endpoint's expired flag. Zero value is returned when no KMS provider
// is configured — the admin endpoints surface 503 SigningKeyDisabled
// in that case.
func buildSigningKeyAdminConfig(cfg *config.Config, provider kms.Provider, resolver *auth.BucketSigningResolver, logger *slog.Logger) adminapi.SigningKeyConfig {
	if provider == nil {
		return adminapi.SigningKeyConfig{}
	}
	defaultKeyID := ""
	if cfg != nil {
		defaultKeyID = cfg.KMS.DefaultKeyID
	}
	acfg := adminapi.SigningKeyConfig{
		Provider:     provider,
		DefaultKeyID: defaultKeyID,
		MaxAge:       keyMaxAge(cfg, logger),
	}
	if resolver != nil {
		acfg.Cache = resolver.Cache
	}
	return acfg
}

// classifyKMSUnwrapErr translates a kms.Provider error into one of the
// auth-side typed sentinels so the gateway emits the right HTTP code
// (US-002 fail-closed). Transient errors (kms.ErrKMSUnavailable) become
// auth.ErrKMSUnavailable → 503 + Retry-After:30. Authoritative denies
// (kms.ErrKeyIDMismatch + every other unknown) become auth.ErrKMSDenied
// → 401 KeyDenied; the silent fallback to the IAM access-key path is
// deliberately closed off.
func classifyKMSUnwrapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, auth.ErrKMSUnavailable) || errors.Is(err, auth.ErrKMSDenied) || errors.Is(err, auth.ErrKMSTampered) {
		return err
	}
	if errors.Is(err, kms.ErrKMSUnavailable) {
		return fmt.Errorf("%w: %v", auth.ErrKMSUnavailable, err)
	}
	return fmt.Errorf("%w: %v", auth.ErrKMSDenied, err)
}

// kmsConfigFromAppConfig projects the relevant fields from cfg.KMS into
// the kms package's self-contained Config shape. Keeps the kms package
// free of an internal/config import (the dependency direction is config
// knows about kms, never the reverse).
func kmsConfigFromAppConfig(cfg *config.Config) kms.Config {
	if cfg == nil {
		return kms.Config{}
	}
	return kms.Config{
		Adapter:      cfg.KMS.Adapter,
		AWSRegion:    cfg.KMS.AWS.Region,
		AWSEndpoint:  cfg.KMS.AWS.Endpoint,
		VaultAddr:    cfg.KMS.Vault.Address,
		VaultPath:    cfg.KMS.Vault.Mount,
		VaultToken:   cfg.KMS.Vault.Token,
		VaultRoleID:  cfg.KMS.Vault.RoleID,
		VaultSecret:  cfg.KMS.Vault.SecretID,
		LocalHSMSeed: cfg.KMS.LocalHSM.Seed,
	}
}

// silence unused-import lint when only one helper above touches os.
var _ = os.Getenv
