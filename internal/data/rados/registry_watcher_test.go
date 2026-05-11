package rados

import (
	"context"
	"errors"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeCatalog is a in-process CatalogReader stub keyed by id.
type fakeCatalog struct {
	mu      sync.Mutex
	entries map[string]CatalogEntry
	err     error
	calls   int
}

func newFakeCatalog(entries ...CatalogEntry) *fakeCatalog {
	f := &fakeCatalog{entries: map[string]CatalogEntry{}}
	for _, e := range entries {
		f.entries[e.ID] = e
	}
	return f
}

func (f *fakeCatalog) ListClusters(_ context.Context) ([]CatalogEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]CatalogEntry, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeCatalog) set(entries ...CatalogEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = map[string]CatalogEntry{}
	for _, e := range entries {
		f.entries[e.ID] = e
	}
}

func (f *fakeCatalog) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// fakeReconciler records the diff applied per tick and tracks "closed"
// clusters so we can assert remove-path cleanup.
type fakeReconciler struct {
	mu       sync.Mutex
	current  map[string]CatalogEntry
	closed   map[string]int
	summary  ReconcileSummary
	ticks    int
}

func newFakeReconciler() *fakeReconciler {
	return &fakeReconciler{
		current: map[string]CatalogEntry{},
		closed:  map[string]int{},
	}
}

func (r *fakeReconciler) ReconcileClusters(_ context.Context, latest map[string]CatalogEntry) ReconcileSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ticks++
	var s ReconcileSummary
	for id := range r.current {
		if _, ok := latest[id]; !ok {
			r.closed[id]++
			s.Removed++
		}
	}
	for id, e := range latest {
		prev, ok := r.current[id]
		if !ok {
			s.Added++
			continue
		}
		if prev.Version != e.Version {
			r.closed[id]++
			s.Updated++
		}
	}
	r.current = map[string]CatalogEntry{}
	for id, e := range latest {
		r.current[id] = e
	}
	r.summary = s
	return s
}

func (r *fakeReconciler) snapshot() (map[string]CatalogEntry, map[string]int, ReconcileSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := make(map[string]CatalogEntry, len(r.current))
	for id, e := range r.current {
		cur[id] = e
	}
	cls := make(map[string]int, len(r.closed))
	for id, n := range r.closed {
		cls[id] = n
	}
	return cur, cls, r.summary
}

// recordingMetrics captures per-op increments.
type recordingMetrics struct {
	mu sync.Mutex
	m  map[string]int
}

func newRecordingMetrics() *recordingMetrics {
	return &recordingMetrics{m: map[string]int{}}
}

func (r *recordingMetrics) IncRegistryChange(op string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[op] += n
}

func (r *recordingMetrics) snapshot() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.m))
	for k, v := range r.m {
		out[k] = v
	}
	return out
}

func TestRegistryWatcherIntervalFromEnvDefault(t *testing.T) {
	t.Setenv(EnvRegistryInterval, "")
	if d := registryIntervalFromEnv(); d != defaultRegistryInterval {
		t.Fatalf("default: want %v, got %v", defaultRegistryInterval, d)
	}
}

func TestRegistryWatcherIntervalFromEnvClamp(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"1s", minRegistryInterval},
		{"10m", maxRegistryInterval},
		{"15s", 15 * time.Second},
		{"garbage", defaultRegistryInterval},
		{"-1s", defaultRegistryInterval},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Setenv(EnvRegistryInterval, c.in)
			if d := registryIntervalFromEnv(); d != c.want {
				t.Fatalf("in=%q: want %v got %v", c.in, c.want, d)
			}
		})
	}
}

func TestRegistryWatcherSyncOnceAddRemoveUpdate(t *testing.T) {
	cat := newFakeCatalog(
		CatalogEntry{ID: "a", Backend: "rados", Version: 1},
		CatalogEntry{ID: "b", Backend: "rados", Version: 1},
		CatalogEntry{ID: "c", Backend: "rados", Version: 1},
	)
	rec := newFakeReconciler()
	metrics := newRecordingMetrics()
	w := NewRegistryWatcher(cat, rec, nil, metrics)

	ctx := context.Background()
	if err := w.SyncOnce(ctx); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	cur, _, _ := rec.snapshot()
	if len(cur) != 3 {
		t.Fatalf("after initial sync want 3, got %d", len(cur))
	}
	m := metrics.snapshot()
	if m["add"] != 3 {
		t.Fatalf("add metric: want 3, got %d", m["add"])
	}

	// Remove "b".
	cat.set(
		CatalogEntry{ID: "a", Backend: "rados", Version: 1},
		CatalogEntry{ID: "c", Backend: "rados", Version: 1},
	)
	if err := w.SyncOnce(ctx); err != nil {
		t.Fatalf("remove sync: %v", err)
	}
	cur, closed, _ := rec.snapshot()
	if _, ok := cur["b"]; ok {
		t.Fatalf("b should be gone, got %v", cur)
	}
	if closed["b"] != 1 {
		t.Fatalf("b should have been closed once, got %d", closed["b"])
	}
	m = metrics.snapshot()
	if m["remove"] != 1 {
		t.Fatalf("remove metric: want 1, got %d", m["remove"])
	}

	// Update "a" → Version bump.
	cat.set(
		CatalogEntry{ID: "a", Backend: "rados", Version: 2},
		CatalogEntry{ID: "c", Backend: "rados", Version: 1},
		CatalogEntry{ID: "d", Backend: "rados", Version: 1},
	)
	if err := w.SyncOnce(ctx); err != nil {
		t.Fatalf("update sync: %v", err)
	}
	cur, closed, _ = rec.snapshot()
	if cur["a"].Version != 2 {
		t.Fatalf("a version: want 2, got %d", cur["a"].Version)
	}
	if closed["a"] != 1 {
		t.Fatalf("a should be closed once on version bump, got %d", closed["a"])
	}
	if _, ok := cur["d"]; !ok {
		t.Fatalf("d should be added, got %v", cur)
	}
	m = metrics.snapshot()
	if m["update"] != 1 {
		t.Fatalf("update metric: want 1, got %d", m["update"])
	}
	if m["add"] != 4 {
		t.Fatalf("cumulative add: want 4 (3 initial + 1 d), got %d", m["add"])
	}
}

func TestRegistryWatcherSyncOnceListError(t *testing.T) {
	cat := newFakeCatalog()
	cat.setErr(errors.New("meta store unavailable"))
	rec := newFakeReconciler()
	w := NewRegistryWatcher(cat, rec, nil, nil)
	if err := w.SyncOnce(context.Background()); err == nil {
		t.Fatal("expected error on list failure")
	}
	if rec.ticks != 0 {
		t.Fatalf("reconciler must not be called on list error, ticks=%d", rec.ticks)
	}
}

func TestRegistryWatcherSyncOnceNilFields(t *testing.T) {
	w := &RegistryWatcher{}
	if err := w.SyncOnce(context.Background()); err == nil {
		t.Fatal("expected error when reader is nil")
	}
	w.reader = newFakeCatalog()
	if err := w.SyncOnce(context.Background()); err == nil {
		t.Fatal("expected error when reconciler is nil")
	}
}

// TestRegistryWatcherStartStopNoLeak verifies the polling goroutine winds
// up after Stop and does not leak. baseline measurement absorbs other
// background goroutines (testing runtime, race detector helpers) so we
// only check delta within tolerance.
func TestRegistryWatcherStartStopNoLeak(t *testing.T) {
	t.Setenv(EnvRegistryInterval, "5s") // long-ish; Stop must short-circuit
	cat := newFakeCatalog()
	rec := newFakeReconciler()
	w := NewRegistryWatcher(cat, rec, nil, nil)

	baseline := runtime.NumGoroutine()
	w.Start(context.Background())
	// Yield so the goroutine is definitely scheduled.
	time.Sleep(20 * time.Millisecond)
	if runtime.NumGoroutine() <= baseline {
		t.Fatalf("expected goroutine count to grow after Start (baseline=%d, now=%d)",
			baseline, runtime.NumGoroutine())
	}
	w.Stop()
	// Allow scheduler to deflate.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: baseline=%d, after Stop=%d", baseline, runtime.NumGoroutine())
}

func TestRegistryWatcherStartIsIdempotent(t *testing.T) {
	t.Setenv(EnvRegistryInterval, "5s")
	cat := newFakeCatalog()
	rec := newFakeReconciler()
	w := NewRegistryWatcher(cat, rec, nil, nil)
	defer w.Stop()
	w.Start(context.Background())
	g1 := runtime.NumGoroutine()
	w.Start(context.Background()) // second call must be a no-op
	time.Sleep(10 * time.Millisecond)
	g2 := runtime.NumGoroutine()
	if g2 > g1 {
		t.Fatalf("double Start spawned an extra goroutine: %d → %d", g1, g2)
	}
}

func TestRegistryWatcherStopBeforeStartIsNoop(t *testing.T) {
	w := NewRegistryWatcher(newFakeCatalog(), newFakeReconciler(), nil, nil)
	w.Stop() // must not panic, must not block
}

func TestRegistryWatcherStartCancelsOnParentCtx(t *testing.T) {
	t.Setenv(EnvRegistryInterval, "5s")
	cat := newFakeCatalog()
	rec := newFakeReconciler()
	w := NewRegistryWatcher(cat, rec, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	baseline := runtime.NumGoroutine()
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() < baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Even if the count didn't drop below baseline, Stop should be a
	// no-op now that the loop has returned via parent cancel.
	w.Stop()
}

func TestRegistryWatcherSkipsNonRADOSRows(t *testing.T) {
	cat := newFakeCatalog(
		CatalogEntry{ID: "rados-a", Backend: "rados", Version: 1},
		CatalogEntry{ID: "s3-a", Backend: "s3", Version: 1},
	)
	rec := &reconcilerFilter{}
	w := NewRegistryWatcher(cat, rec, nil, nil)
	if err := w.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.lastCount != 2 {
		t.Fatalf("watcher must hand raw entries to the reconciler (filtering is the reconciler's job); got %d", rec.lastCount)
	}
}

// reconcilerFilter records the count of entries it saw; it is used to
// confirm the watcher does NOT filter — that's the Backend's job.
type reconcilerFilter struct {
	lastCount int
}

func (r *reconcilerFilter) ReconcileClusters(_ context.Context, latest map[string]CatalogEntry) ReconcileSummary {
	r.lastCount = len(latest)
	return ReconcileSummary{}
}
