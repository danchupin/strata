package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/rebalance"
)

func TestClusterDrainProgress_RequiresTracker(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503 (rebalance worker not running)", rr.Code)
	}
}

func TestClusterDrainProgress_LiveStateNullsNumericFields(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.RebalanceProgress = rebalance.NewProgressTracker(time.Minute)

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var got ClusterDrainProgressResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != meta.ClusterStateLive {
		t.Fatalf("state: got %q want %q", got.State, meta.ClusterStateLive)
	}
	if got.ChunksOnCluster != nil || got.BytesOnCluster != nil || got.ETASeconds != nil || got.DeregisterReady != nil {
		t.Fatalf("live state must null numeric fields, got %+v", got)
	}
}

func TestClusterDrainProgress_DrainingFlipsDeregisterReady(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	tracker := rebalance.NewProgressTracker(time.Minute)
	s.RebalanceProgress = tracker
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate); err != nil {
		t.Fatalf("seed: %v", err)
	}
	now := time.Now().UTC()

	// First scan: 5 chunks remaining.
	tracker.CommitScan([]string{"c1"}, map[string]int64{"c1": 5}, map[string]int64{"c1": 5 * 1024}, now)

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var got ClusterDrainProgressResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ChunksOnCluster == nil || *got.ChunksOnCluster != 5 {
		t.Fatalf("ChunksOnCluster: got %v want 5", got.ChunksOnCluster)
	}
	if got.BaseChunks == nil || *got.BaseChunks != 5 {
		t.Fatalf("BaseChunks: got %v want 5", got.BaseChunks)
	}
	if got.DeregisterReady == nil || *got.DeregisterReady {
		t.Fatalf("DeregisterReady: got %v want false (5 chunks remaining)", got.DeregisterReady)
	}

	// Next scan: chunks drained to zero.
	tracker.CommitScan([]string{"c1"}, map[string]int64{"c1": 0}, map[string]int64{"c1": 0}, now.Add(time.Second))
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DeregisterReady == nil || !*got.DeregisterReady {
		t.Fatalf("DeregisterReady: got %v want true", got.DeregisterReady)
	}
	if got.BaseChunks == nil || *got.BaseChunks != 5 {
		t.Fatalf("BaseChunks must persist across scans; got %v", got.BaseChunks)
	}
}

func TestClusterDrainProgress_StaleCacheWarning(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	tracker := rebalance.NewProgressTracker(time.Minute)
	s.RebalanceProgress = tracker
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Commit a scan that is older than 2 × interval.
	tracker.CommitScan([]string{"c1"}, map[string]int64{"c1": 7}, map[string]int64{"c1": 7}, time.Now().Add(-10*time.Minute))

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var got ClusterDrainProgressResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, msg := range got.Warnings {
		if msg == "progress data stale" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'progress data stale' warning, got %+v", got.Warnings)
	}
	if got.ChunksOnCluster == nil || *got.ChunksOnCluster != 7 {
		t.Fatalf("stale warning must NOT null counts: got %v", got.ChunksOnCluster)
	}
}

func TestClusterDrainProgress_PendingScanCarriesWarning(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.RebalanceProgress = rebalance.NewProgressTracker(time.Minute)
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var got ClusterDrainProgressResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ChunksOnCluster != nil {
		t.Fatalf("ChunksOnCluster must be nil before first scan: %v", got.ChunksOnCluster)
	}
	if len(got.Warnings) == 0 {
		t.Fatalf("expected pending-scan warning, got none")
	}
}

func TestClusterDrainProgress_UnknownClusterReturns400(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.RebalanceProgress = rebalance.NewProgressTracker(time.Minute)
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/zzz/drain-progress", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rr.Code)
	}
}
