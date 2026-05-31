package gc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/metrics"
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

	mu       sync.Mutex
	deletes  int64
	inFlight int64
	maxInFly int64
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

// countingBackend records per-OID Delete invocations under a mutex so a
// concurrent fan-out drain can be checked for exactly-once delivery (no
// double-delete from overlapping shards, no skipped chunk).
type countingBackend struct {
	mu      sync.Mutex
	deletes map[string]int
}

func (b *countingBackend) PutChunks(context.Context, io.Reader, string) (*data.Manifest, error) {
	return nil, nil
}

func (b *countingBackend) GetChunks(context.Context, *data.Manifest, int64, int64) (io.ReadCloser, error) {
	return nil, nil
}

func (b *countingBackend) Delete(_ context.Context, m *data.Manifest) error {
	if m == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deletes == nil {
		b.deletes = map[string]int{}
	}
	for _, c := range m.Chunks {
		b.deletes[c.OID]++
	}
	return nil
}

func (b *countingBackend) Close() error { return nil }

func (b *countingBackend) snapshot() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]int, len(b.deletes))
	for k, v := range b.deletes {
		out[k] = v
	}
	return out
}

// TestWorker_FanOutExactlyOnceConcurrent is the US-012 anchor for the GC
// per-shard fan-out exactly-once invariant: with shardCount workers draining
// the SAME queue + data backend concurrently, every chunk is deleted exactly
// once. The fan-out partitions by `entry.ShardID % shardCount == shardID`, so
// each chunk belongs to exactly one shard — a routing bug would show up as a
// double-delete (count>1 from overlapping shards) or a skip (count==0 / queue
// not drained). Distinct from TestWorker_DrainConcurrency (single-worker
// errgroup speedup) — this fans out across SHARD workers, not chunks.
func TestWorker_FanOutExactlyOnceConcurrent(t *testing.T) {
	const (
		n          = 2000
		shardCount = 8
	)
	ctx := context.Background()
	store := memory.New()

	chunks := make([]data.ChunkRef, 0, n)
	for i := range n {
		chunks = append(chunks, data.ChunkRef{
			Cluster: "default",
			Pool:    "hot",
			OID:     fmt.Sprintf("oid-%05d", i),
			Size:    int64(i + 1),
		})
	}
	if err := store.EnqueueChunkDeletion(ctx, "default", chunks); err != nil {
		t.Fatalf("EnqueueChunkDeletion: %v", err)
	}

	be := &countingBackend{}
	var (
		wg        sync.WaitGroup
		processed int64
	)
	for sid := range shardCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := &Worker{
				Meta:        store,
				Data:        be,
				Region:      "default",
				ShardID:     sid,
				ShardCount:  shardCount,
				Batch:       n,
				Concurrency: 8,
				Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			atomic.AddInt64(&processed, int64(w.RunOnce(ctx)))
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&processed); got != int64(n) {
		t.Fatalf("processed across shards=%d want %d (skip or double-ack)", got, n)
	}

	deletes := be.snapshot()
	if len(deletes) != n {
		t.Fatalf("distinct OIDs deleted=%d want %d (skipped chunks)", len(deletes), n)
	}
	for i := range n {
		oid := fmt.Sprintf("oid-%05d", i)
		switch deletes[oid] {
		case 1:
			// exactly once — correct.
		case 0:
			t.Fatalf("oid %s never deleted (shard skip)", oid)
		default:
			t.Fatalf("oid %s deleted %d times (shard overlap / double-delete)", oid, deletes[oid])
		}
	}

	remaining, err := store.ListGCEntries(ctx, "default", time.Now().Add(time.Hour), n+100)
	if err != nil {
		t.Fatalf("ListGCEntries: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("after concurrent fan-out drain: %d entries remain (leak)", len(remaining))
	}
}

// programmableBackend returns a caller-supplied error per chunk OID on
// Delete. Used by TestWorker_TerminalAckOnChunkNotFound to drive the
// US-001 gc.Worker ENOENT classifier without standing up a real RADOS
// or S3 backend.
type programmableBackend struct {
	mu      sync.Mutex
	errs    map[string]error
	deletes map[string]int
}

func (b *programmableBackend) PutChunks(context.Context, io.Reader, string) (*data.Manifest, error) {
	return nil, nil
}

func (b *programmableBackend) GetChunks(context.Context, *data.Manifest, int64, int64) (io.ReadCloser, error) {
	return nil, nil
}

func (b *programmableBackend) Delete(_ context.Context, m *data.Manifest) error {
	if m == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deletes == nil {
		b.deletes = map[string]int{}
	}
	for _, c := range m.Chunks {
		b.deletes[c.OID]++
		if err, ok := b.errs[c.OID]; ok {
			return err
		}
	}
	return nil
}

func (b *programmableBackend) Close() error { return nil }

func (b *programmableBackend) deleteCount(oid string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deletes[oid]
}

// TestWorker_TerminalAckOnChunkNotFound pins the US-001 acceptance: a
// Delete returning data.ErrChunkNotFound is treated as terminal — the
// gc worker ack's the queue entry (so it never re-appears), bumps the
// strata_gc_terminal_ack_total{reason="enoent"} counter, and does NOT
// stickyErr the iteration. Non-ENOENT errors keep the legacy loop-fail
// behaviour: no ack, stickyErr surfaces, entry re-appears next tick.
func TestWorker_TerminalAckOnChunkNotFound(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	chunks := []data.ChunkRef{
		{Cluster: "default", Pool: "hot", OID: "X", Size: 10},
		{Cluster: "default", Pool: "hot", OID: "Y", Size: 20},
		{Cluster: "default", Pool: "hot", OID: "Z", Size: 30},
	}
	if err := store.EnqueueChunkDeletion(ctx, "default", chunks); err != nil {
		t.Fatalf("EnqueueChunkDeletion: %v", err)
	}

	transportErr := errors.New("transport fail")
	be := &programmableBackend{
		errs: map[string]error{
			"X": fmt.Errorf("chunk X: %w", data.ErrChunkNotFound),
			"Y": transportErr,
		},
	}

	terminalBefore := testutil.ToFloat64(metrics.GCTerminalAck.WithLabelValues("enoent"))

	w := &Worker{
		Meta:   store,
		Data:   be,
		Region: "default",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	processed := w.RunOnce(ctx)
	if processed != 2 {
		t.Fatalf("processed=%d want 2 (X + Z; Y not ack'd)", processed)
	}

	if got := testutil.ToFloat64(metrics.GCTerminalAck.WithLabelValues("enoent")) - terminalBefore; got != 1 {
		t.Errorf("strata_gc_terminal_ack_total{reason=\"enoent\"} delta=%v want 1", got)
	}

	remaining, err := store.ListGCEntries(ctx, "default", time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("ListGCEntries: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("after RunOnce: %d remaining want 1 (Y)", len(remaining))
	}
	if remaining[0].Chunk.OID != "Y" {
		t.Fatalf("remaining OID=%q want Y", remaining[0].Chunk.OID)
	}

	// Two further RunOnce passes — X never re-appears (ack'd); Y still
	// retries because the transport error stays loop-fail.
	for i := range 2 {
		w.RunOnce(ctx)
		remaining, err = store.ListGCEntries(ctx, "default", time.Now().Add(time.Hour), 100)
		if err != nil {
			t.Fatalf("ListGCEntries (pass %d): %v", i+2, err)
		}
		if len(remaining) != 1 || remaining[0].Chunk.OID != "Y" {
			t.Fatalf("pass %d: remaining=%v want [Y]", i+2, remaining)
		}
	}

	if be.deleteCount("X") != 1 {
		t.Errorf("Delete(X) count=%d want 1 (ack'd after first pass)", be.deleteCount("X"))
	}
	if be.deleteCount("Y") != 3 {
		t.Errorf("Delete(Y) count=%d want 3 (retried each pass)", be.deleteCount("Y"))
	}
}
