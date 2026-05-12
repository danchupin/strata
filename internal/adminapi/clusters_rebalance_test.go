package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/promclient"
)

// vectorResponseBody is a Prom /api/v1/query instant-vector payload.
// Helpers in this file mirror matrixResponseBody / matrixSeries shape so
// tests stay self-contained.
const vectorResponseBody = `{"status":"success","data":{"resultType":"vector","result":[%s]}}`

func vectorSample(value string) string {
	return `{"metric":{},"value":[1700000000.0,"` + value + `"]}`
}

// stubProm returns a Prom test server that dispatches /query (instant) and
// /query_range to per-query response bodies keyed by the literal PromQL
// expression. Unknown queries fail the test loudly.
func stubProm(t *testing.T, queries map[string]string, queryRanges map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expr := r.URL.Query().Get("query")
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v1/query"):
			body, ok := queries[expr]
			if !ok {
				t.Errorf("unexpected /query expr=%q", expr)
				return
			}
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/api/v1/query_range"):
			body, ok := queryRanges[expr]
			if !ok {
				t.Errorf("unexpected /query_range expr=%q", expr)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected path=%s", r.URL.Path)
		}
	}))
}

func rebalanceProgressGET(t *testing.T, s *Server, id string) (int, ClusterRebalanceProgressResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/clusters/"+id+"/rebalance-progress", nil), "operator")
	s.routes().ServeHTTP(rr, req)
	var got ClusterRebalanceProgressResponse
	if rr.Body.Len() > 0 && rr.Header().Get("Content-Type") == "application/json" {
		_ = json.NewDecoder(rr.Body).Decode(&got)
	}
	return rr.Code, got
}

func TestClusterRebalanceProgressReturnsTotalsAndSeries(t *testing.T) {
	movedExpr := fmt.Sprintf(rebalanceMovedTotalExprFmt, "alpha")
	refusedExpr := fmt.Sprintf(rebalanceRefusedTotalExprFmt, "alpha")
	rateExpr := fmt.Sprintf(rebalanceMovedRateExprFmt, "alpha")
	prom := stubProm(t,
		map[string]string{
			movedExpr:   fmt.Sprintf(vectorResponseBody, vectorSample("42")),
			refusedExpr: fmt.Sprintf(vectorResponseBody, vectorSample("3")),
		},
		map[string]string{
			rateExpr: fmt.Sprintf(matrixResponseBody, matrixSeries("alpha",
				[][2]string{{"1700000000.0", "0.5"}, {"1700000060.0", "1.5"}})),
		},
	)
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)
	s.KnownClusters = map[string]struct{}{"alpha": {}}

	code, got := rebalanceProgressGET(t, s, "alpha")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if !got.MetricsAvailable {
		t.Fatalf("metrics_available=false")
	}
	if got.MovedTotal != 42 {
		t.Errorf("moved_total=%v want 42", got.MovedTotal)
	}
	if got.RefusedTotal != 3 {
		t.Errorf("refused_total=%v want 3", got.RefusedTotal)
	}
	if len(got.Series) != 2 {
		t.Fatalf("series len=%d want 2", len(got.Series))
	}
	if got.Series[0][0] != 1_700_000_000_000 {
		t.Errorf("series[0].ts=%.0f want epoch-ms", got.Series[0][0])
	}
	if got.Series[1][1] != 1.5 {
		t.Errorf("series[1].value=%v want 1.5", got.Series[1][1])
	}
}

func TestClusterRebalanceProgressDegradesWhenPromUnavailable(t *testing.T) {
	s := newTestServer() // Prom == nil-equivalent

	code, got := rebalanceProgressGET(t, s, "alpha")
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	if got.MetricsAvailable {
		t.Errorf("metrics_available=true want false")
	}
	if got.Series == nil {
		t.Errorf("series must be non-nil empty array")
	}
}

func TestClusterRebalanceProgressDegradesWhenPromErrors(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)

	code, got := rebalanceProgressGET(t, s, "alpha")
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	if got.MetricsAvailable {
		t.Errorf("metrics_available=true want false on upstream err")
	}
}

func TestClusterRebalanceProgressRejectsUnknownCluster(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"alpha": {}}
	code, _ := rebalanceProgressGET(t, s, "ghost")
	if code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", code)
	}
}
