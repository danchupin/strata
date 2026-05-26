package serverapp

import (
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
	"testing"
	"time"

	"github.com/danchupin/strata/internal/config"
)

// TestBuildTLSConfigDisabledByDefault asserts an empty TLSConfig surface
// yields nil — caller falls back to plain HTTP.
func TestBuildTLSConfigDisabledByDefault(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	got, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil TLS config when CertFile empty, got %#v", got)
	}
}

// TestBuildTLSConfigDefaultsToMozillaModern asserts the default profile
// pins the three TLS 1.3 AEAD suites and MinVersion=TLS1.2.
func TestBuildTLSConfigDefaultsToMozillaModern(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, "localhost")
	t.Setenv("STRATA_TLS_CERT_FILE", certPath)
	t.Setenv("STRATA_TLS_KEY_FILE", keyPath)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsCfg == nil {
		t.Fatal("expected non-nil TLS config")
	}
	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion=%x want %x (TLS1.2)", tlsCfg.MinVersion, tls.VersionTLS12)
	}
	wantSuites := mozillaModernCipherSuites
	if len(tlsCfg.CipherSuites) != len(wantSuites) {
		t.Fatalf("CipherSuites len=%d want %d", len(tlsCfg.CipherSuites), len(wantSuites))
	}
	for i, s := range wantSuites {
		if tlsCfg.CipherSuites[i] != s {
			t.Errorf("CipherSuites[%d]=%x want %x", i, tlsCfg.CipherSuites[i], s)
		}
	}
}

// TestBuildTLSConfigMinVersionTLS13 asserts STRATA_TLS_MIN_VERSION=TLS1.3
// flows through.
func TestBuildTLSConfigMinVersionTLS13(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, "localhost")
	t.Setenv("STRATA_TLS_CERT_FILE", certPath)
	t.Setenv("STRATA_TLS_KEY_FILE", keyPath)
	t.Setenv("STRATA_TLS_MIN_VERSION", "TLS1.3")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsCfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion=%x want %x (TLS1.3)", tlsCfg.MinVersion, tls.VersionTLS13)
	}
}

// TestBuildTLSConfigIntermediateProfile asserts the intermediate profile
// expands to the Mozilla Intermediate AEAD suite list.
func TestBuildTLSConfigIntermediateProfile(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, "localhost")
	t.Setenv("STRATA_TLS_CERT_FILE", certPath)
	t.Setenv("STRATA_TLS_KEY_FILE", keyPath)
	t.Setenv("STRATA_TLS_CIPHER_PROFILE", "mozilla-intermediate")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if len(tlsCfg.CipherSuites) != len(mozillaIntermediateCipherSuites) {
		t.Fatalf("CipherSuites len=%d want %d", len(tlsCfg.CipherSuites), len(mozillaIntermediateCipherSuites))
	}
}

// TestBuildTLSConfigGoDefaultProfile asserts go-default leaves CipherSuites
// nil so Go's curated defaults apply.
func TestBuildTLSConfigGoDefaultProfile(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, "localhost")
	t.Setenv("STRATA_TLS_CERT_FILE", certPath)
	t.Setenv("STRATA_TLS_KEY_FILE", keyPath)
	t.Setenv("STRATA_TLS_CIPHER_PROFILE", "go-default")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsCfg.CipherSuites != nil {
		t.Errorf("CipherSuites=%v want nil (go-default)", tlsCfg.CipherSuites)
	}
}

// TestConfigLoadRejectsInvalidMinVersion asserts unknown TLS protocol
// version fails fast at boot.
func TestConfigLoadRejectsInvalidMinVersion(t *testing.T) {
	t.Setenv("STRATA_TLS_MIN_VERSION", "TLS1.1")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestConfigLoadRejectsInvalidCipherProfile asserts unknown cipher profile
// fails fast at boot.
func TestConfigLoadRejectsInvalidCipherProfile(t *testing.T) {
	t.Setenv("STRATA_TLS_CIPHER_PROFILE", "frankencipher")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestConfigLoadRejectsHalfTLSPair asserts setting only cert (without key)
// fails fast — protects operators from silently degrading to plain HTTP.
func TestConfigLoadRejectsHalfTLSPair(t *testing.T) {
	t.Setenv("STRATA_TLS_CERT_FILE", "/tmp/cert.pem")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestHTTPSHandshake runs a real handshake against a server wired with the
// US-002 TLS config. Asserts an HTTPS client with the cert in its root CA
// pool can reach the server.
func TestHTTPSHandshake(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, "127.0.0.1")
	t.Setenv("STRATA_TLS_CERT_FILE", certPath)
	t.Setenv("STRATA_TLS_KEY_FILE", keyPath)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = tlsCfg
	ts.StartTLS()
	t.Cleanup(ts.Close)

	pool := x509.NewCertPool()
	pem, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("AppendCertsFromPEM failed")
	}
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		Timeout:   3 * time.Second,
	}
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("https get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status=%d want %d", resp.StatusCode, http.StatusNoContent)
	}
	if resp.TLS == nil {
		t.Fatal("expected resp.TLS to be populated")
	}
	if resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("negotiated TLS version %x below TLS 1.2", resp.TLS.Version)
	}
}

// TestTLS13MinRejectsTLS12Client asserts MinVersion=TLS1.3 refuses a TLS
// 1.2 client.
func TestTLS13MinRejectsTLS12Client(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, "127.0.0.1")
	t.Setenv("STRATA_TLS_CERT_FILE", certPath)
	t.Setenv("STRATA_TLS_KEY_FILE", keyPath)
	t.Setenv("STRATA_TLS_MIN_VERSION", "TLS1.3")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = tlsCfg
	ts.StartTLS()
	t.Cleanup(ts.Close)

	pool := x509.NewCertPool()
	pemBytes, _ := os.ReadFile(certPath)
	pool.AppendCertsFromPEM(pemBytes)

	clientCfg := &tls.Config{
		RootCAs:    pool,
		MaxVersion: tls.VersionTLS12,
	}
	host := ts.Listener.Addr().String()
	conn, err := tls.Dial("tcp", host, clientCfg)
	if err == nil {
		conn.Close()
		t.Fatal("expected handshake failure for TLS 1.2 client against TLS1.3-min server")
	}
}

// TestMozillaModernRejectsWeakCipher asserts the default mozilla-modern
// profile refuses a TLS 1.2 client that offers only ciphers outside the
// pinned AEAD-GCM/CHACHA set (e.g. RC4-flavoured RSA). PRD US-002 (d).
func TestMozillaModernRejectsWeakCipher(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, "127.0.0.1")
	t.Setenv("STRATA_TLS_CERT_FILE", certPath)
	t.Setenv("STRATA_TLS_KEY_FILE", keyPath)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = tlsCfg
	ts.StartTLS()
	t.Cleanup(ts.Close)

	pool := x509.NewCertPool()
	pemBytes, _ := os.ReadFile(certPath)
	pool.AppendCertsFromPEM(pemBytes)

	// Client offers only legacy non-ECDHE-GCM ciphers — mozilla-modern
	// has none of these; handshake_failure expected.
	clientCfg := &tls.Config{
		RootCAs:    pool,
		MaxVersion: tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_RSA_WITH_RC4_128_SHA,
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		},
	}
	host := ts.Listener.Addr().String()
	conn, err := tls.Dial("tcp", host, clientCfg)
	if err == nil {
		conn.Close()
		t.Fatal("expected handshake failure: mozilla-modern has no matching cipher")
	}
}

// writeSelfSignedCert creates a fresh self-signed P-256 ECDSA cert valid
// for the given host. Returns absolute paths to PEM-encoded cert + key
// files cleaned up via t.Cleanup.
func writeSelfSignedCert(t *testing.T, host string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:         true,
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}
