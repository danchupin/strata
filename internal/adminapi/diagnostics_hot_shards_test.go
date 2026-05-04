package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/danchupin/strata/internal/promclient"
)

func shardSeries(shard string, points [][2]string) string {
	var b strings.Builder
	b.WriteString(`{"metric":{"shard":"` + shard + `"},"values":[`)
	for i, p := range points {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("[" + p[0] + `,"` + p[1] + `"]`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestDiagnosticsHotShardsReturnsMatrix(t *testing.T) {
	var calls atomic.Int32
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("path=%q", r.URL.Path)
		}
		expr := r.URL.Query().Get("query")
		want := fmt.Sprintf(hotShardsExprFmt, "alpha")
		if expr != want {
			t.Errorf("query=%q want %q", expr, want)
		}
		body := fmt.Sprintf(matrixResponseBody, strings.Join([]string{
			shardSeries("3", [][2]string{{"1700000000.0", "1"}, {"1700000060.0", "2"}}),
			shardSeries("17", [][2]string{{"1700000000.0", "10"}, {"1700000060.0", "20"}}),
		}, ","))
		_, _ = w.Write([]byte(body))
	}))
	defer prom.Close()

	s := newTestServer()
	s.DataBackend = "rados"
	s.Prom = promclient.New(prom.URL)

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/diagnostics/hot-shards/alpha?range=1h&step=1m", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got HotShardsResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Empty {
		t.Fatalf("rados backend should not be empty")
	}
	if len(got.Matrix) != 2 {
		t.Fatalf("matrix len=%d want 2", len(got.Matrix))
	}
	if got.Matrix[0].Shard != "17" {
		t.Errorf("first=%q want 17", got.Matrix[0].Shard)
	}
	if got.Matrix[1].Shard != "3" {
		t.Errorf("second=%q want 3", got.Matrix[1].Shard)
	}
}

func TestDiagnosticsHotShardsS3BackendEmpty(t *testing.T) {
	var promCalls atomic.Int32
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		promCalls.Add(1)
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer prom.Close()

	s := newTestServer()
	s.DataBackend = "s3"
	s.Prom = promclient.New(prom.URL) // wired but must be skipped

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/diagnostics/hot-shards/anybucket", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got HotShardsResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Empty {
		t.Fatalf("s3 backend must short-circuit with empty=true")
	}
	if !strings.Contains(got.Reason, "s3-over-s3") {
		t.Errorf("reason=%q must explain s3-over-s3", got.Reason)
	}
	if got.Matrix != nil {
		t.Errorf("matrix must be nil on empty=true; got %+v", got.Matrix)
	}
	if promCalls.Load() != 0 {
		t.Errorf("prom called %d times; s3 path must skip Prom roundtrip", promCalls.Load())
	}
}

func TestDiagnosticsHotShardsPromUnavailable(t *testing.T) {
	s := newTestServer()
	s.DataBackend = "rados"
	// s.Prom remains nil-equivalent: BaseURL empty
	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/diagnostics/hot-shards/alpha", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rr.Code)
	}
	var er errorResponse
	if err := json.NewDecoder(rr.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.Code != "MetricsUnavailable" {
		t.Errorf("code=%q want MetricsUnavailable", er.Code)
	}
}

func TestDiagnosticsHotShardsRejectsBadRange(t *testing.T) {
	s := newTestServer()
	s.DataBackend = "rados"
	cases := []string{
		"range=oops",
		"range=-1h",
		"range=0",
		"step=garbage",
		"step=-1m",
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
				"/admin/v1/diagnostics/hot-shards/alpha?"+q, nil), "operator")
			s.routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestDiagnosticsHotShardsCachesPerBucket(t *testing.T) {
	var calls atomic.Int32
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		body := fmt.Sprintf(matrixResponseBody, shardSeries("0",
			[][2]string{{"1700000000.0", "1"}}))
		_, _ = w.Write([]byte(body))
	}))
	defer prom.Close()

	s := newTestServer()
	s.DataBackend = "rados"
	s.Prom = promclient.New(prom.URL)

	hit := func(bucket string) {
		t.Helper()
		rr := httptest.NewRecorder()
		req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
			"/admin/v1/diagnostics/hot-shards/"+bucket+"?range=1h&step=1m", nil), "operator")
		s.routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
	hit("alpha")
	hit("alpha")
	if got := calls.Load(); got != 1 {
		t.Errorf("alpha cache: prom calls=%d want 1", got)
	}
	hit("beta")
	if got := calls.Load(); got != 2 {
		t.Errorf("distinct bucket: prom calls=%d want 2", got)
	}
}
