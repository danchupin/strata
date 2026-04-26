package master

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRotationProvider_Resolve(t *testing.T) {
	p, err := NewRotationProvider([]KeyEntry{
		{ID: "k1", Key: mustHex(hexKey32)},
		{ID: "k2", Key: mustHex(hexKeyB)},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	key, id, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != "k1" {
		t.Fatalf("id = %q, want k1 (active)", id)
	}
	if string(key) != string(mustHex(hexKey32)) {
		t.Fatalf("Resolve returned wrong key bytes for active id")
	}
	if got := p.ActiveID(); got != "k1" {
		t.Fatalf("ActiveID = %q", got)
	}
	if ids := p.IDs(); len(ids) != 2 || ids[0] != "k1" || ids[1] != "k2" {
		t.Fatalf("IDs = %v", ids)
	}
}

func TestRotationProvider_ResolveByID(t *testing.T) {
	p, err := NewRotationProvider([]KeyEntry{
		{ID: "active", Key: mustHex(hexKey32)},
		{ID: "old", Key: mustHex(hexKeyB)},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.ResolveByID(context.Background(), "old")
	if err != nil {
		t.Fatalf("resolve old: %v", err)
	}
	if string(got) != string(mustHex(hexKeyB)) {
		t.Fatalf("wrong key bytes for 'old'")
	}
	got, err = p.ResolveByID(context.Background(), "active")
	if err != nil {
		t.Fatalf("resolve active: %v", err)
	}
	if string(got) != string(mustHex(hexKey32)) {
		t.Fatalf("wrong key bytes for 'active'")
	}
	if _, err := p.ResolveByID(context.Background(), "nope"); !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("err = %v, want ErrUnknownKeyID", err)
	}
}

func TestRotationProvider_Empty(t *testing.T) {
	if _, err := NewRotationProvider(nil); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("err = %v", err)
	}
}

func TestRotationProvider_BadKeyLength(t *testing.T) {
	_, err := NewRotationProvider([]KeyEntry{
		{ID: "k1", Key: make([]byte, 16)},
	})
	if !errors.Is(err, ErrInvalidKeyLength) {
		t.Fatalf("err = %v", err)
	}
}

func TestRotationProvider_DuplicateID(t *testing.T) {
	_, err := NewRotationProvider([]KeyEntry{
		{ID: "same", Key: mustHex(hexKey32)},
		{ID: "same", Key: mustHex(hexKeyB)},
	})
	if !errors.Is(err, ErrDuplicateKeyID) {
		t.Fatalf("err = %v", err)
	}
}

func TestRotationProvider_EmptyID(t *testing.T) {
	_, err := NewRotationProvider([]KeyEntry{
		{ID: "", Key: mustHex(hexKey32)},
	})
	if err == nil || !strings.Contains(err.Error(), "empty key id") {
		t.Fatalf("err = %v", err)
	}
}

func TestRotationProvider_FromEnv(t *testing.T) {
	t.Setenv(EnvMasterKeys, "k1:"+hexKey32+",k2:"+hexKeyB)
	t.Setenv(EnvMasterKeyVault, "")
	t.Setenv(EnvMasterKeyFile, "")
	t.Setenv(EnvMasterKey, "")

	p, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	rp, ok := p.(*RotationProvider)
	if !ok {
		t.Fatalf("got %T, want *RotationProvider", p)
	}
	if rp.ActiveID() != "k1" {
		t.Fatalf("ActiveID = %q", rp.ActiveID())
	}
	got, err := rp.ResolveByID(context.Background(), "k2")
	if err != nil || string(got) != string(mustHex(hexKeyB)) {
		t.Fatalf("ResolveByID k2: %v", err)
	}
}

func TestRotationProvider_FromEnv_Whitespace(t *testing.T) {
	t.Setenv(EnvMasterKeys, "  k1 : "+hexKey32+" , k2 : "+hexKeyB+" ,")
	t.Setenv(EnvMasterKeyVault, "")
	t.Setenv(EnvMasterKeyFile, "")
	t.Setenv(EnvMasterKey, "")

	p, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	rp := p.(*RotationProvider)
	if got := rp.IDs(); len(got) != 2 || got[0] != "k1" || got[1] != "k2" {
		t.Fatalf("IDs = %v", got)
	}
}

func TestRotationProvider_FromEnv_MissingHex(t *testing.T) {
	t.Setenv(EnvMasterKeys, "lonely")
	t.Setenv(EnvMasterKeyVault, "")
	t.Setenv(EnvMasterKeyFile, "")
	t.Setenv(EnvMasterKey, "")

	_, err := FromEnv()
	if err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("err = %v", err)
	}
}

func TestFromEnv_RotationPreferred(t *testing.T) {
	t.Setenv(EnvMasterKeys, "k1:"+hexKey32)
	t.Setenv(EnvMasterKeyVault, "")
	t.Setenv(EnvMasterKeyFile, "")
	t.Setenv(EnvMasterKey, hexKeyB)

	p, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if _, ok := p.(*RotationProvider); !ok {
		t.Fatalf("got %T, want *RotationProvider when STRATA_SSE_MASTER_KEYS set", p)
	}
}

func TestResolveByID_Resolver(t *testing.T) {
	p, err := NewRotationProvider([]KeyEntry{
		{ID: "k1", Key: mustHex(hexKey32)},
		{ID: "k2", Key: mustHex(hexKeyB)},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveByID(context.Background(), p, "k2")
	if err != nil {
		t.Fatalf("ResolveByID: %v", err)
	}
	if string(got) != string(mustHex(hexKeyB)) {
		t.Fatalf("wrong key bytes")
	}
}

func TestResolveByID_SingleProviderMatching(t *testing.T) {
	t.Setenv(EnvMasterKey, hexKey32)
	t.Setenv(EnvMasterKeyID, "env-1")
	p := NewEnvProvider()

	got, err := ResolveByID(context.Background(), p, "env-1")
	if err != nil {
		t.Fatalf("ResolveByID matching: %v", err)
	}
	if string(got) != string(mustHex(hexKey32)) {
		t.Fatalf("wrong key bytes")
	}
}

func TestResolveByID_SingleProviderEmptyKeyID(t *testing.T) {
	t.Setenv(EnvMasterKey, hexKey32)
	t.Setenv(EnvMasterKeyID, "env-1")
	p := NewEnvProvider()

	got, err := ResolveByID(context.Background(), p, "")
	if err != nil {
		t.Fatalf("ResolveByID empty id (legacy row): %v", err)
	}
	if string(got) != string(mustHex(hexKey32)) {
		t.Fatalf("wrong key bytes for empty-id legacy row")
	}
}

func TestResolveByID_SingleProviderMismatching(t *testing.T) {
	t.Setenv(EnvMasterKey, hexKey32)
	t.Setenv(EnvMasterKeyID, "env-1")
	p := NewEnvProvider()

	_, err := ResolveByID(context.Background(), p, "old-key")
	if !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("err = %v, want ErrUnknownKeyID", err)
	}
}

func mustHex(s string) []byte {
	b, err := decodeHexKey(s)
	if err != nil {
		panic(err)
	}
	return b
}
