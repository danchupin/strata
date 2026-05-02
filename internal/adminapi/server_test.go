package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/heartbeat"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

// newTestServer returns a Server pointed at the in-memory meta backend with
// no static credentials. Tests that need to bypass SigV4 validation hit
// s.routes() directly; the auth-wrapped Handler() is reserved for the
// 401-on-anonymous test.
func newTestServer() *Server {
	creds := auth.NewStaticStore(map[string]*auth.Credential{})
	s := New(Config{
		Meta:        metamem.New(),
		Creds:       creds,
		Heartbeat:   heartbeat.NewMemoryStore(),
		Version:     "test-sha",
		ClusterName: "test-cluster",
		Region:      "test-region",
		MetaBackend: "memory",
		DataBackend: "memory",
		JWTSecret:   []byte("0123456789abcdef0123456789abcdef"),
	})
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
	// Seed one live heartbeat so status derives "healthy" + node_count=1.
	if err := s.Heartbeat.WriteHeartbeat(context.Background(), heartbeat.Node{
		ID: "node-a", Address: "127.0.0.1:9000", Version: "test-sha",
		StartedAt: time.Unix(1_700_000_000, 0), LastHeartbeat: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}
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
	if got.Status != "healthy" {
		t.Errorf("status field: %q want healthy", got.Status)
	}
	if got.Version != "test-sha" {
		t.Errorf("version field: %q", got.Version)
	}
	if got.StartedAt != 1_700_000_000 {
		t.Errorf("started_at: got %d", got.StartedAt)
	}
	if got.ClusterName != "test-cluster" {
		t.Errorf("cluster_name: got %q want %q", got.ClusterName, "test-cluster")
	}
	if got.NodeCount != 1 || got.NodeCountHealthy != 1 {
		t.Errorf("node_count=%d/%d want 1/1", got.NodeCountHealthy, got.NodeCount)
	}
	if got.MetaBackend != "memory" || got.DataBackend != "memory" {
		t.Errorf("backends: meta=%q data=%q", got.MetaBackend, got.DataBackend)
	}
	if got.UptimeSec < 0 {
		t.Errorf("uptime_sec negative: %d", got.UptimeSec)
	}
}

func TestClusterStatusEmptyHeartbeatsUnhealthy(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/cluster/status", nil)
	s.routes().ServeHTTP(rr, req)
	var got ClusterStatus
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Status != "unhealthy" {
		t.Errorf("empty heartbeat status: %q want unhealthy", got.Status)
	}
	if got.NodeCount != 0 {
		t.Errorf("node_count: %d want 0", got.NodeCount)
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

func TestClusterNodesReturnsHeartbeats(t *testing.T) {
	s := newTestServer()
	now := time.Now().UTC()
	if err := s.Heartbeat.WriteHeartbeat(context.Background(), heartbeat.Node{
		ID: "node-1", Address: "10.0.0.1:9000", Version: "v1",
		StartedAt: now.Add(-2 * time.Hour), LastHeartbeat: now,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/cluster/nodes", nil)
	s.routes().ServeHTTP(rr, req)

	var got ClusterNodesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].ID != "node-1" {
		t.Fatalf("nodes=%+v", got.Nodes)
	}
	n := got.Nodes[0]
	if n.Address != "10.0.0.1:9000" {
		t.Errorf("address=%q", n.Address)
	}
	if n.Status != "healthy" {
		t.Errorf("status=%q want healthy", n.Status)
	}
	if n.UptimeSec < 7000 || n.UptimeSec > 8000 {
		t.Errorf("uptime_sec=%d out of expected ~7200 window", n.UptimeSec)
	}
	if n.Workers == nil || n.LeaderFor == nil {
		t.Error("workers/leader_for must be empty arrays not null")
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
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics/timeseries?metric=request_rate&range=15m", nil)
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
	if got.MetricsAvailable {
		t.Error("metrics_available: want false (no Prom configured)")
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
		{"/admin/v1/metrics/timeseries?metric=request_rate&range=15m", http.StatusOK},
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
