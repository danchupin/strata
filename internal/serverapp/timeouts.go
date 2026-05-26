package serverapp

import (
	"net/http"

	"github.com/danchupin/strata/internal/config"
)

// newHTTPServer builds the gateway *http.Server with per-connection
// timeouts sourced from cfg.HTTP (US-001 harden-gateway). Range validation
// + upper-bound clamps live in config.validateHTTP / clampHTTP — values
// reaching here are trusted. Zero passes through to net/http as
// "disabled" (dev / loopback profile).
func newHTTPServer(addr string, handler http.Handler, cfg *config.Config) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		MaxHeaderBytes:    cfg.HTTP.MaxHeaderBytes,
	}
}
