package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

// fakeMeta wraps memory.Store and reports a Probe error so the cassandra
// branch in buildHealthHandler is exercised end-to-end without a live
// gocql session.
type fakeMeta struct {
	*metamem.Store
	probeErr error
}

func (f *fakeMeta) Probe(_ context.Context) error { return f.probeErr }

type fakeData struct {
	*datamem.Backend
	probeErr error
}

func (f *fakeData) Probe(_ context.Context, _ string) error { return f.probeErr }

func TestBuildHealthHandlerMemoryBackendsHaveNoProbes(t *testing.T) {
	h := buildHealthHandler(metamem.New(), datamem.New())
	if got := len(h.Probes); got != 0 {
		t.Fatalf("memory backends should register no probes, got %d", got)
	}

	w := httptest.NewRecorder()
	h.Healthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("/healthz status=%d body=%q want 200 ok", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/readyz status=%d body=%q want 200 (no probes => ready)", w.Code, w.Body.String())
	}
}

func TestBuildHealthHandlerProbersAllOK(t *testing.T) {
	h := buildHealthHandler(
		&fakeMeta{Store: metamem.New()},
		&fakeData{Backend: datamem.New()},
	)
	if got := len(h.Probes); got != 2 {
		t.Fatalf("expected 2 probes (cassandra+rados), got %d", got)
	}
	w := httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/readyz status=%d body=%q want 200", w.Code, w.Body.String())
	}
}

func TestBuildHealthHandlerCassandraDown(t *testing.T) {
	h := buildHealthHandler(
		&fakeMeta{Store: metamem.New(), probeErr: errors.New("connection refused")},
		&fakeData{Backend: datamem.New()},
	)
	w := httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status=%d want 503", w.Code)
	}

	w = httptest.NewRecorder()
	h.Healthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/healthz status=%d want 200 (must not consult probes)", w.Code)
	}
}

func TestBuildHealthHandlerRadosDown(t *testing.T) {
	h := buildHealthHandler(
		&fakeMeta{Store: metamem.New()},
		&fakeData{Backend: datamem.New(), probeErr: errors.New("ENOENT pool")},
	)
	w := httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status=%d want 503", w.Code)
	}
}

func TestHealthCanaryOIDDefault(t *testing.T) {
	t.Setenv("STRATA_RADOS_HEALTH_OID", "")
	if got := healthCanaryOID(); got != "strata-readyz-canary" {
		t.Fatalf("default OID=%q", got)
	}
	t.Setenv("STRATA_RADOS_HEALTH_OID", "custom-oid")
	if got := healthCanaryOID(); got != "custom-oid" {
		t.Fatalf("override OID=%q", got)
	}
}
