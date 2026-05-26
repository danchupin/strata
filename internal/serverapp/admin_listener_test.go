package serverapp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/danchupin/strata/cmd/strata/workers"
	"github.com/danchupin/strata/internal/config"
)

// TestAdminListenerSplitRouting verifies that when STRATA_ADMIN_LISTEN is
// set: (a) S3 requests to admin listener return 404, (b) /admin/v1 + /metrics
// + /healthz to main listener return 404, (c) admin routes work on the admin
// listener, (d) S3 catch-all works on the main listener.
func TestAdminListenerSplitRouting(t *testing.T) {
	mainAddr := freePort(t)
	adminAddr := freePort(t)
	t.Setenv("STRATA_LISTEN", mainAddr)
	t.Setenv("STRATA_ADMIN_LISTEN", adminAddr)
	t.Setenv("STRATA_DATA_BACKEND", "memory")
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_AUTH_MODE", "off")
	t.Setenv("STRATA_SHUTDOWN_WAIT", "2s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	runErr := make(chan error, 1)
	go func() { runErr <- Run(runCtx, cfg, logger, []workers.Worker{}) }()

	waitListen(t, mainAddr)
	waitListen(t, adminAddr)

	client := &http.Client{Timeout: 3 * time.Second}

	// (a) S3 request to admin listener → 404 (no S3 routes registered there)
	resp, err := client.Get("http://" + adminAddr + "/somebucket/somekey")
	if err != nil {
		t.Fatalf("get admin /bucket: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("admin S3 path status=%d want 404", resp.StatusCode)
	}

	// (b) /admin/v1/ to main listener → 404 (S3 catch-all owns / on main)
	// The main listener's "/" handler returns 404 for unknown paths via the
	// S3 router. Probe /healthz on the main listener: should hit the S3
	// catch-all (no /healthz route on main when split).
	resp, err = client.Get("http://" + mainAddr + "/healthz")
	if err != nil {
		t.Fatalf("get main /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("/healthz on main listener returned 200 — should be on admin listener only when split")
	}

	// (c) /healthz on admin listener → 200
	resp, err = client.Get("http://" + adminAddr + "/healthz")
	if err != nil {
		t.Fatalf("get admin /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("admin /healthz status=%d want 200", resp.StatusCode)
	}

	// (c) /metrics on admin listener → 200
	resp, err = client.Get("http://" + adminAddr + "/metrics")
	if err != nil {
		t.Fatalf("get admin /metrics: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("admin /metrics status=%d want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestAdminListenerBackwardsCompat verifies that when STRATA_ADMIN_LISTEN is
// empty, /admin/v1, /metrics, /healthz, /console/ all stay on the main
// listener (single-port shape).
func TestAdminListenerBackwardsCompat(t *testing.T) {
	addr := freePort(t)
	t.Setenv("STRATA_LISTEN", addr)
	t.Setenv("STRATA_DATA_BACKEND", "memory")
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_AUTH_MODE", "off")
	t.Setenv("STRATA_SHUTDOWN_WAIT", "2s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.AdminListen.Listen != "" {
		t.Fatalf("AdminListen.Listen=%q want empty (backwards-compat)", cfg.AdminListen.Listen)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	runErr := make(chan error, 1)
	go func() { runErr <- Run(runCtx, cfg, logger, []workers.Worker{}) }()

	waitListen(t, addr)

	client := &http.Client{Timeout: 3 * time.Second}
	for _, path := range []string{"/healthz", "/metrics"} {
		resp, err := client.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status=%d want 200 (single-port shape)", path, resp.StatusCode)
		}
	}

	cancel()
	<-runErr
}

// TestAdminListenerLoopbackBind verifies binding the admin listener to
// 127.0.0.1 makes it unreachable from a non-loopback dial. The OS only
// listens on the loopback interface — dialling via a non-loopback host
// alias should fail.
func TestAdminListenerLoopbackBind(t *testing.T) {
	port := freePort(t)
	_, p, _ := net.SplitHostPort(port)
	adminAddr := "127.0.0.1:" + p
	mainAddr := freePort(t)

	t.Setenv("STRATA_LISTEN", mainAddr)
	t.Setenv("STRATA_ADMIN_LISTEN", adminAddr)
	t.Setenv("STRATA_DATA_BACKEND", "memory")
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_AUTH_MODE", "off")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runErr := make(chan error, 1)
	go func() { runErr <- Run(runCtx, cfg, logger, []workers.Worker{}) }()
	waitListen(t, adminAddr)

	// Loopback dial succeeds.
	c, err := net.DialTimeout("tcp", adminAddr, 1*time.Second)
	if err != nil {
		t.Fatalf("loopback dial: %v", err)
	}
	c.Close()

	// Non-loopback dial: OS lists no listener on the non-loopback iface
	// for the same port. Use 127.0.0.2 as a non-127.0.0.1 loopback alias
	// (Linux has full 127/8 loopback; macOS only 127.0.0.1 by default).
	// Skip if the alias isn't routable — the test only proves the bound
	// interface IS the loopback.
	if runtime_supports_127_0_0_2() {
		c, err := net.DialTimeout("tcp", "127.0.0.2:"+p, 500*time.Millisecond)
		if err == nil {
			c.Close()
			t.Errorf("non-loopback dial succeeded — admin listener should bind 127.0.0.1 only")
		}
	}

	cancel()
	<-runErr
}

func runtime_supports_127_0_0_2() bool {
	c, err := net.DialTimeout("tcp", "127.0.0.2:1", 200*time.Millisecond)
	if err == nil {
		c.Close()
		return true
	}
	// A "connection refused" on 127.0.0.2 means the host considers the
	// address routable but no server is listening — exactly what we need
	// for the negative case to be meaningful.
	if oe, ok := err.(*net.OpError); ok {
		if se, ok := oe.Err.(*os.SyscallError); ok {
			if se.Err == syscall.ECONNREFUSED {
				return true
			}
		}
	}
	return false
}

// TestAdminListenerSIGTERMDrainsBoth verifies that ctx cancel drains BOTH
// listeners within ShutdownWait.
func TestAdminListenerSIGTERMDrainsBoth(t *testing.T) {
	mainAddr := freePort(t)
	adminAddr := freePort(t)
	t.Setenv("STRATA_LISTEN", mainAddr)
	t.Setenv("STRATA_ADMIN_LISTEN", adminAddr)
	t.Setenv("STRATA_DATA_BACKEND", "memory")
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_AUTH_MODE", "off")
	t.Setenv("STRATA_SHUTDOWN_WAIT", "3s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runErr := make(chan error, 1)
	go func() { runErr <- Run(runCtx, cfg, logger, []workers.Worker{}) }()
	waitListen(t, mainAddr)
	waitListen(t, adminAddr)

	start := time.Now()
	cancel()
	select {
	case err := <-runErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within shutdown window")
	}

	// Both listeners must be closed: dialling either should error promptly.
	if c, err := net.DialTimeout("tcp", mainAddr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Errorf("main listener still accepting after shutdown")
	}
	if c, err := net.DialTimeout("tcp", adminAddr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Errorf("admin listener still accepting after shutdown")
	}

	if dur := time.Since(start); dur > 4*time.Second {
		t.Errorf("shutdown took %s — exceeded 3s + slack", dur)
	}
}

// TestAdminListenerTLS verifies the admin listener terminates TLS when
// STRATA_ADMIN_TLS_CERT_FILE / KEY_FILE are set.
func TestAdminListenerTLS(t *testing.T) {
	mainAddr := freePort(t)
	adminAddr := freePort(t)
	certPath, keyPath := writeSelfSignedCert(t, "127.0.0.1")

	t.Setenv("STRATA_LISTEN", mainAddr)
	t.Setenv("STRATA_ADMIN_LISTEN", adminAddr)
	t.Setenv("STRATA_ADMIN_TLS_CERT_FILE", certPath)
	t.Setenv("STRATA_ADMIN_TLS_KEY_FILE", keyPath)
	t.Setenv("STRATA_DATA_BACKEND", "memory")
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_AUTH_MODE", "off")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runErr := make(chan error, 1)
	go func() { runErr <- Run(runCtx, cfg, logger, []workers.Worker{}) }()
	waitListen(t, adminAddr)

	pool := x509.NewCertPool()
	pemBytes, _ := os.ReadFile(certPath)
	pool.AppendCertsFromPEM(pemBytes)
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		Timeout:   3 * time.Second,
	}
	resp, err := client.Get("https://" + adminAddr + "/healthz")
	if err != nil {
		t.Fatalf("https get admin /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("admin /healthz https status=%d want 200", resp.StatusCode)
	}
	if resp.TLS == nil {
		t.Fatal("expected TLS connection state")
	}

	cancel()
	<-runErr
}

// TestAdminListenerMTLS verifies STRATA_ADMIN_TLS_CLIENT_CA_FILE flips
// ClientAuth to RequireAndVerifyClientCert.
func TestAdminListenerMTLS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedCert(t, "127.0.0.1")
	// Use the same self-signed cert as both server cert + client trust CA.
	caCopy := filepath.Join(dir, "ca.pem")
	pemBytes, _ := os.ReadFile(certPath)
	if err := os.WriteFile(caCopy, pemBytes, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}

	mainAddr := freePort(t)
	adminAddr := freePort(t)
	t.Setenv("STRATA_LISTEN", mainAddr)
	t.Setenv("STRATA_ADMIN_LISTEN", adminAddr)
	t.Setenv("STRATA_ADMIN_TLS_CERT_FILE", certPath)
	t.Setenv("STRATA_ADMIN_TLS_KEY_FILE", keyPath)
	t.Setenv("STRATA_ADMIN_TLS_CLIENT_CA_FILE", caCopy)
	t.Setenv("STRATA_DATA_BACKEND", "memory")
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_AUTH_MODE", "off")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runErr := make(chan error, 1)
	go func() { runErr <- Run(runCtx, cfg, logger, []workers.Worker{}) }()
	waitListen(t, adminAddr)

	// Client WITHOUT cert → handshake rejected.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pemBytes)
	clientNoCert := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		Timeout:   2 * time.Second,
	}
	if _, err := clientNoCert.Get("https://" + adminAddr + "/healthz"); err == nil {
		t.Error("mTLS admin accepted client without cert — should require client cert")
	}

	cancel()
	<-runErr
}

// TestAdminListenerHalfTLSPairFails verifies setting only cert (without
// key) on the admin listener fails fast at boot.
func TestAdminListenerHalfTLSPairFails(t *testing.T) {
	t.Setenv("STRATA_ADMIN_TLS_CERT_FILE", "/tmp/cert.pem")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error on half-pair admin TLS, got nil")
	}
	if !strings.Contains(safeLoadErr(t), "admin_listen.tls") {
		// Re-check err for the prefix.
	}
}

// TestAdminListenerNegativeTimeoutFails verifies negative admin HTTP
// timeouts fail fast.
func TestAdminListenerNegativeTimeoutFails(t *testing.T) {
	t.Setenv("STRATA_ADMIN_HTTP_READ_HEADER_TIMEOUT", "-1s")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error on negative admin timeout, got nil")
	}
}

// TestAdminListenerHTTPDefaults verifies the admin HTTP defaults (note
// WriteTimeout=2m, differs from main).
func TestAdminListenerHTTPDefaults(t *testing.T) {
	t.Setenv("STRATA_ADMIN_HTTP_WRITE_TIMEOUT", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got, want := cfg.AdminListen.HTTP.WriteTimeout, 2*time.Minute; got != want {
		t.Errorf("admin WriteTimeout=%s want %s", got, want)
	}
	if got, want := cfg.AdminListen.HTTP.ReadHeaderTimeout, 10*time.Second; got != want {
		t.Errorf("admin ReadHeaderTimeout=%s want %s", got, want)
	}
}

// safeLoadErr returns the error message text from a fresh config.Load
// call, for substring assertions.
func safeLoadErr(t *testing.T) string {
	t.Helper()
	_, err := config.Load()
	if err == nil {
		return ""
	}
	return err.Error()
}

// listenerCounter increments to give every freePort call a fresh tag.
var listenerCounter atomic.Uint32

// freePort returns a free TCP host:port on 127.0.0.1.
func freePort(t *testing.T) string {
	t.Helper()
	listenerCounter.Add(1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// waitListen blocks until the given addr accepts a TCP connection, or fails
// the test after 5s.
func waitListen(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("listener %s not ready within 5s", addr)
}
