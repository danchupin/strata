package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danchupin/strata/internal/data/placement"
	"github.com/danchupin/strata/internal/meta"
)

func TestClustersList_DefaultsToLive(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}, "c2": {}}
	s.ClusterBackends = map[string]string{"c1": "rados", "c2": "rados"}
	// Drain c2 only.
	if err := s.Meta.SetClusterState(context.Background(), "c2", meta.ClusterStateDraining); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ClustersListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Clusters) != 2 {
		t.Fatalf("clusters: got %d want 2 (%v)", len(got.Clusters), got.Clusters)
	}
	by := map[string]ClusterStateEntry{}
	for _, c := range got.Clusters {
		by[c.ID] = c
	}
	if by["c1"].State != meta.ClusterStateLive || by["c1"].Backend != "rados" {
		t.Fatalf("c1: %+v", by["c1"])
	}
	if by["c2"].State != meta.ClusterStateDraining {
		t.Fatalf("c2 state: got %q want %q", by["c2"].State, meta.ClusterStateDraining)
	}
}

func TestClusterDrain_FlipsState(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.ClusterBackends = map[string]string{"c1": "rados"}
	loader := func(ctx context.Context) (map[string]string, error) {
		return s.Meta.ListClusterStates(ctx)
	}
	s.DrainCache = placement.NewDrainCache(loader, 0)

	// Warm cache with current state (empty).
	_ = s.DrainCache.Get(context.Background())

	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c1/drain", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("drain status=%d body=%s", rr.Code, rr.Body.String())
	}
	state, ok, err := s.Meta.GetClusterState(context.Background(), "c1")
	if err != nil || !ok || state != meta.ClusterStateDraining {
		t.Fatalf("post-drain state: got=(%q,%v,%v)", state, ok, err)
	}
	// Cache invalidated → next Get sees the new state.
	if got := s.DrainCache.Get(context.Background()); !got["c1"] {
		t.Fatalf("drain cache did not refresh after drain admin call: %v", got)
	}

	rr = putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c1/undrain", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("undrain status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, ok, _ := s.Meta.GetClusterState(context.Background(), "c1"); ok {
		t.Fatalf("undrain should have dropped row")
	}
	if got := s.DrainCache.Get(context.Background()); got["c1"] {
		t.Fatalf("drain cache should clear c1 after undrain: %v", got)
	}
}

func TestClusterDrain_UnknownClusterRejected(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c9/drain", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "UnknownCluster" {
		t.Errorf("code=%q want UnknownCluster", er.Code)
	}
}

func TestClusterDrain_NilKnownClustersSkipsCheck(t *testing.T) {
	// memory / dev-rig path: KnownClusters nil → handler accepts any id.
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/any-id/drain", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}
