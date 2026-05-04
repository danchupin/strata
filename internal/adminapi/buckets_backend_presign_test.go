package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBucketSetBackendPresign_EnableAndDisable(t *testing.T) {
	s := newTestServer()
	ctx := context.Background()
	if _, err := s.Meta.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/backend-presign",
		map[string]bool{"enabled": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", rr.Code, rr.Body.String())
	}
	b, err := s.Meta.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !b.BackendPresign {
		t.Fatalf("BackendPresign not flipped to true")
	}

	rr = putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/backend-presign",
		map[string]bool{"enabled": false})
	if rr.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rr.Code, rr.Body.String())
	}
	b, err = s.Meta.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if b.BackendPresign {
		t.Fatalf("BackendPresign not flipped to false")
	}
}

func TestBucketSetBackendPresign_NotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/missing/backend-presign",
		map[string]bool{"enabled": true})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q want NoSuchBucket", er.Code)
	}
}

func TestBucketSetBackendPresign_Malformed(t *testing.T) {
	s := newTestServer()
	ctx := context.Background()
	if _, err := s.Meta.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/admin/v1/buckets/bkt/backend-presign",
		bytes.NewReader([]byte("{not-json")))
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketGet_BackendPresignFlag(t *testing.T) {
	s := newTestServer()
	ctx := context.Background()
	if _, err := s.Meta.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Meta.SetBucketBackendPresign(ctx, "bkt", true); err != nil {
		t.Fatalf("flip: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/bkt", nil)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var d BucketDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if !d.BackendPresign {
		t.Fatalf("BucketDetail.BackendPresign not surfaced: %+v", d)
	}
}
