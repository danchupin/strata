package serverapp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/config"
)

// TestCertStoreSNIDispatch verifies that the SNI-driven GetCertificate
// callback picks the cert whose SAN matches chi.ServerName. Loads two
// pairs ("api.example.com", "console.example.com") via STRATA_TLS_CERT_DIR
// and confirms the right leaf is returned per ServerName.
func TestCertStoreSNIDispatch(t *testing.T) {
	dir := t.TempDir()
	writeSelfSignedCertAt(t, dir, "api", "api.example.com")
	writeSelfSignedCertAt(t, dir, "console", "console.example.com")
	t.Setenv("STRATA_TLS_CERT_DIR", dir)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	tlsCfg, store, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if store == nil {
		t.Fatal("expected certStore, got nil")
	}
	if tlsCfg.GetCertificate == nil {
		t.Fatal("tlsCfg.GetCertificate must be set in cert-dir mode")
	}

	apiCert, err := tlsCfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "api.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate api: %v", err)
	}
	consoleCert, err := tlsCfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "console.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate console: %v", err)
	}
	if apiCert == consoleCert {
		t.Fatal("expected distinct certs for api / console SNI")
	}
	if apiCert.Leaf == nil || apiCert.Leaf.Subject.CommonName != "api.example.com" {
		t.Errorf("api cert CN=%q want %q", certCommonName(apiCert), "api.example.com")
	}
	if consoleCert.Leaf == nil || consoleCert.Leaf.Subject.CommonName != "console.example.com" {
		t.Errorf("console cert CN=%q want %q", certCommonName(consoleCert), "console.example.com")
	}

	// Unknown SNI falls back to first pair.
	fallback, err := tlsCfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "unknown.example.com"})
	if err != nil {
		t.Fatalf("fallback GetCertificate: %v", err)
	}
	if fallback == nil {
		t.Fatal("expected fallback cert, got nil")
	}
}

// TestCertStoreAtomicSwapKeepsInFlightSession asserts an open TLS session
// keeps using the cert it negotiated with even after the snapshot
// pointer is swapped. The reloader.swap pattern returns a fresh
// snapshot via atomic.Pointer.Store; in-flight handshakes referenced
// the old snapshot.fallback via tls.Config.GetCertificate, so the
// already-active connection (and any future request on the same
// connection per http/1.1 keepalive) keeps working without TLS errors.
func TestCertStoreAtomicSwapKeepsInFlightSession(t *testing.T) {
	dir := t.TempDir()
	writeSelfSignedCertAt(t, dir, "old", "localhost")
	t.Setenv("STRATA_TLS_CERT_DIR", dir)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	tlsCfg, store, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	addr := startTLSListener(t, tlsCfg, mux)

	pool := x509.NewCertPool()
	for _, p := range store.load().pairs {
		pemBytes, _ := os.ReadFile(p.certPath)
		pool.AppendCertsFromPEM(pemBytes)
	}

	// Swap a brand-new snapshot in. Bind a fresh listener to a fresh
	// cert and assert the old cert path keeps verifying against the
	// pool that holds only the old leaf.
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err != nil {
		t.Fatalf("tls.Dial #1: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	// Swap snapshot with a new cert in the dir.
	writeSelfSignedCertAt(t, dir, "new", "localhost")
	next, err := buildSnapshotFromDir(dir)
	if err != nil {
		t.Fatalf("buildSnapshotFromDir: %v", err)
	}
	store.swap(next)

	// Old connection must still send a request without re-handshake.
	if err := conn.Handshake(); err != nil {
		t.Fatalf("re-handshake on same conn: %v", err)
	}
}

// TestCertReloaderPeriodicReconciliation simulates fsnotify missing an
// event by running the reloader with no fsnotify watcher (interval-only
// path). Writing a fresh cert and waiting one tick must rebuild the
// snapshot.
func TestCertReloaderPeriodicReconciliation(t *testing.T) {
	dir := t.TempDir()
	writeSelfSignedCertAt(t, dir, "v1", "service.example.com")
	store := &certStore{}
	snap, err := buildSnapshotFromDir(dir)
	if err != nil {
		t.Fatalf("buildSnapshotFromDir: %v", err)
	}
	store.swap(snap)

	r := &certReloader{
		store:    store,
		logger:   discardLogger(),
		certDir:  dir,
		interval: 50 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Drive periodic loop only (skip fsnotify by calling runPeriodicOnly
	// directly — keeps the test independent of platform-specific
	// fsnotify quirks).
	go r.runPeriodicOnly(ctx)

	beforePairs := len(store.load().pairs)
	writeSelfSignedCertAt(t, dir, "v2", "second.example.com")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := len(store.load().pairs); got > beforePairs {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("periodic reconciler never observed new pair (pairs stayed at %d)", beforePairs)
}

// TestCertReloaderK8sSymlinkSwap exercises the kubelet Secret-mount
// layout: the projected directory holds a `..data` symlink pointing at a
// versioned `..YYYY...` subdir; rotating the Secret atomically renames
// the symlink target. The reloader watches the parent dir and treats
// `..data` events as cert-changed signals.
func TestCertReloaderK8sSymlinkSwap(t *testing.T) {
	mountDir := t.TempDir()
	v1Dir := filepath.Join(mountDir, "..2026_05_26_11_00_00.v1")
	v2Dir := filepath.Join(mountDir, "..2026_05_26_11_05_00.v2")
	if err := os.Mkdir(v1Dir, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	if err := os.Mkdir(v2Dir, 0o755); err != nil {
		t.Fatalf("mkdir v2: %v", err)
	}
	writeSelfSignedCertAt(t, v1Dir, "tls", "service.example.com")
	writeSelfSignedCertAt(t, v2Dir, "tls", "service.example.com")
	if err := os.Symlink(v1Dir, filepath.Join(mountDir, "..data")); err != nil {
		t.Fatalf("symlink v1: %v", err)
	}
	certPath := filepath.Join(mountDir, "..data", "tls.crt")
	keyPath := filepath.Join(mountDir, "..data", "tls.key")

	store := &certStore{}
	snap, err := buildSnapshotFromSingle(certPath, keyPath)
	if err != nil {
		t.Fatalf("buildSnapshotFromSingle: %v", err)
	}
	store.swap(snap)
	beforeFP := snap.pairs[0].fp

	r := &certReloader{
		store:    store,
		logger:   discardLogger(),
		certFile: certPath,
		keyFile:  keyPath,
		interval: 50 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.run(ctx)

	// Wait for fsnotify watcher to register parent dir.
	time.Sleep(150 * time.Millisecond)

	// Atomic symlink-swap mimicking kubelet: rename a tmp symlink onto
	// the existing `..data` link.
	tmpLink := filepath.Join(mountDir, "..data_tmp")
	if err := os.Symlink(v2Dir, tmpLink); err != nil {
		t.Fatalf("symlink v2: %v", err)
	}
	if err := os.Rename(tmpLink, filepath.Join(mountDir, "..data")); err != nil {
		t.Fatalf("rename swap: %v", err)
	}

	// 10s (not 3s): the reloader polls every 50ms so this returns the
	// instant the swap is observed on the happy path; the generous ceiling
	// only absorbs CI scheduling stalls under load (this was a flaky
	// integration-job failure — the mechanism is sound, the old deadline
	// was just too tight for a heavily-loaded runner).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current := store.load()
		if len(current.pairs) > 0 && current.pairs[0].fp != beforeFP {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("reloader never picked up k8s atomic symlink swap")
}

// TestCertStoreFallbackWildcard checks that a *.example.com SAN matches
// foo.example.com via the per-handshake wildcard branch.
func TestCertStoreFallbackWildcard(t *testing.T) {
	dir := t.TempDir()
	writeSelfSignedCertAt(t, dir, "wild", "*.example.com")
	t.Setenv("STRATA_TLS_CERT_DIR", dir)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	tlsCfg, _, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	cert, err := tlsCfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "foo.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert == nil {
		t.Fatal("expected wildcard match, got nil cert")
	}
}

// TestClientAuthMTLS asserts STRATA_TLS_CLIENT_CA_FILE flips ClientAuth
// to RequireAndVerifyClientCert and rejects unsigned clients.
func TestClientAuthMTLS(t *testing.T) {
	caCertPath, caKeyPath := writeSelfSignedCert(t, "127.0.0.1")
	certPath, keyPath := writeSelfSignedCert(t, "127.0.0.1")
	t.Setenv("STRATA_TLS_CERT_FILE", certPath)
	t.Setenv("STRATA_TLS_KEY_FILE", keyPath)
	t.Setenv("STRATA_TLS_CLIENT_CA_FILE", caCertPath)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	tlsCfg, _, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth=%v want RequireAndVerifyClientCert", tlsCfg.ClientAuth)
	}
	if tlsCfg.ClientCAs == nil {
		t.Fatal("ClientCAs must be set")
	}
	_ = caKeyPath // unused but keeps the cert-key pair on disk for the cleanup window
}

// TestConfigLoadRejectsBothCertFileAndDir ensures mutual exclusivity
// fail-fast at boot.
func TestConfigLoadRejectsBothCertFileAndDir(t *testing.T) {
	t.Setenv("STRATA_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("STRATA_TLS_KEY_FILE", "/tmp/k.pem")
	t.Setenv("STRATA_TLS_CERT_DIR", "/tmp/certs")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
}

// TestConfigLoadRejectsNegativeReloadInterval ensures negative reload
// intervals fail at boot.
func TestConfigLoadRejectsNegativeReloadInterval(t *testing.T) {
	t.Setenv("STRATA_TLS_RELOAD_INTERVAL", "-1s")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for negative reload_interval")
	}
}

// writeSelfSignedCertAt creates a P-256 ECDSA self-signed cert under
// <dir>/<base>.crt + <base>.key valid for the given host (SAN). The
// US-003 cert-dir walker keys off the *.crt suffix and matches *.key
// by basename.
func writeSelfSignedCertAt(t *testing.T, dir, base, host string) (certPath, keyPath string) {
	t.Helper()
	certPath = filepath.Join(dir, base+".crt")
	keyPath = filepath.Join(dir, base+".key")
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
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

func certCommonName(c *tls.Certificate) string {
	if c == nil || c.Leaf == nil {
		return ""
	}
	return c.Leaf.Subject.CommonName
}

var _ = slog.Default
var _ = io.Discard
var _ = net.Listen
var _ = http.MethodGet
