package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/heartbeat"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/metrics"
)

func newConfigEndpointsServer(t *testing.T, hb heartbeat.Store, gc GCConfig, reb RebalanceConfig) *Server {
	t.Helper()
	creds := auth.NewStaticStore(map[string]*auth.Credential{})
	s := New(Config{
		Meta:              metamem.New(),
		Data:              datamem.New(),
		Creds:             creds,
		Heartbeat:         hb,
		Version:           "test-sha",
		ClusterName:       "test-cluster",
		Region:            "test-region",
		MetaBackend:       "memory",
		DataBackend:       "memory",
		JWTSecret:         []byte("0123456789abcdef0123456789abcdef"),
		HeartbeatInterval: 10 * time.Second,
		GCConfig:          gc,
		RebalanceConfig:   reb,
	})
	s.Started = time.Unix(1_700_000_000, 0)
	return s
}

func TestHandleGetGCConfigReturnsInjectedSnapshot(t *testing.T) {
	s := newConfigEndpointsServer(t, heartbeat.NewMemoryStore(),
		GCConfig{GraceSeconds: 300, IntervalSeconds: 30, BatchSize: 100, Concurrency: 2, Shards: 4},
		RebalanceConfig{},
	)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/gc-config", nil)
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got GCConfig
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := GCConfig{GraceSeconds: 300, IntervalSeconds: 30, BatchSize: 100, Concurrency: 2, Shards: 4}
	if got != want {
		t.Errorf("payload: %+v want %+v", got, want)
	}
}

func TestHandleGetGCConfigZeroValuesPass(t *testing.T) {
	s := newConfigEndpointsServer(t, heartbeat.NewMemoryStore(), GCConfig{}, RebalanceConfig{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/gc-config", nil)
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var got GCConfig
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if (got != GCConfig{}) {
		t.Errorf("zero payload: %+v", got)
	}
}

func TestHandleGetRebalanceConfigReturnsInjectedSnapshot(t *testing.T) {
	hb := heartbeat.NewMemoryStore()
	now := time.Now().UTC()
	for _, id := range []string{"strata-a", "strata-b"} {
		if err := hb.WriteHeartbeat(context.Background(), heartbeat.Node{
			ID: id, Address: "127.0.0.1", Version: "test-sha",
			StartedAt: now.Add(-time.Minute), LastHeartbeat: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	s := newConfigEndpointsServer(t, hb,
		GCConfig{},
		RebalanceConfig{IntervalSeconds: 30, RateMBPerSec: 100, Inflight: 4, Shards: 1},
	)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/rebalance-config", nil)
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got RebalanceConfig
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := RebalanceConfig{IntervalSeconds: 30, RateMBPerSec: 100, Inflight: 4, Shards: 1, ReplicasCount: 2}
	if got != want {
		t.Errorf("payload: %+v want %+v", got, want)
	}
}

func TestHandleGetRebalanceConfigFiltersStaleHeartbeats(t *testing.T) {
	hb := heartbeat.NewMemoryStore()
	now := time.Now().UTC()
	// HeartbeatInterval default in test = 10s → ttl = 20s. Stale row must NOT
	// count.
	if err := hb.WriteHeartbeat(context.Background(), heartbeat.Node{
		ID: "strata-a", Version: "test", StartedAt: now, LastHeartbeat: now,
	}); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if err := hb.WriteHeartbeat(context.Background(), heartbeat.Node{
		ID: "strata-stale", Version: "test", StartedAt: now,
		LastHeartbeat: now.Add(-30 * time.Second),
	}); err != nil {
		t.Fatalf("stale: %v", err)
	}
	s := newConfigEndpointsServer(t, hb, GCConfig{}, RebalanceConfig{
		IntervalSeconds: 30, RateMBPerSec: 1, Inflight: 1, Shards: 1,
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/rebalance-config", nil)
	s.routes().ServeHTTP(rr, req)
	var got RebalanceConfig
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ReplicasCount != 1 {
		t.Errorf("replicas_count: %d want 1 (stale row must be excluded)", got.ReplicasCount)
	}
}

// errHeartbeatStore returns a hard error from ListNodes so the handler
// hits the error path. WriteHeartbeat is a no-op (not exercised here).
type errHeartbeatStore struct{}

func (errHeartbeatStore) WriteHeartbeat(context.Context, heartbeat.Node) error { return nil }
func (errHeartbeatStore) ListNodes(context.Context) ([]heartbeat.Node, error) {
	return nil, errors.New("list nodes boom")
}

func TestHandleGetRebalanceConfigCountsErrors(t *testing.T) {
	s := newConfigEndpointsServer(t, errHeartbeatStore{}, GCConfig{}, RebalanceConfig{
		IntervalSeconds: 30, RateMBPerSec: 1, Inflight: 1, Shards: 1,
	})
	before := readConfigErrorCounter(t, "rebalance-config")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/rebalance-config", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got RebalanceConfig
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ReplicasCount != 0 {
		t.Errorf("replicas_count on error: %d want 0", got.ReplicasCount)
	}
	after := readConfigErrorCounter(t, "rebalance-config")
	if after-before != 1 {
		t.Errorf("error counter: delta=%v want 1", after-before)
	}
}

func readConfigErrorCounter(t *testing.T, endpoint string) float64 {
	t.Helper()
	c := metrics.AdminConfigEndpointErrorsTotal.WithLabelValues(endpoint)
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("collect counter: %v", err)
	}
	if m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}
