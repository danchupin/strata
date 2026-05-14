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

	// First scan: 5 chunks remaining (all migratable).
	tracker.CommitScan([]string{"c1"}, map[string]rebalance.ScanResult{"c1": {MigratableChunks: 5, Bytes: 5 * 1024}}, now)

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
	tracker.CommitScan([]string{"c1"}, map[string]rebalance.ScanResult{"c1": {}}, now.Add(time.Second))
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
	tracker.CommitScan([]string{"c1"}, map[string]rebalance.ScanResult{"c1": {MigratableChunks: 7, Bytes: 7}}, time.Now().Add(-10*time.Minute))

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

func TestClusterDrainProgress_CategorizedCountersSurfaced(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	tracker := rebalance.NewProgressTracker(time.Minute)
	s.RebalanceProgress = tracker
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Mixed-category snapshot: 4 migratable, 2 stuck-single, 1 stuck-no-policy.
	tracker.CommitScan([]string{"c1"}, map[string]rebalance.ScanResult{"c1": {
		MigratableChunks:        4,
		StuckSinglePolicyChunks: 2,
		StuckNoPolicyChunks:     1,
		Bytes:                   7 * 1024,
	}}, time.Now().UTC())

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var got ClusterDrainProgressResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MigratableChunks == nil || *got.MigratableChunks != 4 {
		t.Errorf("MigratableChunks: got %v want 4", got.MigratableChunks)
	}
	if got.StuckSinglePolicyChunks == nil || *got.StuckSinglePolicyChunks != 2 {
		t.Errorf("StuckSinglePolicyChunks: got %v want 2", got.StuckSinglePolicyChunks)
	}
	if got.StuckNoPolicyChunks == nil || *got.StuckNoPolicyChunks != 1 {
		t.Errorf("StuckNoPolicyChunks: got %v want 1", got.StuckNoPolicyChunks)
	}
	if got.ChunksOnCluster == nil || *got.ChunksOnCluster != 7 {
		t.Errorf("ChunksOnCluster (total): got %v want 7", got.ChunksOnCluster)
	}
	// deregister_ready false because total > 0.
	if got.DeregisterReady == nil || *got.DeregisterReady {
		t.Errorf("DeregisterReady: got %v want false (total>0)", got.DeregisterReady)
	}
}

func TestClusterDrainProgress_ByBucketSorted(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	tracker := rebalance.NewProgressTracker(time.Minute)
	s.RebalanceProgress = tracker
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tracker.CommitScan([]string{"c1"}, map[string]rebalance.ScanResult{"c1": {
		MigratableChunks:        5,
		StuckSinglePolicyChunks: 3,
		StuckNoPolicyChunks:     2,
		Bytes:                   10 * 1024,
		ByBucket: map[string]rebalance.BucketScanCategory{
			"a-migratable": {Category: "migratable", ChunkCount: 5, BytesUsed: 5 * 1024},
			"b-stuck":      {Category: "stuck_single_policy", ChunkCount: 3, BytesUsed: 3 * 1024},
			"c-residual":   {Category: "stuck_no_policy", ChunkCount: 2, BytesUsed: 2 * 1024},
		},
	}}, time.Now().UTC())

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var got ClusterDrainProgressResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.ByBucket) != 3 {
		t.Fatalf("ByBucket: got %d entries want 3 (%+v)", len(got.ByBucket), got.ByBucket)
	}
	want := []string{"b-stuck", "c-residual", "a-migratable"}
	for i, name := range want {
		if got.ByBucket[i].Name != name {
			t.Errorf("ByBucket[%d].Name: got %q want %q", i, got.ByBucket[i].Name, name)
		}
	}
	if got.ByBucket[0].Category != "stuck_single_policy" {
		t.Errorf("first bucket category: got %q want stuck_single_policy", got.ByBucket[0].Category)
	}
	if got.ByBucket[2].Category != "migratable" {
		t.Errorf("last bucket category: got %q want migratable", got.ByBucket[2].Category)
	}
}

func TestClusterDrainProgress_ReadonlyStateSkipsScan(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.RebalanceProgress = rebalance.NewProgressTracker(time.Minute)
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateDrainingReadonly, meta.ClusterModeReadonly); err != nil {
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
	if got.State != meta.ClusterStateDrainingReadonly {
		t.Fatalf("state: got %q want %q", got.State, meta.ClusterStateDrainingReadonly)
	}
	if got.ChunksOnCluster != nil {
		t.Errorf("readonly state must null counts: %v", got.ChunksOnCluster)
	}
	foundSkipWarn := false
	for _, w := range got.Warnings {
		if w == "stop-writes mode — migration scan skipped; undrain or upgrade to evacuate" {
			foundSkipWarn = true
		}
	}
	if !foundSkipWarn {
		t.Fatalf("expected stop-writes warning, got %+v", got.Warnings)
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
