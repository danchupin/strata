//go:build integration

package cassandra_test

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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tccassandra "github.com/testcontainers/testcontainers-go/modules/cassandra"

	"github.com/danchupin/strata/internal/meta/cassandra"
)

// TestCassandraTLSHandshake exercises US-004's mTLS wiring against a real
// TLS-enabled Cassandra container.
//
// The testcontainers cassandra module ships an in-built WithTLS() option that
// auto-generates a server CA + server cert into a PKCS12 keystore and mounts
// a tuned cassandra-ssl.yaml — but it does NOT expose the CA in PEM form
// (TLSConfig.RootCAs is an opaque CertPool). To feed our file-based
// SessionConfig.TLS shape we probe the SSL port with InsecureSkipVerify=true,
// capture the peer cert chain, and write the root CA back to disk as PEM.
//
// The test asserts:
//   - Probe succeeds when STRATA_CASSANDRA_TLS_CA_FILE points at the valid CA.
//   - Probe fails when STRATA_CASSANDRA_TLS_CA_FILE points at a *different*
//     CA (handshake rejection — not a CQL-level error).
//   - SkipVerify=true overrides CA verification and lets the session attach
//     against the same server (operator escape hatch; gauge-bumped in
//     serverapp).
//
// Server-side require_client_auth is left disabled (the upstream module
// doesn't toggle it); the gateway-side mTLS path is unit-tested via
// TestNewClusterMutualTLS in tls_test.go.
func TestCassandraTLSHandshake(t *testing.T) {
	if os.Getenv("STRATA_SCYLLA_TEST") == "1" {
		t.Skip("STRATA_SCYLLA_TEST=1: ScyllaDB suite runs separately")
	}

	ctx := context.Background()

	// Keeps the container inline (TLSConfig is read off it below) but inherits
	// the same CI-safe JVM tuning as startCassandra (US-010).
	tlsOpts := append([]testcontainers.ContainerCustomizer{tccassandra.WithTLS()}, cassandraTuningOpts()...)
	container, err := tccassandra.Run(ctx, cassandraImage(), tlsOpts...)
	if err != nil {
		t.Fatalf("start cassandra: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate: %v", err)
		}
	})

	host, err := container.ConnectionHost(ctx)
	if err != nil {
		t.Fatalf("connection host: %v", err)
	}

	// Probe to extract the CA cert the container generated. The peer cert
	// chain ends with the self-signed CA (PKCS12 keystore was built with the
	// CA in the chain — see testcontainers tls.go createTLSCerts).
	caPEM := extractServerCA(t, host)

	dir := t.TempDir()
	validCAPath := filepath.Join(dir, "valid-ca.pem")
	if err := os.WriteFile(validCAPath, caPEM, 0o600); err != nil {
		t.Fatalf("write valid CA: %v", err)
	}

	t.Run("valid_ca_succeeds", func(t *testing.T) {
		store := openCassandraWithRetry(t, cassandra.SessionConfig{
			Hosts:       []string{host},
			Keyspace:    "strata_tls_valid",
			LocalDC:     "datacenter1",
			Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
			Timeout:     60 * time.Second,
			TLS:         cassandra.TLSConfig{CAFile: validCAPath},
		}, cassandra.Options{DefaultShardCount: 8})
		t.Cleanup(func() { _ = store.Close() })

		if err := store.Probe(ctx); err != nil {
			t.Fatalf("probe: %v", err)
		}
	})

	t.Run("wrong_ca_fails", func(t *testing.T) {
		wrongCAPath := filepath.Join(dir, "wrong-ca.pem")
		writeUnrelatedSelfSignedCA(t, wrongCAPath)

		_, err := cassandra.Open(cassandra.SessionConfig{
			Hosts:       []string{host},
			Keyspace:    "strata_tls_wrong",
			LocalDC:     "datacenter1",
			Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
			Timeout:     10 * time.Second,
			TLS:         cassandra.TLSConfig{CAFile: wrongCAPath},
		}, cassandra.Options{DefaultShardCount: 8})
		if err == nil {
			t.Fatal("Open with wrong CA: want TLS handshake failure")
		}
	})

	t.Run("skip_verify_succeeds", func(t *testing.T) {
		store := openCassandraWithRetry(t, cassandra.SessionConfig{
			Hosts:       []string{host},
			Keyspace:    "strata_tls_skip",
			LocalDC:     "datacenter1",
			Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
			Timeout:     60 * time.Second,
			TLS:         cassandra.TLSConfig{SkipVerify: true},
		}, cassandra.Options{DefaultShardCount: 8})
		t.Cleanup(func() { _ = store.Close() })

		if err := store.Probe(ctx); err != nil {
			t.Fatalf("probe: %v", err)
		}
	})
}

// openCassandraWithRetry tolerates the TLS container's warmup race on the
// success-expecting subtests. extractServerCA already proved the SSL port is
// up, but gocql's control-connection handshake can still transiently fail with
// "no connections were made when creating the session" when it races a JVM GC
// pause on the CI-tuned heap (US-010) — observed flaking valid_ca_succeeds on
// the Cassandra gate. The condition under test is the TLS HANDSHAKE outcome,
// not container readiness, so retry ONLY a transient dial failure; a genuine
// cert/handshake rejection is non-transient and fails fast (and the wrong_ca
// subtest, which asserts failure, does not use this helper).
func openCassandraWithRetry(t *testing.T, cfg cassandra.SessionConfig, opts cassandra.Options) *cassandra.Store {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for attempt := 1; ; attempt++ {
		store, err := cassandra.Open(cfg, opts)
		if err == nil {
			return store
		}
		if !isTransientCassandraConnErr(err) || time.Now().After(deadline) {
			t.Fatalf("open (attempt %d): %v", attempt, err)
		}
		t.Logf("transient cassandra TLS connect (attempt %d), retrying: %v", attempt, err)
		time.Sleep(2 * time.Second)
	}
}

// isTransientCassandraConnErr reports whether err is a gocql session-creation
// failure caused by the host being momentarily unreachable (container warmup /
// GC pause) rather than a deterministic TLS/cert rejection.
func isTransientCassandraConnErr(err error) bool {
	s := err.Error()
	for _, m := range []string{
		"no connections were made",
		"unable to dial",
		"no hosts available",
		"i/o timeout",
		"connection refused",
		"EOF",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// extractServerCA dials the cassandra SSL port with InsecureSkipVerify=true,
// reads the peer cert chain, and returns the PEM-encoded root CA (last entry
// in the chain). Used to convert the testcontainers WithTLS-generated in-memory
// CA into the file-based shape our SessionConfig consumes.
func extractServerCA(t *testing.T, hostport string) []byte {
	t.Helper()
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		t.Fatalf("split hostport %q: %v", hostport, err)
	}
	conn, err := tls.Dial("tcp", hostport, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         host,
	})
	if err != nil {
		t.Fatalf("tls.Dial %s for CA extraction: %v", hostport, err)
	}
	defer conn.Close()
	chain := conn.ConnectionState().PeerCertificates
	if len(chain) == 0 {
		t.Fatal("peer cert chain empty")
	}
	// Pick the root: self-signed entry (Issuer == Subject) or the last entry
	// when none qualify (Cassandra ships server-leaf + CA; CA matches).
	var ca *x509.Certificate
	for _, c := range chain {
		if c.IsCA && c.Subject.String() == c.Issuer.String() {
			ca = c
			break
		}
	}
	if ca == nil {
		ca = chain[len(chain)-1]
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})
}

// writeUnrelatedSelfSignedCA generates a fresh CA cert unrelated to the
// container's chain and writes it to path. Used to assert the "wrong CA"
// negative path returns a TLS handshake error.
func writeUnrelatedSelfSignedCA(t *testing.T, path string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "strata-unrelated-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("createCert: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}
