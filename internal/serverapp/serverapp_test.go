package serverapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/danchupin/strata/internal/config"
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

func TestParseTiKVEndpoints(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"pd:2379", []string{"pd:2379"}},
		{"pd-1:2379,pd-2:2379, pd-3:2379 ", []string{"pd-1:2379", "pd-2:2379", "pd-3:2379"}},
		{",,pd:2379,,", []string{"pd:2379"}},
	}
	for _, tc := range cases {
		got := parseTiKVEndpoints(tc.in)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("parseTiKVEndpoints(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func TestBuildMetaStoreTiKVEmptyEndpointsRejected(t *testing.T) {
	cfg := &config.Config{MetaBackend: "tikv"}
	store, err := buildMetaStore(cfg, nil, nil)
	if err == nil {
		_ = store.Close()
		t.Fatal("buildMetaStore(tikv) with empty endpoints should fail")
	}
}

func TestBuildLockerTiKVNilWhenStoreMismatched(t *testing.T) {
	cfg := &config.Config{MetaBackend: "tikv"}
	if got := buildLocker(cfg, metamem.New()); got != nil {
		t.Fatalf("buildLocker(tikv) with non-tikv store should return nil, got %T", got)
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
