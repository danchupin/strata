package master

import (
	"context"
	"os"
)

// EnvProvider resolves the master key from process environment variables on
// every call. Hot-reload semantics: setenv between calls is observed.
type EnvProvider struct {
	keyVar string
	idVar  string
}

// NewEnvProvider returns a provider bound to STRATA_SSE_MASTER_KEY /
// STRATA_SSE_MASTER_KEY_ID.
func NewEnvProvider() *EnvProvider {
	return &EnvProvider{keyVar: EnvMasterKey, idVar: EnvMasterKeyID}
}

// newEnvFromConfig wraps cfg.Key + cfg.KeyID into a frozen single-entry
// RotationProvider. Unlike NewEnvProvider it does not re-read os.Getenv
// on every Resolve — the cfg-driven path captures the value once at
// boot.
func newEnvFromConfig(cfg Config) (Provider, error) {
	key, err := decodeHexKey(cfg.Key)
	if err != nil {
		return nil, err
	}
	id := cfg.KeyID
	if id == "" {
		id = DefaultEnvKeyID
	}
	return NewRotationProvider([]KeyEntry{{ID: id, Key: key}})
}

// Resolve reads the configured env var, hex-decodes it, and validates length.
func (p *EnvProvider) Resolve(_ context.Context) ([]byte, string, error) {
	key, err := decodeHexKey(os.Getenv(p.keyVar))
	if err != nil {
		return nil, "", err
	}
	id := os.Getenv(p.idVar)
	if id == "" {
		id = DefaultEnvKeyID
	}
	return key, id, nil
}
