package serverapp

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/danchupin/strata/internal/adminapi"
	"github.com/danchupin/strata/internal/auth"
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

// dekCacheTTL reads STRATA_DEK_CACHE_TTL (Go duration) and clamps to
// [30s, 1h]. Default 5m. Out-of-range values log a WARN and are clamped
// to the nearest valid bound (US-001 auth-dx-trailer-lima).
func dekCacheTTL(logger *slog.Logger) time.Duration {
	raw := os.Getenv("STRATA_DEK_CACHE_TTL")
	if raw == "" {
		return defaultDEKCacheTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		logger.Warn("STRATA_DEK_CACHE_TTL parse failed; using default",
			"value", raw, "default", defaultDEKCacheTTL.String(), "error", err.Error())
		return defaultDEKCacheTTL
	}
	switch {
	case d < minDEKCacheTTL:
		logger.Warn("STRATA_DEK_CACHE_TTL below minimum; clamping",
			"value", d.String(), "min", minDEKCacheTTL.String())
		return minDEKCacheTTL
	case d > maxDEKCacheTTL:
		logger.Warn("STRATA_DEK_CACHE_TTL above maximum; clamping",
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

// keyMaxAge reads STRATA_KEY_MAX_AGE (Go duration) and clamps to
// [24h, 8760h]. Default 90d. Out-of-range values log a WARN and are
// clamped to the nearest valid bound (US-002 auth-dx-trailer-lima).
func keyMaxAge(logger *slog.Logger) time.Duration {
	raw := os.Getenv("STRATA_KEY_MAX_AGE")
	if raw == "" {
		return defaultKeyMaxAge
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		logger.Warn("STRATA_KEY_MAX_AGE parse failed; using default",
			"value", raw, "default", defaultKeyMaxAge.String(), "error", err.Error())
		return defaultKeyMaxAge
	}
	switch {
	case d < minKeyMaxAge:
		logger.Warn("STRATA_KEY_MAX_AGE below minimum; clamping",
			"value", d.String(), "min", minKeyMaxAge.String())
		return minKeyMaxAge
	case d > maxKeyMaxAge:
		logger.Warn("STRATA_KEY_MAX_AGE above maximum; clamping",
			"value", d.String(), "max", maxKeyMaxAge.String())
		return maxKeyMaxAge
	}
	return d
}

// buildBucketSigningResolver wires the meta-store signing-key surface,
// the configured KMS provider, a wall-clock TTL cache and the
// strata_kms_decrypt_total counter into a single resolver consumed by
// the SigV4 middleware.
func buildBucketSigningResolver(store meta.Store, provider kms.Provider, logger *slog.Logger) *auth.BucketSigningResolver {
	ttl := dekCacheTTL(logger)
	cache := auth.NewDEKCache(ttl)
	label := kmsProviderLabel(provider)
	return &auth.BucketSigningResolver{
		Store:    store,
		KMS:      provider,
		Provider: label,
		Cache:    cache,
		MaxAge:   keyMaxAge(logger),
		CounterInc: func(prov, outcome string) {
			metrics.KMSDecryptTotal.WithLabelValues(prov, outcome).Inc()
		},
		ClassifyUnwrap: classifyKMSUnwrapErr,
	}
}

// buildSigningKeyAdminConfig wires the operator-facing admin surface
// (US-002): the live KMS provider for Rotate, the auth resolver's DEK
// cache so Rotate / Delete drop the cached entry synchronously, the
// default CMK id from STRATA_KMS_DEFAULT_KEY_ID (empty falls back to
// the bucket name per resolveCMKID), and the max-age window for the
// Status endpoint's expired flag. Zero value is returned when no KMS
// provider is configured — the admin endpoints surface 503
// SigningKeyDisabled in that case.
func buildSigningKeyAdminConfig(provider kms.Provider, resolver *auth.BucketSigningResolver, logger *slog.Logger) adminapi.SigningKeyConfig {
	if provider == nil {
		return adminapi.SigningKeyConfig{}
	}
	cfg := adminapi.SigningKeyConfig{
		Provider:     provider,
		DefaultKeyID: os.Getenv("STRATA_KMS_DEFAULT_KEY_ID"),
		MaxAge:       keyMaxAge(logger),
	}
	if resolver != nil {
		cfg.Cache = resolver.Cache
	}
	return cfg
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
