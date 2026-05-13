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
	planned      map[string]int
	refused      map[string]int // key = "<reason>:<target>"
	drainsDone   map[string]int
}

func (c *countingMetrics) IncPlannedMove(bucket string) {
	if c.planned == nil {
		c.planned = map[string]int{}
	}
	c.planned[bucket]++
}

func (c *countingMetrics) IncRefused(reason, target string) {
	if c.refused == nil {
		c.refused = map[string]int{}
	}
	c.refused[reason+":"+target]++
}

func (c *countingMetrics) IncDrainComplete(cluster string) {
	if c.drainsDone == nil {
		c.drainsDone = map[string]int{}
	}
	c.drainsDone[cluster]++
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

// seedBackendRefObject installs one Object on the memory store with a
// single BackendRef-shape manifest (S3 backend layout).
func seedBackendRefObject(t *testing.T, m meta.Store, bucketID uuid.UUID, key, backendKey, cluster string) {
	t.Helper()
	if err := m.PutObject(context.Background(), &meta.Object{
		BucketID:     bucketID,
		Key:          key,
		Size:         8,
		ETag:         "deadbeef",
		StorageClass: "STANDARD",
		Mtime:        time.Now().UTC(),
		IsLatest:     true,
		Manifest: &data.Manifest{
			Class: "STANDARD",
			Size:  8,
			BackendRef: &data.BackendRef{
				Backend: "s3",
				Key:     backendKey,
				Size:    8,
				Cluster: cluster,
			},
		},
	}, false); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
}

func TestWorkerEmitsMoveForBackendRefManifest(t *testing.T) {
	m, em, _, w := newRebalanceFixture(t)
	b, err := m.CreateBucket(context.Background(), "s3bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	// Single-cluster policy on c2 → every BackendRef-shape object on c1
	// becomes a planned move.
	if err := m.SetBucketPlacement(context.Background(), b.Name, map[string]int{"c2": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	seedBackendRefObject(t, m, b.ID, "obj-1", "uuid-key-1", "c1")
	seedBackendRefObject(t, m, b.ID, "obj-2", "uuid-key-2", "c1")
	// One already on c2 — should not generate a move.
	seedBackendRefObject(t, m, b.ID, "obj-3", "uuid-key-3", "c2")

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(em.calls) != 1 {
		t.Fatalf("emit count: got %d want 1", len(em.calls))
	}
	c := em.calls[0]
	if got := c.Actual["c1"]; got != 2 {
		t.Errorf("actual[c1]: got %d want 2", got)
	}
	if got := c.Actual["c2"]; got != 1 {
		t.Errorf("actual[c2]: got %d want 1", got)
	}
	if len(c.Moves) != 2 {
		t.Fatalf("moves: got %d want 2", len(c.Moves))
	}
	for _, mv := range c.Moves {
		if mv.FromCluster != "c1" || mv.ToCluster != "c2" {
			t.Errorf("move shape: got from=%q to=%q", mv.FromCluster, mv.ToCluster)
		}
		if mv.ChunkIdx != 0 {
			t.Errorf("BackendRef move ChunkIdx: got %d want 0", mv.ChunkIdx)
		}
		if mv.SrcRef.Cluster != "c1" {
			t.Errorf("SrcRef.Cluster: got %q want c1", mv.SrcRef.Cluster)
		}
		if mv.SrcRef.OID == "" {
			t.Errorf("SrcRef.OID empty; expected BackendRef.Key")
		}
	}
}

func TestWorkerSkipsBackendRefWithoutCluster(t *testing.T) {
	// Pre-US-005 rows have BackendRef.Cluster == "" — the worker must
	// skip them rather than crash; a backfill cycle would later
	// populate Cluster.
	m, em, _, w := newRebalanceFixture(t)
	b, err := m.CreateBucket(context.Background(), "legacy", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := m.SetBucketPlacement(context.Background(), b.Name, map[string]int{"c2": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	seedBackendRefObject(t, m, b.ID, "legacy-obj", "uuid-key", "")

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(em.calls) != 1 {
		t.Fatalf("emit count: got %d want 1", len(em.calls))
	}
	if len(em.calls[0].Moves) != 0 {
		t.Fatalf("moves on legacy BackendRef: got %d want 0", len(em.calls[0].Moves))
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

// fakeStatsProbe surfaces a configurable used/total per cluster id so the
// US-006 target_full safety rail can be exercised without spinning up a
// real RADOS cluster.
type fakeStatsProbe struct {
	stats map[string]struct{ used, total int64 }
	err   map[string]error
	calls map[string]int
}

func (p *fakeStatsProbe) ClusterStats(_ context.Context, id string) (int64, int64, error) {
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[id]++
	if e, ok := p.err[id]; ok {
		return 0, 0, e
	}
	s := p.stats[id]
	return s.used, s.total, nil
}

// TestWorkerRefusesMovesIntoFullTarget — placement says move c1→c2 but
// c2.used/c2.total > 0.90 → the safety rail bumps the
// target_full counter and the move is dropped before emit.
func TestWorkerRefusesMovesIntoFullTarget(t *testing.T) {
	m := metamem.New()
	em := &recordingEmitter{}
	cm := &countingMetrics{}
	probe := &fakeStatsProbe{stats: map[string]struct{ used, total int64 }{
		"c1": {used: 10, total: 100},
		"c2": {used: 95, total: 100}, // 95% — above default 0.90 ceiling
	}}
	w, err := New(Config{
		Meta: m, Data: datamem.New(), Logger: slog.Default(),
		Metrics: cm, Emitter: em, Interval: time.Hour, StatsProbe: probe,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := m.CreateBucket(context.Background(), "fill", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	// Policy targets c2 exclusively so every move planned goes c1→c2.
	if err := m.SetBucketPlacement(context.Background(), b.Name, map[string]int{"c2": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	seedObject(t, m, b.ID, "x", []string{"c1", "c1", "c1"})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(em.calls) != 1 {
		t.Fatalf("emit count: got %d want 1", len(em.calls))
	}
	if got := em.calls[0].Moves; len(got) != 0 {
		t.Fatalf("expected zero moves after target_full refusal, got %d", len(got))
	}
	if cm.refused["target_full:c2"] != 3 {
		t.Fatalf("target_full counter: got %d want 3", cm.refused["target_full:c2"])
	}
	if cm.planned[b.Name] != 0 {
		t.Fatalf("planned counter should remain zero after refusal: %d", cm.planned[b.Name])
	}
	// Probe should only have been called once for c2 (per-iteration cache).
	if probe.calls["c2"] != 1 {
		t.Fatalf("fill probe should be cached per iteration, got %d calls", probe.calls["c2"])
	}
}

// TestWorkerSkipsRefusedTargetWhenProbeUnsupported — backend returns
// ErrClusterStatsNotSupported → safety rail no-ops and moves proceed.
func TestWorkerSkipsRefusedTargetWhenProbeUnsupported(t *testing.T) {
	m := metamem.New()
	em := &recordingEmitter{}
	cm := &countingMetrics{}
	probe := &fakeStatsProbe{err: map[string]error{
		"c2": data.ErrClusterStatsNotSupported,
	}}
	w, err := New(Config{
		Meta: m, Data: datamem.New(), Logger: slog.Default(),
		Metrics: cm, Emitter: em, Interval: time.Hour, StatsProbe: probe,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := m.CreateBucket(context.Background(), "noprobe", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := m.SetBucketPlacement(context.Background(), b.Name, map[string]int{"c2": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	seedObject(t, m, b.ID, "y", []string{"c1", "c1"})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := em.calls[0].Moves; len(got) != 2 {
		t.Fatalf("expected 2 moves when probe unsupported, got %d", len(got))
	}
	if cm.refused["target_full:c2"] != 0 {
		t.Fatalf("unsupported probe should not bump target_full: got %d", cm.refused["target_full:c2"])
	}
}

// TestWorkerSkipsDrainingTargetsAtPlanTime — c2 is draining, picker
// excludes it, so moves into c2 are never even computed. With policy
// {c1:1, c2:1}, the worker still finds c1 chunks "in equilibrium"
// (PickClusterExcluding picks c1 for every chunk) so no moves emitted.
func TestWorkerSkipsDrainingTargetsAtPlanTime(t *testing.T) {
	m, em, cm, w := newRebalanceFixture(t)
	b, err := m.CreateBucket(context.Background(), "drain1", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := m.SetBucketPlacement(context.Background(), b.Name, map[string]int{"c1": 1, "c2": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	if err := m.SetClusterState(context.Background(), "c2", meta.ClusterStateDraining); err != nil {
		t.Fatalf("SetClusterState: %v", err)
	}
	// All chunks on c1 — with draining c2, picker keeps them on c1.
	seedObject(t, m, b.ID, "z", []string{"c1", "c1", "c1"})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := em.calls[0].Moves; len(got) != 0 {
		t.Fatalf("expected zero moves when c2 draining and all chunks on c1, got %d", len(got))
	}
	_ = cm
}

// TestWorkerMovesOutOfDrainingCluster — c1 is draining, chunks on c1
// should migrate to c2.
func TestWorkerMovesOutOfDrainingCluster(t *testing.T) {
	m, em, cm, w := newRebalanceFixture(t)
	b, err := m.CreateBucket(context.Background(), "drain2", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := m.SetBucketPlacement(context.Background(), b.Name, map[string]int{"c1": 1, "c2": 1}); err != nil {
		t.Fatalf("SetBucketPlacement: %v", err)
	}
	if err := m.SetClusterState(context.Background(), "c1", meta.ClusterStateDraining); err != nil {
		t.Fatalf("SetClusterState: %v", err)
	}
	seedObject(t, m, b.ID, "w", []string{"c1", "c1", "c1"})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	moves := em.calls[0].Moves
	if len(moves) != 3 {
		t.Fatalf("expected 3 moves out of draining c1, got %d", len(moves))
	}
	for _, mv := range moves {
		if mv.FromCluster != "c1" || mv.ToCluster != "c2" {
			t.Errorf("move %+v: want c1 → c2", mv)
		}
	}
	if cm.refused["target_draining:c2"] != 0 {
		t.Fatalf("target_draining must not fire for c2 (only c1 draining): %d", cm.refused["target_draining:c2"])
	}
}

// TestWorkerRefusesIntoDrainingDefenseInDepth — manually post-plant a
// move whose target is draining (race between scan + drain flip) and
// assert the post-filter catches it. Drives applySafetyRails directly.
func TestWorkerRefusesIntoDrainingDefenseInDepth(t *testing.T) {
	m, _, cm, w := newRebalanceFixture(t)
	b := &meta.Bucket{Name: "race", ID: uuid.New()}
	moves := []Move{{Bucket: b.Name, BucketID: b.ID, ToCluster: "c2"}}
	out := w.applySafetyRails(context.Background(), b, moves,
		map[string]bool{"c2": true}, w.newFillCache(context.Background()))
	if len(out) != 0 {
		t.Fatalf("expected refusal: got %d moves", len(out))
	}
	if cm.refused["target_draining:c2"] != 1 {
		t.Fatalf("counter: got %v", cm.refused)
	}
	_ = m
}
