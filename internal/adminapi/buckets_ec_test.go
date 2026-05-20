package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// fakeECBackend wraps an underlying data.Backend so the gateway path stays
// happy while overriding ClusterECCapability with operator-supplied
// per-cluster verdicts. Used to drive happy + mismatch paths in the
// admin EC handler tests without spinning up RADOS.
type fakeECBackend struct {
	data.Backend
	caps map[string]struct {
		EC   bool
		K, M int
	}
	notSupported bool
}

func (f *fakeECBackend) ClusterECCapability(ctx context.Context, clusterID string) (bool, int, int, error) {
	if f.notSupported {
		return false, 0, 0, data.ErrClusterUnknown
	}
	c, ok := f.caps[clusterID]
	if !ok {
		return false, 0, 0, data.ErrClusterUnknown
	}
	return c.EC, c.K, c.M, nil
}

func seedECBucket(t *testing.T, s *Server, name, owner string) {
	t.Helper()
	if _, err := s.Meta.CreateBucket(context.Background(), name, owner, "STANDARD"); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
}

func TestBucketECPolicy_GetNotConfigured(t *testing.T) {
	s := newTestServer()
	seedECBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/ec-policy", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchECPolicy" {
		t.Errorf("code=%q want NoSuchECPolicy", er.Code)
	}
}

func TestBucketECPolicy_PutRejectsBadStruct(t *testing.T) {
	s := newTestServer()
	seedECBucket(t, s, "bkt", "alice")
	for name, body := range map[string]BucketECPolicyJSON{
		"both-zero": {K: 0, M: 0},
		"k-zero":    {K: 0, M: 2},
		"m-zero":    {K: 4, M: 0},
		"k-neg":     {K: -1, M: 2},
	} {
		t.Run(name, func(t *testing.T) {
			rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/ec-policy", body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var er errorResponse
			_ = json.Unmarshal(rr.Body.Bytes(), &er)
			if er.Code != "InvalidECPolicy" {
				t.Errorf("code=%q want InvalidECPolicy", er.Code)
			}
		})
	}
}

func TestBucketECPolicy_PutWithoutPlacementRejected(t *testing.T) {
	s := newTestServer()
	seedECBucket(t, s, "bkt", "alice")
	s.Data = &fakeECBackend{
		Backend: s.Data,
		caps: map[string]struct {
			EC   bool
			K, M int
		}{
			"c1": {EC: true, K: 4, M: 2},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/ec-policy",
		BucketECPolicyJSON{K: 4, M: 2})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoPlacement" {
		t.Errorf("code=%q want NoPlacement", er.Code)
	}
}

func TestBucketECPolicy_PutMismatchReplicatedCluster(t *testing.T) {
	s := newTestServer()
	seedECBucket(t, s, "bkt", "alice")
	if err := s.Meta.SetBucketPlacement(context.Background(), "bkt", map[string]int{"c1": 100}); err != nil {
		t.Fatalf("seed placement: %v", err)
	}
	s.Data = &fakeECBackend{
		Backend: s.Data,
		caps: map[string]struct {
			EC   bool
			K, M int
		}{
			"c1": {EC: false},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/ec-policy",
		BucketECPolicyJSON{K: 4, M: 2})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "InconsistentECPolicy" {
		t.Errorf("code=%q want InconsistentECPolicy", er.Code)
	}
}

func TestBucketECPolicy_PutMismatchKMValues(t *testing.T) {
	s := newTestServer()
	seedECBucket(t, s, "bkt", "alice")
	if err := s.Meta.SetBucketPlacement(context.Background(), "bkt", map[string]int{"c1": 50, "c2": 50}); err != nil {
		t.Fatalf("seed placement: %v", err)
	}
	s.Data = &fakeECBackend{
		Backend: s.Data,
		caps: map[string]struct {
			EC   bool
			K, M int
		}{
			"c1": {EC: true, K: 4, M: 2},
			"c2": {EC: true, K: 8, M: 3}, // mismatch
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/ec-policy",
		BucketECPolicyJSON{K: 4, M: 2})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "InconsistentECPolicy" {
		t.Errorf("code=%q want InconsistentECPolicy", er.Code)
	}
}

func TestBucketECPolicy_PutHappyPath(t *testing.T) {
	s := newTestServer()
	seedECBucket(t, s, "bkt", "alice")
	if err := s.Meta.SetBucketPlacement(context.Background(), "bkt", map[string]int{"c1": 50, "c2": 50}); err != nil {
		t.Fatalf("seed placement: %v", err)
	}
	s.Data = &fakeECBackend{
		Backend: s.Data,
		caps: map[string]struct {
			EC   bool
			K, M int
		}{
			"c1": {EC: true, K: 4, M: 2},
			"c2": {EC: true, K: 4, M: 2},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/ec-policy",
		BucketECPolicyJSON{K: 4, M: 2})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Round-trip GET.
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/ec-policy", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body.String())
	}
	body, _ := io.ReadAll(rr.Body)
	var got BucketECPolicyJSON
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.K != 4 || got.M != 2 {
		t.Errorf("round-trip: got=%+v want k=4 m=2", got)
	}
}

func TestBucketECPolicy_DeleteHappyAndIdempotent(t *testing.T) {
	s := newTestServer()
	seedECBucket(t, s, "bkt", "alice")
	// Seed via the meta store directly to bypass capability probing.
	if err := s.Meta.SetBucketECPolicy(context.Background(), "bkt", meta.ECPolicy{K: 4, M: 2}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for i := range 2 {
		rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/bkt/ec-policy", nil)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("delete[%d] status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/ec-policy", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status=%d body=%s", rr.Code, rr.Body.String())
	}
}
