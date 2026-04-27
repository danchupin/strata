package kms

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeVault stands in for a Vault Transit mount: AppRole login plus
// /datakey/plaintext/<key> and /decrypt/<key> endpoints. Symmetric round-trip
// is implemented by storing ciphertext->plaintext in an in-memory map, keyed
// by an opaque "vault:v1:<random>" string so the wrapped DEK looks realistic.
type fakeVault struct {
	t *testing.T

	loginCalls   int32
	datakeyCalls int32
	decryptCalls int32

	loginStatus   atomic.Int32
	datakeyStatus atomic.Int32
	decryptStatus atomic.Int32

	knownKeys map[string]struct{}

	mu     sync.Mutex
	wraps  map[string][]byte
	nextID int

	srv *httptest.Server
}

func newFakeVault(t *testing.T, knownKeyIDs ...string) *fakeVault {
	fv := &fakeVault{
		t:         t,
		wraps:     map[string][]byte{},
		knownKeys: map[string]struct{}{},
	}
	for _, k := range knownKeyIDs {
		fv.knownKeys[k] = struct{}{}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/approle/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fv.loginCalls, 1)
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req["role_id"] == "" || req["secret_id"] == "" {
			http.Error(w, "missing approle creds", http.StatusBadRequest)
			return
		}
		if s := fv.loginStatus.Load(); s != 0 {
			http.Error(w, "forced", int(s))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "hvs.token-" + req["role_id"],
				"lease_duration": 3600,
				"renewable":      true,
			},
		})
	})
	mux.HandleFunc("/v1/transit/datakey/plaintext/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fv.datakeyCalls, 1)
		if r.Header.Get("X-Vault-Token") == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		if s := fv.datakeyStatus.Load(); s != 0 {
			http.Error(w, "forced", int(s))
			return
		}
		keyID := strings.TrimPrefix(r.URL.Path, "/v1/transit/datakey/plaintext/")
		if _, ok := fv.knownKeys[keyID]; !ok {
			http.Error(w, "unknown key", http.StatusNotFound)
			return
		}
		dek := make([]byte, DEKSize)
		if _, err := rand.Read(dek); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fv.mu.Lock()
		fv.nextID++
		ciphertext := fmt.Sprintf("vault:v1:%s:%d", keyID, fv.nextID)
		fv.wraps[ciphertext] = dek
		fv.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"plaintext":  base64.StdEncoding.EncodeToString(dek),
				"ciphertext": ciphertext,
			},
		})
	})
	mux.HandleFunc("/v1/transit/decrypt/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fv.decryptCalls, 1)
		if r.Header.Get("X-Vault-Token") == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		if s := fv.decryptStatus.Load(); s != 0 {
			http.Error(w, "forced", int(s))
			return
		}
		keyID := strings.TrimPrefix(r.URL.Path, "/v1/transit/decrypt/")
		if _, ok := fv.knownKeys[keyID]; !ok {
			http.Error(w, "unknown key", http.StatusNotFound)
			return
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ct := req["ciphertext"]
		fv.mu.Lock()
		dek, ok := fv.wraps[ct]
		fv.mu.Unlock()
		if !ok {
			http.Error(w, "unknown ciphertext", http.StatusBadRequest)
			return
		}
		// Enforce key-id match — wrapped under one key cannot be unwrapped under another.
		if !strings.HasPrefix(ct, "vault:v1:"+keyID+":") {
			http.Error(w, "key id mismatch", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"plaintext": base64.StdEncoding.EncodeToString(dek),
			},
		})
	})

	fv.srv = httptest.NewServer(mux)
	t.Cleanup(fv.srv.Close)
	return fv
}

func (fv *fakeVault) addr() string { return fv.srv.URL }

func newTestVaultProvider(t *testing.T, fv *fakeVault) *VaultProvider {
	t.Helper()
	p, err := NewVaultProvider(VaultConfig{
		Addr:        fv.addr(),
		TransitPath: "transit",
		RoleID:      "role-1",
		SecretID:    "secret-1",
		HTTPClient:  fv.srv.Client(),
		Logger:      slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewVaultProvider: %v", err)
	}
	return p
}

func TestVaultProvider_RoundTrip(t *testing.T) {
	fv := newFakeVault(t, "strata-master")
	p := newTestVaultProvider(t, fv)

	dek, wrapped, err := p.GenerateDataKey(context.Background(), "strata-master")
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if len(dek) != DEKSize {
		t.Fatalf("dek len = %d", len(dek))
	}
	if len(wrapped) == 0 {
		t.Fatal("wrapped DEK empty")
	}

	got, err := p.UnwrapDEK(context.Background(), "strata-master", wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("unwrapped DEK does not match plaintext")
	}
	if c := atomic.LoadInt32(&fv.loginCalls); c != 1 {
		t.Fatalf("loginCalls = %d, want 1 (token cached)", c)
	}
}

func TestVaultProvider_MissingKeyID(t *testing.T) {
	fv := newFakeVault(t, "strata-master")
	p := newTestVaultProvider(t, fv)

	_, _, err := p.GenerateDataKey(context.Background(), "")
	if !errors.Is(err, ErrMissingKeyID) {
		t.Fatalf("GenerateDataKey err = %v, want ErrMissingKeyID", err)
	}

	_, err = p.UnwrapDEK(context.Background(), "", []byte("vault:v1:x:1"))
	if !errors.Is(err, ErrMissingKeyID) {
		t.Fatalf("UnwrapDEK err = %v, want ErrMissingKeyID", err)
	}
}

func TestVaultProvider_UnknownKeyID(t *testing.T) {
	fv := newFakeVault(t, "strata-master")
	p := newTestVaultProvider(t, fv)

	_, _, err := p.GenerateDataKey(context.Background(), "missing-key")
	if err == nil {
		t.Fatal("expected error for unknown key id")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want 404", err)
	}
}

func TestVaultProvider_KeyIDMismatchOnUnwrap(t *testing.T) {
	fv := newFakeVault(t, "alpha", "beta")
	p := newTestVaultProvider(t, fv)

	_, wrapped, err := p.GenerateDataKey(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}

	// Try to unwrap under a different key id — Vault refuses.
	_, err = p.UnwrapDEK(context.Background(), "beta", wrapped)
	if err == nil {
		t.Fatal("expected error on mismatched key id")
	}
}

func TestVaultProvider_LoginFailureIsFatal(t *testing.T) {
	fv := newFakeVault(t, "strata-master")
	fv.loginStatus.Store(int32(http.StatusInternalServerError))

	p := newTestVaultProvider(t, fv)
	_, _, err := p.GenerateDataKey(context.Background(), "strata-master")
	if err == nil {
		t.Fatal("expected login failure")
	}
	if !strings.Contains(err.Error(), "vault login") {
		t.Fatalf("err = %v, want vault login", err)
	}
}

func TestVaultProvider_EmptyWrappedDEK(t *testing.T) {
	fv := newFakeVault(t, "strata-master")
	p := newTestVaultProvider(t, fv)

	_, err := p.UnwrapDEK(context.Background(), "strata-master", nil)
	if err == nil || !strings.Contains(err.Error(), "empty wrapped DEK") {
		t.Fatalf("err = %v, want empty wrapped DEK", err)
	}
}

func TestVaultProvider_TokenCachedAcrossCalls(t *testing.T) {
	fv := newFakeVault(t, "strata-master")
	base := time.Unix(1_700_000_000, 0)
	now := base
	p, err := NewVaultProvider(VaultConfig{
		Addr:        fv.addr(),
		TransitPath: "transit",
		RoleID:      "role-1",
		SecretID:    "secret-1",
		HTTPClient:  fv.srv.Client(),
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewVaultProvider: %v", err)
	}
	for i := range 3 {
		if _, _, err := p.GenerateDataKey(context.Background(), "strata-master"); err != nil {
			t.Fatalf("GenerateDataKey %d: %v", i, err)
		}
		now = now.Add(10 * time.Second)
	}
	if c := atomic.LoadInt32(&fv.loginCalls); c != 1 {
		t.Fatalf("loginCalls = %d, want 1", c)
	}
}

func TestNewVaultProviderFromEnv_NoConfig(t *testing.T) {
	t.Setenv(EnvVaultAddr, "")
	t.Setenv(EnvVaultPath, "")
	_, err := NewVaultProviderFromEnv()
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("err = %v, want ErrNoConfig", err)
	}
}

func TestNewVaultProviderFromEnv_PartialConfig(t *testing.T) {
	t.Setenv(EnvVaultAddr, "https://vault.example.com")
	t.Setenv(EnvVaultPath, "")
	_, err := NewVaultProviderFromEnv()
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("addr-only err = %v, want ErrNoConfig", err)
	}

	t.Setenv(EnvVaultAddr, "")
	t.Setenv(EnvVaultPath, "transit")
	_, err = NewVaultProviderFromEnv()
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("path-only err = %v, want ErrNoConfig", err)
	}
}

func TestNewVaultProviderFromEnv_MissingCreds(t *testing.T) {
	t.Setenv(EnvVaultAddr, "https://vault.example.com")
	t.Setenv(EnvVaultPath, "transit")
	t.Setenv(EnvVaultRoleID, "")
	t.Setenv(EnvVaultSecretID, "")
	_, err := NewVaultProviderFromEnv()
	if err == nil || !strings.Contains(err.Error(), "role_id and secret_id required") {
		t.Fatalf("err = %v, want role_id/secret_id required", err)
	}
}

func TestNewVaultProviderFromEnv_OK(t *testing.T) {
	t.Setenv(EnvVaultAddr, "https://vault.example.com")
	t.Setenv(EnvVaultPath, "transit")
	t.Setenv(EnvVaultRoleID, "role-1")
	t.Setenv(EnvVaultSecretID, "secret-1")
	p, err := NewVaultProviderFromEnv()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if p == nil {
		t.Fatal("provider nil")
	}
}

func TestFromEnv_PicksVault(t *testing.T) {
	t.Setenv(EnvVaultAddr, "https://vault.example.com")
	t.Setenv(EnvVaultPath, "transit")
	t.Setenv(EnvVaultRoleID, "role-1")
	t.Setenv(EnvVaultSecretID, "secret-1")
	p, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if _, ok := p.(*VaultProvider); !ok {
		t.Fatalf("got %T, want *VaultProvider", p)
	}
}

func TestFromEnv_NoConfig(t *testing.T) {
	t.Setenv(EnvVaultAddr, "")
	t.Setenv(EnvVaultPath, "")
	_, err := FromEnv()
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("err = %v, want ErrNoConfig", err)
	}
}

func TestVaultProvider_ReloginOn401(t *testing.T) {
	fv := newFakeVault(t, "strata-master")
	p := newTestVaultProvider(t, fv)

	if _, _, err := p.GenerateDataKey(context.Background(), "strata-master"); err != nil {
		t.Fatalf("GenerateDataKey1: %v", err)
	}
	if c := atomic.LoadInt32(&fv.loginCalls); c != 1 {
		t.Fatalf("loginCalls after first call = %d, want 1", c)
	}

	// Force the next datakey call to 401 — provider should re-login then retry.
	fv.datakeyStatus.Store(int32(http.StatusUnauthorized))
	_, _, _ = p.GenerateDataKey(context.Background(), "strata-master")

	if c := atomic.LoadInt32(&fv.loginCalls); c < 2 {
		t.Fatalf("loginCalls after 401 = %d, want >= 2", c)
	}
}
