package kms

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
	"strings"
	"sync"
	"time"
)

const (
	// EnvVaultAddr supplies the Vault server address.
	EnvVaultAddr = "STRATA_KMS_VAULT_ADDR"
	// EnvVaultPath supplies the Transit mount path (e.g. "transit").
	EnvVaultPath = "STRATA_KMS_VAULT_PATH"
	// EnvVaultRoleID and EnvVaultSecretID are the AppRole credentials, shared
	// with the SSE-S3 master-key Vault provider (US-002).
	EnvVaultRoleID   = "STRATA_SSE_VAULT_ROLE_ID"
	EnvVaultSecretID = "STRATA_SSE_VAULT_SECRET_ID"

	// DEKSize is the required plaintext DEK length (AES-256).
	DEKSize = 32

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
	Now        func() time.Time
}

// VaultProvider implements Provider against a HashiCorp Vault Transit mount.
// GenerateDataKey calls /datakey/plaintext/<key>; UnwrapDEK calls
// /decrypt/<key>. AppRole tokens are cached until the lease expires; a stale
// token triggers exactly one re-login on demand.
type VaultProvider struct {
	cfg VaultConfig

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// NewVaultProvider validates cfg and returns a provider.
func NewVaultProvider(cfg VaultConfig) (*VaultProvider, error) {
	if cfg.Addr == "" {
		return nil, errors.New("kms vault: addr required")
	}
	if cfg.TransitPath == "" {
		return nil, errors.New("kms vault: transit path required")
	}
	if cfg.RoleID == "" || cfg.SecretID == "" {
		return nil, errors.New("kms vault: role_id and secret_id required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultVaultHTTPTimeout}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &VaultProvider{cfg: cfg}, nil
}

// NewVaultProviderFromEnv reads STRATA_KMS_VAULT_ADDR + STRATA_KMS_VAULT_PATH
// plus the shared AppRole credentials. Returns ErrNoConfig when either of the
// addr/path vars is unset.
func NewVaultProviderFromEnv() (*VaultProvider, error) {
	addr := strings.TrimSpace(os.Getenv(EnvVaultAddr))
	path := strings.TrimSpace(os.Getenv(EnvVaultPath))
	if addr == "" || path == "" {
		return nil, ErrNoConfig
	}
	return NewVaultProvider(VaultConfig{
		Addr:        addr,
		TransitPath: path,
		RoleID:      os.Getenv(EnvVaultRoleID),
		SecretID:    os.Getenv(EnvVaultSecretID),
	})
}

// GenerateDataKey returns a fresh 32-byte DEK plus the Vault-wrapped form.
func (p *VaultProvider) GenerateDataKey(ctx context.Context, keyID string) ([]byte, []byte, error) {
	if keyID == "" {
		return nil, nil, ErrMissingKeyID
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureTokenLocked(ctx); err != nil {
		return nil, nil, err
	}

	plain, wrapped, err := p.callDatakeyLocked(ctx, keyID)
	if errors.Is(err, errVaultAuth) {
		p.token = ""
		if err2 := p.ensureTokenLocked(ctx); err2 != nil {
			return nil, nil, err2
		}
		plain, wrapped, err = p.callDatakeyLocked(ctx, keyID)
	}
	if err != nil {
		return nil, nil, err
	}
	return plain, wrapped, nil
}

// UnwrapDEK reverses GenerateDataKey via the Transit /decrypt endpoint.
func (p *VaultProvider) UnwrapDEK(ctx context.Context, keyID string, wrapped []byte) ([]byte, error) {
	if keyID == "" {
		return nil, ErrMissingKeyID
	}
	if len(wrapped) == 0 {
		return nil, errors.New("kms vault: empty wrapped DEK")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureTokenLocked(ctx); err != nil {
		return nil, err
	}
	plain, err := p.callDecryptLocked(ctx, keyID, wrapped)
	if errors.Is(err, errVaultAuth) {
		p.token = ""
		if err2 := p.ensureTokenLocked(ctx); err2 != nil {
			return nil, err2
		}
		plain, err = p.callDecryptLocked(ctx, keyID, wrapped)
	}
	if err != nil {
		return nil, err
	}
	return plain, nil
}

var errVaultAuth = errors.New("kms vault: auth required")

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
		return fmt.Errorf("kms vault login: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("kms vault login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kms vault login: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var v vaultLoginResp
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return fmt.Errorf("kms vault login decode: %w", err)
	}
	if v.Auth.ClientToken == "" {
		return errors.New("kms vault login: empty client_token")
	}
	lease := time.Duration(v.Auth.LeaseDuration) * time.Second
	if lease <= 0 {
		lease = time.Hour
	}
	p.token = v.Auth.ClientToken
	p.tokenExp = p.cfg.Now().Add(lease - tokenRenewSlack)
	return nil
}

type datakeyResp struct {
	Data struct {
		Plaintext  string `json:"plaintext"`
		Ciphertext string `json:"ciphertext"`
	} `json:"data"`
}

func (p *VaultProvider) callDatakeyLocked(ctx context.Context, keyID string) ([]byte, []byte, error) {
	body, _ := json.Marshal(map[string]int{"bits": 256})
	url := strings.TrimRight(p.cfg.Addr, "/") + "/v1/" + strings.Trim(p.cfg.TransitPath, "/") + "/datakey/plaintext/" + keyID
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("kms vault datakey: %w", err)
	}
	req.Header.Set("X-Vault-Token", p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("kms vault datakey: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, nil, errVaultAuth
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("kms vault datakey: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var v datakeyResp
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, nil, fmt.Errorf("kms vault datakey decode: %w", err)
	}
	if v.Data.Plaintext == "" || v.Data.Ciphertext == "" {
		return nil, nil, errors.New("kms vault datakey: empty response")
	}
	plain, err := base64.StdEncoding.DecodeString(v.Data.Plaintext)
	if err != nil {
		return nil, nil, fmt.Errorf("kms vault datakey: plaintext decode: %w", err)
	}
	if len(plain) != DEKSize {
		return nil, nil, fmt.Errorf("kms vault datakey: plaintext %d bytes, want %d", len(plain), DEKSize)
	}
	return plain, []byte(v.Data.Ciphertext), nil
}

type decryptResp struct {
	Data struct {
		Plaintext string `json:"plaintext"`
	} `json:"data"`
}

func (p *VaultProvider) callDecryptLocked(ctx context.Context, keyID string, wrapped []byte) ([]byte, error) {
	body, _ := json.Marshal(map[string]string{"ciphertext": string(wrapped)})
	url := strings.TrimRight(p.cfg.Addr, "/") + "/v1/" + strings.Trim(p.cfg.TransitPath, "/") + "/decrypt/" + keyID
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("kms vault decrypt: %w", err)
	}
	req.Header.Set("X-Vault-Token", p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kms vault decrypt: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errVaultAuth
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("kms vault decrypt: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var v decryptResp
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("kms vault decrypt decode: %w", err)
	}
	if v.Data.Plaintext == "" {
		return nil, errors.New("kms vault decrypt: empty plaintext")
	}
	plain, err := base64.StdEncoding.DecodeString(v.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("kms vault decrypt: plaintext decode: %w", err)
	}
	return plain, nil
}
