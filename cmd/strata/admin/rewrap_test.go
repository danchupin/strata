package admin

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"
)

func hexKey(b byte) string {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return hex.EncodeToString(k)
}

// TestApp_Rewrap_MissingMasterKeys verifies the rewrap subcommand errors
// out cleanly when STRATA_SSE_MASTER_KEYS is unset.
func TestApp_Rewrap_MissingMasterKeys(t *testing.T) {
	t.Setenv("STRATA_SSE_MASTER_KEYS", "")
	t.Setenv("STRATA_SSE_MASTER_KEY", "")
	t.Setenv("STRATA_SSE_MASTER_KEY_FILE", "")
	t.Setenv("STRATA_SSE_MASTER_KEY_VAULT", "")
	t.Setenv("STRATA_META_BACKEND", "memory")

	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{"rewrap"})
	err := a.run(context.Background())
	if err == nil {
		t.Fatalf("expected error when STRATA_SSE_MASTER_KEYS unset")
	}
	if !strings.Contains(err.Error(), "rotation provider") {
		t.Fatalf("expected rotation-provider error, got %v", err)
	}
}

// TestApp_Rewrap_RunsAgainstMemory verifies a happy-path run against the
// memory meta backend with two keys configured.
func TestApp_Rewrap_RunsAgainstMemory(t *testing.T) {
	t.Setenv("STRATA_SSE_MASTER_KEYS", "active:"+hexKey(0x11)+",old:"+hexKey(0x22))
	t.Setenv("STRATA_META_BACKEND", "memory")

	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{"rewrap"})
	if err := a.run(context.Background()); err != nil {
		t.Fatalf("rewrap: %v stderr=%s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "rewrap: active=active") {
		t.Fatalf("missing active line in stdout: %q", out)
	}
	// stderr captures the structured slog output.
	if !strings.Contains(stderr.String(), "rewrap complete") {
		t.Fatalf("missing rewrap-complete log in stderr: %q", stderr.String())
	}
}

// TestApp_Rewrap_TargetKeyIDReorders verifies --target-key-id picks the
// destination wrap key when the env list has it in a non-leading slot.
func TestApp_Rewrap_TargetKeyIDReorders(t *testing.T) {
	// active in env is "first"; CLI overrides to "second".
	t.Setenv("STRATA_SSE_MASTER_KEYS", "first:"+hexKey(0x11)+",second:"+hexKey(0x22))
	t.Setenv("STRATA_META_BACKEND", "memory")

	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{"rewrap", "--target-key-id", "second"})
	if err := a.run(context.Background()); err != nil {
		t.Fatalf("rewrap: %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "rewrap: active=second") {
		t.Fatalf("expected target reorder to make 'second' active, got %q", stdout.String())
	}
}

// TestApp_Rewrap_TargetKeyIDUnknown rejects a target id missing from the
// rotation list.
func TestApp_Rewrap_TargetKeyIDUnknown(t *testing.T) {
	t.Setenv("STRATA_SSE_MASTER_KEYS", "first:"+hexKey(0x11))
	t.Setenv("STRATA_META_BACKEND", "memory")

	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{"rewrap", "--target-key-id", "missing"})
	err := a.run(context.Background())
	if err == nil {
		t.Fatalf("expected error for unknown target id; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected unknown-id error, got %v", err)
	}
}

// TestApp_Rewrap_DryRun runs with --dry-run and ensures the dry-run log line
// surfaces in stderr.
func TestApp_Rewrap_DryRun(t *testing.T) {
	t.Setenv("STRATA_SSE_MASTER_KEYS", "active:"+hexKey(0x11))
	t.Setenv("STRATA_META_BACKEND", "memory")

	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{"rewrap", "--dry-run"})
	if err := a.run(context.Background()); err != nil {
		t.Fatalf("rewrap: %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "dry-run requested") {
		t.Fatalf("missing dry-run log: %q", stderr.String())
	}
}
