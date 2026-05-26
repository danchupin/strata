package serverapp

import (
	"net/http"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/config"
)

// TestNewHTTPServerDefaults asserts the gateway listener picks up the
// slowloris-safe defaults baked into config.defaults() when the env knobs
// are unset.
func TestNewHTTPServerDefaults(t *testing.T) {
	t.Setenv("STRATA_HTTP_READ_HEADER_TIMEOUT", "")
	t.Setenv("STRATA_HTTP_READ_TIMEOUT", "")
	t.Setenv("STRATA_HTTP_WRITE_TIMEOUT", "")
	t.Setenv("STRATA_HTTP_IDLE_TIMEOUT", "")
	t.Setenv("STRATA_HTTP_MAX_HEADER_BYTES", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	srv := newHTTPServer(":0", http.NewServeMux(), cfg)
	if got, want := srv.ReadHeaderTimeout, 10*time.Second; got != want {
		t.Errorf("ReadHeaderTimeout=%s want %s", got, want)
	}
	if got, want := srv.ReadTimeout, 60*time.Second; got != want {
		t.Errorf("ReadTimeout=%s want %s", got, want)
	}
	if got, want := srv.WriteTimeout, 30*time.Minute; got != want {
		t.Errorf("WriteTimeout=%s want %s", got, want)
	}
	if got, want := srv.IdleTimeout, 120*time.Second; got != want {
		t.Errorf("IdleTimeout=%s want %s", got, want)
	}
	if got, want := srv.MaxHeaderBytes, 1<<20; got != want {
		t.Errorf("MaxHeaderBytes=%d want %d", got, want)
	}
}

// TestNewHTTPServerEnvOverride asserts every STRATA_HTTP_* knob beats the
// default.
func TestNewHTTPServerEnvOverride(t *testing.T) {
	t.Setenv("STRATA_HTTP_READ_HEADER_TIMEOUT", "3s")
	t.Setenv("STRATA_HTTP_READ_TIMEOUT", "15s")
	t.Setenv("STRATA_HTTP_WRITE_TIMEOUT", "5m")
	t.Setenv("STRATA_HTTP_IDLE_TIMEOUT", "45s")
	t.Setenv("STRATA_HTTP_MAX_HEADER_BYTES", "524288")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	srv := newHTTPServer(":0", http.NewServeMux(), cfg)
	if got, want := srv.ReadHeaderTimeout, 3*time.Second; got != want {
		t.Errorf("ReadHeaderTimeout=%s want %s", got, want)
	}
	if got, want := srv.ReadTimeout, 15*time.Second; got != want {
		t.Errorf("ReadTimeout=%s want %s", got, want)
	}
	if got, want := srv.WriteTimeout, 5*time.Minute; got != want {
		t.Errorf("WriteTimeout=%s want %s", got, want)
	}
	if got, want := srv.IdleTimeout, 45*time.Second; got != want {
		t.Errorf("IdleTimeout=%s want %s", got, want)
	}
	if got, want := srv.MaxHeaderBytes, 524288; got != want {
		t.Errorf("MaxHeaderBytes=%d want %d", got, want)
	}
}

// TestNewHTTPServerWriteTimeoutDisabled asserts the explicit-zero path
// (dev / loopback profile) survives config.Load().
func TestNewHTTPServerWriteTimeoutDisabled(t *testing.T) {
	t.Setenv("STRATA_HTTP_WRITE_TIMEOUT", "0s")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	srv := newHTTPServer(":0", http.NewServeMux(), cfg)
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout=%s want 0 (explicit-zero = disabled)", srv.WriteTimeout)
	}
}

// TestConfigLoadRejectsNegativeHTTPTimeout asserts negative durations fail
// fast at boot rather than silently degrading the listener.
func TestConfigLoadRejectsNegativeHTTPTimeout(t *testing.T) {
	cases := []struct {
		env string
		val string
	}{
		{"STRATA_HTTP_READ_HEADER_TIMEOUT", "-1s"},
		{"STRATA_HTTP_READ_TIMEOUT", "-1s"},
		{"STRATA_HTTP_WRITE_TIMEOUT", "-1s"},
		{"STRATA_HTTP_IDLE_TIMEOUT", "-1s"},
		{"STRATA_HTTP_MAX_HEADER_BYTES", "-1"},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(tc.env, tc.val)
			if _, err := config.Load(); err == nil {
				t.Fatalf("%s=%s: expected error, got nil", tc.env, tc.val)
			}
		})
	}
}

// TestConfigClampHTTPWriteTimeoutUpperBound asserts WriteTimeout > 24h
// clamps to 24h with a WARN log rather than disabling slowloris protection
// through a fat-finger 999h value.
func TestConfigClampHTTPWriteTimeoutUpperBound(t *testing.T) {
	t.Setenv("STRATA_HTTP_WRITE_TIMEOUT", "48h")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got, want := cfg.HTTP.WriteTimeout, 24*time.Hour; got != want {
		t.Fatalf("WriteTimeout=%s want clamped %s", got, want)
	}
}

// TestConfigClampHTTPMaxHeaderBytesUpperBound asserts MaxHeaderBytes >
// 16 MiB clamps to 16 MiB.
func TestConfigClampHTTPMaxHeaderBytesUpperBound(t *testing.T) {
	t.Setenv("STRATA_HTTP_MAX_HEADER_BYTES", "33554432") // 32 MiB
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got, want := cfg.HTTP.MaxHeaderBytes, 16<<20; got != want {
		t.Fatalf("MaxHeaderBytes=%d want clamped %d", got, want)
	}
}
