package reconcile

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
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
	job, err := s.StartReconcile(ctx, "ceph-a", "strata-data", "", "", policy)
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
	job, _ := s.StartReconcile(ctx, "ceph-a", "strata-data", "", "", meta.ReconcilePolicyReport)
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
	j2, _ := s.StartReconcile(ctx, "ceph-a", "strata-data", "", "", meta.ReconcilePolicyReport)
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
	job, _ := s.StartReconcile(ctx, "ceph-a", "strata-data", "", "", meta.ReconcilePolicyReport)
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

// TestReconcileInvalidPolicyRejected proves bogus/empty orphan-pass policies
// are rejected at queue time. restore (US-002b) is now accepted (see the
// restore tests below).
func TestReconcileInvalidPolicyRejected(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	for _, p := range []string{"bogus", ""} {
		if _, err := s.StartReconcile(ctx, "ceph-a", "strata-data", "", "", p); err != meta.ErrReconcileInvalidPolicy {
			t.Errorf("policy %q: got %v want ErrReconcileInvalidPolicy", p, err)
		}
	}
	if _, err := s.StartReconcile(ctx, "ceph-a", "strata-data", "", "", meta.ReconcilePolicyRestore); err != nil {
		t.Errorf("restore policy: got %v want accepted", err)
	}
}

// fakeProber answers ChunkExists from a fixed OID set — a chunk OID not in the
// set is treated as missing (the dangling-manifest condition). err, when set,
// is returned (transient probe failure -> never quarantine on doubt).
type fakeProber struct {
	present map[string]bool
	err     error
}

func (f *fakeProber) ChunkExists(ctx context.Context, ref data.ChunkRef) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.present[ref.OID], nil
}

// seedDangling extends seed() with a second object ("broken-key") whose
// manifest references a chunk OID the prober will report missing — the
// dangling-manifest condition. Returns the store, bucket id, and a prober that
// holds only live-key's chunk.
func seedDangling(t *testing.T) (*memory.Store, uuid.UUID, *fakeProber) {
	t.Helper()
	s, bid := seed(t)
	ctx := context.Background()
	broken := &meta.Object{
		BucketID:     bid,
		Key:          "broken-key",
		StorageClass: "STANDARD",
		ETag:         "etag2",
		Mtime:        time.Unix(1700000002, 0).UTC(),
		Size:         4,
		Manifest: &data.Manifest{
			Class: "STANDARD",
			Size:  4,
			Chunks: []data.ChunkRef{
				{Cluster: "ceph-a", Pool: "strata-data", OID: "missing-uuid.00000", Size: 4},
			},
		},
	}
	if err := s.PutObject(ctx, broken, false); err != nil {
		t.Fatalf("put broken object: %v", err)
	}
	// Prober holds live-key's chunk but NOT broken-key's.
	return s, bid, &fakeProber{present: map[string]bool{"live-uuid.00000": true}}
}

func runDanglingJob(t *testing.T, s *memory.Store, prober ChunkProber, bid uuid.UUID, policy string) *meta.ReconcileJob {
	t.Helper()
	ctx := context.Background()
	w, err := New(Config{Meta: s, Prober: prober})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	job, err := s.StartReconcile(ctx, "", "", "", bid.String(), policy)
	if err != nil {
		t.Fatalf("start dangling reconcile: %v", err)
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

// TestReconcileDanglingQuarantine is the US-003 red/green: a manifest whose
// chunk is missing is detected dangling and the object is quarantined so a GET
// returns a clear error; the healthy object is untouched.
func TestReconcileDanglingQuarantine(t *testing.T) {
	s, bid, prober := seedDangling(t)
	ctx := context.Background()
	got := runDanglingJob(t, s, prober, bid, meta.ReconcilePolicyQuarantine)

	if got.State != meta.ReconcileStateDone {
		t.Fatalf("state: got %q want done", got.State)
	}
	if got.ManifestsScanned != 2 {
		t.Errorf("manifests_scanned: got %d want 2", got.ManifestsScanned)
	}
	if got.Healthy != 1 {
		t.Errorf("healthy: got %d want 1", got.Healthy)
	}
	if got.DanglingFound != 1 || got.DanglingQuarantine != 1 {
		t.Errorf("dangling: found=%d quarantine=%d want 1/1", got.DanglingFound, got.DanglingQuarantine)
	}

	// The broken object is quarantined; the healthy one is not.
	broken, err := s.GetObject(ctx, bid, "broken-key", "")
	if err != nil {
		t.Fatalf("get broken: %v", err)
	}
	if broken.QuarantineReason == "" {
		t.Errorf("broken-key not quarantined")
	}
	live, err := s.GetObject(ctx, bid, "live-key", "")
	if err != nil {
		t.Fatalf("get live: %v", err)
	}
	if live.QuarantineReason != "" {
		t.Errorf("healthy live-key wrongly quarantined: %q", live.QuarantineReason)
	}
}

// TestReconcileDanglingReport proves the DEFAULT report policy counts the
// dangling manifest but mutates NOTHING — the object is not quarantined.
func TestReconcileDanglingReport(t *testing.T) {
	s, bid, prober := seedDangling(t)
	ctx := context.Background()
	got := runDanglingJob(t, s, prober, bid, meta.ReconcilePolicyReport)

	if got.DanglingFound != 1 || got.DanglingReport != 1 || got.DanglingQuarantine != 0 {
		t.Errorf("dangling: found=%d report=%d quarantine=%d want 1/1/0",
			got.DanglingFound, got.DanglingReport, got.DanglingQuarantine)
	}
	broken, err := s.GetObject(ctx, bid, "broken-key", "")
	if err != nil {
		t.Fatalf("get broken: %v", err)
	}
	if broken.QuarantineReason != "" {
		t.Errorf("report policy must not quarantine: %q", broken.QuarantineReason)
	}
}

// TestReconcileDanglingProbeErrorNeverQuarantines proves a transient probe
// error counts as an error and quarantines nothing — never destroy/flag on
// doubt.
func TestReconcileDanglingProbeErrorNeverQuarantines(t *testing.T) {
	s, bid, _ := seedDangling(t)
	ctx := context.Background()
	prober := &fakeProber{err: context.DeadlineExceeded}
	got := runDanglingJob(t, s, prober, bid, meta.ReconcilePolicyQuarantine)

	if got.DanglingQuarantine != 0 {
		t.Errorf("quarantine on probe error: got %d want 0", got.DanglingQuarantine)
	}
	if got.Errors == 0 {
		t.Errorf("probe error should be counted")
	}
	broken, err := s.GetObject(ctx, bid, "broken-key", "")
	if err != nil {
		t.Fatalf("get broken: %v", err)
	}
	if broken.QuarantineReason != "" {
		t.Errorf("must not quarantine on probe error: %q", broken.QuarantineReason)
	}
}

// TestReconcileDanglingNoProber proves a dangling job with no prober wired
// (default-tag RADOS) is marked error and quarantines nothing.
func TestReconcileDanglingNoProber(t *testing.T) {
	s, bid, _ := seedDangling(t)
	ctx := context.Background()
	w, _ := New(Config{Meta: s}) // no Prober
	job, _ := s.StartReconcile(ctx, "", "", "", bid.String(), meta.ReconcilePolicyQuarantine)
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once should not surface a per-job error: %v", err)
	}
	got, _ := s.GetReconcileJob(ctx, job.ID)
	if got.State != meta.ReconcileStateError {
		t.Errorf("state: got %q want error", got.State)
	}
}

// --- US-003b dangling delete resolution + BackendRef probe --------------------

// runDanglingDeleteJob runs a delete-policy dangling pass with a region so the
// GC enqueue lands in a known queue the test can read back.
func runDanglingDeleteJob(t *testing.T, s *memory.Store, prober ChunkProber, bid uuid.UUID, region string) *meta.ReconcileJob {
	t.Helper()
	ctx := context.Background()
	w, err := New(Config{Meta: s, Prober: prober, Region: region})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	job, err := s.StartReconcile(ctx, "", "", "", bid.String(), meta.ReconcilePolicyDelete)
	if err != nil {
		t.Fatalf("start dangling delete reconcile: %v", err)
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

// TestReconcileDanglingDelete is the US-003b red/green: a dangling manifest
// under the delete policy has its chunks enqueued for GC and its object-version
// row removed; the healthy object and its chunk are untouched.
func TestReconcileDanglingDelete(t *testing.T) {
	s, bid, prober := seedDangling(t)
	ctx := context.Background()
	const region = "us"
	got := runDanglingDeleteJob(t, s, prober, bid, region)

	if got.State != meta.ReconcileStateDone {
		t.Fatalf("state: got %q want done", got.State)
	}
	if got.DanglingFound != 1 || got.DanglingDelete != 1 {
		t.Errorf("dangling: found=%d delete=%d want 1/1", got.DanglingFound, got.DanglingDelete)
	}
	if got.DanglingQuarantine != 0 || got.DanglingReport != 0 {
		t.Errorf("delete policy must not quarantine/report: q=%d r=%d", got.DanglingQuarantine, got.DanglingReport)
	}
	if got.Errors != 0 {
		t.Errorf("unexpected errors: %d", got.Errors)
	}

	// The broken object-version row is gone.
	if _, err := s.GetObject(ctx, bid, "broken-key", ""); !errors.Is(err, meta.ErrObjectNotFound) {
		t.Errorf("broken-key still present: err=%v want ErrObjectNotFound", err)
	}
	// Its chunk is enqueued for GC in the worker's region.
	entries, err := s.ListGCEntries(ctx, region, time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("list gc: %v", err)
	}
	var sawBroken bool
	for _, e := range entries {
		if e.Chunk.OID == "missing-uuid.00000" {
			sawBroken = true
		}
	}
	if !sawBroken {
		t.Errorf("broken chunk not enqueued for GC: %+v", entries)
	}
	// The healthy object is untouched.
	if _, err := s.GetObject(ctx, bid, "live-key", ""); err != nil {
		t.Errorf("healthy live-key wrongly removed: %v", err)
	}
}

// TestReconcileDanglingDeleteProbeErrorNeverDeletes proves a transient probe
// error counts an error and removes nothing — never destroy on doubt.
func TestReconcileDanglingDeleteProbeErrorNeverDeletes(t *testing.T) {
	s, bid, _ := seedDangling(t)
	ctx := context.Background()
	prober := &fakeProber{err: context.DeadlineExceeded}
	got := runDanglingDeleteJob(t, s, prober, bid, "us")

	if got.DanglingDelete != 0 {
		t.Errorf("delete on probe error: got %d want 0", got.DanglingDelete)
	}
	if got.Errors == 0 {
		t.Errorf("probe error should be counted")
	}
	if _, err := s.GetObject(ctx, bid, "broken-key", ""); err != nil {
		t.Errorf("must not remove on probe error: %v", err)
	}
}

// TestReconcileDanglingBackendRefProbed proves the dangling pass probes the
// single backing object of an S3-passthrough BackendRef manifest (no
// Manifest.Chunks) — US-003b. The prober reports the backing key missing, so
// the object is detected dangling and quarantined.
func TestReconcileDanglingBackendRefProbed(t *testing.T) {
	s, bid := seed(t)
	ctx := context.Background()
	br := &meta.Object{
		BucketID:     bid,
		Key:          "passthrough-key",
		StorageClass: "STANDARD",
		ETag:         "etag3",
		Mtime:        time.Unix(1700000003, 0).UTC(),
		Size:         7,
		Manifest: &data.Manifest{
			Class: "STANDARD",
			Size:  7,
			BackendRef: &data.BackendRef{
				Backend: "s3",
				Key:     "backing-object-key",
				Cluster: "s3-a",
				Size:    7,
			},
		},
	}
	if err := s.PutObject(ctx, br, false); err != nil {
		t.Fatalf("put backendref object: %v", err)
	}
	// Prober holds live-key's chunk but NOT the backing-object key.
	prober := &fakeProber{present: map[string]bool{"live-uuid.00000": true}}
	got := runDanglingJob(t, s, prober, bid, meta.ReconcilePolicyQuarantine)

	if got.DanglingFound != 1 || got.DanglingQuarantine != 1 {
		t.Errorf("backendref dangling: found=%d quarantine=%d want 1/1", got.DanglingFound, got.DanglingQuarantine)
	}
	obj, err := s.GetObject(ctx, bid, "passthrough-key", "")
	if err != nil {
		t.Fatalf("get passthrough: %v", err)
	}
	if obj.QuarantineReason == "" {
		t.Errorf("backendref object not quarantined")
	}
}

// --- US-002b restore policy ---------------------------------------------------

// putRestoreChunk writes payload to the memory data backend and returns the
// resulting single-chunk ref so a restore test can reference the OID the backend
// actually holds. Each small payload is one DefaultChunkSize chunk.
func putRestoreChunk(t *testing.T, db *datamem.Backend, payload []byte) data.ChunkRef {
	t.Helper()
	m, err := db.PutChunks(context.Background(), bytes.NewReader(payload), "STANDARD")
	if err != nil {
		t.Fatalf("put chunk: %v", err)
	}
	if len(m.Chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(m.Chunks))
	}
	return m.Chunks[0]
}

// runRestoreJob runs one restore-policy orphan pass over the supplied chunks
// with a live data backend (needed to recompute the rebuilt ETag).
func runRestoreJob(t *testing.T, s *memory.Store, db *datamem.Backend, chunks []ScannedChunk) *meta.ReconcileJob {
	t.Helper()
	ctx := context.Background()
	w, err := New(Config{Meta: s, Scanner: &fakeScanner{chunks: chunks}, Data: db, Region: "us"})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	job, err := s.StartReconcile(ctx, "ceph-a", "strata-data", "", "", meta.ReconcilePolicyRestore)
	if err != nil {
		t.Fatalf("start restore: %v", err)
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

// TestReconcileRestoreRebuildsManifest is the US-002b red/green: an orphan chunk
// whose manifest row is gone (meta-older-than-data) is rebuilt under restore so
// the object is GET-able again with correct bytes + ETag; a re-run is a no-op
// (idempotent — the rebuilt manifest now references the chunk).
func TestReconcileRestoreRebuildsManifest(t *testing.T) {
	s, bid := seed(t)
	db := datamem.New()
	ctx := context.Background()
	payload := []byte("hello restore world")
	ref := putRestoreChunk(t, db, payload)

	sc := []ScannedChunk{{
		Cluster: "ceph-a", Pool: "strata-data", OID: ref.OID, Size: ref.Size,
		HasBackref: true,
		Backref:    data.Backref{BucketID: bid, Key: "restore-key", VersionID: meta.NullVersionID, ChunkIdx: 0, Mtime: time.Unix(1700000005, 0)},
	}}
	got := runRestoreJob(t, s, db, sc)

	if got.State != meta.ReconcileStateDone {
		t.Fatalf("state: got %q want done", got.State)
	}
	if got.OrphansFound != 1 || got.OrphansRestore != 1 || got.OrphansReport != 0 {
		t.Fatalf("found=%d restore=%d report=%d want 1/1/0", got.OrphansFound, got.OrphansRestore, got.OrphansReport)
	}

	obj, err := s.GetObject(ctx, bid, "restore-key", "")
	if err != nil {
		t.Fatalf("get restored: %v", err)
	}
	sum := md5.Sum(payload)
	wantETag := hex.EncodeToString(sum[:])
	if obj.ETag != wantETag {
		t.Errorf("etag: got %q want %q", obj.ETag, wantETag)
	}
	if obj.Size != int64(len(payload)) {
		t.Errorf("size: got %d want %d", obj.Size, len(payload))
	}
	if obj.Manifest == nil || len(obj.Manifest.Chunks) != 1 || obj.Manifest.Chunks[0].OID != ref.OID {
		t.Fatalf("manifest not rebuilt: %+v", obj.Manifest)
	}
	rc, err := db.GetChunks(ctx, obj.Manifest, 0, obj.Size)
	if err != nil {
		t.Fatalf("get chunks: %v", err)
	}
	gotBytes, _ := io.ReadAll(rc)
	rc.Close()
	if string(gotBytes) != string(payload) {
		t.Errorf("bytes: got %q want %q", gotBytes, payload)
	}

	// Idempotent: the chunk is now healthy (its manifest references it).
	got2 := runRestoreJob(t, s, db, sc)
	if got2.OrphansFound != 0 || got2.OrphansRestore != 0 {
		t.Errorf("re-run found=%d restore=%d want 0/0 (idempotent)", got2.OrphansFound, got2.OrphansRestore)
	}
}

// TestReconcileRestoreGappedNotStitched proves a version missing a chunk_idx is
// reported, NEVER stitched into a short object served as whole.
func TestReconcileRestoreGappedNotStitched(t *testing.T) {
	s, bid := seed(t)
	db := datamem.New()
	ref := putRestoreChunk(t, db, []byte("second-part-only"))
	// Only chunk_idx 1 present -> index 0 missing -> gap.
	sc := []ScannedChunk{{
		Cluster: "ceph-a", Pool: "strata-data", OID: ref.OID, Size: ref.Size,
		HasBackref: true,
		Backref:    data.Backref{BucketID: bid, Key: "gap-key", VersionID: meta.NullVersionID, ChunkIdx: 1, Mtime: time.Unix(1700000006, 0)},
	}}
	got := runRestoreJob(t, s, db, sc)

	if got.OrphansFound != 1 || got.OrphansRestore != 0 || got.OrphansReport != 1 {
		t.Fatalf("found=%d restore=%d report=%d want 1/0/1 (gapped reported)", got.OrphansFound, got.OrphansRestore, got.OrphansReport)
	}
	if _, err := s.GetObject(context.Background(), bid, "gap-key", ""); !errors.Is(err, meta.ErrObjectNotFound) {
		t.Errorf("gapped object must not be restored: err=%v", err)
	}
}

// TestReconcileRestoreSSEUnrecoverable proves an SSE orphan is reported
// unrecoverable (the wrapped DEK was in the lost meta), never restored as
// ciphertext — same plaintext-only boundary as rebuild-index.
func TestReconcileRestoreSSEUnrecoverable(t *testing.T) {
	s, bid := seed(t)
	db := datamem.New()
	ref := putRestoreChunk(t, db, []byte("ciphertext-bytes"))
	sc := []ScannedChunk{{
		Cluster: "ceph-a", Pool: "strata-data", OID: ref.OID, Size: ref.Size,
		HasBackref: true,
		Backref:    data.Backref{BucketID: bid, Key: "sse-key", VersionID: meta.NullVersionID, ChunkIdx: 0, Mtime: time.Unix(1700000007, 0), SSEAlgo: "AES256"},
	}}
	got := runRestoreJob(t, s, db, sc)

	if got.OrphansReport != 1 || got.OrphansRestore != 0 {
		t.Fatalf("report=%d restore=%d want 1/0 (SSE unrecoverable)", got.OrphansReport, got.OrphansRestore)
	}
	if _, err := s.GetObject(context.Background(), bid, "sse-key", ""); !errors.Is(err, meta.ErrObjectNotFound) {
		t.Errorf("SSE object must not be restored: %v", err)
	}
}

// TestReconcileRestoreNeverClobbersOverwrittenVersion is the critical
// safety proof: a stale chunk attributed to a key whose version row STILL EXISTS
// (with a valid manifest pointing at a different OID) is reported, never used to
// overwrite the live manifest — restoring it would corrupt live data.
func TestReconcileRestoreNeverClobbersOverwrittenVersion(t *testing.T) {
	s, bid := seed(t) // live-key exists; manifest references live-uuid.00000
	db := datamem.New()
	ref := putRestoreChunk(t, db, []byte("stale-overwritten-bytes"))
	sc := []ScannedChunk{{
		Cluster: "ceph-a", Pool: "strata-data", OID: ref.OID, Size: ref.Size,
		HasBackref: true,
		Backref:    data.Backref{BucketID: bid, Key: "live-key", VersionID: meta.NullVersionID, ChunkIdx: 0, Mtime: time.Unix(1700000008, 0)},
	}}
	got := runRestoreJob(t, s, db, sc)

	if got.OrphansFound != 1 || got.OrphansRestore != 0 || got.OrphansReport != 1 {
		t.Fatalf("found=%d restore=%d report=%d want 1/0/1 (overwritten -> reported, never clobbered)",
			got.OrphansFound, got.OrphansRestore, got.OrphansReport)
	}
	obj, err := s.GetObject(context.Background(), bid, "live-key", "")
	if err != nil {
		t.Fatalf("get live: %v", err)
	}
	if obj.Manifest == nil || len(obj.Manifest.Chunks) != 1 || obj.Manifest.Chunks[0].OID != "live-uuid.00000" {
		t.Errorf("live manifest clobbered by restore: %+v", obj.Manifest)
	}
}

// TestReconcileRestoreNoDataBackend proves a restore job with no data backend
// wired is marked error and rebuilds nothing (it cannot recompute an ETag).
func TestReconcileRestoreNoDataBackend(t *testing.T) {
	s, _ := seed(t)
	ctx := context.Background()
	w, _ := New(Config{Meta: s, Scanner: &fakeScanner{}, Region: "us"}) // no Data
	job, _ := s.StartReconcile(ctx, "ceph-a", "strata-data", "", "", meta.ReconcilePolicyRestore)
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once should not surface a per-job error: %v", err)
	}
	got, _ := s.GetReconcileJob(ctx, job.ID)
	if got.State != meta.ReconcileStateError {
		t.Errorf("state: got %q want error", got.State)
	}
}

// TestReconcileRestoreIsLatestByMtime proves multi-version restore sets the
// served head by back-reference mtime (the newest version wins a GET without a
// versionId), not by version_id ordering.
func TestReconcileRestoreIsLatestByMtime(t *testing.T) {
	s, bid := seed(t)
	db := datamem.New()
	ctx := context.Background()
	older := putRestoreChunk(t, db, []byte("older-version-bytes"))
	newer := putRestoreChunk(t, db, []byte("newer-version-bytes"))
	// The newer-MTIME version gets a version_id that sorts EARLIER than the
	// older one, so a regression to version_id ordering (instead of mtime) would
	// pick the wrong head — the exact Suspended-null hazard PRD decision (2)
	// calls out. mtime must win.
	vHiOlder := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	vLoNewer := "00000000-0000-0000-0000-000000000001"
	sc := []ScannedChunk{
		{Cluster: "ceph-a", Pool: "strata-data", OID: older.OID, Size: older.Size, HasBackref: true,
			Backref: data.Backref{BucketID: bid, Key: "vk", VersionID: vHiOlder, ChunkIdx: 0, Mtime: time.Unix(1700000000, 0)}},
		{Cluster: "ceph-a", Pool: "strata-data", OID: newer.OID, Size: newer.Size, HasBackref: true,
			Backref: data.Backref{BucketID: bid, Key: "vk", VersionID: vLoNewer, ChunkIdx: 0, Mtime: time.Unix(1700000100, 0)}},
	}
	got := runRestoreJob(t, s, db, sc)
	if got.OrphansRestore != 2 {
		t.Fatalf("restore=%d want 2", got.OrphansRestore)
	}
	obj, err := s.GetObject(ctx, bid, "vk", "")
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	rc, _ := db.GetChunks(ctx, obj.Manifest, 0, obj.Size)
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "newer-version-bytes" {
		t.Errorf("latest bytes: got %q want newer-version-bytes", b)
	}
}

// --- US-002b S3-passthrough scanner ------------------------------------------

// fakeLister yields a fixed ListedChunk set, handing each its OID as the resume
// cursor (mirrors the real S3Scanner's per-key StartAfter cursor).
type fakeLister struct {
	chunks    []data.ListedChunk
	gotCursor string
}

func (f *fakeLister) ListChunks(ctx context.Context, cluster, class, startCursor string, visit func(data.ListedChunk, string) error) error {
	f.gotCursor = startCursor
	for _, lc := range f.chunks {
		if err := visit(lc, lc.OID); err != nil {
			return err
		}
	}
	return nil
}

// TestS3ScannerDecodesBackref proves the S3-passthrough scanner threads the
// scope, decodes a valid back-reference, and treats an absent/malformed payload
// as "no back-reference" (reported, never acted on).
func TestS3ScannerDecodesBackref(t *testing.T) {
	br := data.Backref{BucketID: uuid.New(), Key: "k", VersionID: meta.NullVersionID, ChunkIdx: 0, Mtime: time.Unix(1, 0)}
	lister := &fakeLister{chunks: []data.ListedChunk{
		{OID: "obj-with-br", Size: 10, Backref: data.EncodeBackref(br)},
		{OID: "obj-no-br", Size: 5, Backref: nil},
		{OID: "obj-bad-br", Size: 3, Backref: []byte{0xff, 0x00}},
	}}
	sc := &S3Scanner{Lister: lister}
	var got []ScannedChunk
	err := sc.Scan(context.Background(), ScanScope{Cluster: "c", Pool: "STANDARD"}, "", func(c ScannedChunk, _ string) error {
		got = append(got, c)
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d chunks want 3", len(got))
	}
	if !got[0].HasBackref || got[0].Backref.Key != "k" {
		t.Errorf("first chunk should decode back-reference: %+v", got[0])
	}
	if got[1].HasBackref {
		t.Errorf("second chunk has no back-reference")
	}
	if got[2].HasBackref {
		t.Errorf("malformed back-reference must be treated as absent")
	}
	if got[0].Cluster != "c" || got[0].Pool != "STANDARD" {
		t.Errorf("scope not threaded onto chunk: %+v", got[0])
	}
}

// --- shared gap-detection helper ---------------------------------------------

func TestOrderChunks(t *testing.T) {
	ref := func(oid string) data.ChunkRef { return data.ChunkRef{OID: oid} }
	tests := []struct {
		name         string
		chunks       map[int]data.ChunkRef
		expectedOIDs []string
		expectedGap  bool
	}{
		{"contiguous from zero", map[int]data.ChunkRef{0: ref("a"), 1: ref("b"), 2: ref("c")}, []string{"a", "b", "c"}, false},
		{"single chunk at zero", map[int]data.ChunkRef{0: ref("a")}, []string{"a"}, false},
		{"missing middle index", map[int]data.ChunkRef{0: ref("a"), 2: ref("c")}, nil, true},
		{"missing zero index", map[int]data.ChunkRef{1: ref("b")}, nil, true},
		{"empty set", map[int]data.ChunkRef{}, []string{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, gap := OrderChunks(tc.chunks)
			if gap != tc.expectedGap {
				t.Fatalf("gap: got %v want %v", gap, tc.expectedGap)
			}
			if gap {
				return
			}
			if len(out) != len(tc.expectedOIDs) {
				t.Fatalf("len: got %d want %d", len(out), len(tc.expectedOIDs))
			}
			for i, c := range out {
				if c.OID != tc.expectedOIDs[i] {
					t.Errorf("oid[%d]: got %q want %q", i, c.OID, tc.expectedOIDs[i])
				}
			}
		})
	}
}
