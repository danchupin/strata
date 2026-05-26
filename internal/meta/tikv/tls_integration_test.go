//go:build integration

package tikv

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPDClientTLSHandshakeAgainstHTTPSPDStub stands up a TLS-only httptest
// server that mimics PD's `/pd/api/v1/stores` payload, then drives our
// pdClient through the full TLS handshake using the server's self-signed
// CA. Verifies the CAFile → RootCAs path on the control plane.
//
// A full TLS-enabled PD + TiKV container pair would require custom image
// configuration (cert volume mounts + PD YAML with `security.cacert-path`
// + `security.cert-path` + `security.key-path` + matching TiKV flags) —
// outside the scope of US-005's first cut. This stub exercises the TLS
// surface we own (pdclient.go); the gRPC data-plane path is bound to
// tikv-client-go's global Security and exercised by smoke runs against
// real clusters operators stand up themselves.
func TestPDClientTLSHandshakeAgainstHTTPSPDStub(t *testing.T) {
	ctx := context.Background()
	ts := httptest.NewTLSServer(stubPDStoresHandler())
	t.Cleanup(ts.Close)

	caPath := writeServerCAToTempfile(t, ts.Certificate())
	endpoint := strings.TrimPrefix(ts.URL, "https://")

	c := newPDClientWithTLS([]string{endpoint}, TLSConfig{CAFile: caPath})
	resp, err := c.listStores(ctx)
	if err != nil {
		t.Fatalf("listStores via TLS PD stub: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("Count=%d want 1", resp.Count)
	}
}

// TestPDClientTLSSkipVerifyAgainstHTTPSPDStub exercises the operator-escape
// hatch — when a self-signed cluster ships no operator CA, SkipVerify=true
// keeps the PD control plane reachable. The gauge bump + WARN log are
// emitted at the buildMetaStore boundary (covered by unit tests).
func TestPDClientTLSSkipVerifyAgainstHTTPSPDStub(t *testing.T) {
	ctx := context.Background()
	ts := httptest.NewTLSServer(stubPDStoresHandler())
	t.Cleanup(ts.Close)

	endpoint := strings.TrimPrefix(ts.URL, "https://")
	c := newPDClientWithTLS([]string{endpoint}, TLSConfig{SkipVerify: true})
	if _, err := c.listStores(ctx); err != nil {
		t.Fatalf("listStores skip-verify: %v", err)
	}
}

// TestPDClientTLSRejectsUnrelatedCA confirms that an unrelated CA bundle
// surfaces an x509 verification error instead of silently downgrading.
func TestPDClientTLSRejectsUnrelatedCA(t *testing.T) {
	ctx := context.Background()
	ts := httptest.NewTLSServer(stubPDStoresHandler())
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	unrelatedCA, _, _ := writeTiKVTestCertPair(t, dir)
	endpoint := strings.TrimPrefix(ts.URL, "https://")

	c := newPDClientWithTLS([]string{endpoint}, TLSConfig{CAFile: unrelatedCA})
	_, err := c.listStores(ctx)
	if err == nil {
		t.Fatal("listStores with unrelated CA: want TLS error")
	}
	if !strings.Contains(err.Error(), "x509") && !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "tls") {
		t.Fatalf("err=%q must mention TLS/cert verification failure", err.Error())
	}
}

// stubPDStoresHandler returns an http.Handler that responds to
// `/pd/api/v1/stores` with a minimal Count=1 / Stores=[stub] payload so the
// pdClient consumer can complete a full handshake + decode round-trip.
func stubPDStoresHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pd/api/v1/stores" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"stores":[{"store":{"id":1,"address":"tikv-stub:20160","status_address":"tikv-stub:20180","version":"7.5.0","state_name":"Up","labels":[]},"status":{"leader_count":42,"region_count":42,"available":"1GiB","capacity":"10GiB"}}]}`))
	})
}

func writeServerCAToTempfile(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	path := filepath.Join(t.TempDir(), "pd-server-ca.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write pd-server-ca: %v", err)
	}
	return path
}
