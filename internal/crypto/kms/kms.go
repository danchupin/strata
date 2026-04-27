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

// FromEnv returns a Provider built from the first env-recognised configuration.
// Currently only the Vault Transit backend is wired; aws-kms and local-hsm
// land in US-037.
func FromEnv() (Provider, error) {
	if p, err := NewVaultProviderFromEnv(); err == nil {
		return p, nil
	} else if !errors.Is(err, ErrNoConfig) {
		return nil, err
	}
	return nil, ErrNoConfig
}
