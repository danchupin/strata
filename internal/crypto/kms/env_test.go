package kms

import (
	"encoding/hex"
	"errors"
	"testing"
)

// clearAllProviderEnv resets every provider env var so FromEnv tests do not
// pick up ambient process state. Equivalent of the master-key story rule:
// "every test that calls FromEnv must Setenv ALL precedence vars".
func clearAllProviderEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvVaultAddr, "")
	t.Setenv(EnvVaultPath, "")
	t.Setenv(EnvVaultRoleID, "")
	t.Setenv(EnvVaultSecretID, "")
	t.Setenv(EnvAWSKMSRegion, "")
	t.Setenv(EnvLocalHSMSeed, "")
}

func TestFromEnv_PicksAWSKMS(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv(EnvAWSKMSRegion, "us-east-1")
	fk := newFakeKMS("k")
	p, err := FromEnv(WithAWSKMSClientFactory(func(region string) (KMSAPI, error) {
		if region != "us-east-1" {
			t.Fatalf("region=%q", region)
		}
		return fk, nil
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if _, ok := p.(*AWSKMSProvider); !ok {
		t.Fatalf("got %T, want *AWSKMSProvider", p)
	}
}

func TestFromEnv_AWSKMSWithoutFactoryFails(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv(EnvAWSKMSRegion, "us-east-1")
	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error: aws region set without factory")
	}
	if errors.Is(err, ErrNoConfig) {
		t.Fatal("aws-kms misconfig must not surface as ErrNoConfig")
	}
}

func TestFromEnv_PicksLocalHSM(t *testing.T) {
	clearAllProviderEnv(t)
	seed := make([]byte, localHSMSeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	t.Setenv(EnvLocalHSMSeed, hex.EncodeToString(seed))
	p, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if _, ok := p.(*LocalHSMProvider); !ok {
		t.Fatalf("got %T, want *LocalHSMProvider", p)
	}
}

func TestFromEnv_VaultBeatsAWSKMSBeatsLocalHSM(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv(EnvVaultAddr, "https://vault.example.com")
	t.Setenv(EnvVaultPath, "transit")
	t.Setenv(EnvVaultRoleID, "r")
	t.Setenv(EnvVaultSecretID, "s")
	t.Setenv(EnvAWSKMSRegion, "us-east-1")
	seed := make([]byte, localHSMSeedSize)
	t.Setenv(EnvLocalHSMSeed, hex.EncodeToString(seed))

	p, err := FromEnv(WithAWSKMSClientFactory(func(string) (KMSAPI, error) {
		return newFakeKMS("k"), nil
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if _, ok := p.(*VaultProvider); !ok {
		t.Fatalf("got %T, want *VaultProvider (vault wins)", p)
	}
}

func TestFromEnv_AWSKMSBeatsLocalHSM(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv(EnvAWSKMSRegion, "us-east-1")
	seed := make([]byte, localHSMSeedSize)
	t.Setenv(EnvLocalHSMSeed, hex.EncodeToString(seed))

	p, err := FromEnv(WithAWSKMSClientFactory(func(string) (KMSAPI, error) {
		return newFakeKMS("k"), nil
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if _, ok := p.(*AWSKMSProvider); !ok {
		t.Fatalf("got %T, want *AWSKMSProvider (aws wins over local-hsm)", p)
	}
}
