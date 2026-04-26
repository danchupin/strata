package master

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeVault stands in for a Vault server: AppRole login and Transit export
// endpoints. Both responses are settable so tests can simulate rotation or
// outages.
type fakeVault struct {
	t           *testing.T
	loginCalls  int32
	exportCalls int32

	loginStatus  atomic.Int32 // HTTP status; 0 means default 200
	exportStatus atomic.Int32

	keys atomic.Pointer[map[int][]byte]

	srv *httptest.Server
}

func newFakeVault(t *testing.T, initial map[int][]byte) *fakeVault {
	fv := &fakeVault{t: t}
	fv.keys.Store(&initial)

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
	mux.HandleFunc("/v1/transit/export/encryption-key/strata-master/latest", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fv.exportCalls, 1)
		if r.Header.Get("X-Vault-Token") == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		if s := fv.exportStatus.Load(); s != 0 {
			http.Error(w, "forced", int(s))
			return
		}
		out := map[string]string{}
		for ver, raw := range *fv.keys.Load() {
			out[fmt.Sprintf("%d", ver)] = base64.StdEncoding.EncodeToString(raw)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"keys": out,
				"name": "strata-master",
				"type": "aes256-gcm96",
			},
		})
	})

	fv.srv = httptest.NewServer(mux)
	t.Cleanup(fv.srv.Close)
	return fv
}

func (fv *fakeVault) addr() string { return fv.srv.URL }

func (fv *fakeVault) setKeys(m map[int][]byte) { fv.keys.Store(&m) }

func mustHexBytes(t *testing.T, h string) []byte {
	t.Helper()
	b, err := decodeHexKey(h)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return b
}

func newTestVaultProvider(t *testing.T, fv *fakeVault, refresh time.Duration, now func() time.Time, logger *slog.Logger) *VaultProvider {
	t.Helper()
	p, err := NewVaultProvider(VaultConfig{
		Addr:        fv.addr(),
		TransitPath: "transit/export/encryption-key/strata-master",
		RoleID:      "role-1",
		SecretID:    "secret-1",
		HTTPClient:  fv.srv.Client(),
		Logger:      logger,
		Refresh:     refresh,
		Now:         now,
	})
	if err != nil {
		t.Fatalf("NewVaultProvider: %v", err)
	}
	return p
}

func TestVaultProvider_FirstFetch(t *testing.T) {
	fv := newFakeVault(t, map[int][]byte{1: mustHexBytes(t, hexKey32)})

	p := newTestVaultProvider(t, fv, 5*time.Minute, time.Now, slog.Default())
	key, id, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(key) != KeySize {
		t.Fatalf("key len = %d", len(key))
	}
	if id != "vault-v1" {
		t.Fatalf("id = %q, want vault-v1", id)
	}
	if got := atomic.LoadInt32(&fv.loginCalls); got != 1 {
		t.Fatalf("loginCalls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&fv.exportCalls); got != 1 {
		t.Fatalf("exportCalls = %d, want 1", got)
	}
}

func TestVaultProvider_CachedBetweenRefreshes(t *testing.T) {
	fv := newFakeVault(t, map[int][]byte{1: mustHexBytes(t, hexKey32)})

	base := time.Unix(1_700_000_000, 0)
	now := base
	p := newTestVaultProvider(t, fv, 5*time.Minute, func() time.Time { return now }, slog.Default())

	for i := range 5 {
		if _, _, err := p.Resolve(context.Background()); err != nil {
			t.Fatalf("Resolve %d: %v", i, err)
		}
		now = now.Add(30 * time.Second)
	}
	if got := atomic.LoadInt32(&fv.exportCalls); got != 1 {
		t.Fatalf("exportCalls = %d, want 1 (cache hit)", got)
	}
}

func TestVaultProvider_RefreshAfterTTL(t *testing.T) {
	fv := newFakeVault(t, map[int][]byte{1: mustHexBytes(t, hexKey32)})

	base := time.Unix(1_700_000_000, 0)
	now := base
	p := newTestVaultProvider(t, fv, 5*time.Minute, func() time.Time { return now }, slog.Default())

	_, id1, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve1: %v", err)
	}
	if id1 != "vault-v1" {
		t.Fatalf("id1 = %q", id1)
	}

	fv.setKeys(map[int][]byte{
		1: mustHexBytes(t, hexKey32),
		2: mustHexBytes(t, hexKeyB),
	})
	now = now.Add(6 * time.Minute)

	key2, id2, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve2: %v", err)
	}
	if id2 != "vault-v2" {
		t.Fatalf("id2 = %q, want vault-v2", id2)
	}
	if !bytes.Equal(key2, mustHexBytes(t, hexKeyB)) {
		t.Fatalf("key2 did not pick up rotated key")
	}
}

func TestVaultProvider_InitialFetchFatal(t *testing.T) {
	fv := newFakeVault(t, map[int][]byte{1: mustHexBytes(t, hexKey32)})
	fv.loginStatus.Store(int32(http.StatusInternalServerError))

	p := newTestVaultProvider(t, fv, 5*time.Minute, time.Now, slog.Default())
	_, _, err := p.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error on initial fetch")
	}
	if !strings.Contains(err.Error(), "vault login") {
		t.Fatalf("err = %v", err)
	}
}

func TestVaultProvider_RefreshFailureKeepsLastGoodAndWarns(t *testing.T) {
	fv := newFakeVault(t, map[int][]byte{1: mustHexBytes(t, hexKey32)})

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	base := time.Unix(1_700_000_000, 0)
	now := base
	p := newTestVaultProvider(t, fv, 5*time.Minute, func() time.Time { return now }, logger)

	key1, id1, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve1: %v", err)
	}

	// Vault becomes unreachable for the next refresh.
	fv.exportStatus.Store(int32(http.StatusBadGateway))
	now = now.Add(6 * time.Minute)

	key2, id2, err := p.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve2 should not error when refresh fails: %v", err)
	}
	if !bytes.Equal(key1, key2) || id1 != id2 {
		t.Fatalf("expected last-good key on refresh failure; key match=%v id1=%q id2=%q",
			bytes.Equal(key1, key2), id1, id2)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, `"level":"WARN"`) {
		t.Fatalf("expected WARN log, got: %s", logs)
	}
	if !strings.Contains(logs, "vault master key refresh failed") {
		t.Fatalf("expected refresh-failed message in log, got: %s", logs)
	}
}

func TestVaultProvider_BadKeyLength(t *testing.T) {
	short := make([]byte, 16)
	for i := range short {
		short[i] = byte(i + 1)
	}
	fv := newFakeVault(t, map[int][]byte{1: short})

	p := newTestVaultProvider(t, fv, 5*time.Minute, time.Now, slog.Default())
	_, _, err := p.Resolve(context.Background())
	if !errors.Is(err, ErrInvalidKeyLength) {
		t.Fatalf("err = %v, want ErrInvalidKeyLength", err)
	}
}

func TestVaultProvider_ReloginOn401(t *testing.T) {
	fv := newFakeVault(t, map[int][]byte{1: mustHexBytes(t, hexKey32)})

	base := time.Unix(1_700_000_000, 0)
	now := base
	p := newTestVaultProvider(t, fv, 5*time.Minute, func() time.Time { return now }, slog.Default())

	if _, _, err := p.Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve1: %v", err)
	}
	if got := atomic.LoadInt32(&fv.loginCalls); got != 1 {
		t.Fatalf("loginCalls after first Resolve = %d, want 1", got)
	}

	fv.exportStatus.Store(int32(http.StatusUnauthorized))
	now = now.Add(6 * time.Minute)
	// Refresh hits 401, package re-logs in once and retries; second 401 yields
	// errVaultAuth → refreshLocked returns it → Resolve logs WARN and returns
	// the cached key. We just want to observe the re-login attempt.
	_, _, _ = p.Resolve(context.Background())

	if got := atomic.LoadInt32(&fv.loginCalls); got < 2 {
		t.Fatalf("loginCalls after 401 = %d, want >= 2 (forced re-login)", got)
	}
}

func TestParseVaultAddrPath(t *testing.T) {
	cases := []struct {
		in       string
		addr     string
		path     string
		ok       bool
		describe string
	}{
		{"https://vault:8200:transit/export/encryption-key/strata", "https://vault:8200", "transit/export/encryption-key/strata", true, "addr with port"},
		{"http://vault:transit/keys/x", "http://vault", "transit/keys/x", true, "addr without port"},
		{"", "", "", false, "empty"},
		{"no-colon", "", "", false, "no colon"},
		{":path", "", "", false, "leading colon"},
		{"addr:", "", "", false, "trailing colon"},
	}
	for _, c := range cases {
		t.Run(c.describe, func(t *testing.T) {
			a, p, ok := parseVaultAddrPath(c.in)
			if ok != c.ok || a != c.addr || p != c.path {
				t.Fatalf("parse(%q) = (%q,%q,%v); want (%q,%q,%v)", c.in, a, p, ok, c.addr, c.path, c.ok)
			}
		})
	}
}

func TestFromEnv_VaultPreferred(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte(hexKey32+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvMasterKeyVault, "https://vault:8200:transit/export/encryption-key/strata")
	t.Setenv(EnvVaultRoleID, "r")
	t.Setenv(EnvVaultSecretID, "s")
	t.Setenv(EnvMasterKeyFile, keyPath)
	t.Setenv(EnvMasterKey, hexKey32)

	p, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if _, ok := p.(*VaultProvider); !ok {
		t.Fatalf("got %T, want *VaultProvider", p)
	}
}

func TestFromEnv_VaultMissingCreds(t *testing.T) {
	t.Setenv(EnvMasterKeyVault, "https://vault:8200:transit/export/encryption-key/strata")
	t.Setenv(EnvVaultRoleID, "")
	t.Setenv(EnvVaultSecretID, "")
	t.Setenv(EnvMasterKeyFile, "")
	t.Setenv(EnvMasterKey, "")

	_, err := FromEnv()
	if err == nil || !strings.Contains(err.Error(), "role_id and secret_id required") {
		t.Fatalf("err = %v, want role_id/secret_id required", err)
	}
}
