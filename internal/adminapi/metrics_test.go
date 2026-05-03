package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/promclient"
)

func TestMetricsTimeseriesRequiresMetric(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries?range=1h", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rr.Code)
	}
}

func TestMetricsTimeseriesUnknownMetric(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries?metric=cpu&range=1h", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "request_rate") {
		t.Errorf("error body should list supported metrics: %s", rr.Body.String())
	}
}

func TestMetricsTimeseriesRequiresRange(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries?metric=request_rate", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rr.Code)
	}
}

func TestMetricsTimeseriesBadRange(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries?metric=request_rate&range=banana", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rr.Code)
	}
}

func TestMetricsTimeseriesBadStep(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries?metric=request_rate&range=1h&step=banana", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rr.Code)
	}
}

func TestMetricsTimeseriesWithoutPromMetricsUnavailable(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries?metric=request_rate&range=1h", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rr.Code)
	}
	var got MetricsTimeseriesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MetricsAvailable {
		t.Error("metrics_available: want false")
	}
	if got.Series == nil {
		t.Error("series must be empty array, not nil")
	}
}

// TestMetricsTimeseriesRequestRate stands up a stub Prometheus and asserts the
// handler issues a query_range for the rate counter and returns the matrix
// result as a single MetricSeries with [epoch_ms, value] points sorted by
// timestamp.
func TestMetricsTimeseriesRequestRate(t *testing.T) {
	var capturedExpr, capturedStep string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/v1/query_range") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		capturedExpr = r.URL.Query().Get("query")
		capturedStep = r.URL.Query().Get("step")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{},"values":[[1700000000.0,"1.5"],[1700000060.0,"2.5"]]}
		]}}`))
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries?metric=request_rate&range=1h", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(capturedExpr, "rate(strata_http_requests_total[") {
		t.Errorf("expr=%q must use rate(strata_http_requests_total[...])", capturedExpr)
	}
	if capturedStep == "" {
		t.Error("step query param not set")
	}
	var got MetricsTimeseriesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.MetricsAvailable {
		t.Error("metrics_available: want true")
	}
	if len(got.Series) != 1 || got.Series[0].Name != "request_rate" {
		t.Fatalf("series=%+v want [{request_rate}]", got.Series)
	}
	if len(got.Series[0].Points) != 2 {
		t.Fatalf("points=%v want 2", got.Series[0].Points)
	}
	if got.Series[0].Points[0][0] != 1_700_000_000_000 {
		t.Errorf("first ts = %.0f want 1700000000000 (epoch-ms)", got.Series[0].Points[0][0])
	}
	if got.Series[0].Points[1][1] != 2.5 {
		t.Errorf("second value = %v want 2.5", got.Series[0].Points[1][1])
	}
}

func TestMetricsTimeseriesLatencyP95UsesHistogramQuantile(t *testing.T) {
	var capturedExpr string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedExpr = r.URL.Query().Get("query")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{},"values":[[1700000000.0,"0.123"]]}
		]}}`))
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries?metric=latency_p95&range=15m", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if !strings.Contains(capturedExpr, "histogram_quantile(0.95") {
		t.Errorf("expr=%q must contain histogram_quantile(0.95...)", capturedExpr)
	}
	if !strings.Contains(capturedExpr, "strata_http_request_duration_seconds_bucket") {
		t.Errorf("expr=%q must reference _bucket", capturedExpr)
	}
	var got MetricsTimeseriesResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Series[0].Name != "p95" {
		t.Errorf("series.name=%q want p95", got.Series[0].Name)
	}
}

func TestMetricsTimeseriesErrorRateExpr(t *testing.T) {
	var capturedExpr string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedExpr = r.URL.Query().Get("query")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries?metric=error_rate&range=1h", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if !strings.Contains(capturedExpr, `code=~"5..`) {
		t.Errorf("expr=%q must filter on 5xx codes", capturedExpr)
	}
}

func TestMetricsTimeseriesPromUpstreamErrorDegradesGracefully(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries?metric=request_rate&range=1h", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (graceful)", rr.Code)
	}
	var got MetricsTimeseriesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MetricsAvailable {
		t.Error("metrics_available: want false on upstream 500")
	}
}

func TestMetricsTimeseriesRangeAndStepPropagate(t *testing.T) {
	var capturedStart, capturedEnd, capturedStep string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedStart = r.URL.Query().Get("start")
		capturedEnd = r.URL.Query().Get("end")
		capturedStep = r.URL.Query().Get("step")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)

	// 7d range with explicit step=1h.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries?metric=request_rate&range=7d&step=1h", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if capturedStart == "" || capturedEnd == "" {
		t.Errorf("start=%q end=%q must be passed", capturedStart, capturedEnd)
	}
	// step should be 3600 seconds for "1h".
	if capturedStep != "3600" {
		t.Errorf("step=%q want 3600", capturedStep)
	}
}

func TestMetricsTimeseries24hDefaultStepIs15m(t *testing.T) {
	var capturedStep string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedStep = r.URL.Query().Get("step")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries?metric=request_rate&range=24h", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	// 15min = 900s.
	if capturedStep != "900" {
		t.Errorf("default step for 24h: got %q want 900", capturedStep)
	}
}

func TestParseDurationParamSupports7d(t *testing.T) {
	d, err := parseDurationParam("7d")
	if err != nil {
		t.Fatalf("parse 7d: %v", err)
	}
	if d != 7*24*time.Hour {
		t.Errorf("7d=%v want 168h", d)
	}
}

// Sanity that url.QueryEscape on the expression still round-trips to the
// upstream — guards against accidental "+" vs " " confusion.
func TestPromQueryRoundtripsEscaping(t *testing.T) {
	got := url.Values{}
	got.Set("query", `sum(rate(strata_http_requests_total{code=~"5.."}[1m]))`)
	enc := got.Encode()
	parsed, err := url.ParseQuery(enc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Get("query") != got.Get("query") {
		t.Errorf("roundtrip mismatch: %q vs %q", parsed.Get("query"), got.Get("query"))
	}
}
