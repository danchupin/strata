package kms

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

const (
	// EnvLocalHSMSeed is a 64-char hex seed (32 bytes) that drives the
	// local-hsm provider. Setting this enables the in-process KMS stub —
	// suitable for tests and dev only.
	EnvLocalHSMSeed = "STRATA_KMS_LOCAL_HSM_SEED"

	localHSMNonceSize = 16
	localHSMTagSize   = 32
	localHSMSeedSize  = 32
)

// LocalHSMProvider is a deterministic in-process KMS stub for tests. It does
// NOT talk to any external service and is NOT a security boundary; the seed
// lives in process memory and anyone with the seed can unwrap. Use the Vault
// or AWS providers in production.
//
// Wire format of the wrapped DEK: nonce(16) || mac(32). On UnwrapDEK we
// re-derive both the DEK and the mac from (seed, keyID, nonce); a wrong keyID
// yields a mac mismatch and ErrKeyIDMismatch.
type LocalHSMProvider struct {
	seed []byte
}

// NewLocalHSMProvider builds the provider from a 32-byte seed.
func NewLocalHSMProvider(seed []byte) (*LocalHSMProvider, error) {
	if len(seed) != localHSMSeedSize {
		return nil, fmt.Errorf("kms local-hsm: seed must be %d bytes, got %d", localHSMSeedSize, len(seed))
	}
	cp := make([]byte, len(seed))
	copy(cp, seed)
	return &LocalHSMProvider{seed: cp}, nil
}

// NewLocalHSMProviderFromEnv reads STRATA_KMS_LOCAL_HSM_SEED (hex 32 bytes)
// and returns ErrNoConfig when unset.
func NewLocalHSMProviderFromEnv() (*LocalHSMProvider, error) {
	raw := strings.TrimSpace(os.Getenv(EnvLocalHSMSeed))
	if raw == "" {
		return nil, ErrNoConfig
	}
	seed, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("kms local-hsm: %s decode: %w", EnvLocalHSMSeed, err)
	}
	return NewLocalHSMProvider(seed)
}

// GenerateDataKey returns a deterministic-from-(seed, keyID, nonce) DEK plus
// the wrapped form. The nonce is fresh per call so DEKs differ across objects
// even under the same keyID.
func (p *LocalHSMProvider) GenerateDataKey(ctx context.Context, keyID string) ([]byte, []byte, error) {
	if keyID == "" {
		return nil, nil, ErrMissingKeyID
	}
	nonce := make([]byte, localHSMNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("kms local-hsm: nonce: %w", err)
	}
	dek := p.deriveDEK(keyID, nonce)
	mac := p.deriveMAC(keyID, nonce, dek)
	wrapped := make([]byte, 0, localHSMNonceSize+localHSMTagSize)
	wrapped = append(wrapped, nonce...)
	wrapped = append(wrapped, mac...)
	return dek, wrapped, nil
}

// UnwrapDEK reverses GenerateDataKey. A keyID mismatch surfaces as
// ErrKeyIDMismatch (the recomputed mac will not match), which the gateway
// maps to AccessDenied.
func (p *LocalHSMProvider) UnwrapDEK(ctx context.Context, keyID string, wrapped []byte) ([]byte, error) {
	if keyID == "" {
		return nil, ErrMissingKeyID
	}
	if len(wrapped) != localHSMNonceSize+localHSMTagSize {
		return nil, fmt.Errorf("kms local-hsm: wrapped DEK %d bytes, want %d", len(wrapped), localHSMNonceSize+localHSMTagSize)
	}
	nonce := wrapped[:localHSMNonceSize]
	mac := wrapped[localHSMNonceSize:]
	dek := p.deriveDEK(keyID, nonce)
	want := p.deriveMAC(keyID, nonce, dek)
	if !hmac.Equal(mac, want) {
		return nil, fmt.Errorf("%w: local-hsm mac mismatch", ErrKeyIDMismatch)
	}
	return dek, nil
}

func (p *LocalHSMProvider) deriveDEK(keyID string, nonce []byte) []byte {
	h := hmac.New(sha256.New, p.seed)
	_, _ = h.Write([]byte("strata-local-hsm-dek\x00"))
	_, _ = h.Write([]byte(keyID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(nonce)
	return h.Sum(nil)
}

func (p *LocalHSMProvider) deriveMAC(keyID string, nonce, dek []byte) []byte {
	h := hmac.New(sha256.New, p.seed)
	_, _ = h.Write([]byte("strata-local-hsm-mac\x00"))
	_, _ = h.Write([]byte(keyID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(nonce)
	_, _ = h.Write(dek)
	return h.Sum(nil)
}

var _ Provider = (*LocalHSMProvider)(nil)
