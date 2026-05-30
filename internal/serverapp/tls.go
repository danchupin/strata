package serverapp

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/danchupin/strata/internal/config"
)

// buildTLSConfig builds a *tls.Config from cfg.TLS (US-002 + US-003
// harden-gateway). Returns (nil, nil, nil) when neither CertFile nor
// CertDir is set — caller falls back to plain HTTP. config.validateTLS
// guarantees CertFile + KeyFile are both set or both empty and that
// CertFile + CertDir are mutually exclusive.
//
// MinVersion enum: "TLS1.2" → tls.VersionTLS12, "TLS1.3" → tls.VersionTLS13.
// Empty falls back to TLS 1.2.
//
// CipherProfile shapes the TLS 1.2 CipherSuites field:
//   - mozilla-modern (default): the three TLS 1.3 AEAD suites plus two
//     TLS 1.2 ECDHE-AES-128-GCM entries required by Go's http2 server.
//   - mozilla-intermediate: ECDHE + AEAD suites per Mozilla Intermediate.
//   - go-default: CipherSuites left nil; Go's curated safe defaults.
//
// CipherSuites is informational on TLS 1.3 connections per RFC 8446 (Go's
// tls package ignores the field there).
//
// US-003 adds:
//   - SNI multi-cert dispatch via a certStore-backed GetCertificate
//     callback (atomic.Pointer snapshot; lock-free per-handshake reads).
//   - Optional client-cert verification via ClientCAFile (PEM); enabling
//     it flips ClientAuth to RequireAndVerifyClientCert.
//
// The returned certStore is non-nil when TLS is enabled; the caller
// hands it to a certReloader for hot-reload + periodic reconciliation.
func buildTLSConfig(cfg *config.Config) (*tls.Config, *certStore, error) {
	if cfg.TLS.CertFile == "" && cfg.TLS.CertDir == "" {
		return nil, nil, nil
	}
	store := &certStore{}
	var initial *certSnapshot
	var err error
	if cfg.TLS.CertDir != "" {
		initial, err = buildSnapshotFromDir(cfg.TLS.CertDir)
	} else {
		initial, err = buildSnapshotFromSingle(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("tls init: %w", err)
	}
	store.swap(initial)

	// MinVersion comes from tlsMinVersion(), which floors at tls.VersionTLS12
	// (default) and only ever goes up to TLS1.3 — never below 1.2. gosec G402
	// can't trace the helper return, so it false-positives "MinVersion too low".
	tlsCfg := &tls.Config{ // #nosec G402 -- MinVersion >= TLS1.2 via tlsMinVersion()
		GetCertificate: store.GetCertificate,
		MinVersion:     tlsMinVersion(cfg.TLS.MinVersion),
		CipherSuites:   tlsCipherSuites(cfg.TLS.CipherProfile),
		NextProtos:     []string{"h2", "http/1.1"},
	}
	if cfg.TLS.ClientCAFile != "" {
		caPEM, err := os.ReadFile(cfg.TLS.ClientCAFile)
		if err != nil {
			return nil, nil, fmt.Errorf("tls client CA %s: %w", cfg.TLS.ClientCAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, nil, fmt.Errorf("tls client CA %s: AppendCertsFromPEM failed (no valid PEM blocks)", cfg.TLS.ClientCAFile)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return tlsCfg, store, nil
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
