package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

// newTestServer returns a Server pointed at the in-memory meta backend with
// no static credentials. Tests that need to bypass SigV4 validation hit
// s.routes() directly; the auth-wrapped Handler() is reserved for the
// 401-on-anonymous test.
func newTestServer() *Server {
	creds := auth.NewStaticStore(map[string]*auth.Credential{})
	s := New(metamem.New(), creds, "test-sha")
	s.Started = time.Unix(1_700_000_000, 0)
	return s
}

func TestHandlerAnonymousReturns401(t *testing.T) {
	s := newTestServer()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/admin/v1/cluster/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q want application/json", ct)
	}
	var body errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code == "" || body.Message == "" {
		t.Errorf("empty error body: %+v", body)
	}
}

func TestHandlerJSONContentType(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/cluster/status", nil)
	s.routes().ServeHTTP(rr, req)
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type: got %q want application/json", got)
	}
}

func TestClusterStatusShape(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/cluster/status", nil)
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got ClusterStatus
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("status field: %q", got.Status)
	}
	if got.Version != "test-sha" {
		t.Errorf("version field: %q", got.Version)
	}
	if got.StartedAt != 1_700_000_000 {
		t.Errorf("started_at: got %d", got.StartedAt)
	}
}

func TestClusterNodesShape(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/cluster/nodes", nil)
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var got ClusterNodesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Nodes == nil {
		t.Error("nodes field nil; expected empty slice")
	}
	if len(got.Nodes) != 0 {
		t.Errorf("expected empty nodes, got %d", len(got.Nodes))
	}
}

func TestBucketsListShape(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets", nil)
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var got BucketsListResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Buckets == nil {
		t.Error("buckets field nil")
	}
	if got.Total != 0 {
		t.Errorf("total: %d", got.Total)
	}
}

func TestBucketGetReturns404(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/missing", nil)
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404; body=%s", rr.Code, rr.Body.String())
	}
	var got errorResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Code != "NoSuchBucket" {
		t.Errorf("code: got %q want NoSuchBucket", got.Code)
	}
}

func TestObjectsListShape(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/anything/objects", nil)
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var got ObjectsListResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Objects == nil {
		t.Error("objects field nil")
	}
	if got.IsTruncated {
		t.Error("is_truncated: want false")
	}
}

func TestBucketsTopShape(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/top", nil)
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var got BucketsTopResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Buckets == nil {
		t.Error("buckets field nil")
	}
}

func TestConsumersTopShape(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/consumers/top", nil)
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var got ConsumersTopResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Consumers == nil {
		t.Error("consumers field nil")
	}
}

func TestMetricsTimeseriesShape(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries", nil)
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var got MetricsTimeseriesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Series == nil {
		t.Error("series field nil")
	}
}

func TestRouteCoverageMatchesPRD(t *testing.T) {
	s := newTestServer()
	cases := []struct {
		path string
		want int
	}{
		{"/admin/v1/cluster/status", http.StatusOK},
		{"/admin/v1/cluster/nodes", http.StatusOK},
		{"/admin/v1/buckets", http.StatusOK},
		{"/admin/v1/buckets/top", http.StatusOK},
		{"/admin/v1/buckets/foo", http.StatusNotFound},
		{"/admin/v1/buckets/foo/objects", http.StatusOK},
		{"/admin/v1/consumers/top", http.StatusOK},
		{"/admin/v1/metrics/timeseries", http.StatusOK},
	}
	for _, tc := range cases {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		s.routes().ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Errorf("%s: status got %d want %d", tc.path, rr.Code, tc.want)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: content-type %q", tc.path, ct)
		}
	}
}
