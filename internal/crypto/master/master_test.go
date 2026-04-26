package master

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	hexKey32 = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	hexKey16 = "0102030405060708090a0b0c0d0e0f10"
	hexKeyB  = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
)

func TestEnvProvider_Resolve(t *testing.T) {
	t.Setenv(EnvMasterKey, hexKey32)
	t.Setenv(EnvMasterKeyID, "")

	p := NewEnvProvider()
	key, id, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(key) != KeySize {
		t.Fatalf("key len = %d, want %d", len(key), KeySize)
	}
	if id != DefaultEnvKeyID {
		t.Fatalf("id = %q, want %q", id, DefaultEnvKeyID)
	}
}

func TestEnvProvider_CustomKeyID(t *testing.T) {
	t.Setenv(EnvMasterKey, hexKey32)
	t.Setenv(EnvMasterKeyID, "rot-2026-04")

	_, id, err := NewEnvProvider().Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != "rot-2026-04" {
		t.Fatalf("id = %q", id)
	}
}

func TestEnvProvider_Missing(t *testing.T) {
	t.Setenv(EnvMasterKey, "")
	_, _, err := NewEnvProvider().Resolve(context.Background())
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("err = %v, want ErrNoConfig", err)
	}
}

func TestEnvProvider_BadHex(t *testing.T) {
	t.Setenv(EnvMasterKey, "not-hex-zz")
	_, _, err := NewEnvProvider().Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid hex") {
		t.Fatalf("err = %v, want invalid hex", err)
	}
}

func TestEnvProvider_WrongLength(t *testing.T) {
	t.Setenv(EnvMasterKey, hexKey16)
	_, _, err := NewEnvProvider().Resolve(context.Background())
	if !errors.Is(err, ErrInvalidKeyLength) {
		t.Fatalf("err = %v, want ErrInvalidKeyLength", err)
	}
}

func TestEnvProvider_HotReload(t *testing.T) {
	t.Setenv(EnvMasterKey, hexKey32)
	t.Setenv(EnvMasterKeyID, "k1")
	p := NewEnvProvider()
	k1, id1, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve1: %v", err)
	}

	t.Setenv(EnvMasterKey, hexKeyB)
	t.Setenv(EnvMasterKeyID, "k2")
	k2, id2, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve2: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("id should change after env reload")
	}
	if string(k1) == string(k2) {
		t.Fatalf("key bytes should change after env reload")
	}
}

func writeKeyFile(t *testing.T, path, hexKey string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(hexKey+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestFileProvider_Resolve(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	writeKeyFile(t, path, hexKey32)

	t.Setenv(EnvMasterKeyID, "")
	p := NewFileProvider(path)
	key, id, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(key) != KeySize {
		t.Fatalf("key len = %d", len(key))
	}
	if id != DefaultFileKeyID {
		t.Fatalf("id = %q, want %q", id, DefaultFileKeyID)
	}
}

func TestFileProvider_Missing(t *testing.T) {
	p := NewFileProvider(filepath.Join(t.TempDir(), "does-not-exist"))
	_, _, err := p.Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stat master key file") {
		t.Fatalf("err = %v", err)
	}
}

func TestFileProvider_BadLength(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	writeKeyFile(t, path, hexKey16)
	_, _, err := NewFileProvider(path).Resolve(context.Background())
	if !errors.Is(err, ErrInvalidKeyLength) {
		t.Fatalf("err = %v, want ErrInvalidKeyLength", err)
	}
}

func TestFileProvider_HotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	writeKeyFile(t, path, hexKey32)

	// Force an mtime far enough in the past that a rewrite is observable on
	// any filesystem.
	old := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	p := NewFileProvider(path)
	k1, _, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve1: %v", err)
	}

	// Cache hit when mtime unchanged.
	k1b, _, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve1b: %v", err)
	}
	if &k1[0] != &k1b[0] {
		// Same backing array — cache hit.
		// (We don't strictly require pointer equality; just byte equality.)
		if string(k1) != string(k1b) {
			t.Fatalf("cache hit returned different bytes")
		}
	}

	// Rewrite with new contents and bump mtime.
	writeKeyFile(t, path, hexKeyB)
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	k2, _, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve2: %v", err)
	}
	if string(k1) == string(k2) {
		t.Fatalf("hot-reload did not pick up new key bytes")
	}
}

func TestFileProvider_BadHexFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("not hex zz"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := NewFileProvider(path).Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid hex") {
		t.Fatalf("err = %v", err)
	}
}

func TestFromEnv_FilePreferred(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	writeKeyFile(t, path, hexKey32)

	t.Setenv(EnvMasterKeyVault, "")
	t.Setenv(EnvMasterKeyFile, path)
	t.Setenv(EnvMasterKey, hexKeyB)

	p, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if _, ok := p.(*FileProvider); !ok {
		t.Fatalf("got %T, want *FileProvider when both vars set", p)
	}
}

func TestFromEnv_EnvFallback(t *testing.T) {
	t.Setenv(EnvMasterKeyVault, "")
	t.Setenv(EnvMasterKeyFile, "")
	t.Setenv(EnvMasterKey, hexKey32)

	p, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if _, ok := p.(*EnvProvider); !ok {
		t.Fatalf("got %T, want *EnvProvider", p)
	}
}

func TestFromEnv_NoConfig(t *testing.T) {
	t.Setenv(EnvMasterKeyVault, "")
	t.Setenv(EnvMasterKeyFile, "")
	t.Setenv(EnvMasterKey, "")
	_, err := FromEnv()
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("err = %v", err)
	}
}
