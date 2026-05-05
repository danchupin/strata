package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// distributionGET drives GET /admin/v1/buckets/{bucket}/distribution and
// returns the decoded body alongside the HTTP status — the per-test boilerplate
// otherwise repeats five times. Adding new tests should follow this shape.
func distributionGET(t *testing.T, s *Server, bucket string) (int, BucketDistributionResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/buckets/"+bucket+"/distribution", nil), "operator")
	s.routes().ServeHTTP(rr, req)
	var got BucketDistributionResponse
	if rr.Body.Len() > 0 && rr.Header().Get("Content-Type") == "application/json" {
		_ = json.NewDecoder(rr.Body).Decode(&got)
	}
	return rr.Code, got
}

func TestBucketDistributionReturnsRowPerShard(t *testing.T) {
	s := newTestServer()
	seedBucketWithOwner(t, s.Meta, "alpha", "alice", 0, 0)
	// Seed a few keys; exact distribution is FNV-driven so just assert the
	// invariant: every shard ID 0..N-1 appears, totals sum across shards.
	for i := range 16 {
		putObject(t, s, "alpha", fmt.Sprintf("key-%03d", i), 100, "etag", "STANDARD")
	}

	code, got := distributionGET(t, s, "alpha")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if len(got.Shards) != 64 {
		t.Fatalf("shards len=%d want 64 (memory store default ShardCount)", len(got.Shards))
	}
	for i, row := range got.Shards {
		if row.Shard != i {
			t.Errorf("shards[%d].shard=%d want contiguous", i, row.Shard)
		}
	}
	var totalBytes, totalObjects int64
	for _, row := range got.Shards {
		totalBytes += row.Bytes
		totalObjects += row.Objects
	}
	if totalBytes != 16*100 {
		t.Errorf("sum(bytes)=%d want 1600", totalBytes)
	}
	if totalObjects != 16 {
		t.Errorf("sum(objects)=%d want 16", totalObjects)
	}
}

func TestBucketDistributionEmptyBucketZeroFills(t *testing.T) {
	s := newTestServer()
	seedBucketWithOwner(t, s.Meta, "empty", "alice", 0, 0)

	code, got := distributionGET(t, s, "empty")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if len(got.Shards) != 64 {
		t.Fatalf("shards len=%d want 64 zero-filled", len(got.Shards))
	}
	for _, row := range got.Shards {
		if row.Bytes != 0 || row.Objects != 0 {
			t.Errorf("shard %d nonzero on empty bucket: %+v", row.Shard, row)
		}
	}
}

func TestBucketDistributionMissingBucket404(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/buckets/missing/distribution", nil), "operator")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rr.Code)
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q want NoSuchBucket", er.Code)
	}
}
