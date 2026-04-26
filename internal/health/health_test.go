package health

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthzAlwaysOK(t *testing.T) {
	h := &Handler{Probes: map[string]Probe{
		"always-fail": func(context.Context) error { return errors.New("nope") },
	}}
	w := httptest.NewRecorder()
	h.Healthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	if got := w.Body.String(); got != "ok" {
		t.Fatalf("body=%q want %q", got, "ok")
	}
}

func TestReadyzNoProbesReturnsOK(t *testing.T) {
	h := &Handler{}
	w := httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
}

func TestReadyzAllSucceed(t *testing.T) {
	h := &Handler{Probes: map[string]Probe{
		"cassandra": func(context.Context) error { return nil },
		"rados":     func(context.Context) error { return nil },
	}}
	w := httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%q", w.Code, w.Body.String())
	}
}

func TestReadyzCassandraDown(t *testing.T) {
	h := &Handler{Probes: map[string]Probe{
		"cassandra": func(context.Context) error { return errors.New("connection refused") },
		"rados":     func(context.Context) error { return nil },
	}}
	w := httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "cassandra: connection refused") {
		t.Fatalf("body=%q missing cassandra failure", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "rados") {
		t.Fatalf("body=%q should not mention rados (it succeeded)", w.Body.String())
	}
}

func TestReadyzRadosDown(t *testing.T) {
	h := &Handler{Probes: map[string]Probe{
		"cassandra": func(context.Context) error { return nil },
		"rados":     func(context.Context) error { return errors.New("ENOENT pool") },
	}}
	w := httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "rados: ENOENT pool") {
		t.Fatalf("body=%q missing rados failure", w.Body.String())
	}
}

func TestReadyzTimeoutFailsProbe(t *testing.T) {
	h := &Handler{
		Timeout: 50 * time.Millisecond,
		Probes: map[string]Probe{
			"slow": func(ctx context.Context) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(2 * time.Second):
					return nil
				}
			},
		},
	}
	w := httptest.NewRecorder()
	start := time.Now()
	h.Readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	elapsed := time.Since(start)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", w.Code)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("readyz took %v, expected to honour 50ms timeout", elapsed)
	}
}

func TestHealthzAndReadyzServeMux(t *testing.T) {
	h := &Handler{Probes: map[string]Probe{
		"ok": func(context.Context) error { return nil },
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.Healthz)
	mux.HandleFunc("/readyz", h.Readyz)

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/healthz", http.StatusOK},
		{"/readyz", http.StatusOK},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code != tc.want {
			b, _ := io.ReadAll(w.Body)
			t.Fatalf("%s: status=%d want %d body=%s", tc.path, w.Code, tc.want, b)
		}
	}
}
