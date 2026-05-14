package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danchupin/strata/internal/data/placement"
	"github.com/danchupin/strata/internal/meta"
)

func drainBody(mode string) drainRequestBody {
	return drainRequestBody{Mode: mode}
}

func TestClustersList_DefaultsToLive(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}, "c2": {}}
	s.ClusterBackends = map[string]string{"c1": "rados", "c2": "rados"}
	// Evacuate c2 only.
	if err := s.Meta.SetClusterState(context.Background(), "c2", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
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
	if by["c1"].State != meta.ClusterStateLive || by["c1"].Mode != "" || by["c1"].Backend != "rados" {
		t.Fatalf("c1: %+v", by["c1"])
	}
	if by["c2"].State != meta.ClusterStateEvacuating || by["c2"].Mode != meta.ClusterModeEvacuate {
		t.Fatalf("c2: got=%+v want=(evacuating,evacuate)", by["c2"])
	}
}

// TestClustersList_NoDrainStrictField pins the US-007 removal: the
// drain_strict bool was retired from the GET /admin/v1/clusters wire
// shape (drain is now unconditionally strict).
func TestClustersList_NoDrainStrictField(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.ClusterBackends = map[string]string{"c1": "rados"}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["drain_strict"]; ok {
		t.Fatalf("drain_strict must be absent from clusters response: %s", rr.Body.String())
	}
}

func TestClusterDrain_FlipsStateEvacuate(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.ClusterBackends = map[string]string{"c1": "rados"}
	loader := func(ctx context.Context) (map[string]meta.ClusterStateRow, error) {
		return s.Meta.ListClusterStates(ctx)
	}
	s.DrainCache = placement.NewDrainCache(loader, 0)
	_ = s.DrainCache.Get(context.Background())

	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c1/drain", drainBody(meta.ClusterModeEvacuate))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("drain status=%d body=%s", rr.Code, rr.Body.String())
	}
	row, ok, err := s.Meta.GetClusterState(context.Background(), "c1")
	if err != nil || !ok || row.State != meta.ClusterStateEvacuating || row.Mode != meta.ClusterModeEvacuate {
		t.Fatalf("post-drain row: got=(%+v,%v,%v)", row, ok, err)
	}
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

func TestClusterDrain_FlipsStateReadonly(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c1/drain", drainBody(meta.ClusterModeReadonly))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("drain readonly status=%d body=%s", rr.Code, rr.Body.String())
	}
	row, ok, err := s.Meta.GetClusterState(context.Background(), "c1")
	if err != nil || !ok || row.State != meta.ClusterStateDrainingReadonly || row.Mode != meta.ClusterModeReadonly {
		t.Fatalf("post-drain row: got=(%+v,%v,%v)", row, ok, err)
	}
}

func TestClusterDrain_UpgradeReadonlyToEvacuate(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	// Start in draining_readonly.
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateDrainingReadonly, meta.ClusterModeReadonly, 0); err != nil {
		t.Fatalf("seed readonly: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c1/drain", drainBody(meta.ClusterModeEvacuate))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("upgrade status=%d body=%s", rr.Code, rr.Body.String())
	}
	row, ok, _ := s.Meta.GetClusterState(context.Background(), "c1")
	if !ok || row.State != meta.ClusterStateEvacuating || row.Mode != meta.ClusterModeEvacuate {
		t.Fatalf("post-upgrade row: %+v", row)
	}
}

func TestClusterDrain_BodyRequired(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c1/drain", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 BadRequest: got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestClusterDrain_BadMode(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c1/drain", drainBody("drain-it-all"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 BadRequest for bad mode: got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestClusterDrain_InvalidTransitionReturns409(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	// Start in evacuating, attempt readonly (refused — undrain is the
	// only way back to live).
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
		t.Fatalf("seed evacuating: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c1/drain", drainBody(meta.ClusterModeReadonly))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body invalidTransitionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 409: %v", err)
	}
	if body.Code != "InvalidTransition" || body.CurrentState != meta.ClusterStateEvacuating || body.RequestedMode != meta.ClusterModeReadonly {
		t.Errorf("bad body: %+v", body)
	}
}

func TestClusterUndrain_FromReadonly(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateDrainingReadonly, meta.ClusterModeReadonly, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c1/undrain", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("undrain status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, ok, _ := s.Meta.GetClusterState(context.Background(), "c1"); ok {
		t.Fatalf("undrain should drop row")
	}
}

func TestClusterUndrain_FromLiveRefused(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c1/undrain", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict undraining live cluster: got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestClusterDrain_UnknownClusterRejected(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/c9/drain", drainBody(meta.ClusterModeReadonly))
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
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/clusters/any-id/drain", drainBody(meta.ClusterModeReadonly))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}
