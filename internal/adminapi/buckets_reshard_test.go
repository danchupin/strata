package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/s3api"
)

func seedReshardBucket(t *testing.T, s *Server, name, owner string) {
	t.Helper()
	if _, err := s.Meta.CreateBucket(context.Background(), name, owner, "STANDARD"); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
}

// TestBucketReshard_GetIdle: a fresh bucket has no job in flight → state
// "idle" with the steady-state shard count. The memory backend reports
// supported=false (range-scan / flat-map → reshard is a no-op).
func TestBucketReshard_GetIdle(t *testing.T) {
	s := newTestServer()
	seedReshardBucket(t, s, "bkt", "alice")

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/reshard", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got BucketReshardJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != "idle" {
		t.Errorf("state=%q want idle", got.State)
	}
	if got.Supported {
		t.Errorf("supported=true on memory backend, want false")
	}
	if got.ShardCount != 64 {
		t.Errorf("shard_count=%d want 64", got.ShardCount)
	}
}

// TestBucketReshard_GetSupportedOnCassandra: the supported flag tracks the
// active meta backend. Only Cassandra implements meta.ReshardMigrator, so a
// "cassandra" backend reports supported=true; the UI keeps the Reshard action
// enabled there and disables it everywhere else.
func TestBucketReshard_GetSupportedOnCassandra(t *testing.T) {
	s := newTestServer()
	s.MetaBackend = "cassandra"
	seedReshardBucket(t, s, "bkt", "alice")

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/reshard", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got BucketReshardJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Supported {
		t.Errorf("supported=false on cassandra backend, want true")
	}
}

// TestBucketReshard_PostQueuesJob: POST returns 202 with state "queued" and a
// follow-up GET reflects the in-flight job (queued before the worker advances
// the watermark). Proves the async-queue contract the console polls against.
func TestBucketReshard_PostQueuesJob(t *testing.T) {
	s := newTestServer()
	seedReshardBucket(t, s, "bkt", "alice")

	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/buckets/bkt/reshard",
		bucketReshardPostRequest{Target: 128})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("post status=%d body=%s", rr.Code, rr.Body.String())
	}
	var posted BucketReshardJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &posted); err != nil {
		t.Fatalf("decode post: %v", err)
	}
	if posted.State != "queued" {
		t.Errorf("post state=%q want queued", posted.State)
	}
	if posted.Source != 64 || posted.Target != 128 {
		t.Errorf("post source/target=%d/%d want 64/128", posted.Source, posted.Target)
	}

	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/reshard", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got BucketReshardJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.State != "queued" {
		t.Errorf("get state=%q want queued", got.State)
	}
	if got.Target != 128 {
		t.Errorf("get target=%d want 128", got.Target)
	}
}

// TestBucketReshard_PostInvalidTarget: a non-power-of-two target is rejected
// 400 before any job is created.
func TestBucketReshard_PostInvalidTarget(t *testing.T) {
	s := newTestServer()
	seedReshardBucket(t, s, "bkt", "alice")

	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/buckets/bkt/reshard",
		bucketReshardPostRequest{Target: 7})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s want 400", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "InvalidArgument" {
		t.Errorf("code=%q want InvalidArgument", er.Code)
	}
}

// TestBucketReshard_PostBucketNotFound: a missing bucket is 404 NoSuchBucket.
func TestBucketReshard_PostBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/buckets/missing/reshard",
		bucketReshardPostRequest{Target: 128})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s want 404", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q want NoSuchBucket", er.Code)
	}
}

// TestBucketReshard_PostInProgressConflict: a second POST while a job is in
// flight is rejected 409 OperationAborted.
func TestBucketReshard_PostInProgressConflict(t *testing.T) {
	s := newTestServer()
	seedReshardBucket(t, s, "bkt", "alice")

	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/buckets/bkt/reshard",
		bucketReshardPostRequest{Target: 128})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("first post status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/buckets/bkt/reshard",
		bucketReshardPostRequest{Target: 256})
	if rr.Code != http.StatusConflict {
		t.Fatalf("second post status=%d body=%s want 409", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "OperationAborted" {
		t.Errorf("code=%q want OperationAborted", er.Code)
	}
}

// TestBucketReshard_PostAudited: a successful POST stamps an
// admin:BucketReshard audit override with resource bucket:<name>. Mirrors
// TestSettingsRotateAuditRow — wrap s.routes() in the audit middleware so the
// SetAuditOverride stamped inside the handler is persisted, then read it back.
func TestBucketReshard_PostAudited(t *testing.T) {
	s := newTestServer()
	seedReshardBucket(t, s, "bkt", "alice")
	b, err := s.Meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}

	mw := s3api.NewAuditMiddleware(s.Meta, time.Hour, s.routes())
	rr := httptest.NewRecorder()
	body, _ := json.Marshal(bucketReshardPostRequest{Target: 128})
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/buckets/bkt/reshard", bytes.NewReader(body))
	req = req.WithContext(auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: "AKIATEST", Owner: "alice"}))
	mw.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	rows, err := s.Meta.ListAudit(context.Background(), b.ID, 100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows: %d want 1", len(rows))
	}
	if rows[0].Action != "admin:BucketReshard" {
		t.Errorf("action=%q want admin:BucketReshard", rows[0].Action)
	}
	if rows[0].Resource != "bucket:bkt" {
		t.Errorf("resource=%q want bucket:bkt", rows[0].Resource)
	}
	if rows[0].Principal != "alice" {
		t.Errorf("principal=%q want alice", rows[0].Principal)
	}
}
