package serverapp

import (
	"crypto/tls"
	"fmt"

	"github.com/danchupin/strata/internal/config"
)

// buildTLSConfig builds a *tls.Config from cfg.TLS (US-002 harden-gateway).
// Returns (nil, nil) when CertFile is empty — caller falls back to plain
// HTTP. config.validateTLS guarantees CertFile + KeyFile are both set or
// both empty, so a non-empty CertFile here implies KeyFile is non-empty
// too. The certificate is loaded at boot via tls.LoadX509KeyPair and
// stamped onto Certificates so srv.ListenAndServeTLS("","") picks it up.
//
// MinVersion enum: "TLS1.2" → tls.VersionTLS12, "TLS1.3" → tls.VersionTLS13.
// Empty falls back to TLS 1.2.
//
// CipherProfile shapes the TLS 1.2 CipherSuites field:
//   - mozilla-modern (default): only the three TLS 1.3 AEAD suites
//     (TLS_AES_128_GCM_SHA256, TLS_AES_256_GCM_SHA384,
//     TLS_CHACHA20_POLY1305_SHA256). TLS 1.2 clients have no matching
//     cipher and the handshake fails — effectively TLS-1.3-only mode.
//   - mozilla-intermediate: ECDHE + AEAD suites per Mozilla Intermediate
//     profile; supports TLS 1.2 + 1.3.
//   - go-default: CipherSuites left nil; Go's curated safe defaults apply.
//
// CipherSuites is informational on TLS 1.3 connections per RFC 8446 (Go's
// tls package ignores the field there).
func buildTLSConfig(cfg *config.Config) (*tls.Config, error) {
	if cfg.TLS.CertFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls load key pair: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tlsMinVersion(cfg.TLS.MinVersion),
		CipherSuites: tlsCipherSuites(cfg.TLS.CipherProfile),
		NextProtos:   []string{"h2", "http/1.1"},
	}
	return tlsCfg, nil
}

func tlsMinVersion(v string) uint16 {
	switch v {
	case "TLS1.3":
		return tls.VersionTLS13
	default:
		return tls.VersionTLS12
	}
}

// mozillaModernCipherSuites pins the three TLS 1.3 AEAD suites per Mozilla
// Modern profile, plus the two TLS 1.2 ECDHE-AES-128-GCM ciphers required
// by RFC 7540 / Go's golang.org/x/net/http2 ConfigureServer. Without the
// h2 minimum, ListenAndServeTLS refuses to wire HTTP/2 even when only TLS
// 1.3 is actually negotiated. Pair mozilla-modern with MinVersion=TLS1.3
// (recommended) to refuse TLS 1.2 clients regardless of cipher offer.
var mozillaModernCipherSuites = []uint16{
	tls.TLS_AES_128_GCM_SHA256,
	tls.TLS_AES_256_GCM_SHA384,
	tls.TLS_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
}

// mozillaIntermediateCipherSuites is the TLS 1.2 AEAD suite set from the
// Mozilla SSL Config Generator Intermediate profile. TLS 1.3 handshakes
// ignore the list per RFC 8446.
var mozillaIntermediateCipherSuites = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
}

func tlsCipherSuites(profile string) []uint16 {
	switch profile {
	case "mozilla-intermediate":
		return mozillaIntermediateCipherSuites
	case "go-default":
		return nil
	default:
		return mozillaModernCipherSuites
	}
}
