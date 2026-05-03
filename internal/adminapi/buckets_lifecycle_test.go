package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seedLifecycleBucket creates an in-memory bucket for the lifecycle test cases.
func seedLifecycleBucket(t *testing.T, s *Server, name, owner string) {
	t.Helper()
	if _, err := s.Meta.CreateBucket(context.Background(), name, owner, "STANDARD"); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
}

func TestBucketLifecycle_GetNotConfigured(t *testing.T) {
	s := newTestServer()
	seedLifecycleBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/lifecycle", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchLifecycleConfiguration" {
		t.Errorf("code=%q want NoSuchLifecycleConfiguration", er.Code)
	}
}

func TestBucketLifecycle_BucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/missing/lifecycle", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketLifecycle_PutHappyExpirationDays(t *testing.T) {
	s := newTestServer()
	seedLifecycleBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"rules": []map[string]any{
			{
				"id":     "expire-30d",
				"status": "Enabled",
				"filter": map[string]any{"prefix": "logs/"},
				"expiration": map[string]any{
					"days": 30,
				},
			},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/lifecycle", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Round-trip: GET returns the same shape we put in.
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/lifecycle", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got LifecycleConfigJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rules) != 1 {
		t.Fatalf("rules=%d want 1", len(got.Rules))
	}
	r := got.Rules[0]
	if r.ID != "expire-30d" || r.Status != "Enabled" {
		t.Errorf("rule meta: %+v", r)
	}
	if r.Filter == nil || r.Filter.Prefix != "logs/" {
		t.Errorf("filter: %+v", r.Filter)
	}
	if r.Expiration == nil || r.Expiration.Days != 30 {
		t.Errorf("expiration: %+v", r.Expiration)
	}
}

func TestBucketLifecycle_PutAllRuleSurfaces(t *testing.T) {
	s := newTestServer()
	seedLifecycleBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"rules": []map[string]any{
			{
				"id":     "all-surfaces",
				"status": "Enabled",
				"filter": map[string]any{
					"prefix": "data/",
					"tags": []map[string]any{
						{"key": "team", "value": "platform"},
						{"key": "env", "value": "prod"},
					},
				},
				"transitions": []map[string]any{
					{"days": 7, "storage_class": "STANDARD_IA"},
					{"days": 90, "storage_class": "GLACIER"},
				},
				"expiration": map[string]any{"days": 365},
				"noncurrent_version_transitions": []map[string]any{
					{"noncurrent_days": 30, "storage_class": "STANDARD_IA"},
				},
				"noncurrent_version_expiration": map[string]any{"noncurrent_days": 180},
				"abort_incomplete_multipart_upload": map[string]any{
					"days_after_initiation": 7,
				},
			},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/lifecycle", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/lifecycle", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got LifecycleConfigJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rules) != 1 {
		t.Fatalf("rules=%d want 1", len(got.Rules))
	}
	r := got.Rules[0]
	if r.Filter == nil || r.Filter.Prefix != "data/" || len(r.Filter.Tags) != 2 {
		t.Errorf("filter round-trip: %+v", r.Filter)
	}
	if len(r.Transitions) != 2 {
		t.Errorf("transitions=%d want 2", len(r.Transitions))
	}
	if r.NoncurrentVersionExpiration == nil || r.NoncurrentVersionExpiration.NoncurrentDays != 180 {
		t.Errorf("noncurrent expiration: %+v", r.NoncurrentVersionExpiration)
	}
	if len(r.NoncurrentVersionTransitions) != 1 {
		t.Errorf("noncurrent transitions=%d", len(r.NoncurrentVersionTransitions))
	}
	if r.AbortIncompleteMultipartUpload == nil || r.AbortIncompleteMultipartUpload.DaysAfterInitiation != 7 {
		t.Errorf("abort: %+v", r.AbortIncompleteMultipartUpload)
	}
}

func TestBucketLifecycle_PutEmptyRulesRejected(t *testing.T) {
	s := newTestServer()
	seedLifecycleBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/lifecycle",
		map[string]any{"rules": []any{}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketLifecycle_PutBadStatusRejected(t *testing.T) {
	s := newTestServer()
	seedLifecycleBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"rules": []map[string]any{
			{
				"id":         "bad",
				"status":     "Bogus",
				"expiration": map[string]any{"days": 1},
			},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/lifecycle", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketLifecycle_PutDaysAndDateExclusive(t *testing.T) {
	s := newTestServer()
	seedLifecycleBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"rules": []map[string]any{
			{
				"id":     "both",
				"status": "Enabled",
				"expiration": map[string]any{
					"days": 10,
					"date": "2030-01-01T00:00:00Z",
				},
			},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/lifecycle", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketLifecycle_PutNoActionRejected(t *testing.T) {
	s := newTestServer()
	seedLifecycleBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"rules": []map[string]any{
			{
				"id":     "naked",
				"status": "Enabled",
			},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/lifecycle", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketLifecycle_PutBucketNotFound(t *testing.T) {
	s := newTestServer()
	body := map[string]any{
		"rules": []map[string]any{
			{
				"id":         "x",
				"status":     "Enabled",
				"expiration": map[string]any{"days": 1},
			},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/missing/lifecycle", body)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketLifecycle_PutMalformedJSON(t *testing.T) {
	s := newTestServer()
	seedLifecycleBucket(t, s, "bkt", "alice")
	req := httptest.NewRequest(http.MethodPut, "/admin/v1/buckets/bkt/lifecycle",
		strings.NewReader(`{ not-json`))
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketLifecycle_TagOnlyFilterRoundTrips(t *testing.T) {
	s := newTestServer()
	seedLifecycleBucket(t, s, "bkt", "alice")
	body := map[string]any{
		"rules": []map[string]any{
			{
				"id":     "tag-only",
				"status": "Enabled",
				"filter": map[string]any{
					"tags": []map[string]any{{"key": "archive", "value": "yes"}},
				},
				"expiration": map[string]any{"days": 60},
			},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/lifecycle", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/lifecycle", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d", rr.Code)
	}
	var got LifecycleConfigJSON
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Rules) != 1 || got.Rules[0].Filter == nil ||
		len(got.Rules[0].Filter.Tags) != 1 ||
		got.Rules[0].Filter.Tags[0].Key != "archive" {
		t.Errorf("tag round-trip: %+v", got)
	}
}
