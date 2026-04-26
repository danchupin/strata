package master

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// EnvMasterKeyVault encodes the Vault address and Transit export path:
	//   STRATA_SSE_MASTER_KEY_VAULT=<addr>:<transit-export-path>
	// Example: https://vault.example.com:8200:transit/export/encryption-key/strata-master
	EnvMasterKeyVault = "STRATA_SSE_MASTER_KEY_VAULT"
	// EnvVaultRoleID supplies the AppRole role_id.
	EnvVaultRoleID = "STRATA_SSE_VAULT_ROLE_ID"
	// EnvVaultSecretID supplies the AppRole secret_id.
	EnvVaultSecretID = "STRATA_SSE_VAULT_SECRET_ID"

	// DefaultVaultRefresh is the cached-key TTL between Vault refreshes.
	DefaultVaultRefresh = 5 * time.Minute

	defaultVaultHTTPTimeout = 10 * time.Second
	tokenRenewSlack         = 30 * time.Second
)

// VaultConfig wires a VaultProvider. Addr/TransitPath/RoleID/SecretID are
// required; the rest default sensibly.
type VaultConfig struct {
	Addr        string
	TransitPath string
	RoleID      string
	SecretID    string

	HTTPClient *http.Client
	Logger     *slog.Logger
	Refresh    time.Duration
	Now        func() time.Time
}

// VaultProvider resolves the master key from a HashiCorp Vault Transit key
// using AppRole auth. The exported key bytes are cached for cfg.Refresh; on
// refresh failure the last good key is returned and a WARN is logged.
type VaultProvider struct {
	cfg VaultConfig

	mu          sync.Mutex
	token       string
	tokenExp    time.Time
	cacheKey    []byte
	cacheID     string
	cacheTime   time.Time
	initialized bool
}

// NewVaultProvider validates cfg and returns a provider. The first Resolve
// performs the initial fetch; if that fails the caller treats it as fatal.
func NewVaultProvider(cfg VaultConfig) (*VaultProvider, error) {
	if cfg.Addr == "" {
		return nil, errors.New("vault: addr required")
	}
	if cfg.TransitPath == "" {
		return nil, errors.New("vault: transit path required")
	}
	if cfg.RoleID == "" || cfg.SecretID == "" {
		return nil, errors.New("vault: role_id and secret_id required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultVaultHTTPTimeout}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Refresh == 0 {
		cfg.Refresh = DefaultVaultRefresh
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &VaultProvider{cfg: cfg}, nil
}

// NewVaultProviderFromEnv reads STRATA_SSE_MASTER_KEY_VAULT plus the AppRole
// vars. Returns ErrNoConfig when the vault var is unset.
func NewVaultProviderFromEnv() (*VaultProvider, error) {
	addr, path, ok := parseVaultAddrPath(os.Getenv(EnvMasterKeyVault))
	if !ok {
		return nil, ErrNoConfig
	}
	return NewVaultProvider(VaultConfig{
		Addr:        addr,
		TransitPath: path,
		RoleID:      os.Getenv(EnvVaultRoleID),
		SecretID:    os.Getenv(EnvVaultSecretID),
	})
}

// parseVaultAddrPath splits "<addr>:<transit-path>" by the LAST colon so that
// addresses containing a port (https://host:8200) survive.
func parseVaultAddrPath(raw string) (addr, path string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	i := strings.LastIndex(raw, ":")
	if i <= 0 || i == len(raw)-1 {
		return "", "", false
	}
	addr = strings.TrimSpace(raw[:i])
	path = strings.TrimSpace(raw[i+1:])
	if addr == "" || path == "" {
		return "", "", false
	}
	return addr, path, true
}

// Resolve returns the cached master key when fresh, refreshes when stale, and
// keeps the last good key (logging WARN) when Vault is unreachable on refresh.
// First-call failures bubble up so the caller can fatal.
func (p *VaultProvider) Resolve(ctx context.Context) ([]byte, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.initialized {
		if err := p.refreshLocked(ctx); err != nil {
			return nil, "", err
		}
		p.initialized = true
		return p.cacheKey, p.cacheID, nil
	}

	if p.cfg.Now().Sub(p.cacheTime) >= p.cfg.Refresh {
		if err := p.refreshLocked(ctx); err != nil {
			p.cfg.Logger.Warn("vault master key refresh failed; reusing cached key",
				"error", err.Error(),
				"addr", p.cfg.Addr,
			)
			return p.cacheKey, p.cacheID, nil
		}
	}

	return p.cacheKey, p.cacheID, nil
}

func (p *VaultProvider) refreshLocked(ctx context.Context) error {
	if err := p.ensureTokenLocked(ctx); err != nil {
		return err
	}
	key, version, err := p.fetchExportedKeyLocked(ctx)
	if errors.Is(err, errVaultAuth) {
		// Stale token: re-login once and retry.
		p.token = ""
		if err2 := p.ensureTokenLocked(ctx); err2 != nil {
			return err2
		}
		key, version, err = p.fetchExportedKeyLocked(ctx)
	}
	if err != nil {
		return err
	}
	if len(key) != KeySize {
		return fmt.Errorf("%w: vault returned %d bytes", ErrInvalidKeyLength, len(key))
	}
	p.cacheKey = key
	p.cacheID = fmt.Sprintf("vault-v%d", version)
	p.cacheTime = p.cfg.Now()
	return nil
}

var errVaultAuth = errors.New("vault: auth required")

type vaultLoginResp struct {
	Auth struct {
		ClientToken   string `json:"client_token"`
		LeaseDuration int    `json:"lease_duration"`
	} `json:"auth"`
}

func (p *VaultProvider) ensureTokenLocked(ctx context.Context) error {
	if p.token != "" && p.cfg.Now().Before(p.tokenExp) {
		return nil
	}
	body, _ := json.Marshal(map[string]string{
		"role_id":   p.cfg.RoleID,
		"secret_id": p.cfg.SecretID,
	})
	url := strings.TrimRight(p.cfg.Addr, "/") + "/v1/auth/approle/login"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("vault login: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("vault login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vault login: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var v vaultLoginResp
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return fmt.Errorf("vault login decode: %w", err)
	}
	if v.Auth.ClientToken == "" {
		return errors.New("vault login: empty client_token")
	}
	lease := time.Duration(v.Auth.LeaseDuration) * time.Second
	if lease <= 0 {
		lease = time.Hour
	}
	p.token = v.Auth.ClientToken
	p.tokenExp = p.cfg.Now().Add(lease - tokenRenewSlack)
	return nil
}

type vaultExportResp struct {
	Data struct {
		Keys map[string]string `json:"keys"`
		Name string            `json:"name"`
	} `json:"data"`
}

func (p *VaultProvider) fetchExportedKeyLocked(ctx context.Context) ([]byte, int, error) {
	url := strings.TrimRight(p.cfg.Addr, "/") + "/v1/" + strings.Trim(p.cfg.TransitPath, "/") + "/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("vault export: %w", err)
	}
	req.Header.Set("X-Vault-Token", p.token)
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("vault export: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, 0, errVaultAuth
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, 0, fmt.Errorf("vault export: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var v vaultExportResp
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, 0, fmt.Errorf("vault export decode: %w", err)
	}
	if len(v.Data.Keys) == 0 {
		return nil, 0, errors.New("vault export: no keys in response")
	}
	var (
		bestVer int
		bestB64 string
	)
	for ver, b64 := range v.Data.Keys {
		n, convErr := strconv.Atoi(ver)
		if convErr != nil {
			continue
		}
		if n > bestVer {
			bestVer = n
			bestB64 = b64
		}
	}
	if bestVer == 0 {
		return nil, 0, errors.New("vault export: no numbered key versions")
	}
	raw, err := base64.StdEncoding.DecodeString(bestB64)
	if err != nil {
		return nil, 0, fmt.Errorf("vault export: decode key: %w", err)
	}
	return raw, bestVer, nil
}
