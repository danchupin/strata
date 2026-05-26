package cassandra

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTLSConfigHasAny enumerates the four shapes that decide between
// plain-TCP (zero value) and SslOpts-wired TLS at the cluster builder
// boundary.
func TestTLSConfigHasAny(t *testing.T) {
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

// TestNewClusterPlainTCP confirms that the zero-value TLS subset leaves
// SslOpts nil so the gocql session falls back to plain-TCP (backwards-compat
// baseline).
func TestNewClusterPlainTCP(t *testing.T) {
	c, err := newCluster(SessionConfig{
		Hosts: []string{"127.0.0.1:9042"},
	})
	if err != nil {
		t.Fatalf("newCluster: %v", err)
	}
	if c.SslOpts != nil {
		t.Fatalf("plain-TCP cfg: SslOpts=%v want nil", c.SslOpts)
	}
}

// TestNewClusterMutualTLS exercises the full mTLS shape: PEM-encoded CA +
// client cert/key written to disk, fed through buildSslOptions, asserts the
// resulting tls.Config carries one client cert and a non-empty root pool with
// EnableHostVerification=true (the safe default).
func TestNewClusterMutualTLS(t *testing.T) {
	dir := t.TempDir()
	caPath, certPath, keyPath := writeTestCertPair(t, dir)

	c, err := newCluster(SessionConfig{
		Hosts: []string{"127.0.0.1:9042"},
		TLS: TLSConfig{
			CAFile:   caPath,
			CertFile: certPath,
			KeyFile:  keyPath,
		},
	})
	if err != nil {
		t.Fatalf("newCluster: %v", err)
	}
	if c.SslOpts == nil {
		t.Fatal("SslOpts=nil for mTLS cfg")
	}
	if !c.SslOpts.EnableHostVerification {
		t.Fatal("EnableHostVerification=false for non-skip-verify cfg")
	}
	if c.SslOpts.Config == nil {
		t.Fatal("SslOpts.Config=nil")
	}
	if c.SslOpts.Config.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify=true for non-skip-verify cfg")
	}
	if c.SslOpts.Config.RootCAs == nil {
		t.Fatal("RootCAs=nil after CAFile load")
	}
	if got := len(c.SslOpts.Config.Certificates); got != 1 {
		t.Fatalf("Certificates len=%d want 1", got)
	}
}

// TestNewClusterSkipVerify confirms skip_verify=true wires both
// InsecureSkipVerify=true AND EnableHostVerification=false (matched pair —
// either alone leaves gocql in a half-checked state).
func TestNewClusterSkipVerify(t *testing.T) {
	c, err := newCluster(SessionConfig{
		Hosts: []string{"127.0.0.1:9042"},
		TLS:   TLSConfig{SkipVerify: true},
	})
	if err != nil {
		t.Fatalf("newCluster: %v", err)
	}
	if c.SslOpts == nil {
		t.Fatal("SslOpts=nil for skip-verify cfg")
	}
	if !c.SslOpts.Config.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify=false for skip-verify cfg")
	}
	if c.SslOpts.EnableHostVerification {
		t.Fatal("EnableHostVerification=true for skip-verify cfg")
	}
}

// TestNewClusterCAOnly confirms that a CA-only TLS subset (no client cert)
// is accepted — server-auth-only TLS is a valid shape.
func TestNewClusterCAOnly(t *testing.T) {
	dir := t.TempDir()
	caPath, _, _ := writeTestCertPair(t, dir)

	c, err := newCluster(SessionConfig{
		Hosts: []string{"127.0.0.1:9042"},
		TLS:   TLSConfig{CAFile: caPath},
	})
	if err != nil {
		t.Fatalf("newCluster: %v", err)
	}
	if c.SslOpts == nil {
		t.Fatal("SslOpts=nil for CA-only cfg")
	}
	if c.SslOpts.Config.RootCAs == nil {
		t.Fatal("RootCAs=nil after CAFile load")
	}
	if len(c.SslOpts.Config.Certificates) != 0 {
		t.Fatalf("Certificates len=%d want 0 (server-auth-only)", len(c.SslOpts.Config.Certificates))
	}
}

// TestNewClusterMissingCAFails surfaces a clear error when the CA path is
// bogus — better than a delayed handshake-time failure against the cluster.
func TestNewClusterMissingCAFails(t *testing.T) {
	_, err := newCluster(SessionConfig{
		Hosts: []string{"127.0.0.1:9042"},
		TLS:   TLSConfig{CAFile: "/nonexistent/ca.pem"},
	})
	if err == nil {
		t.Fatal("newCluster with missing ca_file: want error")
	}
	if !strings.Contains(err.Error(), "ca_file") {
		t.Fatalf("err=%q must name ca_file", err.Error())
	}
}

// TestNewClusterMalformedCAFails surfaces a clear error when the CA bundle
// contains no parseable PEM blocks.
func TestNewClusterMalformedCAFails(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad-ca.pem")
	if err := os.WriteFile(bad, []byte("not pem at all"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := newCluster(SessionConfig{
		Hosts: []string{"127.0.0.1:9042"},
		TLS:   TLSConfig{CAFile: bad},
	})
	if err == nil {
		t.Fatal("newCluster with malformed ca_file: want error")
	}
	if !strings.Contains(err.Error(), "no certificates parsed") {
		t.Fatalf("err=%q must mention parse failure", err.Error())
	}
}

// writeTestCertPair generates a self-signed P-256 ECDSA cert+key pair and
// writes the PEM-encoded CA + cert + key to dir. Returns the three paths.
// Shared shape across US-002/US-003 TLS tests (see internal/serverapp/tls_test.go).
func writeTestCertPair(t *testing.T, dir string) (caPath, certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "strata-cassandra-test"},
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
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return caPath, certPath, keyPath
}
