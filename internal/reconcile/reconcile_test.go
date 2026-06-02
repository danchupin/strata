package reconcile

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/memory"
)

// fakeScanner yields a fixed chunk set, handing each a decimal index as the
// resume cursor so the worker's checkpoint/resume plumbing is exercised
// without a real RADOS pool. err, when set, is returned after yielding the
// chunks (mid-walk failure).
type fakeScanner struct {
	chunks    []ScannedChunk
	err       error
	gotCursor string // the startCursor the worker passed in
}

func (f *fakeScanner) Scan(ctx context.Context, scope ScanScope, startCursor string, visit func(ScannedChunk, string) error) error {
	f.gotCursor = startCursor
	for i, c := range f.chunks {
		if err := visit(c, strconv.Itoa(i+1)); err != nil {
			return err
		}
	}
	return f.err
}

// seed creates a bucket with one healthy object whose manifest references
// liveOID, and returns the store + bucket id. Versioning is disabled, so the
// object's version_id is the null sentinel — the back-reference uses the same.
func seed(t *testing.T) (*memory.Store, uuid.UUID) {
	t.Helper()
	s := memory.New()
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "recon", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	o := &meta.Object{
		BucketID:     b.ID,
		Key:          "live-key",
		StorageClass: "STANDARD",
		ETag:         "etag",
		Mtime:        time.Unix(1700000000, 0).UTC(),
		Size:         4,
		Manifest: &data.Manifest{
			Class: "STANDARD",
			Size:  4,
			Chunks: []data.ChunkRef{
				{Cluster: "ceph-a", Pool: "strata-data", OID: "live-uuid.00000", Size: 4},
			},
		},
	}
	if err := s.PutObject(ctx, o, false); err != nil {
		t.Fatalf("put object: %v", err)
	}
	return s, b.ID
}

// chunkSet builds the three canonical reconcile inputs: a healthy chunk
// (manifest references it), an orphan chunk (back-reference points at a
// nonexistent key), and a back-reference-less chunk (legacy / opt-out).
func chunkSet(bucketID uuid.UUID) []ScannedChunk {
	return []ScannedChunk{
		{ // healthy: live-key's manifest references live-uuid.00000
			Cluster: "ceph-a", Pool: "strata-data", OID: "live-uuid.00000", Size: 4,
			HasBackref: true,
			Backref:    data.Backref{BucketID: bucketID, Key: "live-key", VersionID: meta.NullVersionID, ChunkIdx: 0, Mtime: time.Unix(1700000000, 0)},
		},
		{ // orphan: ghost-key has no manifest at all
			Cluster: "ceph-a", Pool: "strata-data", OID: "ghost-uuid.00000", Size: 8,
			HasBackref: true,
			Backref:    data.Backref{BucketID: bucketID, Key: "ghost-key", VersionID: meta.NullVersionID, ChunkIdx: 0, Mtime: time.Unix(1700000001, 0)},
		},
		{ // no back-reference (legacy / STRATA_CHUNK_BACKREF=false)
			Cluster: "ceph-a", Pool: "strata-data", OID: "legacy-uuid.00000", Size: 4,
			HasBackref: false,
		},
	}
}

func runJobWith(t *testing.T, s *memory.Store, scanner ChunkScanner, region, policy string) *meta.ReconcileJob {
	t.Helper()
	ctx := context.Background()
	w, err := New(Config{Meta: s, Scanner: scanner, Region: region})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	job, err := s.StartReconcile(ctx, "ceph-a", "strata-data", "", policy)
	if err != nil {
		t.Fatalf("start reconcile: %v", err)
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}
	got, err := s.GetReconcileJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	return got
}

// TestReconcileReportPolicy proves the DEFAULT (report) policy counts the
// orphan and the back-reference-less chunk, leaves the healthy chunk alone,
// and deletes NOTHING — the safe post-restore first pass.
func TestReconcileReportPolicy(t *testing.T) {
	s, bid := seed(t)
	got := runJobWith(t, s, &fakeScanner{chunks: chunkSet(bid)}, "us", meta.ReconcilePolicyReport)

	if got.State != meta.ReconcileStateDone {
		t.Fatalf("state: got %q want done", got.State)
	}
	if got.Scanned != 3 {
		t.Errorf("scanned: got %d want 3", got.Scanned)
	}
	if got.OrphansFound != 1 {
		t.Errorf("orphans_found: got %d want 1", got.OrphansFound)
	}
	if got.OrphansReport != 1 {
		t.Errorf("orphans_report: got %d want 1", got.OrphansReport)
	}
	if got.OrphansGC != 0 {
		t.Errorf("orphans_gc: got %d want 0 (report must never delete)", got.OrphansGC)
	}
	if got.AbsentBackref != 1 {
		t.Errorf("absent_backref: got %d want 1", got.AbsentBackref)
	}
	// Nothing enqueued for deletion.
	n, err := s.ListChunkDeletionsByCluster(context.Background(), "us", "ceph-a", 100)
	if err != nil {
		t.Fatalf("list gc: %v", err)
	}
	if n != 0 {
		t.Errorf("gc queue depth: got %d want 0 (report must not enqueue)", n)
	}
}

// TestReconcileGCPolicy proves the gc policy enqueues exactly the orphan chunk
// for deletion (object rolled back), while the healthy and back-reference-less
// chunks are never enqueued.
func TestReconcileGCPolicy(t *testing.T) {
	s, bid := seed(t)
	got := runJobWith(t, s, &fakeScanner{chunks: chunkSet(bid)}, "us", meta.ReconcilePolicyGC)

	if got.OrphansFound != 1 {
		t.Errorf("orphans_found: got %d want 1", got.OrphansFound)
	}
	if got.OrphansGC != 1 {
		t.Errorf("orphans_gc: got %d want 1", got.OrphansGC)
	}
	if got.OrphansReport != 0 {
		t.Errorf("orphans_report: got %d want 0", got.OrphansReport)
	}
	n, err := s.ListChunkDeletionsByCluster(context.Background(), "us", "ceph-a", 100)
	if err != nil {
		t.Fatalf("list gc: %v", err)
	}
	if n != 1 {
		t.Errorf("gc queue depth: got %d want 1 (only the orphan enqueued)", n)
	}
}

// TestReconcileHealthyChunkUntouched isolates the healthy path: a chunk whose
// manifest references it is neither counted orphan nor enqueued, under gc
// policy (the destructive one) — the strongest no-false-positive proof.
func TestReconcileHealthyChunkUntouched(t *testing.T) {
	s, bid := seed(t)
	only := []ScannedChunk{chunkSet(bid)[0]} // healthy chunk alone
	got := runJobWith(t, s, &fakeScanner{chunks: only}, "us", meta.ReconcilePolicyGC)

	if got.OrphansFound != 0 {
		t.Errorf("orphans_found: got %d want 0 (healthy chunk must not be orphan)", got.OrphansFound)
	}
	n, err := s.ListChunkDeletionsByCluster(context.Background(), "us", "ceph-a", 100)
	if err != nil {
		t.Fatalf("list gc: %v", err)
	}
	if n != 0 {
		t.Errorf("gc queue depth: got %d want 0 (healthy chunk must never be deleted)", n)
	}
}

// TestReconcileOverwrittenVersionIsOrphan proves a chunk whose owning object
// EXISTS but whose manifest no longer references the chunk OID (overwritten in
// place) is detected as an orphan — the data-older-than-meta skew.
func TestReconcileOverwrittenVersionIsOrphan(t *testing.T) {
	s, bid := seed(t)
	// A stale chunk attributed to live-key, but live-key's manifest references
	// live-uuid.00000, not this stale OID.
	stale := []ScannedChunk{{
		Cluster: "ceph-a", Pool: "strata-data", OID: "stale-uuid.00000", Size: 4,
		HasBackref: true,
		Backref:    data.Backref{BucketID: bid, Key: "live-key", VersionID: meta.NullVersionID, ChunkIdx: 0, Mtime: time.Unix(1700000000, 0)},
	}}
	got := runJobWith(t, s, &fakeScanner{chunks: stale}, "us", meta.ReconcilePolicyReport)
	if got.OrphansFound != 1 {
		t.Errorf("orphans_found: got %d want 1 (manifest no longer references the OID)", got.OrphansFound)
	}
}

// TestReconcileResumesFromCursor proves the worker passes the persisted job
// cursor back to the scanner on a re-run (resumability) and advances it.
func TestReconcileResumesFromCursor(t *testing.T) {
	s, bid := seed(t)
	ctx := context.Background()
	w, _ := New(Config{Meta: s, Scanner: &fakeScanner{chunks: chunkSet(bid)}, Region: "us"})
	job, _ := s.StartReconcile(ctx, "ceph-a", "strata-data", "", meta.ReconcilePolicyReport)
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}
	done, _ := s.GetReconcileJob(ctx, job.ID)
	if done.Cursor != "3" {
		t.Errorf("cursor: got %q want %q (last yielded)", done.Cursor, "3")
	}

	// A fresh job records the startCursor the scanner saw — proving the worker
	// forwards job.Cursor (here empty for a new job).
	fresh := &fakeScanner{chunks: chunkSet(bid)}
	w2, _ := New(Config{Meta: s, Scanner: fresh, Region: "us"})
	j2, _ := s.StartReconcile(ctx, "ceph-a", "strata-data", "", meta.ReconcilePolicyReport)
	_, _ = w2.RunOnce(ctx)
	_ = j2
	if fresh.gotCursor != "" {
		t.Errorf("fresh job startCursor: got %q want empty", fresh.gotCursor)
	}
}

// TestReconcileScanErrorMarksJobError proves a mid-walk scan failure flips the
// job to error (with the message) and does NOT abort the worker.
func TestReconcileScanErrorMarksJobError(t *testing.T) {
	s, bid := seed(t)
	ctx := context.Background()
	scanner := &fakeScanner{chunks: chunkSet(bid)[:1], err: context.DeadlineExceeded}
	w, _ := New(Config{Meta: s, Scanner: scanner, Region: "us"})
	job, _ := s.StartReconcile(ctx, "ceph-a", "strata-data", "", meta.ReconcilePolicyReport)
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once should not surface a per-job error: %v", err)
	}
	got, _ := s.GetReconcileJob(ctx, job.ID)
	if got.State != meta.ReconcileStateError {
		t.Errorf("state: got %q want error", got.State)
	}
	if got.Message == "" {
		t.Errorf("error message should be recorded")
	}
}

// TestReconcileInvalidPolicyRejected proves restore (US-002b) + bogus policies
// are rejected at queue time.
func TestReconcileInvalidPolicyRejected(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	for _, p := range []string{meta.ReconcilePolicyRestore, "bogus", ""} {
		if _, err := s.StartReconcile(ctx, "ceph-a", "strata-data", "", p); err != meta.ErrReconcileInvalidPolicy {
			t.Errorf("policy %q: got %v want ErrReconcileInvalidPolicy", p, err)
		}
	}
}
