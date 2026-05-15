package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danchupin/strata/internal/data/placement"
	"github.com/danchupin/strata/internal/meta"
)

func TestClusterActivate_PendingToLive(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	loader := func(ctx context.Context) (map[string]meta.ClusterStateRow, error) {
		return s.Meta.ListClusterStates(ctx)
	}
	s.DrainCache = placement.NewDrainCache(loader, 0)

	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStatePending, "", 0); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c1/activate", weightRequestBody{Weight: 25})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	row, _, _ := s.Meta.GetClusterState(context.Background(), "c1")
	if row.State != meta.ClusterStateLive || row.Weight != 25 {
		t.Errorf("post-activate row: %+v want=(live,25)", row)
	}
}

func TestClusterActivate_RefusesNonPending(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateLive, "", 50); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c1/activate", weightRequestBody{Weight: 10})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestClusterActivate_RejectsBadWeight(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	_ = s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStatePending, "", 0)
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c1/activate", weightRequestBody{Weight: 150})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestClusterUpdateWeight_LiveRoundTrip(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	loader := func(ctx context.Context) (map[string]meta.ClusterStateRow, error) {
		return s.Meta.ListClusterStates(ctx)
	}
	s.DrainCache = placement.NewDrainCache(loader, 0)

	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateLive, "", 10); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/clusters/c1/weight", weightRequestBody{Weight: 75})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	row, _, _ := s.Meta.GetClusterState(context.Background(), "c1")
	if row.Weight != 75 {
		t.Errorf("post-update weight: %d want 75", row.Weight)
	}
}

func TestClusterUpdateWeight_RefusesNonLive(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	_ = s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStatePending, "", 0)
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/clusters/c1/weight", weightRequestBody{Weight: 50})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestClustersList_CarriesWeight(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}, "c2": {}}
	s.ClusterBackends = map[string]string{"c1": "rados", "c2": "rados"}
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateLive, "", 60); err != nil {
		t.Fatalf("seed c1: %v", err)
	}
	if err := s.Meta.SetClusterState(context.Background(), "c2", meta.ClusterStatePending, "", 0); err != nil {
		t.Fatalf("seed c2: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var got ClustersListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	by := map[string]ClusterStateEntry{}
	for _, c := range got.Clusters {
		by[c.ID] = c
	}
	if by["c1"].Weight != 60 || by["c1"].State != meta.ClusterStateLive {
		t.Errorf("c1: %+v", by["c1"])
	}
	// Pending row reports weight=0 regardless of stored value.
	if by["c2"].Weight != 0 || by["c2"].State != meta.ClusterStatePending {
		t.Errorf("c2: %+v want=(pending,0)", by["c2"])
	}
}

func TestClusterActivate_UnknownClusterRejected(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/zzz/activate", weightRequestBody{Weight: 10})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}
