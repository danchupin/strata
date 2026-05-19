package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danchupin/strata/internal/promclient"
)

func rebalanceBandwidthGET(t *testing.T, s *Server) (int, RebalanceBandwidthResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/rebalance-bandwidth", nil), "operator")
	s.routes().ServeHTTP(rr, req)
	var got RebalanceBandwidthResponse
	if rr.Body.Len() > 0 && rr.Header().Get("Content-Type") == "application/json" {
		_ = json.NewDecoder(rr.Body).Decode(&got)
	}
	return rr.Code, got
}

func TestRebalanceBandwidthReturnsAggregateRates(t *testing.T) {
	prom := stubProm(t,
		map[string]string{
			rebalanceBytesRate1mAllExpr:  fmt.Sprintf(vectorResponseBody, vectorSample("52428800")),
			rebalanceChunksRate1mAllExpr: fmt.Sprintf(vectorResponseBody, vectorSample("12.5")),
		},
		map[string]string{},
	)
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)

	code, got := rebalanceBandwidthGET(t, s)
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if !got.MetricsAvailable {
		t.Fatalf("metrics_available=false")
	}
	if got.BytesPerSec != 52_428_800 {
		t.Errorf("bytes_per_sec=%v want 52428800 (50 MiB/s)", got.BytesPerSec)
	}
	if got.ChunksPerSec != 12.5 {
		t.Errorf("chunks_per_sec=%v want 12.5", got.ChunksPerSec)
	}
}

func TestRebalanceBandwidthDegradesWhenPromUnavailable(t *testing.T) {
	s := newTestServer()
	code, got := rebalanceBandwidthGET(t, s)
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	if got.MetricsAvailable {
		t.Errorf("metrics_available=true want false")
	}
	if got.BytesPerSec != 0 || got.ChunksPerSec != 0 {
		t.Errorf("non-zero rates on unavailable Prom: %+v", got)
	}
}

func TestRebalanceBandwidthDegradesWhenPromErrors(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)

	code, got := rebalanceBandwidthGET(t, s)
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	if got.MetricsAvailable {
		t.Errorf("metrics_available=true on upstream err")
	}
}
