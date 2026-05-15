package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
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
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
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
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
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
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
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
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
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
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
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
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateDrainingReadonly, meta.ClusterModeReadonly, 0); err != nil {
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

// seedEvacuatingZeroChunks plants a fully-drained progress snapshot for
// cluster `id` so deregister_ready hinges entirely on the GC-queue + open-
// multipart safety probes — used by the not_ready_reasons tests below.
func seedEvacuatingZeroChunks(t *testing.T, s *Server, id string) {
	t.Helper()
	if err := s.Meta.SetClusterState(context.Background(), id, meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	s.RebalanceProgress.CommitScan([]string{id}, map[string]rebalance.ScanResult{id: {}}, time.Now().UTC())
}

func decodeDrainProgress(t *testing.T, body []byte) ClusterDrainProgressResponse {
	t.Helper()
	var got ClusterDrainProgressResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func containsReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

// TestClusterDrainProgress_NotReadyReasons_GCQueueOnly seeds a fully-drained
// scan but leaves one chunk in the GC queue for the cluster — the
// deregister-ready chip must stay false and surface "gc_queue_pending".
func TestClusterDrainProgress_NotReadyReasons_GCQueueOnly(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.RebalanceProgress = rebalance.NewProgressTracker(time.Minute)
	seedEvacuatingZeroChunks(t, s, "c1")
	if err := s.Meta.EnqueueChunkDeletion(context.Background(), "test-region", []data.ChunkRef{
		{Cluster: "c1", Pool: "p", OID: "oid-1", Size: 1},
	}); err != nil {
		t.Fatalf("enqueue gc: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	got := decodeDrainProgress(t, rr.Body.Bytes())
	if got.DeregisterReady == nil || *got.DeregisterReady {
		t.Fatalf("DeregisterReady: got %v want false", got.DeregisterReady)
	}
	if !containsReason(got.NotReadyReasons, DrainNotReadyGCQueuePending) {
		t.Errorf("NotReadyReasons: missing gc_queue_pending in %v", got.NotReadyReasons)
	}
	if containsReason(got.NotReadyReasons, DrainNotReadyChunksRemaining) {
		t.Errorf("NotReadyReasons: chunks_remaining must NOT be present when chunks=0: %v", got.NotReadyReasons)
	}
	if containsReason(got.NotReadyReasons, DrainNotReadyOpenMultipart) {
		t.Errorf("NotReadyReasons: open_multipart must NOT be present: %v", got.NotReadyReasons)
	}
}

// TestClusterDrainProgress_NotReadyReasons_OpenMultipartOnly seeds a
// fully-drained scan + empty GC queue + one in-flight multipart upload
// whose BackendUploadID points at the cluster. deregister_ready stays
// false with reason "open_multipart".
func TestClusterDrainProgress_NotReadyReasons_OpenMultipartOnly(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.RebalanceProgress = rebalance.NewProgressTracker(time.Minute)
	seedEvacuatingZeroChunks(t, s, "c1")
	b, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := s.Meta.CreateMultipartUpload(context.Background(), &meta.MultipartUpload{
		BucketID:        b.ID,
		UploadID:        "11111111-1111-1111-1111-111111111111",
		Key:             "k",
		StorageClass:    "STANDARD",
		ContentType:     "application/octet-stream",
		BackendUploadID: "c1\x00bkt-backend\x00<obj-uuid>\x00sdk-upload-id",
		InitiatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create mu: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	got := decodeDrainProgress(t, rr.Body.Bytes())
	if got.DeregisterReady == nil || *got.DeregisterReady {
		t.Fatalf("DeregisterReady: got %v want false", got.DeregisterReady)
	}
	if !containsReason(got.NotReadyReasons, DrainNotReadyOpenMultipart) {
		t.Errorf("NotReadyReasons: missing open_multipart in %v", got.NotReadyReasons)
	}
}

// TestClusterDrainProgress_NotReadyReasons_AllThree seeds every blocker —
// chunks on cluster, GC queue, open multipart — and asserts every reason
// token surfaces. Order in the slice is fixed: chunks_remaining first,
// then gc_queue_pending, then open_multipart.
func TestClusterDrainProgress_NotReadyReasons_AllThree(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.RebalanceProgress = rebalance.NewProgressTracker(time.Minute)
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	s.RebalanceProgress.CommitScan([]string{"c1"}, map[string]rebalance.ScanResult{"c1": {MigratableChunks: 2, Bytes: 2 * 1024}}, time.Now().UTC())
	if err := s.Meta.EnqueueChunkDeletion(context.Background(), "test-region", []data.ChunkRef{
		{Cluster: "c1", Pool: "p", OID: "oid-1", Size: 1},
	}); err != nil {
		t.Fatalf("enqueue gc: %v", err)
	}
	b, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := s.Meta.CreateMultipartUpload(context.Background(), &meta.MultipartUpload{
		BucketID:        b.ID,
		UploadID:        "22222222-2222-2222-2222-222222222222",
		Key:             "k",
		StorageClass:    "STANDARD",
		BackendUploadID: "c1\x00bkt-backend\x00<obj-uuid>\x00sdk-id",
		InitiatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create mu: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	got := decodeDrainProgress(t, rr.Body.Bytes())
	if got.DeregisterReady == nil || *got.DeregisterReady {
		t.Fatalf("DeregisterReady: got %v want false", got.DeregisterReady)
	}
	wantSet := []string{DrainNotReadyChunksRemaining, DrainNotReadyGCQueuePending, DrainNotReadyOpenMultipart}
	for _, want := range wantSet {
		if !containsReason(got.NotReadyReasons, want) {
			t.Errorf("NotReadyReasons missing %s in %v", want, got.NotReadyReasons)
		}
	}
	// Stable order: chunks_remaining → gc_queue_pending → open_multipart.
	if joined := strings.Join(got.NotReadyReasons, ","); joined != strings.Join(wantSet, ",") {
		t.Errorf("NotReadyReasons order: got %q want %q", joined, strings.Join(wantSet, ","))
	}
}

// TestClusterDrainProgress_NotReadyReasons_AllClearFlipsReady covers the
// happy path: empty chunks + empty GC queue + no open multipart →
// deregister_ready=true with no reasons.
func TestClusterDrainProgress_NotReadyReasons_AllClearFlipsReady(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.RebalanceProgress = rebalance.NewProgressTracker(time.Minute)
	seedEvacuatingZeroChunks(t, s, "c1")
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	got := decodeDrainProgress(t, rr.Body.Bytes())
	if got.DeregisterReady == nil || !*got.DeregisterReady {
		t.Fatalf("DeregisterReady: got %v want true", got.DeregisterReady)
	}
	if len(got.NotReadyReasons) != 0 {
		t.Errorf("NotReadyReasons: got %v want empty", got.NotReadyReasons)
	}
}
