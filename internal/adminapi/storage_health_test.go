package adminapi

// Tests for GET /admin/v1/storage/health (US-005). The handler combines
// meta + data probes into a single ok/warnings/source triple consumed by the
// <StorageDegradedBanner> on every page.
//
// STRATA_STORAGE_HEALTH_OVERRIDE: when set to a JSON object the handler
// returns it verbatim; intended for Playwright e2e to simulate degraded
// states without standing up a broken backend. Set the env-var on the e2e
// CI job (see web/e2e/storage.spec.ts) and unset it elsewhere.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStorageHealthHappyPath(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/storage/health", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got StorageHealthResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK {
		t.Errorf("ok=false on memory backends; warnings=%v source=%q",
			got.Warnings, got.Source)
	}
	if got.Source != "" {
		t.Errorf("source=%q want empty when ok=true", got.Source)
	}
}

func TestStorageHealthOverrideEnv(t *testing.T) {
	// Override is the e2e simulation knob — when set the handler echoes the
	// JSON verbatim. We assert echoes match and ignore the live probes.
	const payload = `{"ok":false,"warnings":["simulated"],"source":"meta"}`
	t.Setenv("STRATA_STORAGE_HEALTH_OVERRIDE", payload)
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/storage/health", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got StorageHealthResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OK {
		t.Errorf("override ignored: ok=true")
	}
	if got.Source != "meta" {
		t.Errorf("source=%q want meta", got.Source)
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != "simulated" {
		t.Errorf("warnings=%v want [simulated]", got.Warnings)
	}
}

func TestStorageHealthInvalidOverrideFallsThrough(t *testing.T) {
	t.Setenv("STRATA_STORAGE_HEALTH_OVERRIDE", "not-json")
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/storage/health", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var got StorageHealthResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK {
		t.Errorf("invalid override should be ignored — live probe path returned ok=false: %+v", got)
	}
}

