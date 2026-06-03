package rebuild

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/reconcile"
)

// fakeScanner yields a fixed list of ScannedChunk, decoupling the engine's
// classify/group/write logic from any real pool walk so the red/green proof
// runs CI-green on the memory backend (no RADOS).
type fakeScanner struct {
	chunks []reconcile.ScannedChunk
}

func (f *fakeScanner) Scan(ctx context.Context, _ reconcile.ScanScope, _ string, visit func(reconcile.ScannedChunk, string) error) error {
	for i, c := range f.chunks {
		if err := visit(c, ""); err != nil {
			return err
		}
		_ = i
	}
	return nil
}

// seedObject writes body into the memory data backend and returns the single
// chunk (small bodies stay under DefaultChunkSize so each object is one chunk)
// plus the manifest ETag for assertion. The meta row is deliberately NOT
// written — that is the "lost meta backup" state rebuild repairs.
func seedObject(t *testing.T, d *datamem.Backend, body string) (reconcile.ScannedChunk, string) {
	t.Helper()
	m, err := d.PutChunks(context.Background(), strings.NewReader(body), "STANDARD")
	if err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	if len(m.Chunks) != 1 {
		t.Fatalf("seedObject expects a single chunk, got %d (body too large?)", len(m.Chunks))
	}
	c := m.Chunks[0]
	return reconcile.ScannedChunk{
		Cluster: c.Cluster,
		Pool:    c.Pool,
		OID:     c.OID,
		Size:    c.Size,
	}, m.ETag
}

func mustGetBytes(t *testing.T, d data.Backend, m *data.Manifest) string {
	t.Helper()
	rc, err := d.GetChunks(context.Background(), m, 0, m.Size)
	if err != nil {
		t.Fatalf("GetChunks: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(b)
}

func newRebuilder(t *testing.T, m meta.Store, d data.Backend, chunks []reconcile.ScannedChunk, force, dry bool) *Rebuilder {
	t.Helper()
	r, err := New(Config{
		Meta:    m,
		Data:    d,
		Scanner: &fakeScanner{chunks: chunks},
		Force:   force,
		DryRun:  dry,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// TestRebuildVersionedObjectFromDataScan is the US-004 red/green core: a bucket
// whose manifest rows are gone is reconstructed from a data-tier scan — every
// version GET-able again with correct bytes + ETag, IsLatest correct across
// versions by back-reference mtime.
func TestRebuildVersionedObjectFromDataScan(t *testing.T) {
	ctx := context.Background()
	d := datamem.New()
	m := metamem.New()
	b, err := m.CreateBucket(ctx, "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	v1ID := uuid.NewString()
	v2ID := uuid.NewString()
	t1 := time.Unix(1_717_000_000, 0).UTC()
	t2 := t1.Add(time.Hour) // v2 is newer

	c1, etag1 := seedObject(t, d, "the-first-version-body")
	c1.HasBackref = true
	c1.Backref = data.Backref{BucketID: b.ID, Key: "doc", VersionID: v1ID, ChunkIdx: 0, Mtime: t1}

	c2, etag2 := seedObject(t, d, "the-second-and-newer-version-body!!")
	c2.HasBackref = true
	c2.Backref = data.Backref{BucketID: b.ID, Key: "doc", VersionID: v2ID, ChunkIdx: 0, Mtime: t2}

	r := newRebuilder(t, m, d, []reconcile.ScannedChunk{c1, c2}, false, false)
	stats, err := r.Run(ctx, reconcile.ScanScope{Cluster: "mem", Pool: "mem"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Rebuilt != 2 {
		t.Fatalf("Rebuilt: want 2, got %d (reports: %+v)", stats.Rebuilt, stats.Reports)
	}

	// Latest (no version id) resolves to v2 by mtime.
	latest, err := m.GetObject(ctx, b.ID, "doc", "")
	if err != nil {
		t.Fatalf("GetObject latest: %v", err)
	}
	if latest.VersionID != v2ID {
		t.Fatalf("latest version: want %s (v2, higher mtime), got %s", v2ID, latest.VersionID)
	}
	if !latest.IsLatest {
		t.Fatal("rebuilt v2 must carry IsLatest=true on the row (ordered backends read this field)")
	}
	if latest.ETag != etag2 {
		t.Fatalf("latest ETag: want %s, got %s", etag2, latest.ETag)
	}
	if got := mustGetBytes(t, d, latest.Manifest); got != "the-second-and-newer-version-body!!" {
		t.Fatalf("latest bytes: got %q", got)
	}

	// Older version still addressable by id, with its own bytes + ETag.
	v1, err := m.GetObject(ctx, b.ID, "doc", v1ID)
	if err != nil {
		t.Fatalf("GetObject v1: %v", err)
	}
	if v1.ETag != etag1 {
		t.Fatalf("v1 ETag: want %s, got %s", etag1, v1.ETag)
	}
	if got := mustGetBytes(t, d, v1.Manifest); got != "the-first-version-body" {
		t.Fatalf("v1 bytes: got %q", got)
	}
	if v1.IsLatest {
		t.Fatal("v1 must not be IsLatest")
	}
}

// TestRebuildLatestFallsBackWhenNewestUnrecoverable exercises the latestIdx
// skip branch: when the max-mtime version is gapped/SSE (cannot be served),
// the newest RECOVERABLE version must inherit IsLatest so a GET-latest returns
// a valid object rather than nothing.
func TestRebuildLatestFallsBackWhenNewestUnrecoverable(t *testing.T) {
	ctx := context.Background()
	d := datamem.New()
	m := metamem.New()
	b, err := m.CreateBucket(ctx, "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	okID := uuid.NewString()
	sseID := uuid.NewString()
	tOk := time.Unix(1_717_000_000, 0).UTC()
	tNewer := tOk.Add(time.Hour) // the SSE version is newer but unrecoverable

	cOK, _ := seedObject(t, d, "the-recoverable-older-version")
	cOK.HasBackref = true
	cOK.Backref = data.Backref{BucketID: b.ID, Key: "doc", VersionID: okID, ChunkIdx: 0, Mtime: tOk}

	cSSE, _ := seedObject(t, d, "newer-but-encrypted")
	cSSE.HasBackref = true
	cSSE.Backref = data.Backref{BucketID: b.ID, Key: "doc", VersionID: sseID, ChunkIdx: 0, Mtime: tNewer, SSEAlgo: data.SSEAlgorithmKMS}

	stats, err := newRebuilder(t, m, d, []reconcile.ScannedChunk{cOK, cSSE}, false, false).Run(ctx, reconcile.ScanScope{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Rebuilt != 1 || stats.Unrecoverable != 1 {
		t.Fatalf("want Rebuilt=1 Unrecoverable=1, got Rebuilt=%d Unrecoverable=%d", stats.Rebuilt, stats.Unrecoverable)
	}
	latest, err := m.GetObject(ctx, b.ID, "doc", "")
	if err != nil {
		t.Fatalf("GetObject latest: %v (the recoverable version must be servable as latest)", err)
	}
	if latest.VersionID != okID || !latest.IsLatest {
		t.Fatalf("latest: want recoverable version %s with IsLatest=true, got %s IsLatest=%v", okID, latest.VersionID, latest.IsLatest)
	}
}

// TestRebuildSuspendedNullIsLatestByMtime is the PRD headline correctness
// case: a Suspended-mode null version with a LATER mtime than an older
// TimeUUID version must be served as latest — version_id order alone gets this
// wrong (the null sentinel sorts last), so rebuild must pick latest by the
// back-reference mtime.
func TestRebuildSuspendedNullIsLatestByMtime(t *testing.T) {
	ctx := context.Background()
	d := datamem.New()
	m := metamem.New()
	b, err := m.CreateBucket(ctx, "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	tuidID := uuid.NewString()
	tOld := time.Unix(1_717_000_000, 0).UTC()
	tNew := tOld.Add(time.Hour) // the null version is the newer write

	cTuid, _ := seedObject(t, d, "older-timeuuid-version")
	cTuid.HasBackref = true
	cTuid.Backref = data.Backref{BucketID: b.ID, Key: "doc", VersionID: tuidID, ChunkIdx: 0, Mtime: tOld}

	cNull, nullETag := seedObject(t, d, "newer-null-version-wins")
	cNull.HasBackref = true
	cNull.Backref = data.Backref{BucketID: b.ID, Key: "doc", VersionID: meta.NullVersionID, ChunkIdx: 0, Mtime: tNew}

	stats, err := newRebuilder(t, m, d, []reconcile.ScannedChunk{cTuid, cNull}, false, false).Run(ctx, reconcile.ScanScope{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Rebuilt != 2 {
		t.Fatalf("Rebuilt: want 2, got %d (%+v)", stats.Rebuilt, stats.Reports)
	}
	latest, err := m.GetObject(ctx, b.ID, "doc", "")
	if err != nil {
		t.Fatalf("GetObject latest: %v", err)
	}
	if latest.VersionID != meta.NullVersionID {
		t.Fatalf("latest: want null version (higher mtime), got %s", latest.VersionID)
	}
	if !latest.IsNull {
		t.Fatal("latest null version must carry IsNull=true")
	}
	if latest.ETag != nullETag {
		t.Fatalf("latest ETag: want %s, got %s", nullETag, latest.ETag)
	}
	if got := mustGetBytes(t, d, latest.Manifest); got != "newer-null-version-wins" {
		t.Fatalf("latest bytes: got %q", got)
	}
	// The older TimeUUID version is still addressable by id.
	if _, err := m.GetObject(ctx, b.ID, "doc", tuidID); err != nil {
		t.Fatalf("GetObject older timeuuid: %v", err)
	}
}

// TestRebuildSkipsGappedAndSSE proves the safety rails: a version missing a
// chunk_idx is flagged gapped (never stitched short), and an SSE object is
// reported unrecoverable (never served as plaintext). Neither writes a row.
func TestRebuildSkipsGappedAndSSE(t *testing.T) {
	ctx := context.Background()
	d := datamem.New()
	m := metamem.New()
	b, err := m.CreateBucket(ctx, "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	mtime := time.Unix(1_717_000_000, 0).UTC()

	// Gapped: chunk_idx {0, 2} — index 1 missing. Detected before any read,
	// so the OIDs need not exist in the data backend.
	gapped := []reconcile.ScannedChunk{
		{Cluster: "mem", Pool: "mem", OID: "gap.0000", Size: 10, HasBackref: true,
			Backref: data.Backref{BucketID: b.ID, Key: "gappy", VersionID: uuid.NewString(), ChunkIdx: 0, Mtime: mtime}},
		{Cluster: "mem", Pool: "mem", OID: "gap.0002", Size: 10, HasBackref: true,
			Backref: data.Backref{BucketID: b.ID, Key: "gappy", VersionID: "", ChunkIdx: 2, Mtime: mtime}},
	}
	// Fix: both gapped chunks must share the same version to form one group.
	gapped[1].Backref.VersionID = gapped[0].Backref.VersionID

	// SSE: a recoverable-looking single chunk but flagged AES256.
	sseChunk, _ := seedObject(t, d, "ciphertext-bytes")
	sseChunk.HasBackref = true
	sseChunk.Backref = data.Backref{BucketID: b.ID, Key: "secret", VersionID: uuid.NewString(), ChunkIdx: 0, Mtime: mtime, SSEAlgo: data.SSEAlgorithmAES256}

	chunks := append(gapped, sseChunk)
	r := newRebuilder(t, m, d, chunks, false, false)
	stats, err := r.Run(ctx, reconcile.ScanScope{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Gapped != 1 {
		t.Fatalf("Gapped: want 1, got %d", stats.Gapped)
	}
	if stats.Unrecoverable != 1 {
		t.Fatalf("Unrecoverable: want 1, got %d", stats.Unrecoverable)
	}
	if stats.Rebuilt != 0 {
		t.Fatalf("Rebuilt: want 0 (both rejected), got %d", stats.Rebuilt)
	}
	if _, err := m.GetObject(ctx, b.ID, "gappy", ""); err == nil {
		t.Fatal("gapped object must NOT be written")
	}
	if _, err := m.GetObject(ctx, b.ID, "secret", ""); err == nil {
		t.Fatal("SSE object must NOT be written")
	}
}

// TestRebuildIdempotentAndForce proves a re-run skips already-rebuilt rows
// (idempotent) and that --force overwrites them.
func TestRebuildIdempotentAndForce(t *testing.T) {
	ctx := context.Background()
	d := datamem.New()
	m := metamem.New()
	b, err := m.CreateBucket(ctx, "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	versionID := uuid.NewString()
	mtime := time.Unix(1_717_000_000, 0).UTC()
	c, _ := seedObject(t, d, "rebuild-me-once")
	c.HasBackref = true
	c.Backref = data.Backref{BucketID: b.ID, Key: "doc", VersionID: versionID, ChunkIdx: 0, Mtime: mtime}

	// First pass writes it.
	stats, err := newRebuilder(t, m, d, []reconcile.ScannedChunk{c}, false, false).Run(ctx, reconcile.ScanScope{})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if stats.Rebuilt != 1 {
		t.Fatalf("first pass Rebuilt: want 1, got %d", stats.Rebuilt)
	}

	// Second pass (no force) skips it.
	stats, err = newRebuilder(t, m, d, []reconcile.ScannedChunk{c}, false, false).Run(ctx, reconcile.ScanScope{})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if stats.SkippedExist != 1 || stats.Rebuilt != 0 {
		t.Fatalf("second pass: want SkippedExist=1 Rebuilt=0, got SkippedExist=%d Rebuilt=%d", stats.SkippedExist, stats.Rebuilt)
	}

	// Third pass with force overwrites it — prove the CONTENT is replaced, not
	// just the branch taken: seed fresh bytes for the SAME {bucket,key,version}
	// and assert the new ETag + bytes landed.
	c2, etag2 := seedObject(t, d, "rebuilt-with-different-and-longer-bytes")
	c2.HasBackref = true
	c2.Backref = data.Backref{BucketID: b.ID, Key: "doc", VersionID: versionID, ChunkIdx: 0, Mtime: mtime}
	stats, err = newRebuilder(t, m, d, []reconcile.ScannedChunk{c2}, true, false).Run(ctx, reconcile.ScanScope{})
	if err != nil {
		t.Fatalf("force Run: %v", err)
	}
	if stats.Rebuilt != 1 || stats.SkippedExist != 0 {
		t.Fatalf("force pass: want Rebuilt=1 SkippedExist=0, got Rebuilt=%d SkippedExist=%d", stats.Rebuilt, stats.SkippedExist)
	}
	got, err := m.GetObject(ctx, b.ID, "doc", versionID)
	if err != nil {
		t.Fatalf("GetObject after force: %v", err)
	}
	if got.ETag != etag2 {
		t.Fatalf("force overwrite ETag: want %s (new bytes), got %s", etag2, got.ETag)
	}
	if body := mustGetBytes(t, d, got.Manifest); body != "rebuilt-with-different-and-longer-bytes" {
		t.Fatalf("force overwrite bytes: got %q", body)
	}
}

// TestRebuildDryRunWritesNothing proves --dry-run classifies + reports without
// writing a row.
func TestRebuildDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	d := datamem.New()
	m := metamem.New()
	b, err := m.CreateBucket(ctx, "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	c, _ := seedObject(t, d, "dry-run-body")
	c.HasBackref = true
	c.Backref = data.Backref{BucketID: b.ID, Key: "doc", VersionID: uuid.NewString(), ChunkIdx: 0, Mtime: time.Unix(1_717_000_000, 0).UTC()}

	stats, err := newRebuilder(t, m, d, []reconcile.ScannedChunk{c}, false, true).Run(ctx, reconcile.ScanScope{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stats.Reports) != 1 || stats.Reports[0].Status != StatusWouldRebuild {
		t.Fatalf("dry-run report: want one would_rebuild, got %+v", stats.Reports)
	}
	if _, err := m.GetObject(ctx, b.ID, "doc", ""); err == nil {
		t.Fatal("dry-run must NOT write a row")
	}
}

// TestRebuildBucketFilterScopesToOneBucket proves --bucket-id restricts the
// rebuild to a single bucket: a pool holds chunks for two buckets, the filter
// names one, and only that bucket's rows land. A disaster-recovery run rebuilds
// exactly the requested bucket, never a sibling's manifest.
func TestRebuildBucketFilterScopesToOneBucket(t *testing.T) {
	ctx := context.Background()
	d := datamem.New()
	m := metamem.New()
	want, err := m.CreateBucket(ctx, "wanted", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket wanted: %v", err)
	}
	other, err := m.CreateBucket(ctx, "other", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket other: %v", err)
	}
	mtime := time.Unix(1_717_000_000, 0).UTC()

	cWant, _ := seedObject(t, d, "wanted-bucket-body")
	cWant.HasBackref = true
	cWant.Backref = data.Backref{BucketID: want.ID, Key: "doc", VersionID: uuid.NewString(), ChunkIdx: 0, Mtime: mtime}

	cOther, _ := seedObject(t, d, "other-bucket-body")
	cOther.HasBackref = true
	cOther.Backref = data.Backref{BucketID: other.ID, Key: "doc", VersionID: uuid.NewString(), ChunkIdx: 0, Mtime: mtime}

	r, err := New(Config{
		Meta:         m,
		Data:         d,
		Scanner:      &fakeScanner{chunks: []reconcile.ScannedChunk{cWant, cOther}},
		BucketFilter: want.ID,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stats, err := r.Run(ctx, reconcile.ScanScope{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Both chunks scanned, but only the wanted bucket's group is processed.
	if stats.ChunksScanned != 2 {
		t.Fatalf("ChunksScanned: want 2, got %d", stats.ChunksScanned)
	}
	if stats.GroupsSeen != 1 || stats.Rebuilt != 1 {
		t.Fatalf("want GroupsSeen=1 Rebuilt=1 (wanted bucket only), got GroupsSeen=%d Rebuilt=%d", stats.GroupsSeen, stats.Rebuilt)
	}
	// The out-of-scope chunk must NOT be counted as absent-backref (it has one).
	if stats.AbsentBackref != 0 {
		t.Fatalf("AbsentBackref: want 0 (out-of-scope chunk has a back-ref), got %d", stats.AbsentBackref)
	}
	if _, err := m.GetObject(ctx, want.ID, "doc", ""); err != nil {
		t.Fatalf("wanted bucket object must be rebuilt: %v", err)
	}
	if _, err := m.GetObject(ctx, other.ID, "doc", ""); err == nil {
		t.Fatal("other bucket must NOT be rebuilt under a bucket filter")
	}
}

// TestRebuildMissingBucketReportsActionableError proves the full-meta-loss
// case (the bucket row itself is gone) is reported with an actionable error
// instead of confusing per-object PutObject failures or a panic — rebuild
// cannot fabricate a bucket (owner/ACL/versioning are not in the back-ref).
func TestRebuildMissingBucketReportsActionableError(t *testing.T) {
	ctx := context.Background()
	d := datamem.New()
	m := metamem.New()
	// Deliberately do NOT CreateBucket — the bucket row is absent.
	bucketID := uuid.New()
	c, _ := seedObject(t, d, "bytes-without-a-bucket-row")
	c.HasBackref = true
	c.Backref = data.Backref{BucketID: bucketID, Key: "doc", VersionID: uuid.NewString(), ChunkIdx: 0, Mtime: time.Unix(1_717_000_000, 0).UTC()}

	stats, err := newRebuilder(t, m, d, []reconcile.ScannedChunk{c}, false, false).Run(ctx, reconcile.ScanScope{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Errors != 1 || stats.Rebuilt != 0 {
		t.Fatalf("missing-bucket: want Errors=1 Rebuilt=0, got Errors=%d Rebuilt=%d", stats.Errors, stats.Rebuilt)
	}
	if len(stats.Reports) != 1 || stats.Reports[0].Status != StatusError {
		t.Fatalf("missing-bucket report: want one StatusError, got %+v", stats.Reports)
	}
	if !strings.Contains(stats.Reports[0].Detail, "recreate the bucket") {
		t.Fatalf("missing-bucket detail should be actionable, got %q", stats.Reports[0].Detail)
	}
}

// TestRebuildAbsentBackrefNeverRebuilt proves a chunk with no back-reference
// is counted but never attributed/rebuilt (legacy / STRATA_CHUNK_BACKREF=off).
func TestRebuildAbsentBackrefNeverRebuilt(t *testing.T) {
	ctx := context.Background()
	d := datamem.New()
	m := metamem.New()
	if _, err := m.CreateBucket(ctx, "bkt", "owner", "STANDARD"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	c, _ := seedObject(t, d, "orphan-no-backref")
	c.HasBackref = false

	stats, err := newRebuilder(t, m, d, []reconcile.ScannedChunk{c}, false, false).Run(ctx, reconcile.ScanScope{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.AbsentBackref != 1 || stats.Rebuilt != 0 || stats.GroupsSeen != 0 {
		t.Fatalf("absent-backref: want AbsentBackref=1 Rebuilt=0 GroupsSeen=0, got %+v", stats)
	}
}
