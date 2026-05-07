package serverapp

import (
	"bytes"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBootstrapSharedJWTSecret_EmptyDirCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")

	b, ok := bootstrapSharedJWTSecret(path, discardLogger())
	if !ok {
		t.Fatalf("bootstrapSharedJWTSecret: ok=false, want true")
	}
	if len(b) != 32 {
		t.Fatalf("secret length = %d, want 32", len(b))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	decoded, err := hex.DecodeString(string(bytes.TrimSpace(raw)))
	if err != nil {
		t.Fatalf("hex decode: %v", err)
	}
	if !bytes.Equal(decoded, b) {
		t.Fatalf("file payload != returned secret")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("file mode = %o, want 0600", mode)
	}
}

func TestBootstrapSharedJWTSecret_PreexistingFileReturnedVerbatim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	want := bytes.Repeat([]byte{0xab}, 32)
	if err := os.WriteFile(path, []byte(hex.EncodeToString(want)), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	got, ok := bootstrapSharedJWTSecret(path, discardLogger())
	if !ok {
		t.Fatalf("ok=false")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("returned secret != seeded; got %x want %x", got, want)
	}
}

func TestBootstrapSharedJWTSecret_ConcurrentCallsAgree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")

	const n = 4
	var wg sync.WaitGroup
	results := make([][]byte, n)
	oks := make([]bool, n)
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			b, ok := bootstrapSharedJWTSecret(path, discardLogger())
			results[i] = b
			oks[i] = ok
		}()
	}
	wg.Wait()

	for i, ok := range oks {
		if !ok {
			t.Fatalf("goroutine %d: ok=false", i)
		}
	}
	for i := 1; i < n; i++ {
		if !bytes.Equal(results[0], results[i]) {
			t.Fatalf("goroutine %d secret diverges from goroutine 0:\n  0: %x\n  %d: %x", i, results[0], i, results[i])
		}
	}
}

func TestBootstrapSharedJWTSecret_EmptyPathSkips(t *testing.T) {
	if _, ok := bootstrapSharedJWTSecret("", discardLogger()); ok {
		t.Fatalf("ok=true for empty path, want false")
	}
}

func TestBootstrapSharedJWTSecret_MissingParentDirFallsThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "secret")
	if _, ok := bootstrapSharedJWTSecret(path, discardLogger()); ok {
		t.Fatalf("ok=true for missing parent dir, want false")
	}
}

func TestLoadJWTSecretFrom_EnvWinsAndDoesNotTouchSharedFile(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "secret")
	envHex := hex.EncodeToString(bytes.Repeat([]byte{0x55}, 32))

	b, source, target := loadJWTSecretFrom(envHex, "/some/where/jwt-secret", shared, discardLogger())
	if source != "STRATA_CONSOLE_JWT_SECRET" {
		t.Fatalf("source = %q, want STRATA_CONSOLE_JWT_SECRET", source)
	}
	if target != "/some/where/jwt-secret" {
		t.Fatalf("target = %q, want /some/where/jwt-secret", target)
	}
	if !bytes.Equal(b, bytes.Repeat([]byte{0x55}, 32)) {
		t.Fatalf("decoded secret mismatch")
	}
	if _, err := os.Stat(shared); !os.IsNotExist(err) {
		t.Fatalf("shared file unexpectedly created (err=%v); env-set path must NOT touch shared file", err)
	}
}

func TestLoadJWTSecretFrom_SecretFileWinsAndDoesNotTouchSharedFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "rotated")
	shared := filepath.Join(dir, "shared")
	want := bytes.Repeat([]byte{0x77}, 32)
	if err := os.WriteFile(secretPath, []byte(hex.EncodeToString(want)), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	b, source, _ := loadJWTSecretFrom("", secretPath, shared, discardLogger())
	if source != "STRATA_JWT_SECRET_FILE" {
		t.Fatalf("source = %q, want STRATA_JWT_SECRET_FILE", source)
	}
	if !bytes.Equal(b, want) {
		t.Fatalf("secret mismatch")
	}
	if _, err := os.Stat(shared); !os.IsNotExist(err) {
		t.Fatalf("shared file unexpectedly created (err=%v); rotated-file path must NOT touch shared file", err)
	}
}

func TestLoadJWTSecretFrom_FallsThroughToShared(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "missing-rotated")
	shared := filepath.Join(dir, "shared")

	b, source, _ := loadJWTSecretFrom("", secretPath, shared, discardLogger())
	if source != "STRATA_JWT_SHARED" {
		t.Fatalf("source = %q, want STRATA_JWT_SHARED", source)
	}
	if len(b) != 32 {
		t.Fatalf("secret len = %d, want 32", len(b))
	}
	if _, err := os.Stat(shared); err != nil {
		t.Fatalf("shared file should be created; Stat: %v", err)
	}
}
