package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

// putAdmin runs the given JSON body through routes() with an authenticated
// owner stamped on context, mirroring the helper used by buckets_create_test.go.
func putAdmin(t *testing.T, s *Server, owner, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if owner != "" {
		ctx := auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: owner, Owner: owner})
		req = req.WithContext(ctx)
	}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	return rr
}

func TestBucketSetVersioning_Enabled(t *testing.T) {
	s := newTestServer()
	if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/versioning",
		map[string]string{"state": "Enabled"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	b, err := s.Meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if b.Versioning != meta.VersioningEnabled {
		t.Errorf("versioning=%q want Enabled", b.Versioning)
	}
}

func TestBucketSetVersioning_Suspended(t *testing.T) {
	s := newTestServer()
	if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/versioning",
		map[string]string{"state": "Suspended"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketSetVersioning_DisabledRejected(t *testing.T) {
	s := newTestServer()
	if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/versioning",
		map[string]string{"state": "Disabled"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "InvalidArgument" {
		t.Errorf("code=%q want InvalidArgument", er.Code)
	}
}

func TestBucketSetVersioning_NotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/missing/versioning",
		map[string]string{"state": "Enabled"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketSetVersioning_Malformed(t *testing.T) {
	s := newTestServer()
	if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/admin/v1/buckets/bkt/versioning", bytes.NewReader([]byte("{not-json")))
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketSetObjectLock_Happy(t *testing.T) {
	s := newTestServer()
	ctx := context.Background()
	if _, err := s.Meta.CreateBucket(ctx, "lockbkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Meta.SetBucketObjectLockEnabled(ctx, "lockbkt", true); err != nil {
		t.Fatalf("enable lock: %v", err)
	}
	days := 30
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/lockbkt/object-lock",
		map[string]any{
			"object_lock_enabled": "Enabled",
			"rule": map[string]any{
				"default_retention": map[string]any{
					"mode": "GOVERNANCE",
					"days": days,
				},
			},
		})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Round-trip: the stored XML should decode back to the same default rule.
	b, _ := s.Meta.GetBucket(ctx, "lockbkt")
	blob, err := s.Meta.GetBucketObjectLockConfig(ctx, b.ID)
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	var got objectLockXML
	if err := xml.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Rule == nil || got.Rule.DefaultRetention == nil {
		t.Fatalf("missing default retention: %+v", got)
	}
	dr := got.Rule.DefaultRetention
	if dr.Mode != "GOVERNANCE" || dr.Days == nil || *dr.Days != 30 {
		t.Errorf("dr=%+v days=%v", dr, dr.Days)
	}
}

func TestBucketSetObjectLock_RejectsBucketWithoutLock(t *testing.T) {
	s := newTestServer()
	if _, err := s.Meta.CreateBucket(context.Background(), "plain", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/plain/object-lock",
		map[string]any{"object_lock_enabled": "Enabled"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "ObjectLockNotEnabled" {
		t.Errorf("code=%q", er.Code)
	}
}

func TestBucketSetObjectLock_NotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/missing/object-lock",
		map[string]any{"object_lock_enabled": "Enabled"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketSetObjectLock_DaysAndYearsExclusive(t *testing.T) {
	s := newTestServer()
	ctx := context.Background()
	if _, err := s.Meta.CreateBucket(ctx, "lockbkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Meta.SetBucketObjectLockEnabled(ctx, "lockbkt", true); err != nil {
		t.Fatalf("enable lock: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/lockbkt/object-lock",
		map[string]any{
			"object_lock_enabled": "Enabled",
			"rule": map[string]any{
				"default_retention": map[string]any{
					"mode":  "GOVERNANCE",
					"days":  10,
					"years": 1,
				},
			},
		})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketSetObjectLock_BadMode(t *testing.T) {
	s := newTestServer()
	ctx := context.Background()
	if _, err := s.Meta.CreateBucket(ctx, "lockbkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = s.Meta.SetBucketObjectLockEnabled(ctx, "lockbkt", true)
	days := 5
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/lockbkt/object-lock",
		map[string]any{
			"object_lock_enabled": "Enabled",
			"rule": map[string]any{
				"default_retention": map[string]any{
					"mode": "BOGUS",
					"days": days,
				},
			},
		})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketSetObjectLock_ClearRule(t *testing.T) {
	s := newTestServer()
	ctx := context.Background()
	if _, err := s.Meta.CreateBucket(ctx, "lockbkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = s.Meta.SetBucketObjectLockEnabled(ctx, "lockbkt", true)
	// PUT without rule clears the default retention.
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/lockbkt/object-lock",
		map[string]any{"object_lock_enabled": "Enabled"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	b, _ := s.Meta.GetBucket(ctx, "lockbkt")
	blob, err := s.Meta.GetBucketObjectLockConfig(ctx, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got objectLockXML
	_ = xml.Unmarshal(blob, &got)
	if got.Rule != nil {
		t.Errorf("rule should be nil after clear: %+v", got.Rule)
	}
}

func TestBucketGetObjectLock_RoundTrip(t *testing.T) {
	s := newTestServer()
	ctx := context.Background()
	if _, err := s.Meta.CreateBucket(ctx, "lockbkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = s.Meta.SetBucketObjectLockEnabled(ctx, "lockbkt", true)
	years := 2
	put := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/lockbkt/object-lock",
		map[string]any{
			"object_lock_enabled": "Enabled",
			"rule": map[string]any{
				"default_retention": map[string]any{
					"mode":  "COMPLIANCE",
					"years": years,
				},
			},
		})
	if put.Code != http.StatusOK {
		t.Fatalf("put status=%d", put.Code)
	}
	get := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/lockbkt/object-lock", nil)
	s.routes().ServeHTTP(get, req)
	if get.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", get.Code, get.Body.String())
	}
	var resp ObjectLockConfigJSON
	if err := json.Unmarshal(get.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ObjectLockEnabled != "Enabled" {
		t.Errorf("enabled=%q", resp.ObjectLockEnabled)
	}
	if resp.Rule == nil || resp.Rule.DefaultRetention == nil {
		t.Fatalf("missing rule: %+v", resp)
	}
	dr := resp.Rule.DefaultRetention
	if dr.Mode != "COMPLIANCE" || dr.Years == nil || *dr.Years != 2 {
		t.Errorf("dr=%+v", dr)
	}
}

func TestBucketGetObjectLock_NoRule(t *testing.T) {
	s := newTestServer()
	ctx := context.Background()
	if _, err := s.Meta.CreateBucket(ctx, "lockbkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = s.Meta.SetBucketObjectLockEnabled(ctx, "lockbkt", true)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/lockbkt/object-lock", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp ObjectLockConfigJSON
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.ObjectLockEnabled != "Enabled" {
		t.Errorf("enabled=%q want Enabled", resp.ObjectLockEnabled)
	}
	if resp.Rule != nil {
		t.Errorf("rule should be nil: %+v", resp.Rule)
	}
}

func TestBucketGetObjectLock_NotFound(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/missing/object-lock", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketDetail_ObjectLockReflectsBucketFlag(t *testing.T) {
	s := newTestServer()
	ctx := context.Background()
	if _, err := s.Meta.CreateBucket(ctx, "lockbkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = s.Meta.SetBucketObjectLockEnabled(ctx, "lockbkt", true)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/lockbkt", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var got BucketDetail
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if !got.ObjectLock {
		t.Errorf("object_lock=false; want true after SetBucketObjectLockEnabled")
	}
}
