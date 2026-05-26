package s3

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// resolveClusterTLS picks the effective TLS bundle for one cluster: per-
// cluster `tls` field on the S3ClusterSpec wins outright when set (any single
// key on the block replaces the global block — no merge, to avoid surprise
// semantics when one knob is omitted). Falls back to the global Config.TLS
// when the per-cluster block is nil or fully zero.
func resolveClusterTLS(spec S3ClusterSpec, global ClusterTLS) ClusterTLS {
	if spec.TLS != nil && spec.TLS.HasAny() {
		return *spec.TLS
	}
	return global
}

// buildTLSClient wraps base (or http.DefaultTransport when nil) with a TLS
// transport carrying the resolved CA + client cert + skip-verify knobs.
// Returns (nil, nil) when the bundle is fully zero so callers keep the
// existing HTTP-client semantic (Go default chain). PEM parse failures
// surface as boot-time errors via the connFor call chain.
func buildTLSClient(t ClusterTLS, base *http.Client) (*http.Client, error) {
	if !t.HasAny() {
		return nil, nil
	}
	tlsCfg := &tls.Config{
		InsecureSkipVerify: t.SkipVerify, //nolint:gosec // operator-opt-in; gauged + WARN-logged at boot
	}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("s3 tls: read ca_file %q: %w", t.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("s3 tls: ca_file %q: no certificates parsed", t.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	if t.CertFile != "" && t.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("s3 tls: load client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsCfg
	out := &http.Client{Transport: transport}
	if base != nil {
		out.Timeout = base.Timeout
		out.CheckRedirect = base.CheckRedirect
		out.Jar = base.Jar
	}
	return out, nil
}
