// Package master defines the master-key Provider abstraction used to wrap and
// unwrap per-object DEKs for SSE-S3. Built-in providers source the master key
// from an environment variable or a watched file; further providers (Vault,
// rotation list) compose on top of the same Provider interface.
package master

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	// EnvMasterKey is the env var read by EnvProvider — hex-encoded 32-byte key.
	EnvMasterKey = "STRATA_SSE_MASTER_KEY"
	// EnvMasterKeyID is the optional key-id env var; defaults to DefaultEnvKeyID.
	EnvMasterKeyID = "STRATA_SSE_MASTER_KEY_ID"
	// EnvMasterKeyFile is the env var read by FromEnv to pick FileProvider.
	EnvMasterKeyFile = "STRATA_SSE_MASTER_KEY_FILE"

	// KeySize is the required raw key length (AES-256).
	KeySize = 32

	// DefaultEnvKeyID is the keyID returned by EnvProvider when no override is set.
	DefaultEnvKeyID = "env-1"
	// DefaultFileKeyID is the keyID returned by FileProvider when no override is set.
	DefaultFileKeyID = "file-1"
)

// ErrNoConfig is returned by FromEnv when none of STRATA_SSE_MASTER_KEY,
// STRATA_SSE_MASTER_KEY_FILE, or STRATA_SSE_MASTER_KEY_VAULT is set.
var ErrNoConfig = errors.New("strata: no master key configured (set STRATA_SSE_MASTER_KEY, STRATA_SSE_MASTER_KEY_FILE, or STRATA_SSE_MASTER_KEY_VAULT)")

// ErrInvalidKeyLength is returned when the decoded key is not exactly KeySize bytes.
var ErrInvalidKeyLength = errors.New("strata: master key must be 32 bytes (64 hex chars)")

// Provider yields the active master key plus a stable identifier. Implementations
// must be safe for concurrent use; callers may invoke Resolve from multiple
// goroutines on every request path.
type Provider interface {
	Resolve(ctx context.Context) (key []byte, keyID string, err error)
}

// FromEnv constructs a built-in provider based on which env var is set.
// Precedence (highest first): STRATA_SSE_MASTER_KEY_VAULT >
// STRATA_SSE_MASTER_KEY_FILE > STRATA_SSE_MASTER_KEY. Returns ErrNoConfig
// when none is set.
func FromEnv() (Provider, error) {
	if os.Getenv(EnvMasterKeyVault) != "" {
		return NewVaultProviderFromEnv()
	}
	if path := os.Getenv(EnvMasterKeyFile); path != "" {
		return NewFileProvider(path), nil
	}
	if os.Getenv(EnvMasterKey) != "" {
		return NewEnvProvider(), nil
	}
	return nil, ErrNoConfig
}

// decodeHexKey trims whitespace, hex-decodes, and enforces KeySize.
func decodeHexKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrNoConfig
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid hex master key: %w", err)
	}
	if len(decoded) != KeySize {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeyLength, len(decoded))
	}
	return decoded, nil
}
