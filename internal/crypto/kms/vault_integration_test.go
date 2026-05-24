//go:build integration

package kms_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/danchupin/strata/internal/crypto/kms"
)

// TestVaultProviderAgainstDevMode spins up hashicorp/vault:1.15 in
// dev-mode, enables the Transit + AppRole engines, creates a CMK, and
// exercises the GenerateDataKey → UnwrapDEK round-trip through the
// real VaultProvider over HTTP. Pinned to vault:1.15 per the PRD AC.
//
// Runs only under `go test -tags integration`.
func TestVaultProviderAgainstDevMode(t *testing.T) {
	ctx := context.Background()

	const (
		port = "8200"
		root = "root"
	)
	req := testcontainers.ContainerRequest{
		Image:        "hashicorp/vault:1.15",
		ExposedPorts: []string{port + "/tcp"},
		Env: map[string]string{
			"VAULT_DEV_ROOT_TOKEN_ID":     root,
			"VAULT_DEV_LISTEN_ADDRESS":    "0.0.0.0:" + port,
			"SKIP_SETCAP":                 "true",
		},
		Cmd: []string{"server", "-dev"},
		WaitingFor: wait.ForHTTP("/v1/sys/health").
			WithPort(port + "/tcp").
			WithStartupTimeout(60 * time.Second).
			WithStatusCodeMatcher(func(status int) bool {
				return status == 200 || status == 429 || status == 472 || status == 473
			}),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start vault: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	mapped, err := c.MappedPort(ctx, port)
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}
	addr := fmt.Sprintf("http://%s:%s", host, mapped.Port())

	v := &vaultAdmin{addr: addr, token: root, client: &http.Client{Timeout: 10 * time.Second}}

	// Enable transit + create a CMK.
	if err := v.enableEngine("transit", "transit"); err != nil {
		t.Fatalf("enable transit: %v", err)
	}
	keyID := "strata-bucket-sig-key"
	if err := v.post(fmt.Sprintf("/v1/transit/keys/%s", keyID), nil); err != nil {
		t.Fatalf("create CMK: %v", err)
	}

	// Enable approle + bind a policy that grants transit/datakey + decrypt.
	if err := v.enableAuthMethod("approle", "approle"); err != nil {
		t.Fatalf("enable approle: %v", err)
	}
	policy := fmt.Sprintf(`path "transit/datakey/plaintext/%s" { capabilities = ["update"] }
path "transit/decrypt/%s"             { capabilities = ["update"] }
`, keyID, keyID)
	if err := v.put("/v1/sys/policies/acl/strata-kms", map[string]any{"policy": policy}); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	if err := v.post("/v1/auth/approle/role/strata-kms", map[string]any{
		"token_policies":    "strata-kms",
		"token_ttl":         "1h",
		"token_max_ttl":     "1h",
	}); err != nil {
		t.Fatalf("create approle role: %v", err)
	}
	roleID, err := v.readRoleID()
	if err != nil {
		t.Fatalf("read role-id: %v", err)
	}
	secretID, err := v.mintSecretID()
	if err != nil {
		t.Fatalf("mint secret-id: %v", err)
	}

	provider, err := kms.NewVaultProvider(kms.VaultConfig{
		Addr:        addr,
		TransitPath: "transit",
		RoleID:      roleID,
		SecretID:    secretID,
	})
	if err != nil {
		t.Fatalf("NewVaultProvider: %v", err)
	}

	dek, wrapped, err := provider.GenerateDataKey(ctx, keyID)
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if len(dek) != 32 {
		t.Fatalf("DEK plaintext: got %d bytes, want 32", len(dek))
	}
	if !strings.HasPrefix(string(wrapped), "vault:") {
		t.Fatalf("wrapped DEK shape: got %q want vault:... prefix", string(wrapped))
	}

	round, err := provider.UnwrapDEK(ctx, keyID, wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if string(round) != string(dek) {
		t.Fatalf("roundtrip mismatch: %x vs %x", round, dek)
	}
}

// vaultAdmin is a thin HTTP helper for the dev-mode Vault setup.
type vaultAdmin struct {
	addr   string
	token  string
	client *http.Client
}

func (v *vaultAdmin) do(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, v.addr+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", v.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (v *vaultAdmin) post(path string, body any) error {
	_, err := v.do(http.MethodPost, path, body)
	return err
}

func (v *vaultAdmin) put(path string, body any) error {
	_, err := v.do(http.MethodPut, path, body)
	return err
}

func (v *vaultAdmin) enableEngine(name, kind string) error {
	return v.post("/v1/sys/mounts/"+name, map[string]any{"type": kind})
}

func (v *vaultAdmin) enableAuthMethod(name, kind string) error {
	return v.post("/v1/sys/auth/"+name, map[string]any{"type": kind})
}

func (v *vaultAdmin) readRoleID() (string, error) {
	body, err := v.do(http.MethodGet, "/v1/auth/approle/role/strata-kms/role-id", nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Data struct {
			RoleID string `json:"role_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", err
	}
	if resp.Data.RoleID == "" {
		return "", fmt.Errorf("empty role_id: %s", body)
	}
	return resp.Data.RoleID, nil
}

func (v *vaultAdmin) mintSecretID() (string, error) {
	body, err := v.do(http.MethodPost, "/v1/auth/approle/role/strata-kms/secret-id", nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Data struct {
			SecretID string `json:"secret_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", err
	}
	if resp.Data.SecretID == "" {
		return "", fmt.Errorf("empty secret_id: %s", body)
	}
	return resp.Data.SecretID, nil
}
