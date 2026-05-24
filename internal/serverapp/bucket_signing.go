package serverapp

import (
	"log/slog"
	"os"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/crypto/kms"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
)

const (
	defaultDEKCacheTTL = 5 * time.Minute
	minDEKCacheTTL     = 30 * time.Second
	maxDEKCacheTTL     = 1 * time.Hour
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
		CounterInc: func(prov, outcome string) {
			metrics.KMSDecryptTotal.WithLabelValues(prov, outcome).Inc()
		},
	}
}
