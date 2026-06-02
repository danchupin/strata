package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// TestReconcileStart_OrphanPass: POST with cluster+pool (no bucket) queues an
// orphan pass (data->meta) and returns 202 with state "queued"; a follow-up
// GET by id reflects the same job. Proves the async-queue contract the console
// polls against.
func TestReconcileStart_OrphanPass(t *testing.T) {
	s := newTestServer()

	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/reconcile",
		reconcilePostRequest{Cluster: "ceph-a", Pool: "strata-data", Policy: meta.ReconcilePolicyReport})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ReconcileJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != meta.ReconcileStateQueued {
		t.Errorf("state=%q want queued", got.State)
	}
	if got.ID == "" {
		t.Fatal("empty job id")
	}
	if got.Cluster != "ceph-a" || got.Pool != "strata-data" {
		t.Errorf("cluster/pool=%q/%q want ceph-a/strata-data", got.Cluster, got.Pool)
	}
	if got.Bucket != "" {
		t.Errorf("bucket=%q want empty (orphan pass)", got.Bucket)
	}

	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/reconcile/"+got.ID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var status ReconcileJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	// The status read path (handleReconcileStatus → reconcileJSON) is what the
	// console polls — assert the load-bearing fields survive it, not just the
	// (tautological, same-store) id round-trip.
	if status.ID != got.ID {
		t.Errorf("status id=%q want %q", status.ID, got.ID)
	}
	if status.State != meta.ReconcileStateQueued {
		t.Errorf("status state=%q want queued", status.State)
	}
	if status.Cluster != "ceph-a" || status.Pool != "strata-data" {
		t.Errorf("status cluster/pool=%q/%q want ceph-a/strata-data", status.Cluster, status.Pool)
	}
	if status.Bucket != "" {
		t.Errorf("status bucket=%q want empty (orphan-pass discriminator)", status.Bucket)
	}
	if status.Policy != meta.ReconcilePolicyReport {
		t.Errorf("status policy=%q want report", status.Policy)
	}
}

// TestReconcileStart_MalformedJSON: a body that is not valid JSON is a 400
// MalformedRequest, not a queued job — a client-facing error branch on the
// public admin surface.
func TestReconcileStart_MalformedJSON(t *testing.T) {
	s := newTestServer()

	rr := putAdminRaw(t, s, "alice", http.MethodPost, "/admin/v1/reconcile",
		bytes.NewReader([]byte("{not json")))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s want 400", rr.Code, rr.Body.String())
	}
	var errResp errorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Code != "MalformedRequest" {
		t.Errorf("code=%q want MalformedRequest", errResp.Code)
	}
}

// TestReconcileStart_PostAudited: every admin write stamps an audit override
// (CLAUDE.md: "Add the override stamp to every new admin write"). An orphan
// pass stamps action=admin:Reconcile resource=cluster:<cluster>/<pool>; a
// dangling pass stamps resource=bucket:<name>. Both carry uuid.Nil BucketID
// (non-bucket-scoped, like IAM rows) so they're read via ListAudit(uuid.Nil).
func TestReconcileStart_PostAudited(t *testing.T) {
	t.Run("orphan pass resource", func(t *testing.T) {
		s := newTestServer()
		mw := s3api.NewAuditMiddleware(s.Meta, time.Hour, s.routes())
		rr := httptest.NewRecorder()
		body, _ := json.Marshal(reconcilePostRequest{Cluster: "ceph-a", Pool: "strata-data"})
		req := httptest.NewRequest(http.MethodPost, "/admin/v1/reconcile", bytes.NewReader(body))
		req = req.WithContext(auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: "AKIATEST", Owner: "alice"}))
		mw.ServeHTTP(rr, req)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		rows, err := s.Meta.ListAudit(context.Background(), uuid.Nil, 100)
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("audit rows: %d want 1", len(rows))
		}
		if rows[0].Action != "admin:Reconcile" {
			t.Errorf("action=%q want admin:Reconcile", rows[0].Action)
		}
		if rows[0].Resource != "cluster:ceph-a/strata-data" {
			t.Errorf("resource=%q want cluster:ceph-a/strata-data", rows[0].Resource)
		}
		if rows[0].Principal != "alice" {
			t.Errorf("principal=%q want alice", rows[0].Principal)
		}
	})

	t.Run("dangling pass resource", func(t *testing.T) {
		s := newTestServer()
		if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
			t.Fatalf("seed bucket: %v", err)
		}
		mw := s3api.NewAuditMiddleware(s.Meta, time.Hour, s.routes())
		rr := httptest.NewRecorder()
		body, _ := json.Marshal(reconcilePostRequest{Bucket: "bkt", Policy: meta.ReconcilePolicyQuarantine})
		req := httptest.NewRequest(http.MethodPost, "/admin/v1/reconcile", bytes.NewReader(body))
		req = req.WithContext(auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: "AKIATEST", Owner: "alice"}))
		mw.ServeHTTP(rr, req)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		rows, err := s.Meta.ListAudit(context.Background(), uuid.Nil, 100)
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("audit rows: %d want 1", len(rows))
		}
		if rows[0].Action != "admin:Reconcile" {
			t.Errorf("action=%q want admin:Reconcile", rows[0].Action)
		}
		if rows[0].Resource != "bucket:bkt" {
			t.Errorf("resource=%q want bucket:bkt", rows[0].Resource)
		}
	})
}

// TestReconcileStatus_RendersCounters: reconcileJSON is the whole point of
// US-006 — it surfaces the orphan / dangling counter blocks to the console.
// Populate a job's counters via UpdateReconcileJob and read them back through
// GET so a transposed field (e.g. dangling_report ↔ dangling_quarantine) can't
// ship green. A mocked e2e response cannot catch a Go-side mapping bug.
func TestReconcileStatus_RendersCounters(t *testing.T) {
	s := newTestServer()
	ctx := context.Background()

	job, err := s.Meta.StartReconcile(ctx, "ceph-a", "strata-data", "", "", meta.ReconcilePolicyReport)
	if err != nil {
		t.Fatalf("start reconcile: %v", err)
	}
	job.State = meta.ReconcileStateDone
	job.Cursor = "pg-hash:0x3f"
	job.Scanned = 4096
	job.OrphansFound = 5
	job.OrphansGC = 1
	job.OrphansReport = 4
	job.AbsentBackref = 2
	job.ManifestsScanned = 200
	job.Healthy = 198
	job.DanglingFound = 2
	job.DanglingQuarantine = 1
	job.DanglingReport = 1
	job.Errors = 3
	job.Message = "done"
	if err := s.Meta.UpdateReconcileJob(ctx, job); err != nil {
		t.Fatalf("update reconcile: %v", err)
	}

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/reconcile/"+job.ID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ReconcileJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Every counter field must map 1:1 — pin each so a transposition fails.
	cases := []struct {
		name     string
		got, exp int64
	}{
		{"scanned", got.Scanned, 4096},
		{"orphans_found", got.OrphansFound, 5},
		{"orphans_gc", got.OrphansGC, 1},
		{"orphans_report", got.OrphansReport, 4},
		{"absent_backref", got.AbsentBackref, 2},
		{"manifests_scanned", got.ManifestsScanned, 200},
		{"healthy", got.Healthy, 198},
		{"dangling_found", got.DanglingFound, 2},
		{"dangling_quarantine", got.DanglingQuarantine, 1},
		{"dangling_report", got.DanglingReport, 1},
		{"errors", got.Errors, 3},
	}
	for _, c := range cases {
		if c.got != c.exp {
			t.Errorf("%s=%d want %d", c.name, c.got, c.exp)
		}
	}
	if got.State != meta.ReconcileStateDone {
		t.Errorf("state=%q want done", got.State)
	}
	if got.Cursor != "pg-hash:0x3f" {
		t.Errorf("cursor=%q want pg-hash:0x3f", got.Cursor)
	}
	if got.Message != "done" {
		t.Errorf("message=%q want done", got.Message)
	}
}

// TestReconcileStart_DanglingPass: POST with bucket=<name> resolves the name to
// its UUID and queues a dangling pass (meta->data); the queued job carries the
// resolved UUID in Bucket, not the name.
func TestReconcileStart_DanglingPass(t *testing.T) {
	s := newTestServer()
	b, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("seed bucket: %v", err)
	}

	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/reconcile",
		reconcilePostRequest{Bucket: "bkt", Policy: meta.ReconcilePolicyQuarantine})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ReconcileJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Bucket != b.ID.String() {
		t.Errorf("bucket=%q want resolved uuid %q", got.Bucket, b.ID.String())
	}
	if got.Policy != meta.ReconcilePolicyQuarantine {
		t.Errorf("policy=%q want quarantine", got.Policy)
	}
}

// TestReconcileStart_OrphanRequiresClusterPool: an orphan pass (no bucket) with
// a missing cluster/pool is a 400, not a queued job — the worker cannot scan a
// pool it was not told to walk.
func TestReconcileStart_OrphanRequiresClusterPool(t *testing.T) {
	s := newTestServer()

	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/reconcile",
		reconcilePostRequest{Cluster: "ceph-a"}) // pool missing
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s want 400", rr.Code, rr.Body.String())
	}
}

// TestReconcileStart_RejectsRestorePolicy: restore is deferred to US-002b;
// StartReconcile rejects it with ErrReconcileInvalidPolicy, surfaced as a 400
// so the picker never offers an unsupported destructive policy silently.
func TestReconcileStart_RejectsRestorePolicy(t *testing.T) {
	s := newTestServer()

	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/reconcile",
		reconcilePostRequest{Cluster: "ceph-a", Pool: "strata-data", Policy: meta.ReconcilePolicyRestore})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s want 400", rr.Code, rr.Body.String())
	}
}

// TestReconcileStart_DanglingRejectsGCPolicy: gc is an orphan-only policy; a
// dangling pass with policy=gc is a 400 (IsValidDanglingPolicy gates
// report|quarantine).
func TestReconcileStart_DanglingRejectsGCPolicy(t *testing.T) {
	s := newTestServer()
	if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}

	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/reconcile",
		reconcilePostRequest{Bucket: "bkt", Policy: meta.ReconcilePolicyGC})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s want 400", rr.Code, rr.Body.String())
	}
}

// TestReconcileStart_UnknownBucket: a dangling pass against a missing bucket is
// a 404, not a queued job.
func TestReconcileStart_UnknownBucket(t *testing.T) {
	s := newTestServer()

	rr := putAdmin(t, s, "alice", http.MethodPost, "/admin/v1/reconcile",
		reconcilePostRequest{Bucket: "ghost", Policy: meta.ReconcilePolicyReport})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s want 404", rr.Code, rr.Body.String())
	}
}

// TestReconcileStatus_NotFound: GET on an unknown id is a 404 with the
// NoSuchReconcileJob code.
func TestReconcileStatus_NotFound(t *testing.T) {
	s := newTestServer()

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/reconcile/does-not-exist", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s want 404", rr.Code, rr.Body.String())
	}
	var errResp errorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Code != "NoSuchReconcileJob" {
		t.Errorf("code=%q want NoSuchReconcileJob", errResp.Code)
	}
}
