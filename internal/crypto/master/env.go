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
