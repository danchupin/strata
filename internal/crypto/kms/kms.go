// Package kms defines the KMS Provider abstraction used to wrap and unwrap
// per-object DEKs for SSE-KMS. Each provider talks to an external KMS (Vault
// Transit, AWS KMS) or an in-process stub for tests; callers persist the
// wrapped DEK plus the key id on the object row and pass them back on read.
package kms

import (
	"context"
	"errors"
)

// ErrNoConfig is returned by FromEnv when no provider env vars are set.
var ErrNoConfig = errors.New("strata/crypto/kms: no provider configured")

// ErrMissingKeyID is returned when GenerateDataKey or UnwrapDEK is called with
// an empty key id.
var ErrMissingKeyID = errors.New("strata/crypto/kms: key id required")

// ErrKMSUnavailable signals a transient provider failure (network
// timeout, throttling, sealed Vault, etc.) where the caller MUST surface
// a 503 retry rather than fail-closed-as-denied (US-002). Providers wrap
// retryable errors with this sentinel via WrapTransient; the auth-side
// resolver translates it into auth.ErrKMSUnavailable so the gateway
// emits HTTP 503 KMSUnavailable + Retry-After:30. Non-retryable errors
// (ErrKeyIDMismatch / ErrMissingKeyID / opaque crypto failures) bypass
// this and surface as 401 KeyDenied.
var ErrKMSUnavailable = errors.New("strata/crypto/kms: provider unavailable")

// Provider wraps and unwraps per-object DEKs against a remote KMS.
// Implementations must be safe for concurrent use.
type Provider interface {
	// GenerateDataKey returns a fresh 32-byte plaintext DEK plus its wrapped
	// form under the named KMS key. The wrapped form is opaque to the caller —
	// it is whatever the underlying KMS persists with the request to decrypt.
	GenerateDataKey(ctx context.Context, keyID string) (plaintextDEK, wrappedDEK []byte, err error)

	// UnwrapDEK reverses GenerateDataKey. The keyID and wrapped blob must be
	// the exact pair returned by a prior GenerateDataKey call; mismatches
	// surface as an error from the underlying KMS.
	UnwrapDEK(ctx context.Context, keyID string, wrapped []byte) (plaintextDEK []byte, err error)
}

// EnvOption configures FromEnv. Use WithAWSKMSClientFactory to enable the
// aws-kms provider.
type EnvOption func(*envCfg)

type envCfg struct {
	awsKMSFactory func(region string) (KMSAPI, error)
}

// WithAWSKMSClientFactory wires the AWS SDK KMS client constructor used by
// FromEnv when STRATA_KMS_AWS_REGION is set. Without the factory, FromEnv
// rejects the aws-kms config so misconfiguration surfaces at startup rather
// than per-object on the first PUT.
func WithAWSKMSClientFactory(factory func(region string) (KMSAPI, error)) EnvOption {
	return func(c *envCfg) { c.awsKMSFactory = factory }
}

// FromEnv returns a Provider built from the first env-recognised configuration.
// Precedence: vault > aws-kms > local-hsm.
func FromEnv(opts ...EnvOption) (Provider, error) {
	cfg := envCfg{}
	for _, opt := range opts {
		opt(&cfg)
	}
	if p, err := NewVaultProviderFromEnv(); err == nil {
		return p, nil
	} else if !errors.Is(err, ErrNoConfig) {
		return nil, err
	}
	if p, err := NewAWSKMSProviderFromEnv(cfg.awsKMSFactory); err == nil {
		return p, nil
	} else if !errors.Is(err, ErrNoConfig) {
		return nil, err
	}
	if p, err := NewLocalHSMProviderFromEnv(); err == nil {
		return p, nil
	} else if !errors.Is(err, ErrNoConfig) {
		return nil, err
	}
	return nil, ErrNoConfig
}

// Config mirrors the relevant fields from internal/config.KMSConfig. The
// kms package keeps a self-contained shape so it does not import
// internal/config (which would invert the dependency direction; config is
// allowed to know about kms, not vice versa). serverapp builds this
// struct from cfg.KMS before calling FromConfig.
type Config struct {
	Adapter      string // "vault" | "aws" | "local_hsm" | "" (auto via precedence)
	AWSRegion    string
	AWSEndpoint  string
	VaultAddr    string
	VaultPath    string
	VaultToken   string
	VaultRoleID  string
	VaultSecret  string
	LocalHSMSeed string
}

// FromConfig builds a Provider from an explicit config. When Adapter is
// non-empty it picks that provider directly; empty falls back to the same
// precedence as FromEnv (vault > aws > local_hsm) using cfg fields instead
// of env reads. Returns ErrNoConfig when no provider can be built.
func FromConfig(cfg Config, opts ...EnvOption) (Provider, error) {
	envOpts := envCfg{}
	for _, opt := range opts {
		opt(&envOpts)
	}
	switch cfg.Adapter {
	case "vault":
		return newVaultFromConfig(cfg)
	case "aws", "aws_kms", "aws-kms":
		return newAWSFromConfig(cfg, envOpts.awsKMSFactory)
	case "local_hsm", "local-hsm", "localhsm":
		return newLocalHSMFromConfig(cfg)
	case "":
		// auto-precedence
	default:
		return nil, errors.New("strata/crypto/kms: unknown adapter " + cfg.Adapter)
	}
	if cfg.VaultAddr != "" && cfg.VaultPath != "" {
		return newVaultFromConfig(cfg)
	}
	if cfg.AWSRegion != "" {
		return newAWSFromConfig(cfg, envOpts.awsKMSFactory)
	}
	if cfg.LocalHSMSeed != "" {
		return newLocalHSMFromConfig(cfg)
	}
	return nil, ErrNoConfig
}
