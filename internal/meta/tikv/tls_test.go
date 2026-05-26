package tikv

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tikvcfg "github.com/tikv/client-go/v2/config"
)

// TestTiKVTLSConfigHasAny enumerates the four shapes that decide between
// plain-gRPC (zero value) and Security-wired TLS at the tikv backend
// boundary.
func TestTiKVTLSConfigHasAny(t *testing.T) {
	cases := []struct {
		name string
		cfg  TLSConfig
		want bool
	}{
		{"empty", TLSConfig{}, false},
		{"ca_only", TLSConfig{CAFile: "/x"}, true},
		{"cert_pair", TLSConfig{CertFile: "/c", KeyFile: "/k"}, true},
		{"skip_verify", TLSConfig{SkipVerify: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.HasAny(); got != tc.want {
				t.Fatalf("HasAny()=%v want %v", got, tc.want)
			}
		})
	}
}

// TestApplyTiKVSecurityRejectsMissingCA — the upstream Security.ToTLSConfig
// short-circuits on empty ClusterSSLCA and silently falls back to plain-gRPC,
// which would defeat operator intent. We surface this at boot.
func TestApplyTiKVSecurityRejectsMissingCA(t *testing.T) {
	err := applyTiKVSecurity(TLSConfig{SkipVerify: true})
	if err == nil {
		t.Fatal("applyTiKVSecurity(SkipVerify only): want error")
	}
	if !strings.Contains(err.Error(), "ca_file") {
		t.Fatalf("err=%q must name ca_file", err.Error())
	}
}

// TestApplyTiKVSecurityRejectsHalfPair surfaces a clear error when only one
// of cert/key is set — mirrors the gocql parallel + the config-level guard.
func TestApplyTiKVSecurityRejectsHalfPair(t *testing.T) {
	dir := t.TempDir()
	caPath, certPath, _ := writeTiKVTestCertPair(t, dir)

	err := applyTiKVSecurity(TLSConfig{CAFile: caPath, CertFile: certPath})
	if err == nil {
		t.Fatal("applyTiKVSecurity(half-pair): want error")
	}
	if !strings.Contains(err.Error(), "key_file") {
		t.Fatalf("err=%q must mention key_file", err.Error())
	}
}

// TestApplyTiKVSecurityRejectsBadKeyPair surfaces PEM parse failures at boot.
func TestApplyTiKVSecurityRejectsBadKeyPair(t *testing.T) {
	dir := t.TempDir()
	caPath, _, _ := writeTiKVTestCertPair(t, dir)
	bad := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(bad, []byte("not pem"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := applyTiKVSecurity(TLSConfig{CAFile: caPath, CertFile: bad, KeyFile: bad})
	if err == nil {
		t.Fatal("applyTiKVSecurity(bad cert/key): want error")
	}
	if !strings.Contains(err.Error(), "cert/key") {
		t.Fatalf("err=%q must mention cert/key", err.Error())
	}
}

// TestApplyTiKVSecurityInstallsGlobalSecurity verifies that a valid TLS
// bundle is propagated to tikv-client-go's global config so subsequent
// NewClient calls negotiate mTLS.
func TestApplyTiKVSecurityInstallsGlobalSecurity(t *testing.T) {
	prev := tikvcfg.GetGlobalConfig()
	t.Cleanup(func() { tikvcfg.StoreGlobalConfig(prev) })

	dir := t.TempDir()
	caPath, certPath, keyPath := writeTiKVTestCertPair(t, dir)
	if err := applyTiKVSecurity(TLSConfig{
		CAFile:   caPath,
		CertFile: certPath,
		KeyFile:  keyPath,
	}); err != nil {
		t.Fatalf("applyTiKVSecurity: %v", err)
	}
	got := tikvcfg.GetGlobalConfig().Security
	if got.ClusterSSLCA != caPath {
		t.Errorf("ClusterSSLCA=%q want %q", got.ClusterSSLCA, caPath)
	}
	if got.ClusterSSLCert != certPath {
		t.Errorf("ClusterSSLCert=%q want %q", got.ClusterSSLCert, certPath)
	}
	if got.ClusterSSLKey != keyPath {
		t.Errorf("ClusterSSLKey=%q want %q", got.ClusterSSLKey, keyPath)
	}
	tc, err := got.ToTLSConfig()
	if err != nil {
		t.Fatalf("ToTLSConfig: %v", err)
	}
	if tc == nil || tc.RootCAs == nil {
		t.Fatal("ToTLSConfig returned no RootCAs after CAFile load")
	}
}

// TestNewPDClientWithTLSHTTPSScheme confirms TLS-configured pd clients
// default to https:// when the endpoint omits a scheme so PD's HTTPS-only
// listener responds.
func TestNewPDClientWithTLSHTTPSScheme(t *testing.T) {
	c := newPDClientWithTLS([]string{"pd0:2379"}, TLSConfig{SkipVerify: true})
	if c.scheme != "https" {
		t.Fatalf("scheme=%q want https", c.scheme)
	}
}

// TestNewPDClientPlainScheme — zero TLS keeps plain http for backwards-compat.
func TestNewPDClientPlainScheme(t *testing.T) {
	c := newPDClientWithTLS([]string{"pd0:2379"}, TLSConfig{})
	if c.scheme != "http" {
		t.Fatalf("scheme=%q want http", c.scheme)
	}
}

// TestNewPDClientWithTLSSkipVerify wires InsecureSkipVerify onto the PD HTTP
// transport so operators investigating self-signed clusters get a usable
// control plane even when chain verification fails.
func TestNewPDClientWithTLSSkipVerify(t *testing.T) {
	c := newPDClientWithTLS([]string{"pd0:2379"}, TLSConfig{SkipVerify: true})
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T want *http.Transport", c.http.Transport)
	}
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify=false on PD TLS transport")
	}
}

// TestNewPDClientWithTLSLoadsCABundle confirms a CA PEM bundle populates
// RootCAs on the PD HTTP transport so the control-plane probe verifies the
// PD server cert against operator-supplied roots.
func TestNewPDClientWithTLSLoadsCABundle(t *testing.T) {
	dir := t.TempDir()
	caPath, _, _ := writeTiKVTestCertPair(t, dir)
	c := newPDClientWithTLS([]string{"pd0:2379"}, TLSConfig{CAFile: caPath})
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T want *http.Transport", c.http.Transport)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("RootCAs=nil after CAFile load")
	}
	_ = tls.VersionTLS12 // keep tls import used even if Go pruner moves
}

// writeTiKVTestCertPair generates a self-signed P-256 ECDSA cert+key pair
// and writes the PEM-encoded CA + cert + key to dir. Mirrors the cassandra
// parallel — keeps US-005 tests hermetic without depending on the cassandra
// package.
func writeTiKVTestCertPair(t *testing.T, dir string) (caPath, certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "strata-tikv-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("createCert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshalKey: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	caPath = filepath.Join(dir, "ca.pem")
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	for _, w := range []struct {
		path string
		data []byte
	}{{caPath, certPEM}, {certPath, certPEM}, {keyPath, keyPEM}} {
		if err := os.WriteFile(w.path, w.data, 0o600); err != nil {
			t.Fatalf("write %s: %v", w.path, err)
		}
	}
	return caPath, certPath, keyPath
}
