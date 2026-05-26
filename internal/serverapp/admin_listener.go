package serverapp

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"

	"github.com/danchupin/strata/internal/config"
)

// newAdminHTTPServer builds the admin-listener *http.Server with per-
// connection timeouts sourced from cfg.AdminListen.HTTP (US-008
// harden-gateway). Range validation + upper-bound clamps live in
// config.validateAdminListen / clampAdminListen — values reaching here are
// trusted.
func newAdminHTTPServer(addr string, handler http.Handler, cfg *config.Config) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.AdminListen.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.AdminListen.HTTP.ReadTimeout,
		WriteTimeout:      cfg.AdminListen.HTTP.WriteTimeout,
		IdleTimeout:       cfg.AdminListen.HTTP.IdleTimeout,
		MaxHeaderBytes:    cfg.AdminListen.HTTP.MaxHeaderBytes,
	}
}

// buildAdminTLSConfig loads the admin-listener TLS bundle. Returns
// (nil, nil) when CertFile is empty (plain HTTP — typical loopback shape).
// When ClientCAFile is set, the resulting *tls.Config requires mTLS via
// RequireAndVerifyClientCert. Kept simple: no SNI / cert-dir / hot-reload
// — the admin listener is a single endpoint per replica.
func buildAdminTLSConfig(cfg *config.Config) (*tls.Config, error) {
	t := cfg.AdminListen.TLS
	if t.CertFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("admin tls keypair: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	if t.ClientCAFile != "" {
		caPEM, err := os.ReadFile(t.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("admin tls client CA %s: %w", t.ClientCAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("admin tls client CA %s: AppendCertsFromPEM failed (no valid PEM blocks)", t.ClientCAFile)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return tlsCfg, nil
}
