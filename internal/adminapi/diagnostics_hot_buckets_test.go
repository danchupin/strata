package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/promclient"
)

// matrixResponseBody is a `/api/v1/query_range` matrix payload. The fake
// Prom server below substitutes one entry per (bucket, value) pair.
const matrixResponseBody = `{"status":"success","data":{"resultType":"matrix","result":[%s]}}`

func matrixSeries(bucket string, points [][2]string) string {
	var b strings.Builder
	b.WriteString(`{"metric":{"bucket":"` + bucket + `"},"values":[`)
	for i, p := range points {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("[" + p[0] + `,"` + p[1] + `"]`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestDiagnosticsHotBucketsReturnsMatrix(t *testing.T) {
	var calls atomic.Int32
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("path=%q", r.URL.Path)
		}
		expr := r.URL.Query().Get("query")
		if expr != hotBucketsExpr {
			t.Errorf("query=%q", expr)
		}
		body := fmt.Sprintf(matrixResponseBody, strings.Join([]string{
			matrixSeries("alpha", [][2]string{{"1700000000.0", "1"}, {"1700000060.0", "2"}}),
			matrixSeries("beta", [][2]string{{"1700000000.0", "10"}, {"1700000060.0", "20"}}),
		}, ","))
		_, _ = w.Write([]byte(body))
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/diagnostics/hot-buckets?range=1h&step=1m", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got HotBucketsResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Matrix) != 2 {
		t.Fatalf("matrix len=%d want 2", len(got.Matrix))
	}
	// Ranked DESC by total: beta (10+20=30) > alpha (1+2=3)
	if got.Matrix[0].Bucket != "beta" {
		t.Errorf("first=%q want beta", got.Matrix[0].Bucket)
	}
	if got.Matrix[1].Bucket != "alpha" {
		t.Errorf("second=%q want alpha", got.Matrix[1].Bucket)
	}
	if len(got.Matrix[0].Values) != 2 || got.Matrix[0].Values[1].Value != 20 {
		t.Errorf("beta values=%+v", got.Matrix[0].Values)
	}
}

func TestDiagnosticsHotBucketsCachesPerRangeStep(t *testing.T) {
	var calls atomic.Int32
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body := fmt.Sprintf(matrixResponseBody, matrixSeries("alpha",
			[][2]string{{"1700000000.0", "1"}, {"1700000060.0", "2"}}))
		_, _ = w.Write([]byte(body))
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)

	url1 := "/admin/v1/diagnostics/hot-buckets?range=1h&step=1m"
	url2 := "/admin/v1/diagnostics/hot-buckets?range=15m&step=1m"

	for i := range 2 {
		rr := httptest.NewRecorder()
		req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, url1, nil), "operator")
		s.routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("call %d: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("range=1h: prom calls=%d want 1 (cache miss + cache hit)", got)
	}

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, url2, nil), "operator")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("after distinct (range,step): prom calls=%d want 2", got)
	}
}

func TestDiagnosticsHotBucketsCacheRespectsTTL(t *testing.T) {
	var calls atomic.Int32
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body := fmt.Sprintf(matrixResponseBody, matrixSeries("alpha",
			[][2]string{{"1700000000.0", "1"}}))
		_, _ = w.Write([]byte(body))
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)
	// Pin the cache clock so the second call observes an expired entry
	// without sleeping.
	cache := s.hotBuckets()
	advance := int64(0)
	cache.now = func() time.Time { return time.Unix(1_700_000_000+advance, 0) }

	hit := func() {
		t.Helper()
		rr := httptest.NewRecorder()
		req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
			"/admin/v1/diagnostics/hot-buckets?range=1h&step=1m", nil), "operator")
		s.routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
	hit()
	hit()
	if got := calls.Load(); got != 1 {
		t.Fatalf("within TTL: prom calls=%d want 1", got)
	}
	advance = 31 // jump past the 30s TTL
	hit()
	if got := calls.Load(); got != 2 {
		t.Fatalf("after TTL: prom calls=%d want 2", got)
	}
}

func TestDiagnosticsHotBucketsRejectsBadRange(t *testing.T) {
	s := newTestServer()
	cases := []string{
		"range=oops",
		"range=-1h",
		"range=0",
		"step=oops",
		"step=-1m",
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
				"/admin/v1/diagnostics/hot-buckets?"+q, nil), "operator")
			s.routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestDiagnosticsHotBucketsPromUnavailable(t *testing.T) {
	s := newTestServer() // s.Prom is nil-equivalent: BaseURL=""
	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/diagnostics/hot-buckets", nil), "operator")
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

func TestDiagnosticsHotBucketsTrimsToTopN(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		for i := range hotBucketsTopN + 5 {
			if i > 0 {
				b.WriteString(",")
			}
			// Higher-index buckets ranked higher (larger values) so the
			// top-N slice consistently retains them and drops the tail.
			b.WriteString(matrixSeries(
				fmt.Sprintf("bkt%03d", i),
				[][2]string{{"1700000000.0", fmt.Sprintf("%d", i+1)}},
			))
		}
		body := fmt.Sprintf(matrixResponseBody, b.String())
		_, _ = w.Write([]byte(body))
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/diagnostics/hot-buckets", nil), "operator")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got HotBucketsResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Matrix) != hotBucketsTopN {
		t.Errorf("matrix len=%d want %d", len(got.Matrix), hotBucketsTopN)
	}
}

