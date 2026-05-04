package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

// postCreateBucket runs POST /admin/v1/buckets through the routes() mux
// (skipping authMiddleware) and stamps an authenticated owner in context so
// the handler echoes a real owner on the BucketDetail response.
func postCreateBucket(t *testing.T, s *Server, owner string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/buckets", &buf)
	if owner != "" {
		ctx := auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: owner, Owner: owner})
		req = req.WithContext(ctx)
	}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	return rr
}

func TestBucketCreate_Happy(t *testing.T) {
	s := newTestServer()
	rr := postCreateBucket(t, s, "alice", map[string]any{
		"name":       "newbkt",
		"versioning": "Enabled",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got BucketDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "newbkt" {
		t.Errorf("name=%q want newbkt", got.Name)
	}
	if got.Owner != "alice" {
		t.Errorf("owner=%q want alice", got.Owner)
	}
	if got.Versioning != "Enabled" {
		t.Errorf("versioning=%q want Enabled", got.Versioning)
	}
	if got.Region != "test-region" {
		t.Errorf("region=%q want test-region (cluster default)", got.Region)
	}
	if got.ObjectLock {
		t.Errorf("object_lock=true; not requested")
	}
	// Round-trip: bucket should now be readable via GetBucket.
	b, err := s.Meta.GetBucket(context.Background(), "newbkt")
	if err != nil {
		t.Fatalf("getbucket: %v", err)
	}
	if b.Versioning != meta.VersioningEnabled {
		t.Errorf("stored versioning=%q want Enabled", b.Versioning)
	}
}

func TestBucketCreate_DefaultVersioningSuspended(t *testing.T) {
	s := newTestServer()
	rr := postCreateBucket(t, s, "alice", map[string]any{"name": "bkt-default"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got BucketDetail
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	// Empty versioning means Suspended; a freshly-created bucket starts at
	// VersioningDisabled internally, so SetBucketVersioning is NOT called and
	// the response label maps to "Off".
	if got.Versioning != "Off" {
		t.Errorf("versioning=%q want Off", got.Versioning)
	}
}

func TestBucketCreate_InvalidName(t *testing.T) {
	s := newTestServer()
	rr := postCreateBucket(t, s, "alice", map[string]any{"name": "ab"}) // < 3 chars
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "InvalidBucketName" {
		t.Errorf("code=%q want InvalidBucketName", er.Code)
	}
}

func TestBucketCreate_ReservedName(t *testing.T) {
	s := newTestServer()
	rr := postCreateBucket(t, s, "alice", map[string]any{"name": "console"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "InvalidBucketName" {
		t.Errorf("code=%q want InvalidBucketName for reserved name", er.Code)
	}
}

func TestBucketCreate_Conflict(t *testing.T) {
	s := newTestServer()
	if _, err := s.Meta.CreateBucket(context.Background(), "dupbkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := postCreateBucket(t, s, "alice", map[string]any{"name": "dupbkt"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "BucketAlreadyExists" {
		t.Errorf("code=%q want BucketAlreadyExists", er.Code)
	}
}

func TestBucketCreate_VersioningDisabledRejected(t *testing.T) {
	s := newTestServer()
	rr := postCreateBucket(t, s, "alice", map[string]any{
		"name":       "bkt-bad-vers",
		"versioning": "Disabled",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "InvalidArgument" {
		t.Errorf("code=%q want InvalidArgument", er.Code)
	}
}

func TestBucketCreate_ObjectLockRequiresVersioningEnabled(t *testing.T) {
	s := newTestServer()
	rr := postCreateBucket(t, s, "alice", map[string]any{
		"name":                "bkt-lock",
		"versioning":          "Suspended",
		"object_lock_enabled": true,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "InvalidArgument" {
		t.Errorf("code=%q want InvalidArgument", er.Code)
	}
}

func TestBucketCreate_WithObjectLockEnabled(t *testing.T) {
	s := newTestServer()
	rr := postCreateBucket(t, s, "alice", map[string]any{
		"name":                "bkt-locked",
		"versioning":          "Enabled",
		"object_lock_enabled": true,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got BucketDetail
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if !got.ObjectLock {
		t.Errorf("object_lock=false; want true")
	}
}

func TestBucketCreate_MalformedJSON(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/buckets", strings.NewReader("{not-json"))
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}
