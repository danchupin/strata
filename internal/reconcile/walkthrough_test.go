package reconcile_test

// US-007 metadata-data-reconcile: the single end-to-end exercise that proves
// the WHOLE feature works together — both skew directions, both reconcile
// passes, every orphan policy, and the last-resort rebuild — in one narrative
// against the memory backends (the parity oracle; the RADOS legs are exercised
// by scripts/smoke-metadata-data-reconcile.sh under integration).
//
// This file lives in the EXTERNAL test package (reconcile_test) on purpose: it
// drives BOTH the reconcile worker (internal/reconcile) AND the rebuild engine
// (internal/rebuild, which imports reconcile). An external test package is the
// supported way to depend on a package that imports the package under test
// without an import cycle.
//
// The two cycle-promises it pins, neither possible before this cycle:
//   - NO SILENT LEAK: an orphan chunk (data-older-than-meta) is now visible to
//     reconcile (GC walks meta→data and could never see it) and is counted /
//     resolvable by policy.
//   - NO SILENT CORRUPT GET: a dangling manifest (meta-older-than-data) is
//     detected and the object is quarantined, so a GET returns a clear error
//     instead of a corrupt/short body.

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/reconcile"
	"github.com/danchupin/strata/internal/rebuild"
)

// walkScanner yields a fixed pool snapshot, handing each chunk a decimal
// resume cursor so the worker's checkpoint plumbing runs without a real RADOS
// pool. It implements reconcile.ChunkScanner.
type walkScanner struct{ chunks []reconcile.ScannedChunk }

func (w *walkScanner) Scan(_ context.Context, _ reconcile.ScanScope, _ string, visit func(reconcile.ScannedChunk, string) error) error {
	for i, c := range w.chunks {
		if err := visit(c, strconv.Itoa(i+1)); err != nil {
			return err
		}
	}
	return nil
}

// walkProber answers ChunkExists from a fixed present-OID set — a manifest OID
// not in the set is the dangling-manifest condition. Implements
// reconcile.ChunkProber.
type walkProber struct{ present map[string]bool }

func (w *walkProber) ChunkExists(_ context.Context, ref data.ChunkRef) (bool, error) {
	return w.present[ref.OID], nil
}

// putChunk writes a small body (one chunk under DefaultChunkSize) and returns
// the resulting single chunk ref.
func putChunk(t *testing.T, db *datamem.Backend, body string) data.ChunkRef {
	t.Helper()
	m, err := db.PutChunks(context.Background(), strings.NewReader(body), "STANDARD")
	if err != nil {
		t.Fatalf("PutChunks(%q): %v", body, err)
	}
	if len(m.Chunks) != 1 {
		t.Fatalf("expected one chunk for %q, got %d", body, len(m.Chunks))
	}
	return m.Chunks[0]
}

// runReconcile queues + drains one orphan-pass job and returns the terminal
// job row.
func runReconcile(t *testing.T, s *metamem.Store, db *datamem.Backend, scanner reconcile.ChunkScanner, policy string) *meta.ReconcileJob {
	t.Helper()
	ctx := context.Background()
	w, err := reconcile.New(reconcile.Config{Meta: s, Scanner: scanner, Data: db, Region: "us"})
	if err != nil {
		t.Fatalf("new reconcile worker: %v", err)
	}
	job, err := s.StartReconcile(ctx, "ceph-a", "strata-data", "", "", policy)
	if err != nil {
		t.Fatalf("StartReconcile(%s): %v", policy, err)
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, err := s.GetReconcileJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetReconcileJob: %v", err)
	}
	return got
}

// runDangling queues + drains one dangling-pass job for a bucket.
func runDangling(t *testing.T, s *metamem.Store, prober reconcile.ChunkProber, bucketID uuid.UUID, policy string) *meta.ReconcileJob {
	t.Helper()
	ctx := context.Background()
	w, err := reconcile.New(reconcile.Config{Meta: s, Prober: prober, Region: "us"})
	if err != nil {
		t.Fatalf("new dangling worker: %v", err)
	}
	job, err := s.StartReconcile(ctx, "", "", "", bucketID.String(), policy)
	if err != nil {
		t.Fatalf("StartReconcile(dangling %s): %v", policy, err)
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce dangling: %v", err)
	}
	got, err := s.GetReconcileJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetReconcileJob: %v", err)
	}
	return got
}

func TestEndToEndReconcileWalkthrough(t *testing.T) {
	ctx := context.Background()
	s := metamem.New()
	db := datamem.New()

	b, err := s.CreateBucket(ctx, "walk", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// --- Seed one healthy object the whole walkthrough must never disturb. ---
	keepRef := putChunk(t, db, "keep-me-intact")
	keepManifest := &data.Manifest{Class: "STANDARD", Size: keepRef.Size, Chunks: []data.ChunkRef{keepRef}}
	keep := &meta.Object{
		BucketID: b.ID, Key: "keep", StorageClass: "STANDARD",
		ETag: "keep-etag", Size: keepRef.Size, Mtime: time.Unix(1700000000, 0).UTC(),
		Manifest: keepManifest,
	}
	if err := s.PutObject(ctx, keep, false); err != nil {
		t.Fatalf("seed keep: %v", err)
	}
	healthyScan := reconcile.ScannedChunk{
		Cluster: keepRef.Cluster, Pool: keepRef.Pool, OID: keepRef.OID, Size: keepRef.Size,
		HasBackref: true,
		Backref:    data.Backref{BucketID: b.ID, Key: "keep", VersionID: meta.NullVersionID, ChunkIdx: 0, Mtime: time.Unix(1700000000, 0)},
	}

	// =====================================================================
	// Skew A — data-older-than-meta: an ORPHAN chunk in the pool whose owner
	// has no manifest. Before this cycle GC (meta→data) could never see it;
	// the bytes leaked forever. Now reconcile attributes it via the
	// back-reference and surfaces it.
	// =====================================================================
	orphanScan := reconcile.ScannedChunk{
		Cluster: "ceph-a", Pool: "strata-data", OID: "ghost-uuid.00000", Size: 9,
		HasBackref: true,
		Backref:    data.Backref{BucketID: b.ID, Key: "ghost", VersionID: meta.NullVersionID, ChunkIdx: 0, Mtime: time.Unix(1700000001, 0)},
	}

	// report (DEFAULT): orphan VISIBLE, NOTHING deleted — the safe first pass.
	rep := runReconcile(t, s, db, &walkScanner{chunks: []reconcile.ScannedChunk{healthyScan, orphanScan}}, meta.ReconcilePolicyReport)
	if rep.State != meta.ReconcileStateDone {
		t.Fatalf("report state: got %q want done", rep.State)
	}
	if rep.OrphansFound != 1 || rep.OrphansReport != 1 || rep.OrphansGC != 0 {
		t.Fatalf("report: found=%d report=%d gc=%d want 1/1/0", rep.OrphansFound, rep.OrphansReport, rep.OrphansGC)
	}
	if n, _ := s.ListChunkDeletionsByCluster(ctx, "us", "ceph-a", 100); n != 0 {
		t.Fatalf("report must never enqueue a deletion, gc depth=%d", n)
	}

	// gc: the orphan is enqueued for deletion; the healthy chunk is untouched.
	gc := runReconcile(t, s, db, &walkScanner{chunks: []reconcile.ScannedChunk{healthyScan, orphanScan}}, meta.ReconcilePolicyGC)
	if gc.OrphansGC != 1 {
		t.Fatalf("gc: orphans_gc=%d want 1", gc.OrphansGC)
	}
	if n, _ := s.ListChunkDeletionsByCluster(ctx, "us", "ceph-a", 100); n != 1 {
		t.Fatalf("gc must enqueue exactly the orphan, gc depth=%d want 1", n)
	}

	// =====================================================================
	// Skew B — meta-older-than-data: a real chunk in the pool whose manifest
	// row is GENUINELY ABSENT (a stale meta restore dropped it). restore
	// rebuilds the manifest from the back-reference so the object is GET-able
	// again with correct bytes + recomputed ETag.
	// =====================================================================
	lostBody := "restored-object-payload"
	lostRef := putChunk(t, db, lostBody)
	restoreScan := reconcile.ScannedChunk{
		Cluster: lostRef.Cluster, Pool: lostRef.Pool, OID: lostRef.OID, Size: lostRef.Size,
		HasBackref: true,
		Backref:    data.Backref{BucketID: b.ID, Key: "lost", VersionID: meta.NullVersionID, ChunkIdx: 0, Mtime: time.Unix(1700000002, 0)},
	}
	res := runReconcile(t, s, db, &walkScanner{chunks: []reconcile.ScannedChunk{restoreScan}}, meta.ReconcilePolicyRestore)
	if res.OrphansFound != 1 || res.OrphansRestore != 1 {
		t.Fatalf("restore: found=%d restore=%d want 1/1", res.OrphansFound, res.OrphansRestore)
	}
	lost, err := s.GetObject(ctx, b.ID, "lost", "")
	if err != nil {
		t.Fatalf("restored object not GET-able: %v", err)
	}
	sum := md5.Sum([]byte(lostBody))
	if lost.ETag != hex.EncodeToString(sum[:]) {
		t.Fatalf("restored ETag: got %q want recomputed md5", lost.ETag)
	}
	if gotBytes := mustReadAll(t, db, lost.Manifest); gotBytes != lostBody {
		t.Fatalf("restored bytes: got %q want %q", gotBytes, lostBody)
	}

	// =====================================================================
	// Dangling pass — meta→data: a manifest that points at a chunk the data
	// tier no longer has. Before this cycle a client GET surfaced a corrupt /
	// short body (silent). Now it is detected + quarantined so a GET returns a
	// clear ObjectQuarantined error, never a silent corrupt read.
	// =====================================================================
	brokenManifest := &data.Manifest{Class: "STANDARD", Size: 4, Chunks: []data.ChunkRef{
		{Cluster: "ceph-a", Pool: "strata-data", OID: "missing-uuid.00000", Size: 4},
	}}
	broken := &meta.Object{
		BucketID: b.ID, Key: "broken", StorageClass: "STANDARD",
		ETag: "broken-etag", Size: 4, Mtime: time.Unix(1700000003, 0).UTC(),
		Manifest: brokenManifest,
	}
	if err := s.PutObject(ctx, broken, false); err != nil {
		t.Fatalf("seed broken: %v", err)
	}
	// The prober holds every chunk the pool actually has — keep + the restored
	// lost object — but NOT broken's missing chunk.
	prober := &walkProber{present: map[string]bool{keepRef.OID: true, lostRef.OID: true}}

	// report (DEFAULT): dangling counted, NOTHING quarantined.
	drep := runDangling(t, s, prober, b.ID, meta.ReconcilePolicyReport)
	if drep.DanglingFound != 1 || drep.DanglingReport != 1 || drep.DanglingQuarantine != 0 {
		t.Fatalf("dangling report: found=%d report=%d quarantine=%d want 1/1/0",
			drep.DanglingFound, drep.DanglingReport, drep.DanglingQuarantine)
	}
	if obj, _ := s.GetObject(ctx, b.ID, "broken", ""); obj.QuarantineReason != "" {
		t.Fatalf("report policy must not quarantine: %q", obj.QuarantineReason)
	}

	// quarantine: the broken object is flagged; the healthy objects are not.
	dq := runDangling(t, s, prober, b.ID, meta.ReconcilePolicyQuarantine)
	if dq.DanglingFound != 1 || dq.DanglingQuarantine != 1 {
		t.Fatalf("dangling quarantine: found=%d quarantine=%d want 1/1", dq.DanglingFound, dq.DanglingQuarantine)
	}
	if dq.Healthy != 2 {
		t.Fatalf("dangling healthy: got %d want 2 (keep + restored lost)", dq.Healthy)
	}
	brokenAfter, err := s.GetObject(ctx, b.ID, "broken", "")
	if err != nil {
		t.Fatalf("get broken after quarantine: %v", err)
	}
	if brokenAfter.QuarantineReason == "" {
		t.Fatal("broken object must be quarantined (no silent corrupt GET)")
	}
	for _, k := range []string{"keep", "lost"} {
		o, err := s.GetObject(ctx, b.ID, k, "")
		if err != nil {
			t.Fatalf("get %s: %v", k, err)
		}
		if o.QuarantineReason != "" {
			t.Fatalf("healthy %s wrongly quarantined: %q", k, o.QuarantineReason)
		}
	}

	// =====================================================================
	// Last resort — rebuild-index: the meta backup itself is unusable. The
	// engine reconstructs manifest rows from a pure data-tier scan: a
	// multi-chunk plaintext version recovers fully (correct bytes/size/ETag),
	// a gap is flagged (never stitched short + served), an SSE object is
	// reported unrecoverable (the wrapped DEK was in the lost meta).
	// =====================================================================
	rb, err := s.CreateBucket(ctx, "rebuilt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket rebuilt: %v", err)
	}
	// Two-chunk plaintext version "doc" (idx 0 + 1) — full recovery.
	part0 := putChunk(t, db, "first-half-")
	part1 := putChunk(t, db, "second-half")
	docMtime := time.Unix(1700001000, 0)
	docChunks := []reconcile.ScannedChunk{
		{Cluster: part0.Cluster, Pool: part0.Pool, OID: part0.OID, Size: part0.Size, HasBackref: true,
			Backref: data.Backref{BucketID: rb.ID, Key: "doc", VersionID: meta.NullVersionID, ChunkIdx: 0, Mtime: docMtime}},
		{Cluster: part1.Cluster, Pool: part1.Pool, OID: part1.OID, Size: part1.Size, HasBackref: true,
			Backref: data.Backref{BucketID: rb.ID, Key: "doc", VersionID: meta.NullVersionID, ChunkIdx: 1, Mtime: docMtime}},
	}
	// Gapped version "gappy": only chunk_idx 1 present -> idx 0 missing.
	gapRef := putChunk(t, db, "lonely-tail")
	gapChunk := reconcile.ScannedChunk{
		Cluster: gapRef.Cluster, Pool: gapRef.Pool, OID: gapRef.OID, Size: gapRef.Size, HasBackref: true,
		Backref: data.Backref{BucketID: rb.ID, Key: "gappy", VersionID: meta.NullVersionID, ChunkIdx: 1, Mtime: time.Unix(1700001001, 0)},
	}
	// SSE version "secret": back-reference carries an algo label -> unrecoverable.
	sseRef := putChunk(t, db, "ciphertext-bytes")
	sseChunk := reconcile.ScannedChunk{
		Cluster: sseRef.Cluster, Pool: sseRef.Pool, OID: sseRef.OID, Size: sseRef.Size, HasBackref: true,
		Backref: data.Backref{BucketID: rb.ID, Key: "secret", VersionID: meta.NullVersionID, ChunkIdx: 0, Mtime: time.Unix(1700001002, 0), SSEAlgo: "AES256"},
	}

	allRebuildChunks := append(append([]reconcile.ScannedChunk{}, docChunks...), gapChunk, sseChunk)
	r, err := rebuild.New(rebuild.Config{
		Meta:    s,
		Data:    db,
		Scanner: &walkScanner{chunks: allRebuildChunks},
	})
	if err != nil {
		t.Fatalf("rebuild.New: %v", err)
	}
	stats, err := r.Run(ctx, reconcile.ScanScope{Cluster: "mem", Pool: "mem"})
	if err != nil {
		t.Fatalf("rebuild Run: %v", err)
	}
	if stats.Rebuilt != 1 {
		t.Fatalf("rebuilt: got %d want 1 (the contiguous doc)", stats.Rebuilt)
	}
	if stats.Gapped != 1 {
		t.Fatalf("gapped: got %d want 1 (gappy reported, never stitched)", stats.Gapped)
	}
	if stats.Unrecoverable != 1 {
		t.Fatalf("unrecoverable: got %d want 1 (SSE secret)", stats.Unrecoverable)
	}

	// The recovered doc is GET-able with the correct concatenated bytes + ETag.
	doc, err := s.GetObject(ctx, rb.ID, "doc", "")
	if err != nil {
		t.Fatalf("rebuilt doc not GET-able: %v", err)
	}
	wantBytes := "first-half-second-half"
	if got := mustReadAll(t, db, doc.Manifest); got != wantBytes {
		t.Fatalf("rebuilt doc bytes: got %q want %q", got, wantBytes)
	}
	docSum := md5.Sum([]byte(wantBytes))
	if doc.ETag != hex.EncodeToString(docSum[:]) {
		t.Fatalf("rebuilt doc ETag: got %q want recomputed md5 over both chunks", doc.ETag)
	}
	if doc.Size != int64(len(wantBytes)) {
		t.Fatalf("rebuilt doc size: got %d want %d", doc.Size, len(wantBytes))
	}

	// The gapped + SSE versions were NEVER written — a corrupt short body must
	// not be silently served from a partial recovery.
	if _, err := s.GetObject(ctx, rb.ID, "gappy", ""); !errors.Is(err, meta.ErrObjectNotFound) {
		t.Fatalf("gapped object must not be rebuilt: err=%v", err)
	}
	if _, err := s.GetObject(ctx, rb.ID, "secret", ""); !errors.Is(err, meta.ErrObjectNotFound) {
		t.Fatalf("SSE object must not be rebuilt: err=%v", err)
	}
}

func mustReadAll(t *testing.T, db *datamem.Backend, m *data.Manifest) string {
	t.Helper()
	rc, err := db.GetChunks(context.Background(), m, 0, m.Size)
	if err != nil {
		t.Fatalf("GetChunks: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(got)
}
