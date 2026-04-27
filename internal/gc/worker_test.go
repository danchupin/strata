package gc

import (
	"context"
	"io"
	"log/slog"
	"sync"
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
