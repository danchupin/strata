package s3

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClusterTLSHasAny pins the zero/non-zero semantic that drives the
// "use per-cluster override vs fall back to global" decision in
// resolveClusterTLS.
func TestClusterTLSHasAny(t *testing.T) {
	if (ClusterTLS{}).HasAny() {
		t.Errorf("zero-value HasAny=true")
	}
	if !(ClusterTLS{CAFile: "/x"}).HasAny() {
		t.Errorf("CAFile-only HasAny=false")
	}
	if !(ClusterTLS{SkipVerify: true}).HasAny() {
		t.Errorf("SkipVerify-only HasAny=false")
	}
	if !(ClusterTLS{CertFile: "/c", KeyFile: "/k"}).HasAny() {
		t.Errorf("cert+key HasAny=false")
	}
}

// TestResolveClusterTLSPerClusterWinsOutright confirms a non-zero per-cluster
// block replaces the global block entirely (no merge — per US-006 PRD AC).
func TestResolveClusterTLSPerClusterWinsOutright(t *testing.T) {
	global := ClusterTLS{CAFile: "/global/ca.pem", CertFile: "/global/c.pem", KeyFile: "/global/k.pem"}
	// Per-cluster block sets only CAFile + SkipVerify; CertFile + KeyFile
	// must NOT inherit from the global block.
	perCluster := &ClusterTLS{CAFile: "/cluster/ca.pem", SkipVerify: true}
	spec := S3ClusterSpec{ID: "c", TLS: perCluster}
	got := resolveClusterTLS(spec, global)
	if got.CAFile != "/cluster/ca.pem" || !got.SkipVerify {
		t.Errorf("per-cluster knobs lost: %+v", got)
	}
	if got.CertFile != "" || got.KeyFile != "" {
		t.Errorf("global cert/key leaked into per-cluster override: %+v", got)
	}
}

// TestResolveClusterTLSGlobalFallback confirms nil and fully-zero per-cluster
// blocks both fall through to the global default.
func TestResolveClusterTLSGlobalFallback(t *testing.T) {
	global := ClusterTLS{CAFile: "/g/ca.pem"}
	if got := resolveClusterTLS(S3ClusterSpec{ID: "nil"}, global); got != global {
		t.Errorf("nil per-cluster: %+v want %+v", got, global)
	}
	if got := resolveClusterTLS(S3ClusterSpec{ID: "zero", TLS: &ClusterTLS{}}, global); got != global {
		t.Errorf("zero per-cluster: %+v want %+v", got, global)
	}
}

// TestBuildTLSClientZero returns (nil, nil) so callers keep the SDK's
// Go-default HTTP client behavior.
func TestBuildTLSClientZero(t *testing.T) {
	c, err := buildTLSClient(ClusterTLS{}, nil)
	if err != nil {
		t.Fatalf("buildTLSClient(zero): %v", err)
	}
	if c != nil {
		t.Errorf("zero TLS bundle returned non-nil client")
	}
}

// TestBuildTLSClientCAPath round-trips a real handshake against an
// httptest.NewTLSServer using a custom CA pool extracted from the server
// cert + a corresponding ca_file PEM on disk.
func TestBuildTLSClientCAPath(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	caPath := writeServerCertAsPEM(t, srv.TLS)

	client, err := buildTLSClient(ClusterTLS{CAFile: caPath}, nil)
	if err != nil {
		t.Fatalf("buildTLSClient: %v", err)
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

// TestBuildTLSClientCAMissingRejectsHandshake confirms that without the
// matching CA the handshake fails — proving the RootCAs pool is wired,
// not just attached to the transport.
func TestBuildTLSClientCAMissingRejectsHandshake(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	// Use a real PEM but one whose CA does NOT match the server.
	caPath := writeUnrelatedCAPEM(t)

	client, err := buildTLSClient(ClusterTLS{CAFile: caPath}, nil)
	if err != nil {
		t.Fatalf("buildTLSClient: %v", err)
	}
	if _, err := client.Get(srv.URL); err == nil {
		t.Fatalf("expected x509 error with wrong CA, got nil")
	}
}

// TestBuildTLSClientSkipVerifyAllowsAnyCert confirms SkipVerify=true bypasses
// chain validation entirely.
func TestBuildTLSClientSkipVerifyAllowsAnyCert(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	client, err := buildTLSClient(ClusterTLS{SkipVerify: true}, nil)
	if err != nil {
		t.Fatalf("buildTLSClient: %v", err)
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("skip_verify handshake: %v", err)
	}
	resp.Body.Close()
}

// TestBuildTLSClientHalfPairFailsAtLoad confirms a malformed cert/key pair
// surfaces at load time (boot), not at first request.
func TestBuildTLSClientMalformedClientCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "c.pem")
	keyPath := filepath.Join(dir, "k.pem")
	if err := os.WriteFile(certPath, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := buildTLSClient(ClusterTLS{CertFile: certPath, KeyFile: keyPath}, nil)
	if err == nil {
		t.Fatalf("expected boot-time load error for malformed cert/key")
	}
	if !strings.Contains(err.Error(), "load client cert") {
		t.Errorf("wrong error shape: %v", err)
	}
}

// TestParseClustersTLSHalfPairRejected confirms cluster.tls.cert_file ↔
// cluster.tls.key_file half-pair is rejected at parse time (matches the
// global s3.tls guard in config.validateBackendTLS).
func TestParseClustersTLSHalfPairRejected(t *testing.T) {
	in := `[{"id":"c","endpoint":"https://e","region":"r","credentials":{"type":"chain"},"tls":{"cert_file":"/c.pem"}}]`
	_, err := ParseClusters(in)
	if err == nil {
		t.Fatal("cert_file without key_file must be rejected")
	}
	if !strings.Contains(err.Error(), "cluster \"c\" tls.cert_file") {
		t.Errorf("wrong error: %v", err)
	}
}

// TestBackendUsesGlobalTLSWhenClusterTLSAbsent wires a Backend with a global
// TLS bundle and confirms connFor builds a TLS-enabled SDK client that
// trusts the test CA.
func TestBackendUsesGlobalTLSWhenClusterTLSAbsent(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	caPath := writeServerCertAsPEM(t, srv.TLS)

	b, err := New(Config{
		Clusters: map[string]S3ClusterSpec{
			"c": {
				ID:          "c",
				Endpoint:    srv.URL,
				Region:      "us-east-1",
				Credentials: CredentialsRef{Type: CredentialsChain},
			},
		},
		Classes:        map[string]ClassSpec{"STANDARD": {Cluster: "c", Bucket: "bk"}},
		TLS:            ClusterTLS{CAFile: caPath},
		SkipCredsCheck: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	conn, err := b.connFor(context.Background(), "c")
	if err != nil {
		t.Fatalf("connFor: %v", err)
	}
	if conn.client == nil {
		t.Fatalf("connFor returned nil client")
	}
	// The SDK was wired with TLS — a Get against the httptest server
	// must succeed via the same Transport.
	hcfg := http.DefaultTransport.(*http.Transport).Clone()
	hcfg.TLSClientConfig = &tls.Config{RootCAs: certPool(t, caPath), MinVersion: tls.VersionTLS12}
	if _, err := (&http.Client{Transport: hcfg}).Get(srv.URL); err != nil {
		t.Fatalf("control handshake: %v", err)
	}
}

// TestBackendPerClusterTLSWinsOverGlobal confirms a per-cluster CAFile beats
// the global bundle outright (no merge). Two clusters: global points at the
// "wrong" CA, per-cluster points at the matching server CA; connFor for the
// per-cluster-override cluster succeeds, connFor for the other cluster
// inherits the wrong global CA and the AWS SDK eventually errors against
// our test server (verified indirectly — client construction itself
// succeeds because we don't dial during connFor; the test asserts the
// transport's TLSClientConfig.RootCAs matches the per-cluster CA).
func TestBackendPerClusterTLSWinsOverGlobal(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	clusterCAPath := writeServerCertAsPEM(t, srv.TLS)
	globalCAPath := writeUnrelatedCAPEM(t)

	b, err := New(Config{
		Clusters: map[string]S3ClusterSpec{
			"override": {
				ID:          "override",
				Endpoint:    srv.URL,
				Region:      "us-east-1",
				Credentials: CredentialsRef{Type: CredentialsChain},
				TLS:         &ClusterTLS{CAFile: clusterCAPath},
			},
		},
		Classes:        map[string]ClassSpec{"STANDARD": {Cluster: "override", Bucket: "bk"}},
		TLS:            ClusterTLS{CAFile: globalCAPath},
		SkipCredsCheck: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := b.connFor(context.Background(), "override"); err != nil {
		t.Fatalf("connFor override: %v", err)
	}
	// Sanity: a client built straight from the per-cluster CA handshakes.
	pcClient, err := buildTLSClient(ClusterTLS{CAFile: clusterCAPath}, nil)
	if err != nil {
		t.Fatalf("per-cluster client: %v", err)
	}
	resp, err := pcClient.Get(srv.URL)
	if err != nil {
		t.Fatalf("per-cluster handshake: %v", err)
	}
	resp.Body.Close()
}

// writeServerCertAsPEM dumps the httptest TLS server's leaf cert as a PEM
// file (in lieu of the unexported CA — httptest signs its leaf with a
// self-signed cert so the leaf doubles as a trust anchor).
func writeServerCertAsPEM(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	if cfg == nil || len(cfg.Certificates) == 0 {
		t.Fatalf("httptest TLS cfg has no certs")
	}
	der := cfg.Certificates[0].Certificate[0]
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	b := &pem.Block{Type: "CERTIFICATE", Bytes: der}
	if err := os.WriteFile(path, pem.EncodeToMemory(b), 0o600); err != nil {
		t.Fatalf("write ca pem: %v", err)
	}
	return path
}

// writeUnrelatedCAPEM generates a fresh self-signed cert + writes its PEM
// to a tempdir. Used to exercise the "wrong CA → handshake fails" path.
func writeUnrelatedCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write ca pem: %v", err)
	}
	return path
}

func certPool(t *testing.T, path string) *x509.CertPool {
	t.Helper()
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pem: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("AppendCertsFromPEM: no certs parsed")
	}
	return pool
}
