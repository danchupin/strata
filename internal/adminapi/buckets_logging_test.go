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
)

func seedLoggingBucket(t *testing.T, s *Server, name, owner string) {
	t.Helper()
	if _, err := s.Meta.CreateBucket(context.Background(), name, owner, "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func validLoggingBody() LoggingConfigJSON {
	return LoggingConfigJSON{
		TargetBucket: "logs-bucket",
		TargetPrefix: "access/",
	}
}

func TestBucketLogging_GetNotConfigured(t *testing.T) {
	s := newTestServer()
	seedLoggingBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/logging", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucketLoggingConfiguration" {
		t.Errorf("code=%q want NoSuchBucketLoggingConfiguration", er.Code)
	}
}

func TestBucketLogging_GetBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/missing/logging", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q", er.Code)
	}
}

func TestBucketLogging_PutHappyAndRoundTrip(t *testing.T) {
	s := newTestServer()
	seedLoggingBucket(t, s, "bkt", "alice")
	body := LoggingConfigJSON{
		TargetBucket: "logs-bucket",
		TargetPrefix: "access/",
		TargetGrants: []LoggingGrantJSON{
			{GranteeType: "CanonicalUser", ID: "user-1", DisplayName: "Alice", Permission: "READ"},
			{GranteeType: "Group", URI: "http://acs.amazonaws.com/groups/s3/LogDelivery", Permission: "WRITE"},
			{GranteeType: "AmazonCustomerByEmail", Email: "ops@example.com", Permission: "FULL_CONTROL"},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/logging", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/logging", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got LoggingConfigJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TargetBucket != "logs-bucket" || got.TargetPrefix != "access/" {
		t.Errorf("round-trip: %+v", got)
	}
	if len(got.TargetGrants) != 3 {
		t.Fatalf("grants=%d want 3", len(got.TargetGrants))
	}
	if got.TargetGrants[0].GranteeType != "CanonicalUser" || got.TargetGrants[0].ID != "user-1" ||
		got.TargetGrants[0].Permission != "READ" {
		t.Errorf("grant0=%+v", got.TargetGrants[0])
	}
	if got.TargetGrants[1].GranteeType != "Group" || got.TargetGrants[1].URI == "" ||
		got.TargetGrants[1].Permission != "WRITE" {
		t.Errorf("grant1=%+v", got.TargetGrants[1])
	}
	if got.TargetGrants[2].GranteeType != "AmazonCustomerByEmail" || got.TargetGrants[2].Email != "ops@example.com" {
		t.Errorf("grant2=%+v", got.TargetGrants[2])
	}
}

func TestBucketLogging_PutMinimalNoGrants(t *testing.T) {
	s := newTestServer()
	seedLoggingBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/logging", validLoggingBody())
	if rr.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/logging", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d", rr.Code)
	}
	var got LoggingConfigJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.TargetGrants) != 0 {
		t.Errorf("grants=%v want empty", got.TargetGrants)
	}
}

func TestBucketLogging_PutMissingTargetBucket(t *testing.T) {
	s := newTestServer()
	seedLoggingBucket(t, s, "bkt", "alice")
	body := LoggingConfigJSON{TargetBucket: "  ", TargetPrefix: "x/"}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/logging", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "InvalidArgument" || !strings.Contains(er.Message, "target_bucket") {
		t.Errorf("err=%+v", er)
	}
}

func TestBucketLogging_PutBadGranteeType(t *testing.T) {
	s := newTestServer()
	seedLoggingBucket(t, s, "bkt", "alice")
	body := LoggingConfigJSON{
		TargetBucket: "logs-bucket",
		TargetGrants: []LoggingGrantJSON{
			{GranteeType: "Robot", ID: "x", Permission: "READ"},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/logging", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketLogging_PutBadPermission(t *testing.T) {
	s := newTestServer()
	seedLoggingBucket(t, s, "bkt", "alice")
	body := LoggingConfigJSON{
		TargetBucket: "logs-bucket",
		TargetGrants: []LoggingGrantJSON{
			// READ_ACP is valid for ACL grants but NOT for TargetGrants.
			{GranteeType: "CanonicalUser", ID: "x", Permission: "READ_ACP"},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/logging", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if !strings.Contains(er.Message, "FULL_CONTROL | READ | WRITE") {
		t.Errorf("msg=%q", er.Message)
	}
}

func TestBucketLogging_PutCanonicalRequiresID(t *testing.T) {
	s := newTestServer()
	seedLoggingBucket(t, s, "bkt", "alice")
	body := LoggingConfigJSON{
		TargetBucket: "logs-bucket",
		TargetGrants: []LoggingGrantJSON{
			{GranteeType: "CanonicalUser", Permission: "READ"},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/logging", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketLogging_PutGroupRequiresURI(t *testing.T) {
	s := newTestServer()
	seedLoggingBucket(t, s, "bkt", "alice")
	body := LoggingConfigJSON{
		TargetBucket: "logs-bucket",
		TargetGrants: []LoggingGrantJSON{
			{GranteeType: "Group", Permission: "WRITE"},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/logging", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketLogging_PutBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/missing/logging", validLoggingBody())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q", er.Code)
	}
}

func TestBucketLogging_PutMalformedJSON(t *testing.T) {
	s := newTestServer()
	seedLoggingBucket(t, s, "bkt", "alice")
	req := httptest.NewRequest(http.MethodPut, "/admin/v1/buckets/bkt/logging",
		bytes.NewReader([]byte("not-json")))
	ctx := auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: "alice", Owner: "alice"})
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketLogging_DeleteHappy(t *testing.T) {
	s := newTestServer()
	seedLoggingBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/logging", validLoggingBody())
	if rr.Code != http.StatusOK {
		t.Fatalf("put status=%d", rr.Code)
	}
	rr = putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/bkt/logging", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/logging", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status=%d", rr.Code)
	}
}

func TestBucketLogging_DeleteIdempotent(t *testing.T) {
	s := newTestServer()
	seedLoggingBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/bkt/logging", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketLogging_DeleteBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/missing/logging", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketLogging_StoredAsXMLForS3APIConsumption(t *testing.T) {
	s := newTestServer()
	seedLoggingBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/logging", validLoggingBody())
	if rr.Code != http.StatusOK {
		t.Fatalf("put status=%d", rr.Code)
	}
	b, err := s.Meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	blob, err := s.Meta.GetBucketLogging(context.Background(), b.ID)
	if err != nil {
		t.Fatalf("get-logging: %v", err)
	}
	body := string(blob)
	if !strings.Contains(body, "<BucketLoggingStatus") {
		t.Errorf("missing envelope: %s", body)
	}
	if !strings.Contains(body, "<TargetBucket>logs-bucket</TargetBucket>") {
		t.Errorf("missing TargetBucket: %s", body)
	}
	if !strings.Contains(body, "<TargetPrefix>access/</TargetPrefix>") {
		t.Errorf("missing TargetPrefix: %s", body)
	}
}
