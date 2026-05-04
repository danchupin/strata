package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func seedPolicyBucket(t *testing.T, s *Server, name, owner string) {
	t.Helper()
	if _, err := s.Meta.CreateBucket(context.Background(), name, owner, "STANDARD"); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
}

// rawAdmin sends a raw body through routes() so tests can exercise malformed
// JSON / empty body paths that the JSON-encoding putAdmin helper hides.
func rawAdmin(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	return rr
}

func TestBucketPolicy_GetNotConfigured(t *testing.T) {
	s := newTestServer()
	seedPolicyBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/policy", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucketPolicy" {
		t.Errorf("code=%q want NoSuchBucketPolicy", er.Code)
	}
}

func TestBucketPolicy_GetBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/missing/policy", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q want NoSuchBucket", er.Code)
	}
}

func TestBucketPolicy_PutHappyAndRoundTrip(t *testing.T) {
	s := newTestServer()
	seedPolicyBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{
				"Sid":       "PublicRead",
				"Effect":    "Allow",
				"Principal": "*",
				"Action":    "s3:GetObject",
				"Resource":  "arn:aws:s3:::bkt/*",
			},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/policy", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/policy", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["Version"] != "2012-10-17" {
		t.Errorf("version=%v want 2012-10-17", got["Version"])
	}
	stmts, _ := got["Statement"].([]any)
	if len(stmts) != 1 {
		t.Fatalf("statements=%d want 1", len(stmts))
	}
}

func TestBucketPolicy_PutBucketNotFound(t *testing.T) {
	s := newTestServer()
	body := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::missing/*"},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/missing/policy", body)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketPolicy_PutInvalidEffect(t *testing.T) {
	s := newTestServer()
	seedPolicyBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Effect": "Maybe", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::bkt/*"},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/policy", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "MalformedPolicy" {
		t.Errorf("code=%q want MalformedPolicy", er.Code)
	}
}

func TestBucketPolicy_PutEmptyRejected(t *testing.T) {
	s := newTestServer()
	seedPolicyBucket(t, s, "bkt", "alice")
	rr := rawAdmin(t, s, http.MethodPut, "/admin/v1/buckets/bkt/policy", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketPolicy_PutMalformedJSON(t *testing.T) {
	s := newTestServer()
	seedPolicyBucket(t, s, "bkt", "alice")
	rr := rawAdmin(t, s, http.MethodPut, "/admin/v1/buckets/bkt/policy", `{ not-json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketPolicy_DeleteHappy(t *testing.T) {
	s := newTestServer()
	seedPolicyBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::bkt/*"},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/policy", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("seed put status=%d", rr.Code)
	}
	rr = putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/bkt/policy", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/policy", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("after delete get status=%d", rr.Code)
	}
}

func TestBucketPolicy_DeleteIdempotent(t *testing.T) {
	s := newTestServer()
	seedPolicyBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/bkt/policy", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketPolicy_DeleteBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/missing/policy", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketPolicy_DryRunValid(t *testing.T) {
	s := newTestServer()
	seedPolicyBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::bkt/*"},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/buckets/bkt/policy/dry-run", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp PolicyDryRunResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Valid {
		t.Errorf("valid=%v want true", resp.Valid)
	}
}

func TestBucketPolicy_DryRunInvalid(t *testing.T) {
	s := newTestServer()
	seedPolicyBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Effect": "Maybe", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::bkt/*"},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/buckets/bkt/policy/dry-run", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp PolicyDryRunResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Valid {
		t.Errorf("valid=%v want false", resp.Valid)
	}
	if resp.Message == "" {
		t.Errorf("message empty; want parse error detail")
	}
}

// TestBucketPolicy_DryRunDoesNotPersist confirms dry-run is read-only — a
// valid body returns 200 but a subsequent GET still 404s.
func TestBucketPolicy_DryRunDoesNotPersist(t *testing.T) {
	s := newTestServer()
	seedPolicyBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::bkt/*"},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/buckets/bkt/policy/dry-run", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("dry-run status=%d", rr.Code)
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/policy", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("after dry-run get status=%d body=%s", rr.Code, rr.Body.String())
	}
}
