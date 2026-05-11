package gc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta/memory"
)

// recordingBackend records per-cluster Delete invocations so the GC worker can
// be observed dispatching enqueued chunks to their correct cluster (US-044).
type recordingBackend struct {
	mu      sync.Mutex
	deletes map[string]int
}

func (b *recordingBackend) PutChunks(context.Context, io.Reader, string) (*data.Manifest, error) {
	return nil, nil
}

func (b *recordingBackend) GetChunks(context.Context, *data.Manifest, int64, int64) (io.ReadCloser, error) {
	return nil, nil
}

func (b *recordingBackend) Delete(_ context.Context, m *data.Manifest) error {
	if m == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deletes == nil {
		b.deletes = map[string]int{}
	}
	for _, c := range m.Chunks {
		b.deletes[c.Cluster]++
	}
	return nil
}

func (b *recordingBackend) Close() error { return nil }

func (b *recordingBackend) deleteCount(cluster string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deletes[cluster]
}

// TestGCWorkerDispatchesPerCluster enqueues a mix of default- and remote-
// cluster chunks, drains them through the GC worker, and asserts each Delete
// reaches the right cluster via ChunkRef.Cluster on the data backend.
func TestGCWorkerDispatchesPerCluster(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	chunks := []data.ChunkRef{
		{Cluster: "default", Pool: "hot", OID: "h1", Size: 10},
		{Cluster: "cold-eu", Pool: "cold-pool", OID: "c1", Size: 20},
		{Cluster: "cold-eu", Pool: "cold-pool", Namespace: "frozen", OID: "c2", Size: 30},
	}
	if err := store.EnqueueChunkDeletion(ctx, "default", chunks); err != nil {
		t.Fatalf("EnqueueChunkDeletion: %v", err)
	}

	be := &recordingBackend{}
	w := &Worker{Meta: store, Data: be, Region: "default", Logger: slog.Default()}
	processed := w.RunOnce(ctx)
	if processed != 3 {
		t.Fatalf("processed=%d want 3", processed)
	}
	if got := be.deleteCount("default"); got != 1 {
		t.Errorf("default cluster deletes: %d want 1", got)
	}
	if got := be.deleteCount("cold-eu"); got != 2 {
		t.Errorf("cold-eu cluster deletes: %d want 2", got)
	}
	remaining, err := store.ListGCEntries(ctx, "default", time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("ListGCEntries: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("after drain: %d remaining", len(remaining))
	}
}

// slowBackend simulates a per-Delete latency so the parallelism win is
// observable in wall-clock terms. Tracks max in-flight concurrency.
type slowBackend struct {
	latency time.Duration

	mu        sync.Mutex
	deletes   int64
	inFlight  int64
	maxInFly  int64
}

func (b *slowBackend) PutChunks(context.Context, io.Reader, string) (*data.Manifest, error) {
	return nil, nil
}

func (b *slowBackend) GetChunks(context.Context, *data.Manifest, int64, int64) (io.ReadCloser, error) {
	return nil, nil
}

func (b *slowBackend) Delete(_ context.Context, _ *data.Manifest) error {
	cur := atomic.AddInt64(&b.inFlight, 1)
	defer atomic.AddInt64(&b.inFlight, -1)
	for {
		old := atomic.LoadInt64(&b.maxInFly)
		if cur <= old || atomic.CompareAndSwapInt64(&b.maxInFly, old, cur) {
			break
		}
	}
	time.Sleep(b.latency)
	atomic.AddInt64(&b.deletes, 1)
	return nil
}

func (b *slowBackend) Close() error { return nil }

// TestWorker_DrainConcurrency drives 1k entries with Concurrency=32 against
// in-memory data + meta and asserts wall-clock < 4× the ideal-parallel time.
// US-001: Phase 1 bounded errgroup speedup.
func TestWorker_DrainConcurrency(t *testing.T) {
	const n = 1000
	const concurrency = 32
	const perEntry = 2 * time.Millisecond

	ctx := context.Background()
	store := memory.New()

	chunks := make([]data.ChunkRef, 0, n)
	for i := range n {
		chunks = append(chunks, data.ChunkRef{
			Cluster: "default",
			Pool:    "hot",
			OID:     fmt.Sprintf("c-%d", i),
			Size:    int64(i + 1),
		})
	}
	if err := store.EnqueueChunkDeletion(ctx, "default", chunks); err != nil {
		t.Fatalf("EnqueueChunkDeletion: %v", err)
	}

	be := &slowBackend{latency: perEntry}
	w := &Worker{
		Meta:        store,
		Data:        be,
		Region:      "default",
		Concurrency: concurrency,
		Batch:       n,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	start := time.Now()
	processed := w.RunOnce(ctx)
	elapsed := time.Since(start)

	if processed != n {
		t.Fatalf("processed=%d want %d", processed, n)
	}
	if got := atomic.LoadInt64(&be.deletes); got != int64(n) {
		t.Fatalf("delete count=%d want %d", got, n)
	}
	if max := atomic.LoadInt64(&be.maxInFly); max < 2 {
		t.Fatalf("max in-flight=%d, expected >1 with concurrency=%d", max, concurrency)
	}

	idealParallel := time.Duration(int64(perEntry) * int64(n) / int64(concurrency))
	cap := 4 * idealParallel
	if elapsed > cap {
		t.Fatalf("drain elapsed=%s, expected < %s (4× ideal-parallel %s)",
			elapsed, cap, idealParallel)
	}
}
