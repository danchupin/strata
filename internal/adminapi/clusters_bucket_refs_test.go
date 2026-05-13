package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func seedRefBucket(t *testing.T, s *Server, name, owner string, policy map[string]int, deltaBytes, deltaObjects int64) {
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
	if deltaBytes != 0 || deltaObjects != 0 {
		if _, err := s.Meta.BumpBucketStats(ctx, b.ID, deltaBytes, deltaObjects); err != nil {
			t.Fatalf("seed bucket_stats %q: %v", name, err)
		}
	}
}

func TestClusterBucketReferences_FiltersByPlacement(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}, "c2": {}}

	// b1 → {c1:1, c2:1} (references both)
	// b2 → {c2:1} (only c2)
	// b3 → no policy (matches neither)
	seedRefBucket(t, s, "b1", "alice", map[string]int{"c1": 1, "c2": 1}, 1024, 10)
	seedRefBucket(t, s, "b2", "alice", map[string]int{"c2": 1}, 2048, 20)
	seedRefBucket(t, s, "b3", "alice", nil, 9999, 99)

	// c1 → just b1
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/bucket-references", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("c1 status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var got BucketReferencesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode c1: %v", err)
	}
	if got.TotalBuckets != 1 || len(got.Buckets) != 1 {
		t.Fatalf("c1: want 1 match, got total=%d buckets=%+v", got.TotalBuckets, got.Buckets)
	}
	if got.Buckets[0].Name != "b1" || got.Buckets[0].Weight != 1 {
		t.Fatalf("c1: row mismatch: %+v", got.Buckets[0])
	}
	if got.Buckets[0].ChunkCount != 10 || got.Buckets[0].BytesUsed != 1024 {
		t.Fatalf("c1: stats mismatch: %+v", got.Buckets[0])
	}
	if got.NextOffset != nil {
		t.Fatalf("c1: NextOffset must be nil when not truncated: %v", *got.NextOffset)
	}

	// c2 → b1 + b2 (sorted desc by chunk_count → b2 first)
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c2/bucket-references", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("c2 status: got %d body=%s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode c2: %v", err)
	}
	if got.TotalBuckets != 2 || len(got.Buckets) != 2 {
		t.Fatalf("c2: want 2 matches, got total=%d buckets=%+v", got.TotalBuckets, got.Buckets)
	}
	if got.Buckets[0].Name != "b2" {
		t.Fatalf("c2: sort desc by chunk_count failed; got first=%q chunks=%d, expected b2 (20 chunks first)",
			got.Buckets[0].Name, got.Buckets[0].ChunkCount)
	}
	if got.Buckets[1].Name != "b1" {
		t.Fatalf("c2: second row should be b1, got %q", got.Buckets[1].Name)
	}
}

func TestClusterBucketReferences_PaginatesAndStampsNextOffset(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	// Seed 5 buckets all referencing c1; vary chunk_count so the sort key is
	// deterministic. b-a=50, b-b=40, b-c=30, b-d=20, b-e=10.
	chunks := []int64{50, 40, 30, 20, 10}
	for i, n := range chunks {
		name := string([]byte{'b', '-', byte('a' + i)})
		seedRefBucket(t, s, name, "alice", map[string]int{"c1": 1}, n*1024, n)
	}

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/bucket-references?limit=2&offset=0", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("page 1 status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var page1 BucketReferencesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}
	if page1.TotalBuckets != 5 {
		t.Fatalf("total: got %d want 5", page1.TotalBuckets)
	}
	if len(page1.Buckets) != 2 || page1.Buckets[0].Name != "b-a" || page1.Buckets[1].Name != "b-b" {
		t.Fatalf("page 1 rows: %+v", page1.Buckets)
	}
	if page1.NextOffset == nil || *page1.NextOffset != 2 {
		t.Fatalf("NextOffset: got %v want 2", page1.NextOffset)
	}

	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/bucket-references?limit=2&offset=4", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("page 3 status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var page3 BucketReferencesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &page3); err != nil {
		t.Fatalf("decode page 3: %v", err)
	}
	if len(page3.Buckets) != 1 || page3.Buckets[0].Name != "b-e" {
		t.Fatalf("page 3 rows: %+v", page3.Buckets)
	}
	if page3.NextOffset != nil {
		t.Fatalf("page 3 NextOffset must be nil at end-of-list, got %v", *page3.NextOffset)
	}
}

func TestClusterBucketReferences_UnknownClusterReturns400(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/zzz/bucket-references", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rr.Code)
	}
}

func TestClusterBucketReferences_EmptyWhenNoMatches(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	seedRefBucket(t, s, "lonely", "alice", nil, 100, 5)

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/bucket-references", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var got BucketReferencesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TotalBuckets != 0 || len(got.Buckets) != 0 {
		t.Fatalf("expected empty body, got %+v", got)
	}
	if got.NextOffset != nil {
		t.Fatalf("NextOffset must be nil for empty result, got %v", *got.NextOffset)
	}
}

func TestClusterBucketReferences_ZeroWeightExcluded(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}, "c2": {}}
	// Placement validation requires sum>0, so we cannot persist {c1:0,c2:1}
	// directly; instead seed {c1:1, c2:1} and verify zero-weight semantics by
	// only querying c1 below. Then seed a c2-only bucket so the negative
	// (zero on c1) appears via a separate row.
	seedRefBucket(t, s, "both", "alice", map[string]int{"c1": 1, "c2": 1}, 1024, 10)
	seedRefBucket(t, s, "c2only", "alice", map[string]int{"c2": 1}, 2048, 20)

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/bucket-references", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var got BucketReferencesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TotalBuckets != 1 || got.Buckets[0].Name != "both" {
		t.Fatalf("c1 should only match 'both' (c2only has c1 weight zero, excluded): %+v", got)
	}
}
