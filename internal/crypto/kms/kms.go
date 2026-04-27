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
