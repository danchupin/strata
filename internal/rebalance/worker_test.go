package rebalance

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/data/placement"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

// recordingEmitter captures every emitted plan so assertions can inspect
// per-bucket moves + actual/target maps.
type recordingEmitter struct {
	calls []emitCall
}

type emitCall struct {
	Bucket string
	Actual map[string]int
	Target map[string]int
	Moves  []Move
}

func (r *recordingEmitter) EmitPlan(_ context.Context, b *meta.Bucket, actual, target map[string]int, moves []Move) error {
	r.calls = append(r.calls, emitCall{
		Bucket: b.Name,
		Actual: cloneMap(actual),
		Target: cloneMap(target),
		Moves:  append([]Move(nil), moves...),
	})
	return nil
}

func cloneMap(m map[string]int) map[string]int {
	if m == nil {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// countingMetrics records counter bumps so the test can assert the
// metric was incremented once per planned move.
type countingMetrics struct {
	planned map[string]int
}

func (c *countingMetrics) IncPlannedMove(bucket string) {
	if c.planned == nil {
		c.planned = map[string]int{}
	}
	c.planned[bucket]++
}

// seedObject installs one Object on the memory store with `n` chunks at
// the supplied per-chunk cluster ids.
func seedObject(t *testing.T, m meta.Store, bucketID uuid.UUID, key string, chunkClusters []string) {
	t.Helper()
	chunks := make([]data.ChunkRef, len(chunkClusters))
	for i, c := range chunkClusters {
		chunks[i] = data.ChunkRef{
			Cluster: c,
			Pool:    "default",
			OID:     key + "/" + uuid.NewString(),
			Size:    1024,
		}
	}
	if err := m.PutObject(context.Background(), &meta.Object{
		BucketID:     bucketID,
		Key:          key,
		Size:         int64(len(chunks)) * 1024,
		ETag:         "deadbeef",
		ContentType:  "application/octet-stream",
		StorageClass: "STANDARD",
		Mtime:        time.Now().UTC(),
		IsLatest:     true,
		Manifest:     &data.Manifest{Class: "STANDARD", Chunks: chunks},
	}, false); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
}

func newRebalanceFixture(t *testing.T) (meta.Store, *recordingEmitter, *countingMetrics, *Worker) {
	t.Helper()
	m := metamem.New()
	em := &recordingEmitter{}
	cm := &countingMetrics{}
	w, err := New(Config{
		Meta:     m,
		Data:     datamem.New(),
		Logger:   slog.Default(),
		Metrics:  cm,
		Emitter:  em,
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, em, cm, w
}

func TestWorkerSkipsBucketsWithoutPlacement(t *testing.T) {
	m, em, _, w := newRebalanceFixture(t)
	b, err := m.CreateBucket(context.Background(), "noplacement", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	seedObject(t, m, b.ID, "obj", []string{"c1"})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(em.calls) != 0 {
		t.Fatalf("expected zero emit calls for placement-less bucket, got %d", len(em.calls))
	}
}

func TestWorkerEmitsActualDistribution(t *testing.T) {
	m, em, _, w := newRebalanceFixture(t)
	b, err := m.CreateBucket(context.Background(), "withplacement", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := m.SetBucketPlacement(context.Background(), b.Name, map[string]int{"c1": 1, "c2": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	// Three chunks all on c1; we want actual={c1:3} and at least one
	// move planned (because the picker spreads across c1+c2).
	seedObject(t, m, b.ID, "obj", []string{"c1", "c1", "c1"})

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(em.calls) != 1 {
		t.Fatalf("emit count: got %d want 1", len(em.calls))
	}
	c := em.calls[0]
	if c.Bucket != b.Name {
		t.Fatalf("bucket: got %q want %q", c.Bucket, b.Name)
	}
	if c.Actual["c1"] != 3 {
		t.Fatalf("actual[c1]: got %d want 3", c.Actual["c1"])
	}
	if got := c.Target; got["c1"] != 1 || got["c2"] != 1 {
		t.Fatalf("target: got %v", got)
	}
	if len(c.Moves) == 0 {
		t.Fatalf("expected at least one move for {c1:1,c2:1} on chunks all-on-c1")
	}
	// Validate every move's FromCluster matches the source row and
	// ToCluster matches the picker verdict.
	for _, mv := range c.Moves {
		if mv.FromCluster != "c1" {
			t.Errorf("move From: got %q want c1", mv.FromCluster)
		}
		want := placement.PickCluster(b.ID, mv.ObjectKey, mv.ChunkIdx, map[string]int{"c1": 1, "c2": 1})
		if mv.ToCluster != want {
			t.Errorf("move To: got %q want %q", mv.ToCluster, want)
		}
	}
}

func TestWorkerNoMovesWhenDistributionAlreadyMatches(t *testing.T) {
	m, em, _, w := newRebalanceFixture(t)
	b, err := m.CreateBucket(context.Background(), "even", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := m.SetBucketPlacement(context.Background(), b.Name, map[string]int{"c1": 1, "c2": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	// Build a manifest where every chunk is on the cluster the picker
	// would pick. Borrow the picker to label each chunk's home; the
	// scan should then plan zero moves.
	chunks := make([]string, 16)
	for i := range chunks {
		chunks[i] = placement.PickCluster(b.ID, "obj-even", i, map[string]int{"c1": 1, "c2": 1})
	}
	seedObject(t, m, b.ID, "obj-even", chunks)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(em.calls) != 1 {
		t.Fatalf("emit count: got %d want 1", len(em.calls))
	}
	if got := em.calls[0].Moves; len(got) != 0 {
		t.Fatalf("moves: got %d want 0; moves=%v", len(got), got)
	}
}

func TestWorkerIncrementsMetricPerMove(t *testing.T) {
	m, em, cm, w := newRebalanceFixture(t)
	b, err := m.CreateBucket(context.Background(), "metricbkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	// Single-cluster policy → every chunk not already on c-only needs
	// to move. Seed three chunks on a different cluster.
	if err := m.SetBucketPlacement(context.Background(), b.Name, map[string]int{"only": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	seedObject(t, m, b.ID, "obj-metric", []string{"old", "old", "old"})

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := cm.planned[b.Name]; got != 3 {
		t.Fatalf("planned counter: got %d want 3", got)
	}
	if len(em.calls) != 1 || len(em.calls[0].Moves) != 3 {
		t.Fatalf("expected 3 moves; got %d emit calls / moves", len(em.calls))
	}
}

func TestWorkerSurvivesObjectsWithoutManifest(t *testing.T) {
	m, em, _, w := newRebalanceFixture(t)
	b, err := m.CreateBucket(context.Background(), "noman", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := m.SetBucketPlacement(context.Background(), b.Name, map[string]int{"c1": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	// Object without a manifest (e.g. delete marker shape). PutObject
	// happily stores it; the worker must skip it without panicking.
	if err := m.PutObject(context.Background(), &meta.Object{
		BucketID:       b.ID,
		Key:            "del",
		IsLatest:       true,
		IsDeleteMarker: true,
		Mtime:          time.Now().UTC(),
		StorageClass:   "STANDARD",
	}, false); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(em.calls) != 1 {
		t.Fatalf("emit count: got %d want 1", len(em.calls))
	}
	if len(em.calls[0].Moves) != 0 {
		t.Fatalf("moves: got %d want 0", len(em.calls[0].Moves))
	}
}

func TestWorkerRequiresMetaAndData(t *testing.T) {
	if _, err := New(Config{Data: datamem.New()}); err == nil {
		t.Fatal("expected error when Meta nil")
	}
	if _, err := New(Config{Meta: metamem.New()}); err == nil {
		t.Fatal("expected error when Data nil")
	}
}

func TestPlanLoggerEmitterDefaults(t *testing.T) {
	m, _, cm, _ := newRebalanceFixture(t)
	// Build a worker WITHOUT a custom emitter so the default planLogger
	// is wired; assert it touches Metrics per move.
	w, err := New(Config{Meta: m, Data: datamem.New(), Logger: slog.Default(), Metrics: cm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := m.CreateBucket(context.Background(), "logger", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := m.SetBucketPlacement(context.Background(), b.Name, map[string]int{"only": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	seedObject(t, m, b.ID, "kk", []string{"old", "old"})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if cm.planned[b.Name] != 2 {
		t.Fatalf("default planLogger should have bumped counter twice; got %d", cm.planned[b.Name])
	}
}
