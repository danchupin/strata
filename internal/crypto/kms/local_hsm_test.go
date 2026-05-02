package kms

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
)

func newSeed(t *testing.T) []byte {
	t.Helper()
	s := make([]byte, localHSMSeedSize)
	if _, err := rand.Read(s); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return s
}

func TestLocalHSMRoundTrip(t *testing.T) {
	seed := newSeed(t)
	p, err := NewLocalHSMProvider(seed)
	if err != nil {
		t.Fatalf("NewLocalHSMProvider: %v", err)
	}
	plain, wrapped, err := p.GenerateDataKey(context.Background(), "alias/test")
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if len(plain) != DEKSize {
		t.Fatalf("plain size = %d, want %d", len(plain), DEKSize)
	}
	got, err := p.UnwrapDEK(context.Background(), "alias/test", wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("UnwrapDEK plaintext mismatch")
	}
}

func TestLocalHSMUnwrapKeyIDMismatch(t *testing.T) {
	p, err := NewLocalHSMProvider(newSeed(t))
	if err != nil {
		t.Fatalf("NewLocalHSMProvider: %v", err)
	}
	_, wrapped, err := p.GenerateDataKey(context.Background(), "key-A")
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	_, err = p.UnwrapDEK(context.Background(), "key-B", wrapped)
	if !errors.Is(err, ErrKeyIDMismatch) {
		t.Fatalf("UnwrapDEK wrong keyID err=%v, want ErrKeyIDMismatch", err)
	}
}

func TestLocalHSMDeterministicAcrossInstances(t *testing.T) {
	seed := newSeed(t)
	p1, _ := NewLocalHSMProvider(seed)
	p2, _ := NewLocalHSMProvider(seed)

	plain, wrapped, err := p1.GenerateDataKey(context.Background(), "alias/k")
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	// p2 (different instance, same seed) should unwrap successfully.
	got, err := p2.UnwrapDEK(context.Background(), "alias/k", wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK on second instance: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("second-instance plaintext mismatch")
	}
}

func TestLocalHSMUniqueDEKsPerCall(t *testing.T) {
	p, _ := NewLocalHSMProvider(newSeed(t))
	plain1, _, _ := p.GenerateDataKey(context.Background(), "k")
	plain2, _, _ := p.GenerateDataKey(context.Background(), "k")
	if bytes.Equal(plain1, plain2) {
		t.Fatal("two calls produced identical DEKs (nonce not random)")
	}
}

func TestLocalHSMMissingKeyID(t *testing.T) {
	p, _ := NewLocalHSMProvider(newSeed(t))
	if _, _, err := p.GenerateDataKey(context.Background(), ""); !errors.Is(err, ErrMissingKeyID) {
		t.Fatalf("GenerateDataKey empty keyID err=%v", err)
	}
	if _, err := p.UnwrapDEK(context.Background(), "", make([]byte, localHSMNonceSize+localHSMTagSize)); !errors.Is(err, ErrMissingKeyID) {
		t.Fatalf("UnwrapDEK empty keyID err=%v", err)
	}
}

func TestLocalHSMBadSeedLength(t *testing.T) {
	if _, err := NewLocalHSMProvider(make([]byte, 16)); err == nil {
		t.Fatal("expected error for short seed")
	}
}

func TestLocalHSMUnwrapBadLength(t *testing.T) {
	p, _ := NewLocalHSMProvider(newSeed(t))
	if _, err := p.UnwrapDEK(context.Background(), "k", []byte{1, 2, 3}); err == nil {
		t.Fatal("expected error for short wrapped blob")
	}
}

func TestLocalHSMFromEnv(t *testing.T) {
	t.Setenv(EnvLocalHSMSeed, "")
	if _, err := NewLocalHSMProviderFromEnv(); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("empty env err=%v, want ErrNoConfig", err)
	}

	t.Setenv(EnvLocalHSMSeed, "not-hex")
	if _, err := NewLocalHSMProviderFromEnv(); err == nil {
		t.Fatal("expected hex decode error")
	}

	seed := make([]byte, localHSMSeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	t.Setenv(EnvLocalHSMSeed, hex.EncodeToString(seed))
	p, err := NewLocalHSMProviderFromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	plain, wrapped, err := p.GenerateDataKey(context.Background(), "k")
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	got, err := p.UnwrapDEK(context.Background(), "k", wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("plaintext mismatch")
	}

	// Wrong-length seed via env
	t.Setenv(EnvLocalHSMSeed, hex.EncodeToString(seed[:16]))
	if _, err := NewLocalHSMProviderFromEnv(); err == nil {
		t.Fatal("expected error for short hex seed")
	}
}
