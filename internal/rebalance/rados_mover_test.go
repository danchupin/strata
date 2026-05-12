package rebalance

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

// fakeRadosCluster is a librados-free RadosCluster implementation used
// by the unit tests. Keys are "pool/namespace/oid"; Read returns the
// raw bytes Write planted. Read/Write counters surface so tests can
// assert per-cluster traffic flowed in the expected direction.
type fakeRadosCluster struct {
	id       string
	mu       sync.Mutex
	store    map[string][]byte
	reads    int
	writes   int
	readErr  error
	writeErr error
}

func newFakeCluster(id string) *fakeRadosCluster {
	return &fakeRadosCluster{id: id, store: map[string][]byte{}}
}

func (f *fakeRadosCluster) ID() string { return f.id }

func (f *fakeRadosCluster) key(pool, ns, oid string) string {
	return pool + "/" + ns + "/" + oid
}

func (f *fakeRadosCluster) plant(pool, ns, oid string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]byte(nil), body...)
	f.store[f.key(pool, ns, oid)] = cp
}

func (f *fakeRadosCluster) Read(_ context.Context, pool, ns, oid string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.readErr != nil {
		return nil, f.readErr
	}
	body, ok := f.store[f.key(pool, ns, oid)]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), body...), nil
}

func (f *fakeRadosCluster) Write(_ context.Context, pool, ns, oid string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.writeErr != nil {
		return f.writeErr
	}
	cp := append([]byte(nil), body...)
	f.store[f.key(pool, ns, oid)] = cp
	return nil
}

// fakeMoverMetrics records every counter bump so tests can assert
// per-counter cardinality.
type fakeMoverMetrics struct {
	mu     sync.Mutex
	bytes  map[string]int64
	chunks map[string]int
	confs  map[string]int
}

func newFakeMetrics() *fakeMoverMetrics {
	return &fakeMoverMetrics{
		bytes:  map[string]int64{},
		chunks: map[string]int{},
		confs:  map[string]int{},
	}
}

func (f *fakeMoverMetrics) IncBytesMoved(from, to string, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bytes[from+"->"+to] += n
}
func (f *fakeMoverMetrics) IncChunksMoved(from, to, bucket string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chunks[from+"->"+to+":"+bucket]++
}
func (f *fakeMoverMetrics) IncCASConflict(bucket string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confs[bucket]++
}

func seedRadosObject(t *testing.T, m meta.Store, src *fakeRadosCluster, bucketID uuid.UUID, key string, bodies [][]byte) *meta.Object {
	t.Helper()
	chunks := make([]data.ChunkRef, len(bodies))
	for i, body := range bodies {
		oid := key + "/" + uuid.NewString()
		src.plant("strata.data", "", oid, body)
		chunks[i] = data.ChunkRef{
			Cluster:   src.id,
			Pool:      "strata.data",
			Namespace: "",
			OID:       oid,
			Size:      int64(len(body)),
		}
	}
	if err := m.PutObject(context.Background(), &meta.Object{
		BucketID:     bucketID,
		Key:          key,
		Size:         int64(len(bodies)) * int64(len(bodies[0])),
		ETag:         "deadbeef",
		StorageClass: "STANDARD",
		Mtime:        time.Now().UTC(),
		IsLatest:     true,
		Manifest:     &data.Manifest{Class: "STANDARD", Chunks: chunks},
	}, false); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	obj, err := m.GetObject(context.Background(), bucketID, key, "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	return obj
}

func planFromObject(b *meta.Bucket, o *meta.Object, from, to string) []Move {
	out := make([]Move, 0, len(o.Manifest.Chunks))
	for i, c := range o.Manifest.Chunks {
		if c.Cluster != from {
			continue
		}
		out = append(out, Move{
			Bucket:      b.Name,
			BucketID:    b.ID,
			ObjectKey:   o.Key,
			VersionID:   o.VersionID,
			ChunkIdx:    i,
			FromCluster: from,
			ToCluster:   to,
			SrcRef:      c,
			Class:       o.StorageClass,
		})
	}
	return out
}

func TestRadosMoverHappyPath(t *testing.T) {
	m := metamem.New()
	b, err := m.CreateBucket(context.Background(), "movebkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	src := newFakeCluster("c1")
	tgt := newFakeCluster("c2")
	obj := seedRadosObject(t, m, src, b.ID, "thing", [][]byte{
		bytes.Repeat([]byte("a"), 1024),
		bytes.Repeat([]byte("b"), 512),
	})
	plan := planFromObject(b, obj, "c1", "c2")
	if len(plan) != 2 {
		t.Fatalf("plan size: got %d want 2", len(plan))
	}

	metrics := newFakeMetrics()
	mover := &RadosMover{
		Clusters: map[string]RadosCluster{"c1": src, "c2": tgt},
		Meta:     m,
		Region:   "default",
		Logger:   slog.Default(),
		Metrics:  metrics,
		Inflight: 4,
	}
	if err := mover.Move(context.Background(), plan); err != nil {
		t.Fatalf("Move: %v", err)
	}

	// Manifest now points at c2 with new OIDs.
	post, err := m.GetObject(context.Background(), b.ID, "thing", "")
	if err != nil {
		t.Fatalf("post GetObject: %v", err)
	}
	if post.StorageClass != "STANDARD" {
		t.Errorf("class should not change: got %q", post.StorageClass)
	}
	if len(post.Manifest.Chunks) != 2 {
		t.Fatalf("chunk count: got %d want 2", len(post.Manifest.Chunks))
	}
	for i, c := range post.Manifest.Chunks {
		if c.Cluster != "c2" {
			t.Errorf("chunk %d cluster: got %q want c2", i, c.Cluster)
		}
		if c.Pool != "strata.data" {
			t.Errorf("chunk %d pool: got %q", i, c.Pool)
		}
		if c.OID == obj.Manifest.Chunks[i].OID {
			t.Errorf("chunk %d OID not rewritten: %s", i, c.OID)
		}
	}

	// Old chunks now live in the GC queue under region "default".
	entries, err := m.ListGCEntries(context.Background(), "default", time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("ListGCEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("gc entries: got %d want 2", len(entries))
	}
	oldByOID := map[string]bool{
		obj.Manifest.Chunks[0].OID: true,
		obj.Manifest.Chunks[1].OID: true,
	}
	for _, e := range entries {
		if !oldByOID[e.Chunk.OID] {
			t.Errorf("gc entry OID %q does not match any source chunk", e.Chunk.OID)
		}
		if e.Chunk.Cluster != "c1" {
			t.Errorf("gc entry cluster: got %q want c1", e.Chunk.Cluster)
		}
	}

	// Metrics: 2 chunks, total 1536 bytes c1->c2, no CAS conflicts.
	if got := metrics.bytes["c1->c2"]; got != 1024+512 {
		t.Errorf("bytes_moved: got %d want %d", got, 1024+512)
	}
	if got := metrics.chunks["c1->c2:movebkt"]; got != 2 {
		t.Errorf("chunks_moved: got %d want 2", got)
	}
	if len(metrics.confs) != 0 {
		t.Errorf("cas conflicts: got %#v want none", metrics.confs)
	}
}

func TestRadosMoverCASConflictDiscardsNewChunks(t *testing.T) {
	m := metamem.New()
	b, err := m.CreateBucket(context.Background(), "casbkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	src := newFakeCluster("c1")
	tgt := newFakeCluster("c2")
	obj := seedRadosObject(t, m, src, b.ID, "x", [][]byte{
		bytes.Repeat([]byte("a"), 256),
	})
	plan := planFromObject(b, obj, "c1", "c2")

	// Race: mutate the live manifest's chunk OID so it no longer
	// matches the planned SrcRef. The mover should detect this via
	// buildUpdatedManifest (ok=false) and treat as a CAS conflict.
	mutated := *obj.Manifest
	mutatedChunks := append([]data.ChunkRef(nil), obj.Manifest.Chunks...)
	mutatedChunks[0].OID = "raced-" + uuid.NewString()
	mutated.Chunks = mutatedChunks
	if applied, err := m.SetObjectStorage(context.Background(), b.ID, "x", obj.VersionID, "STANDARD", "STANDARD", &mutated); err != nil || !applied {
		t.Fatalf("SetObjectStorage seed: applied=%v err=%v", applied, err)
	}

	metrics := newFakeMetrics()
	mover := &RadosMover{
		Clusters: map[string]RadosCluster{"c1": src, "c2": tgt},
		Meta:     m,
		Region:   "default",
		Logger:   slog.Default(),
		Metrics:  metrics,
		Inflight: 1,
	}
	// Re-plant the source body under the original OID so the read
	// path succeeds (the mover reads against the planned SrcRef).
	src.plant("strata.data", "", obj.Manifest.Chunks[0].OID, bytes.Repeat([]byte("a"), 256))

	if err := mover.Move(context.Background(), plan); err != nil {
		t.Fatalf("Move: %v", err)
	}

	// Live manifest must NOT be flipped to c2 — the racing write wins.
	post, err := m.GetObject(context.Background(), b.ID, "x", "")
	if err != nil {
		t.Fatalf("post GetObject: %v", err)
	}
	if post.Manifest.Chunks[0].OID != mutatedChunks[0].OID {
		t.Errorf("live manifest unexpectedly rewritten by mover: %q", post.Manifest.Chunks[0].OID)
	}

	// Metric: one CAS conflict.
	if got := metrics.confs["casbkt"]; got != 1 {
		t.Errorf("cas_conflicts: got %d want 1", got)
	}
	if got := metrics.chunks["c1->c2:casbkt"]; got != 0 {
		t.Errorf("chunks_moved on conflict: got %d want 0", got)
	}

	// The freshly-written target chunk should be enqueued in the GC
	// queue (c2-side OID).
	entries, err := m.ListGCEntries(context.Background(), "default", time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("ListGCEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("gc entries on conflict: got %d want 1", len(entries))
	}
	if entries[0].Chunk.Cluster != "c2" {
		t.Errorf("conflict gc entry should reference target cluster; got %q", entries[0].Chunk.Cluster)
	}
}

func TestRadosMoverUnknownClusterLogsAndDrops(t *testing.T) {
	m := metamem.New()
	b, err := m.CreateBucket(context.Background(), "drop", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	src := newFakeCluster("c1")
	obj := seedRadosObject(t, m, src, b.ID, "ob", [][]byte{[]byte("a")})
	plan := planFromObject(b, obj, "c1", "missing-target")
	mover := &RadosMover{
		Clusters: map[string]RadosCluster{"c1": src},
		Meta:     m,
		Region:   "default",
		Logger:   slog.Default(),
		Metrics:  newFakeMetrics(),
		Inflight: 1,
	}
	if err := mover.Move(context.Background(), plan); err != nil {
		t.Fatalf("Move: %v", err)
	}
	// Live manifest still on c1.
	post, _ := m.GetObject(context.Background(), b.ID, "ob", "")
	if post.Manifest.Chunks[0].Cluster != "c1" {
		t.Errorf("manifest should not have moved on unknown target; got cluster %q", post.Manifest.Chunks[0].Cluster)
	}
}

func TestRadosMoverOwnsReportsConfiguredClusters(t *testing.T) {
	m := &RadosMover{Clusters: map[string]RadosCluster{"a": newFakeCluster("a")}}
	if !m.Owns("a") {
		t.Error("expected Owns(a)==true")
	}
	if m.Owns("b") {
		t.Error("expected Owns(b)==false")
	}
}

func TestRadosMoverRequiresMeta(t *testing.T) {
	m := &RadosMover{Clusters: map[string]RadosCluster{"a": newFakeCluster("a")}}
	err := m.Move(context.Background(), []Move{{ToCluster: "a", BucketID: uuid.New()}})
	if err == nil {
		t.Fatal("expected error when Meta nil")
	}
}

func TestRadosMoverGroupingBatchesPerObject(t *testing.T) {
	// Three chunks of the same object → one group; one chunk on a
	// different key → another group. Total of 2 groups.
	plan := []Move{
		{BucketID: [16]byte{1}, ObjectKey: "a", ChunkIdx: 0, ToCluster: "x"},
		{BucketID: [16]byte{1}, ObjectKey: "a", ChunkIdx: 1, ToCluster: "x"},
		{BucketID: [16]byte{1}, ObjectKey: "a", ChunkIdx: 2, ToCluster: "x"},
		{BucketID: [16]byte{1}, ObjectKey: "b", ChunkIdx: 0, ToCluster: "x"},
	}
	groups := groupMovesByObject(plan)
	if len(groups) != 2 {
		t.Fatalf("group count: got %d want 2", len(groups))
	}
	sizes := []int{len(groups[0]), len(groups[1])}
	want3, want1 := false, false
	for _, s := range sizes {
		if s == 3 {
			want3 = true
		}
		if s == 1 {
			want1 = true
		}
	}
	if !want3 || !want1 {
		t.Fatalf("group sizes: got %v want one 3-group + one 1-group", sizes)
	}
}
