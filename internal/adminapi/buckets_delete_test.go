package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/heartbeat"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

// newTestServerWithLocker returns a Server backed by the in-memory meta
// store with its in-process Locker wired in. Required for force-empty
// tests; the default newTestServer builds a Server with Locker=nil so
// the legacy admin tests stay focused on read-only handlers.
func newTestServerWithLocker(t *testing.T) *Server {
	t.Helper()
	store := metamem.New()
	creds := auth.NewStaticStore(map[string]*auth.Credential{})
	s := New(Config{
		Meta:        store,
		Creds:       creds,
		Heartbeat:   heartbeat.NewMemoryStore(),
		Locker:      store.Locker(),
		Version:     "test-sha",
		ClusterName: "test-cluster",
		Region:      "test-region",
		MetaBackend: "memory",
		DataBackend: "memory",
		JWTSecret:   []byte("0123456789abcdef0123456789abcdef"),
	})
	return s
}

// withOwner stamps an authenticated owner on the request context so admin
// handlers see a non-empty principal in audit overrides.
func withOwner(req *http.Request, owner string) *http.Request {
	if owner == "" {
		return req
	}
	ctx := auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: owner, Owner: owner})
	return req.WithContext(ctx)
}

// waitForJobState polls GetAdminJob until the job reaches state, or
// fails the test on timeout. Used to synchronise force-empty drain
// goroutine completion with the test body.
func waitForJobState(t *testing.T, s *Server, id, state string, timeout time.Duration) *meta.AdminJob {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		j, err := s.Meta.GetAdminJob(context.Background(), id)
		if err != nil {
			t.Fatalf("getadminjob: %v", err)
		}
		if j.State == state {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never reached state %q (current=%q deleted=%d)", id, state, j.State, j.Deleted)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBucketDelete_Empty(t *testing.T) {
	s := newTestServerWithLocker(t)
	if _, err := s.Meta.CreateBucket(context.Background(), "todelete", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/admin/v1/buckets/todelete", nil)
	req = withOwner(req, "alice")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := s.Meta.GetBucket(context.Background(), "todelete"); err != meta.ErrBucketNotFound {
		t.Errorf("after delete: %v want ErrBucketNotFound", err)
	}
}

// TestBucketDelete_InvalidatesDrainImpactCache asserts that successful
// bucket deletion synchronously drops every cached drain-impact scan
// before returning 204 (US-002 drain-cleanup). A vanished bucket flips
// its chunks off every preview — failing to invalidate leaves the next
// /drain-impact GET serving a stale count.
func TestBucketDelete_InvalidatesDrainImpactCache(t *testing.T) {
	s := newTestServerWithLocker(t)
	if _, err := s.Meta.CreateBucket(context.Background(), "todelete", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s.drainImpact().set("c1", drainImpactScan{TotalChunks: 5})
	req := httptest.NewRequest(http.MethodDelete, "/admin/v1/buckets/todelete", nil)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, withOwner(req, "alice"))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, ok := s.drainImpact().get("c1"); ok {
		t.Fatal("cache entry survived DELETE bucket — invalidation missing")
	}
}

func TestBucketDelete_NotFound(t *testing.T) {
	s := newTestServerWithLocker(t)
	req := httptest.NewRequest(http.MethodDelete, "/admin/v1/buckets/missing", nil)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, withOwner(req, "alice"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q", er.Code)
	}
}

func TestBucketDelete_NotEmpty_Returns409(t *testing.T) {
	s := newTestServerWithLocker(t)
	ctx := context.Background()
	b, err := s.Meta.CreateBucket(ctx, "notempty", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	if err := s.Meta.PutObject(ctx, &meta.Object{
		BucketID: b.ID,
		Key:      "k1",
		Size:     1,
		ETag:     "x",
		Mtime:    time.Now().UTC(),
		Manifest: &data.Manifest{Class: "STANDARD"},
	}, false); err != nil {
		t.Fatalf("put: %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/admin/v1/buckets/notempty", nil)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, withOwner(req, "alice"))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "BucketNotEmpty" {
		t.Errorf("code=%q want BucketNotEmpty", er.Code)
	}
}

func TestBucketForceEmpty_Drains(t *testing.T) {
	s := newTestServerWithLocker(t)
	ctx := context.Background()
	b, err := s.Meta.CreateBucket(ctx, "drainme", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := s.Meta.PutObject(ctx, &meta.Object{
			BucketID: b.ID,
			Key:      "k" + string(rune('a'+i)),
			Size:     1,
			ETag:     "e",
			Mtime:    time.Now().UTC(),
			Manifest: &data.Manifest{Class: "STANDARD"},
		}, false); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/buckets/drainme/force-empty", nil)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, withOwner(req, "alice"))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp ForceEmptyJobResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.JobID == "" || resp.Bucket != "drainme" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	final := waitForJobState(t, s, resp.JobID, meta.AdminJobStateDone, 5*time.Second)
	if final.Deleted != 5 {
		t.Errorf("deleted=%d want 5", final.Deleted)
	}

	// After the drain finishes, plain DELETE should now succeed.
	delReq := httptest.NewRequest(http.MethodDelete, "/admin/v1/buckets/drainme", nil)
	delRR := httptest.NewRecorder()
	s.routes().ServeHTTP(delRR, withOwner(delReq, "alice"))
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("post-drain delete status=%d body=%s", delRR.Code, delRR.Body.String())
	}
}

func TestBucketForceEmpty_NotFound(t *testing.T) {
	s := newTestServerWithLocker(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/buckets/missing/force-empty", nil)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, withOwner(req, "alice"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketForceEmpty_ConcurrentReturns409(t *testing.T) {
	s := newTestServerWithLocker(t)
	ctx := context.Background()
	if _, err := s.Meta.CreateBucket(ctx, "twice", "alice", "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Pre-acquire the lease so the handler observes Acquired=false. We
	// hold it on a holder distinct from the handler's so the second call
	// sees the row and refuses.
	if ok, err := s.Locker.Acquire(ctx, forceEmptyLockName("twice"), "external-holder", forceEmptyLockTTL); err != nil || !ok {
		t.Fatalf("seed lock: ok=%v err=%v", ok, err)
	}
	t.Cleanup(func() {
		_ = s.Locker.Release(ctx, forceEmptyLockName("twice"), "external-holder")
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/buckets/twice/force-empty", nil)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, withOwner(req, "alice"))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "ForceEmptyInProgress" {
		t.Errorf("code=%q want ForceEmptyInProgress", er.Code)
	}
}

func TestBucketForceEmpty_StatusReturnsRow(t *testing.T) {
	s := newTestServerWithLocker(t)
	ctx := context.Background()
	now := time.Now().UTC()
	job := &meta.AdminJob{
		ID:        "job-xyz",
		Kind:      meta.AdminJobKindForceEmpty,
		Bucket:    "somebkt",
		State:     meta.AdminJobStateRunning,
		Deleted:   42,
		StartedAt: now,
		UpdatedAt: now,
	}
	if err := s.Meta.CreateAdminJob(ctx, job); err != nil {
		t.Fatalf("seed admin job: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/somebkt/force-empty/job-xyz", nil)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, withOwner(req, "alice"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp ForceEmptyJobResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.JobID != "job-xyz" || resp.Deleted != 42 || resp.State != "running" {
		t.Errorf("unexpected: %+v", resp)
	}

	// Lookup against a different bucket name returns 404 (defensive
	// check so a job leaked from one bucket cannot reveal another's
	// progress to operators with bucket-scoped permissions).
	mismatch := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/wrong/force-empty/job-xyz", nil)
	mismatchRR := httptest.NewRecorder()
	s.routes().ServeHTTP(mismatchRR, withOwner(mismatch, "alice"))
	if mismatchRR.Code != http.StatusNotFound {
		t.Fatalf("mismatch bucket status=%d", mismatchRR.Code)
	}

	missing := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/somebkt/force-empty/nope", nil)
	missingRR := httptest.NewRecorder()
	s.routes().ServeHTTP(missingRR, withOwner(missing, "alice"))
	if missingRR.Code != http.StatusNotFound {
		t.Fatalf("missing job status=%d", missingRR.Code)
	}
}
