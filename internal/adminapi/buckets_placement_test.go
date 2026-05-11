package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func seedPlacementBucket(t *testing.T, s *Server, name, owner string) {
	t.Helper()
	if _, err := s.Meta.CreateBucket(context.Background(), name, owner, "STANDARD"); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
}

func TestBucketPlacement_GetNotConfigured(t *testing.T) {
	s := newTestServer()
	seedPlacementBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/placement", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchPlacement" {
		t.Errorf("code=%q want NoSuchPlacement", er.Code)
	}
}

func TestBucketPlacement_GetBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/missing/placement", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q want NoSuchBucket", er.Code)
	}
}

func TestBucketPlacement_PutAndGetRoundTrip(t *testing.T) {
	s := newTestServer()
	seedPlacementBucket(t, s, "bkt", "alice")
	body := BucketPlacementJSON{Placement: map[string]int{"c1": 1, "c2": 3}}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/placement", body)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/placement", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d", rr.Code)
	}
	var got BucketPlacementJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Placement) != 2 || got.Placement["c1"] != 1 || got.Placement["c2"] != 3 {
		t.Errorf("round-trip: got=%+v want=%+v", got, body)
	}
}

func TestBucketPlacement_PutInvalidWeights(t *testing.T) {
	s := newTestServer()
	seedPlacementBucket(t, s, "bkt", "alice")
	cases := []struct {
		name string
		body BucketPlacementJSON
	}{
		{"all-zero", BucketPlacementJSON{Placement: map[string]int{"c1": 0, "c2": 0}}},
		{"negative", BucketPlacementJSON{Placement: map[string]int{"c1": -1}}},
		{"overflow", BucketPlacementJSON{Placement: map[string]int{"c1": 200}}},
		{"empty-id", BucketPlacementJSON{Placement: map[string]int{"": 1}}},
		{"empty-map", BucketPlacementJSON{Placement: map[string]int{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/placement", tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var er errorResponse
			_ = json.Unmarshal(rr.Body.Bytes(), &er)
			if er.Code != "InvalidPlacement" {
				t.Errorf("code=%q want InvalidPlacement", er.Code)
			}
		})
	}
}

func TestBucketPlacement_PutUnknownCluster(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}, "c2": {}}
	seedPlacementBucket(t, s, "bkt", "alice")
	body := BucketPlacementJSON{Placement: map[string]int{"c1": 1, "ghost": 1}}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/placement", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "UnknownCluster" {
		t.Errorf("code=%q want UnknownCluster", er.Code)
	}
	if !strings.Contains(er.Message, "ghost") {
		t.Errorf("message missing offending id: %q", er.Message)
	}
}

func TestBucketPlacement_PutMalformedJSON(t *testing.T) {
	s := newTestServer()
	seedPlacementBucket(t, s, "bkt", "alice")
	rr := putAdminRaw(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/placement",
		strings.NewReader("not-json"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketPlacement_DeleteIdempotent(t *testing.T) {
	s := newTestServer()
	seedPlacementBucket(t, s, "bkt", "alice")
	for range 2 {
		rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/bkt/placement", nil)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("delete status=%d", rr.Code)
		}
	}
}

func TestBucketPlacement_DeleteBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/missing/placement", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}
