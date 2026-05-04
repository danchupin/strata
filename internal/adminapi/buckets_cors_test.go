package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func seedCORSBucket(t *testing.T, s *Server, name, owner string) {
	t.Helper()
	if _, err := s.Meta.CreateBucket(context.Background(), name, owner, "STANDARD"); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
}

func TestBucketCORS_GetNotConfigured(t *testing.T) {
	s := newTestServer()
	seedCORSBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/cors", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchCORSConfiguration" {
		t.Errorf("code=%q want NoSuchCORSConfiguration", er.Code)
	}
}

func TestBucketCORS_GetBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/missing/cors", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q want NoSuchBucket", er.Code)
	}
}

func TestBucketCORS_PutHappyAndRoundTrip(t *testing.T) {
	s := newTestServer()
	seedCORSBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"rules": []map[string]any{
			{
				"id":              "allow-read-from-app",
				"allowed_methods": []string{"GET", "head"},
				"allowed_origins": []string{"https://app.example.com"},
				"allowed_headers": []string{"Authorization", "Content-Type"},
				"expose_headers":  []string{"ETag"},
				"max_age_seconds": 600,
			},
			{
				"allowed_methods": []string{"PUT", "POST", "DELETE"},
				"allowed_origins": []string{"*"},
			},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/cors", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/cors", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got CORSConfigJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rules) != 2 {
		t.Fatalf("rules=%d want 2", len(got.Rules))
	}
	r0 := got.Rules[0]
	if r0.ID != "allow-read-from-app" {
		t.Errorf("rule0.id=%q", r0.ID)
	}
	if len(r0.AllowedMethods) != 2 || r0.AllowedMethods[0] != "GET" || r0.AllowedMethods[1] != "HEAD" {
		t.Errorf("rule0.methods=%v want [GET HEAD]", r0.AllowedMethods)
	}
	if r0.MaxAgeSeconds != 600 {
		t.Errorf("rule0.max_age=%d want 600", r0.MaxAgeSeconds)
	}
	if len(r0.ExposeHeaders) != 1 || r0.ExposeHeaders[0] != "ETag" {
		t.Errorf("rule0.expose=%v", r0.ExposeHeaders)
	}
	r1 := got.Rules[1]
	if len(r1.AllowedMethods) != 3 {
		t.Errorf("rule1.methods=%v", r1.AllowedMethods)
	}
	if len(r1.AllowedOrigins) != 1 || r1.AllowedOrigins[0] != "*" {
		t.Errorf("rule1.origins=%v", r1.AllowedOrigins)
	}
}

func TestBucketCORS_PutEmptyRulesRejected(t *testing.T) {
	s := newTestServer()
	seedCORSBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/cors",
		map[string]any{"rules": []any{}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketCORS_PutMissingMethodsRejected(t *testing.T) {
	s := newTestServer()
	seedCORSBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"rules": []map[string]any{
			{
				"id":              "no-methods",
				"allowed_origins": []string{"*"},
			},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/cors", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketCORS_PutBadMethodRejected(t *testing.T) {
	s := newTestServer()
	seedCORSBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"rules": []map[string]any{
			{
				"allowed_methods": []string{"PURGE"},
				"allowed_origins": []string{"*"},
			},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/cors", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketCORS_PutBucketNotFound(t *testing.T) {
	s := newTestServer()
	body := map[string]any{
		"rules": []map[string]any{
			{
				"allowed_methods": []string{"GET"},
				"allowed_origins": []string{"*"},
			},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/missing/cors", body)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketCORS_PutMalformedJSON(t *testing.T) {
	s := newTestServer()
	seedCORSBucket(t, s, "bkt", "alice")
	req := httptest.NewRequest(http.MethodPut, "/admin/v1/buckets/bkt/cors",
		strings.NewReader(`{ not-json`))
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketCORS_DeleteHappy(t *testing.T) {
	s := newTestServer()
	seedCORSBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"rules": []map[string]any{
			{
				"allowed_methods": []string{"GET"},
				"allowed_origins": []string{"*"},
			},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/cors", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("seed put status=%d", rr.Code)
	}
	rr = putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/bkt/cors", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/cors", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("after delete get status=%d", rr.Code)
	}
}

func TestBucketCORS_DeleteIdempotent(t *testing.T) {
	s := newTestServer()
	seedCORSBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/bkt/cors", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketCORS_DeleteBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/missing/cors", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}
