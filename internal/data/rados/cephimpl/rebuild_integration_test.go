//go:build integration

package cephimpl_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/cephimpl"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/data/rados"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/rebuild"
	"github.com/danchupin/strata/internal/reconcile"
)

// rebuildFakeMeta is a minimal meta.Store the rebuild engine drives: it only
// ever calls GetObject (the bucket probe + the clobber probe) and PutObject.
// A real backend (memory/cassandra) would drag gocql into the cephimpl module's
// go.mod and break the go-ceph/main-module dependency split, so the test embeds
// the interface (nil for every unused method) and implements just the two the
// engine touches. The bucket is treated as always present (ErrObjectNotFound,
// never ErrBucketNotFound) — the lost-meta-rows-but-bucket-recreated case.
type rebuildFakeMeta struct {
	meta.Store
	objects map[string]*meta.Object // (key\x00versionID) -> object
}

func newRebuildFakeMeta() *rebuildFakeMeta {
	return &rebuildFakeMeta{objects: map[string]*meta.Object{}}
}

func (m *rebuildFakeMeta) GetObject(_ context.Context, _ uuid.UUID, key, versionID string) (*meta.Object, error) {
	if key == "" {
		// Bucket-existence probe: bucket present, key absent.
		return nil, meta.ErrObjectNotFound
	}
	if versionID == "" {
		for _, o := range m.objects {
			if o.Key == key && o.IsLatest {
				return o, nil
			}
		}
		return nil, meta.ErrObjectNotFound
	}
	if o, ok := m.objects[key+"\x00"+versionID]; ok {
		return o, nil
	}
	return nil, meta.ErrObjectNotFound
}

func (m *rebuildFakeMeta) PutObject(_ context.Context, o *meta.Object, _ bool) error {
	m.objects[o.Key+"\x00"+o.VersionID] = o
	return nil
}

// TestRebuildIndexFromRADOS proves the US-004b end-to-end path against a live
// Ceph cluster: the rebuild ENGINE driving the real RADOSScanner (which now
// surfaces per-chunk Size via an inline Stat) reconstructs manifest rows from a
// data-tier scan with NO meta rows present (the lost-meta case). It exercises
// all four dispositions the story names:
//
//	plaintext, two versions — both rebuilt, GET-able with the exact bytes and
//	                          size, the later-mtime version marked IsLatest.
//	SSE-labelled            — reported unrecoverable, NEVER written (the wrapped
//	                          DEK was in the lost meta; the back-reference carries
//	                          the algorithm LABEL stamped at PUT, US-004b).
//	gapped (middle chunk
//	removed from the pool)  — flagged gapped, NEVER stitched short + served.
//
// The discriminating proof for the size probe: with Size=0 (the pre-US-004b
// behaviour) readChunk would range-read zero bytes and the rebuilt ETag would
// be md5("") for every object — the byte/size/ETag assertions below would fail.
// Skipped when no cluster is reachable (matches TestRADOSBackend's guard).
func TestRebuildIndexFromRADOS(t *testing.T) {
	confPath := os.Getenv("STRATA_TEST_CEPH_CONF")
	if confPath == "" {
		confPath = "/etc/ceph/ceph.conf"
	}
	if _, err := os.Stat(confPath); err != nil {
		t.Skipf("ceph config not reachable at %s: %v", confPath, err)
	}
	pool := os.Getenv("STRATA_TEST_CEPH_POOL")
	if pool == "" {
		pool = "strata.rgw.buckets.data"
	}
	classesEnv := os.Getenv("STRATA_TEST_CEPH_CLASSES")
	if classesEnv == "" {
		classesEnv = "STANDARD=" + pool
	}
	classes, err := rados.ParseClasses(classesEnv)
	if err != nil {
		t.Fatalf("parse classes %q: %v", classesEnv, err)
	}
	user := envOr("STRATA_TEST_CEPH_USER", "admin")

	t.Setenv("STRATA_CHUNK_BACKREF", "true")
	be, err := cephimpl.New(rados.Config{ConfigFile: confPath, User: user, Pool: pool, Classes: classes})
	if err != nil {
		t.Skipf("cannot connect to ceph (probably no cluster running): %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	ctx := context.Background()
	bid := uuid.New()

	// putObject seeds one version of a key and returns its plaintext bytes +
	// manifest so the test can compare the rebuilt ETag/size and clean up.
	putObject := func(key, sseAlgo string, mtime time.Time, size int) ([]byte, *data.Manifest) {
		ver := meta.NewVersionID()
		body := make([]byte, size)
		if _, rerr := rand.Read(body); rerr != nil {
			t.Fatal(rerr)
		}
		putCtx := data.WithBackref(ctx, data.BackrefAttrs{
			BucketID:  bid,
			Key:       key,
			VersionID: ver,
			Mtime:     mtime,
			SSEAlgo:   sseAlgo,
		})
		m, perr := be.PutChunks(putCtx, bytes.NewReader(body), "STANDARD")
		if perr != nil {
			t.Fatalf("PutChunks %s: %v", key, perr)
		}
		t.Cleanup(func() { _ = be.Delete(context.Background(), m) })
		return body, m
	}

	const (
		plainKey = "rebuild/plain/object"
		sseKey   = "rebuild/sse/object"
		gapKey   = "rebuild/gapped/object"
	)
	base := time.Unix(1717100000, 0).UTC()

	// Plaintext key, two versions: v2 mtime later → must become IsLatest.
	bodyV1, _ := putObject(plainKey, "", base, 5<<20)                // 2 chunks
	bodyV2, _ := putObject(plainKey, "", base.Add(time.Hour), 9<<20) // 3 chunks

	// SSE-labelled object: bytes are seeded plaintext, but the back-reference
	// carries the AES256 algorithm label → rebuild must report unrecoverable
	// purely off the label (it has no key to decrypt with).
	_, _ = putObject(sseKey, data.SSEAlgorithmAES256, base, 4<<20)

	// Gapped object: 3 chunks, then remove the MIDDLE chunk from the pool.
	_, gapM := putObject(gapKey, "", base, 9<<20)
	if len(gapM.Chunks) != 3 {
		t.Fatalf("gapped seed: want 3 chunks, got %d", len(gapM.Chunks))
	}
	if derr := be.Delete(ctx, &data.Manifest{Chunks: gapM.Chunks[1:2]}); derr != nil {
		t.Fatalf("delete middle chunk: %v", derr)
	}

	// Lost-meta case: the bucket row exists, but NO object rows. rebuild must
	// fill them from the data scan alone.
	mem := newRebuildFakeMeta()

	rb, err := rebuild.New(rebuild.Config{
		Meta:         mem,
		Data:         be,
		Scanner:      &reconcile.RADOSScanner{Backend: be},
		BucketFilter: bid,
	})
	if err != nil {
		t.Fatalf("rebuild.New: %v", err)
	}
	stats, err := rb.Run(ctx, reconcile.ScanScope{Cluster: rados.DefaultCluster, Pool: pool})
	if err != nil {
		t.Fatalf("rebuild Run: %v", err)
	}

	// --- plaintext: both versions rebuilt, exact bytes/size, v2 IsLatest. ---
	assertRebuilt := func(versionID string, want []byte, wantLatest bool) {
		o, gerr := mem.GetObject(ctx, bid, plainKey, versionID)
		if gerr != nil {
			t.Fatalf("GetObject %s/%s: %v", plainKey, versionID, gerr)
		}
		if o.Size != int64(len(want)) {
			t.Errorf("%s size: want %d, got %d", versionID, len(want), o.Size)
		}
		h := md5.New()
		h.Write(want)
		wantETag := hex.EncodeToString(h.Sum(nil))
		if o.ETag != wantETag {
			t.Errorf("%s etag: want %s, got %s", versionID, wantETag, o.ETag)
		}
		if o.IsLatest != wantLatest {
			t.Errorf("%s IsLatest: want %v, got %v", versionID, wantLatest, o.IsLatest)
		}
		// Bytes must read back identical through the rebuilt manifest.
		rc, rerr := be.GetChunks(ctx, o.Manifest, 0, o.Size)
		if rerr != nil {
			t.Fatalf("GetChunks %s: %v", versionID, rerr)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !bytes.Equal(got, want) {
			t.Errorf("%s bytes mismatch: got %d bytes, want %d", versionID, len(got), len(want))
		}
	}
	// Identify the two version IDs from the rebuilt reports.
	var v1ID, v2ID string
	for _, rep := range stats.Reports {
		if rep.Key != plainKey {
			continue
		}
		if rep.Status != rebuild.StatusRebuilt {
			t.Errorf("plain version %s: status %s, want %s", rep.VersionID, rep.Status, rebuild.StatusRebuilt)
		}
		if rep.Size == int64(len(bodyV1)) {
			v1ID = rep.VersionID
		}
		if rep.Size == int64(len(bodyV2)) {
			v2ID = rep.VersionID
		}
	}
	if v1ID == "" || v2ID == "" {
		t.Fatalf("did not find both rebuilt plain versions in reports: %+v", stats.Reports)
	}
	assertRebuilt(v1ID, bodyV1, false)
	assertRebuilt(v2ID, bodyV2, true)

	// --- SSE: no row written, reported unrecoverable. ---
	if _, gerr := mem.GetObject(ctx, bid, sseKey, ""); gerr == nil {
		t.Errorf("SSE object %s was written — must stay unrecoverable", sseKey)
	}
	if !hasReport(stats.Reports, sseKey, rebuild.StatusUnrecoverableSSE) {
		t.Errorf("SSE object %s missing %s report; reports=%+v", sseKey, rebuild.StatusUnrecoverableSSE, stats.Reports)
	}

	// --- gapped: no row written, reported gapped (never stitched short). ---
	if _, gerr := mem.GetObject(ctx, bid, gapKey, ""); gerr == nil {
		t.Errorf("gapped object %s was written — must not be served as whole", gapKey)
	}
	if !hasReport(stats.Reports, gapKey, rebuild.StatusGapped) {
		t.Errorf("gapped object %s missing %s report; reports=%+v", gapKey, rebuild.StatusGapped, stats.Reports)
	}

	if stats.Rebuilt != 2 {
		t.Errorf("Rebuilt count: want 2 (both plain versions), got %d", stats.Rebuilt)
	}
}

func hasReport(reports []rebuild.ObjectReport, key, status string) bool {
	for _, r := range reports {
		if r.Key == key && r.Status == status {
			return true
		}
	}
	return false
}
