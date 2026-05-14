package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// seedImpactBucket creates a bucket with optional placement and seeds n
// objects, each carrying a single chunk on `chunkCluster` so the
// /drain-impact scanner sees a non-zero chunk_count on that cluster.
func seedImpactBucket(t *testing.T, s *Server, name, owner string, policy map[string]int, chunkCluster string, n int) {
	t.Helper()
	ctx := context.Background()
	b, err := s.Meta.CreateBucket(ctx, name, owner, "STANDARD")
	if err != nil {
		t.Fatalf("seed bucket %q: %v", name, err)
	}
	if policy != nil {
		if err := s.Meta.SetBucketPlacement(ctx, name, policy); err != nil {
			t.Fatalf("seed placement %q: %v", name, err)
		}
	}
	for i := 0; i < n; i++ {
		key := name + "/obj-" + uuid.NewString()
		err := s.Meta.PutObject(ctx, &meta.Object{
			BucketID:     b.ID,
			Key:          key,
			Size:         1024,
			ETag:         "deadbeef",
			ContentType:  "application/octet-stream",
			StorageClass: "STANDARD",
			Mtime:        time.Now().UTC(),
			IsLatest:     true,
			Manifest: &data.Manifest{Class: "STANDARD", Chunks: []data.ChunkRef{{
				Cluster: chunkCluster, Pool: "default", OID: key + "/chunk", Size: 1024,
			}}},
		}, false)
		if err != nil {
			t.Fatalf("seed object %q: %v", key, err)
		}
	}
}

func TestClusterDrainImpact_CategorizesAndSorts(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}, "c2": {}, "c3": {}}

	// b-migratable → {c1:1, c2:1}; chunks live on c1. Draining c1 leaves
	// c2 as a valid target → migratable.
	seedImpactBucket(t, s, "b-migratable", "alice", map[string]int{"c1": 1, "c2": 1}, "c1", 4)
	// b-stuck-single → {c1:1}; chunks live on c1. Draining c1 leaves no
	// non-excluded target → stuck_single_policy.
	seedImpactBucket(t, s, "b-stuck-single", "alice", map[string]int{"c1": 1}, "c1", 3)
	// b-no-policy → no Placement; chunks live on c1 via class-env routing.
	// Empty policy → stuck_no_policy regardless of cluster set.
	seedImpactBucket(t, s, "b-no-policy", "alice", nil, "c1", 2)
	// b-unaffected → {c2:1, c3:1}; chunks live on c2. No chunks on c1, so
	// it must NOT appear in the by_bucket slice.
	seedImpactBucket(t, s, "b-unaffected", "alice", map[string]int{"c2": 1, "c3": 1}, "c2", 5)

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-impact", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var got ClusterDrainImpactResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ClusterID != "c1" {
		t.Errorf("cluster_id: got %q want c1", got.ClusterID)
	}
	if got.CurrentState != meta.ClusterStateLive {
		t.Errorf("current_state: got %q want live", got.CurrentState)
	}
	if got.MigratableChunks != 4 {
		t.Errorf("migratable: got %d want 4", got.MigratableChunks)
	}
	if got.StuckSinglePolicyChunks != 3 {
		t.Errorf("stuck_single: got %d want 3", got.StuckSinglePolicyChunks)
	}
	if got.StuckNoPolicyChunks != 2 {
		t.Errorf("stuck_no_policy: got %d want 2", got.StuckNoPolicyChunks)
	}
	if got.TotalChunks != 9 {
		t.Errorf("total: got %d want 9", got.TotalChunks)
	}
	if got.TotalBuckets != 3 || len(got.ByBucket) != 3 {
		t.Fatalf("buckets: total=%d slice=%d want 3/3 — b-unaffected must be excluded", got.TotalBuckets, len(got.ByBucket))
	}
	// Sort order: stuck_single_policy → stuck_no_policy → migratable.
	wantOrder := []string{"b-stuck-single", "b-no-policy", "b-migratable"}
	for i, want := range wantOrder {
		if got.ByBucket[i].Name != want {
			t.Errorf("by_bucket[%d].name: got %q want %q (full=%+v)", i, got.ByBucket[i].Name, want, got.ByBucket)
		}
	}
	// stuck_no_policy bucket carries null current_policy + "Set initial
	// policy" suggested labels.
	noPolicy := got.ByBucket[1]
	if noPolicy.Category != "stuck_no_policy" {
		t.Errorf("b-no-policy category: %q", noPolicy.Category)
	}
	if noPolicy.CurrentPolicy != nil {
		t.Errorf("b-no-policy current_policy must be null, got %+v", noPolicy.CurrentPolicy)
	}
	if len(noPolicy.SuggestedPolicies) == 0 {
		t.Fatalf("b-no-policy must have suggested policies")
	}
	if noPolicy.SuggestedPolicies[0].Label != "Set initial policy: live clusters uniform" {
		t.Errorf("first suggestion label: got %q", noPolicy.SuggestedPolicies[0].Label)
	}
	// Uniform suggestion includes every live cluster (c2, c3) at weight 1.
	uniform := noPolicy.SuggestedPolicies[0].Policy
	if uniform["c2"] != 1 || uniform["c3"] != 1 || uniform["c1"] != 0 {
		// c1 omitted because there is no current_policy to neutralize.
		if _, has := uniform["c1"]; has {
			t.Errorf("uniform suggestion must not stamp draining c1 when current is nil; got %+v", uniform)
		}
	}
	// One per-cluster suggestion per live cluster.
	perCluster := 0
	for _, sp := range noPolicy.SuggestedPolicies[1:] {
		if len(sp.Policy) != 1 {
			t.Errorf("single-target suggestion must have one key: %+v", sp.Policy)
		}
		perCluster++
	}
	if perCluster != 2 {
		t.Errorf("single-target suggestions: got %d want 2 (one per live)", perCluster)
	}

	// stuck_single bucket keeps its current policy + offers a uniform
	// suggestion that forces the draining key to 0.
	stuck := got.ByBucket[0]
	if stuck.Category != "stuck_single_policy" {
		t.Errorf("b-stuck-single category: %q", stuck.Category)
	}
	if stuck.CurrentPolicy["c1"] != 1 {
		t.Errorf("b-stuck-single current_policy[c1]: %d", stuck.CurrentPolicy["c1"])
	}
	if stuck.SuggestedPolicies[0].Policy["c1"] != 0 {
		t.Errorf("uniform suggestion must neutralize draining c1, got %+v", stuck.SuggestedPolicies[0].Policy)
	}
}

func TestClusterDrainImpact_RefusesEvacuatingState(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateEvacuating, meta.ClusterModeEvacuate); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-impact", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409 (evacuating must funnel to /drain-progress)", rr.Code)
	}
	var body invalidTransitionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != "InvalidTransition" || body.CurrentState != meta.ClusterStateEvacuating {
		t.Errorf("bad body: %+v", body)
	}
}

func TestClusterDrainImpact_AllowsDrainingReadonlyState(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}, "c2": {}}
	if err := s.Meta.SetClusterState(context.Background(), "c1", meta.ClusterStateDrainingReadonly, meta.ClusterModeReadonly); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedImpactBucket(t, s, "b1", "alice", map[string]int{"c1": 1, "c2": 1}, "c1", 2)
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-impact", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var got ClusterDrainImpactResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CurrentState != meta.ClusterStateDrainingReadonly {
		t.Errorf("current_state: got %q", got.CurrentState)
	}
	if got.MigratableChunks != 2 {
		t.Errorf("migratable: got %d want 2", got.MigratableChunks)
	}
}

func TestClusterDrainImpact_Pagination(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}, "c2": {}}
	// Seed 5 stuck_single buckets so all sit in the same category. Vary
	// chunk_count so the desc sort is deterministic.
	chunks := []int{50, 40, 30, 20, 10}
	for i, n := range chunks {
		name := string([]byte{'b', '-', byte('a' + i)})
		seedImpactBucket(t, s, name, "alice", map[string]int{"c1": 1}, "c1", n)
	}

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-impact?limit=2&offset=0", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("page1 status: %d body=%s", rr.Code, rr.Body.String())
	}
	var page1 ClusterDrainImpactResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	if page1.TotalBuckets != 5 || len(page1.ByBucket) != 2 {
		t.Fatalf("page1 sizing: total=%d slice=%d want 5/2", page1.TotalBuckets, len(page1.ByBucket))
	}
	if page1.ByBucket[0].Name != "b-a" || page1.ByBucket[1].Name != "b-b" {
		t.Fatalf("page1 order: %+v", page1.ByBucket)
	}
	if page1.NextOffset == nil || *page1.NextOffset != 2 {
		t.Fatalf("page1 next_offset: got %v want 2", page1.NextOffset)
	}

	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-impact?limit=2&offset=4", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("page3 status: %d", rr.Code)
	}
	var page3 ClusterDrainImpactResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &page3); err != nil {
		t.Fatalf("decode page3: %v", err)
	}
	if len(page3.ByBucket) != 1 || page3.ByBucket[0].Name != "b-e" {
		t.Fatalf("page3 last row: %+v", page3.ByBucket)
	}
	if page3.NextOffset != nil {
		t.Fatalf("page3 next_offset must be nil at tail: %v", *page3.NextOffset)
	}
}

func TestClusterDrainImpact_CachesAcrossRequests(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}, "c2": {}}
	seedImpactBucket(t, s, "b1", "alice", map[string]int{"c1": 1, "c2": 1}, "c1", 3)

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-impact", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("first status: %d body=%s", rr.Code, rr.Body.String())
	}
	var first ClusterDrainImpactResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first: %v", err)
	}

	// Seed another bucket post-first-request. The second admin call must
	// return the cached scan (no fresh count of the new bucket).
	seedImpactBucket(t, s, "b2", "alice", map[string]int{"c1": 1}, "c1", 99)

	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-impact", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("second status: %d", rr.Code)
	}
	var second ClusterDrainImpactResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if second.TotalChunks != first.TotalChunks {
		t.Fatalf("cache miss: first.total=%d second.total=%d (should match — 5min TTL)",
			first.TotalChunks, second.TotalChunks)
	}
	if second.TotalBuckets != 1 {
		t.Fatalf("cached buckets: got %d want 1", second.TotalBuckets)
	}
}

func TestClusterDrainImpact_UnknownClusterRejected(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c9/drain-impact", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "UnknownCluster" {
		t.Errorf("code=%q want UnknownCluster", er.Code)
	}
}

func TestSuggestedPoliciesForBucket_NoLiveClustersReturnsNil(t *testing.T) {
	got := suggestedPoliciesForBucket(map[string]int{"c1": 1}, map[string]bool{"c1": true}, nil)
	if got != nil {
		t.Fatalf("no-live → nil suggestions; got %+v", got)
	}
}

func TestSuggestedPoliciesForBucket_NeutralizesDrainingKeysInUniform(t *testing.T) {
	current := map[string]int{"c1": 1, "c2": 1}
	exclude := map[string]bool{"c1": true}
	live := []string{"c3"}
	got := suggestedPoliciesForBucket(current, exclude, live)
	if len(got) != 2 {
		t.Fatalf("len: got %d want 2 (uniform + 1 single-target)", len(got))
	}
	uniform := got[0].Policy
	if uniform["c1"] != 0 {
		t.Errorf("uniform must force draining c1 to 0, got %+v", uniform)
	}
	if uniform["c3"] != 1 {
		t.Errorf("uniform must stamp live c3=1, got %+v", uniform)
	}
	if _, hasC2 := uniform["c2"]; hasC2 {
		t.Errorf("uniform must NOT pollute with the non-draining current key c2: %+v", uniform)
	}
}
