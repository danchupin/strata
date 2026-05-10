package rados

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
)

func TestPutChunksParallelPreservesByteOrder(t *testing.T) {
	const chunkSize = 64
	const chunks = 6
	src := bytes.Repeat([]byte("A"), chunkSize)
	src = append(src, bytes.Repeat([]byte("B"), chunkSize)...)
	src = append(src, bytes.Repeat([]byte("C"), chunkSize)...)
	src = append(src, bytes.Repeat([]byte("D"), chunkSize)...)
	src = append(src, bytes.Repeat([]byte("E"), chunkSize)...)
	src = append(src, []byte("tail")...)
	if len(src) != chunkSize*5+4 {
		t.Fatalf("test setup: unexpected src size %d", len(src))
	}

	var mu sync.Mutex
	stored := make(map[string][]byte)
	put := func(ctx context.Context, idx int, body []byte) (data.ChunkRef, error) {
		oid := fmt.Sprintf("oid.%05d", idx)
		mu.Lock()
		stored[oid] = append([]byte(nil), body...)
		mu.Unlock()
		return data.ChunkRef{
			Cluster: "default",
			Pool:    "p",
			OID:     oid,
			Size:    int64(len(body)),
		}, nil
	}

	m, err := putChunksParallelWithChunkSize(context.Background(), bytes.NewReader(src), "STANDARD", 4, chunkSize, put)
	if err != nil {
		t.Fatalf("putChunksParallel: %v", err)
	}
	if got, want := len(m.Chunks), chunks; got != want {
		t.Fatalf("chunk count: got %d want %d", got, want)
	}
	if m.Size != int64(len(src)) {
		t.Fatalf("size: got %d want %d", m.Size, len(src))
	}

	wantHash := md5.Sum(src)
	if m.ETag != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("etag: got %s want %s", m.ETag, hex.EncodeToString(wantHash[:]))
	}

	for i, ref := range m.Chunks {
		wantOID := fmt.Sprintf("oid.%05d", i)
		if ref.OID != wantOID {
			t.Fatalf("chunk %d OID: got %q want %q", i, ref.OID, wantOID)
		}
	}

	var assembled bytes.Buffer
	for _, ref := range m.Chunks {
		body, ok := stored[ref.OID]
		if !ok {
			t.Fatalf("missing body for %s", ref.OID)
		}
		assembled.Write(body)
	}
	if !bytes.Equal(assembled.Bytes(), src) {
		t.Fatalf("reassembled bytes differ from source")
	}
}

// TestPutChunksParallelGoesParallel asserts wall-clock for a 32-chunk object
// against an N=8 worker pool with a 5 ms per-write latency injection. With
// strict-sequential dispatch wall-clock would be ≥ 32 × 5 ms = 160 ms; with
// concurrency=8 the PRD upper bound is ≤ 4× single-chunk latency. We assert
// the helper finishes well under the sequential ceiling so any regression to
// the sequential shape trips the assertion.
func TestPutChunksParallelGoesParallel(t *testing.T) {
	const total = 32
	const chunkSize = 64
	src := bytes.Repeat([]byte{'x'}, total*chunkSize)
	const perOp = 5 * time.Millisecond

	put := func(ctx context.Context, idx int, body []byte) (data.ChunkRef, error) {
		time.Sleep(perOp)
		return data.ChunkRef{OID: fmt.Sprintf("oid.%05d", idx), Size: int64(len(body))}, nil
	}

	start := time.Now()
	m, err := putChunksParallelWithChunkSize(context.Background(), bytes.NewReader(src), "STANDARD", 8, chunkSize, put)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("putChunksParallel: %v", err)
	}
	if len(m.Chunks) != total {
		t.Fatalf("chunk count: got %d want %d", len(m.Chunks), total)
	}
	// concurrency=8 → wall-clock bounded by ceil(32/8) × perOp = 4 × 5 ms = 20 ms
	// (plus overhead). Sequential would be ≥ 160 ms. Assert < 80 ms = half of
	// sequential — generous enough to absorb scheduler jitter on slow CI.
	if elapsed >= time.Duration(total)*perOp/2 {
		t.Fatalf("wall-clock %v suggests sequential dispatch (expected ≪ %v)", elapsed, time.Duration(total)*perOp)
	}
}

func TestPutChunksParallelCancelsOnError(t *testing.T) {
	const total = 16
	const chunkSize = 64
	src := bytes.Repeat([]byte{'x'}, total*chunkSize)
	boom := errors.New("disk on fire")

	var stored sync.Map
	var calls atomic.Int32
	put := func(ctx context.Context, idx int, body []byte) (data.ChunkRef, error) {
		calls.Add(1)
		if idx == 3 {
			return data.ChunkRef{}, boom
		}
		// Slow non-error workers so the error has time to propagate.
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return data.ChunkRef{}, ctx.Err()
		}
		oid := fmt.Sprintf("oid.%05d", idx)
		stored.Store(oid, true)
		return data.ChunkRef{OID: oid, Size: int64(len(body))}, nil
	}

	m, err := putChunksParallelWithChunkSize(context.Background(), bytes.NewReader(src), "STANDARD", 4, chunkSize, put)
	if !errors.Is(err, boom) {
		t.Fatalf("err: got %v want %v", err, boom)
	}
	if m == nil {
		t.Fatal("manifest nil; expected partial manifest for cleanup")
	}
	// Partial manifest must reference only OIDs that actually landed (stored).
	for _, ref := range m.Chunks {
		if ref.OID == "" {
			t.Fatalf("manifest carries empty-OID chunk: %+v", ref)
		}
		if _, ok := stored.Load(ref.OID); !ok {
			t.Fatalf("manifest references %s but it never landed", ref.OID)
		}
	}
	// Total successful calls must be < total (cancel kicked in).
	storedCount := 0
	stored.Range(func(_, _ any) bool {
		storedCount++
		return true
	})
	if storedCount >= total {
		t.Fatalf("expected fewer than %d successful writes, got %d", total, storedCount)
	}
}

func TestPutChunksParallelHonoursReaderCancel(t *testing.T) {
	const chunkSize = 64
	src := bytes.Repeat([]byte{'x'}, 8*chunkSize)
	ctx, cancel := context.WithCancel(context.Background())

	put := func(opCtx context.Context, idx int, body []byte) (data.ChunkRef, error) {
		// First chunk triggers cancel; subsequent calls should see opCtx done
		// (errgroup cancels gctx after the parent ctx fires).
		if idx == 0 {
			cancel()
		}
		select {
		case <-time.After(10 * time.Millisecond):
			return data.ChunkRef{OID: fmt.Sprintf("oid.%05d", idx), Size: int64(len(body))}, nil
		case <-opCtx.Done():
			return data.ChunkRef{}, opCtx.Err()
		}
	}

	_, err := putChunksParallelWithChunkSize(ctx, bytes.NewReader(src), "STANDARD", 2, chunkSize, put)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err: got %v want context.Canceled", err)
	}
}

func TestPutChunksParallelConcurrencyOneIsSequential(t *testing.T) {
	// concurrency=1 must still produce a correct manifest.
	src := []byte("0123456789")
	put := func(ctx context.Context, idx int, body []byte) (data.ChunkRef, error) {
		return data.ChunkRef{OID: fmt.Sprintf("oid.%05d", idx), Size: int64(len(body))}, nil
	}
	m, err := putChunksParallel(context.Background(), bytes.NewReader(src), "STANDARD", 1, put)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Chunks) != 1 {
		t.Fatalf("chunks: %d want 1", len(m.Chunks))
	}
	if m.Size != int64(len(src)) {
		t.Fatalf("size: %d want %d", m.Size, len(src))
	}
	wantHash := md5.Sum(src)
	if m.ETag != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("etag mismatch")
	}
}

func TestPutChunksParallelEmptyReader(t *testing.T) {
	put := func(ctx context.Context, idx int, body []byte) (data.ChunkRef, error) {
		t.Fatalf("put called with idx=%d for empty reader", idx)
		return data.ChunkRef{}, nil
	}
	m, err := putChunksParallel(context.Background(), bytes.NewReader(nil), "STANDARD", 4, put)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Chunks) != 0 {
		t.Fatalf("expected 0 chunks, got %d", len(m.Chunks))
	}
	if m.Size != 0 {
		t.Fatalf("expected size 0, got %d", m.Size)
	}
	if m.ETag != hex.EncodeToString(md5.New().Sum(nil)) {
		t.Fatalf("ETag for empty input must equal MD5 of empty bytes")
	}
}

func TestPutChunksParallelReaderError(t *testing.T) {
	const chunkSize = 64
	r := &erroringReader{
		body:  bytes.NewReader(bytes.Repeat([]byte{'x'}, chunkSize*2)),
		after: chunkSize, // read first chunk fine, error mid-stream
		err:   errors.New("backend torn"),
	}
	var stored sync.Map
	put := func(ctx context.Context, idx int, body []byte) (data.ChunkRef, error) {
		oid := fmt.Sprintf("oid.%05d", idx)
		stored.Store(oid, true)
		return data.ChunkRef{OID: oid, Size: int64(len(body))}, nil
	}
	m, err := putChunksParallelWithChunkSize(context.Background(), r, "STANDARD", 4, chunkSize, put)
	if err == nil || !strings.Contains(err.Error(), "backend torn") {
		t.Fatalf("expected reader error, got %v", err)
	}
	// Cleanup-correctness: every OID returned in the partial manifest must
	// correspond to an actual successful write (no leaked OIDs from a chunk
	// that was dispatched but never written).
	for _, ref := range m.Chunks {
		if ref.OID == "" {
			t.Fatalf("manifest carries empty-OID chunk: %+v", ref)
		}
		if _, ok := stored.Load(ref.OID); !ok {
			t.Fatalf("manifest references %s but it never landed", ref.OID)
		}
	}
}

func TestClampPutConcurrency(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{-5, 1},
		{0, 1},
		{1, 1},
		{32, 32},
		{256, 256},
		{1000, 256},
	}
	for _, tc := range cases {
		if got := clampPutConcurrency(tc.in); got != tc.want {
			t.Errorf("clampPutConcurrency(%d) = %d want %d", tc.in, got, tc.want)
		}
	}
}

func TestPutConcurrencyFromEnv(t *testing.T) {
	t.Setenv("STRATA_RADOS_PUT_CONCURRENCY", "")
	if got := putConcurrencyFromEnv(); got != defaultPutConcurrency {
		t.Fatalf("default: got %d want %d", got, defaultPutConcurrency)
	}
	t.Setenv("STRATA_RADOS_PUT_CONCURRENCY", "16")
	if got := putConcurrencyFromEnv(); got != 16 {
		t.Fatalf("explicit 16: got %d", got)
	}
	t.Setenv("STRATA_RADOS_PUT_CONCURRENCY", "9999")
	if got := putConcurrencyFromEnv(); got != maxPutConcurrency {
		t.Fatalf("clamp high: got %d want %d", got, maxPutConcurrency)
	}
	t.Setenv("STRATA_RADOS_PUT_CONCURRENCY", "garbage")
	if got := putConcurrencyFromEnv(); got != defaultPutConcurrency {
		t.Fatalf("garbage falls back to default: got %d", got)
	}
}

// TestPutChunksParallelAcceptsAnyCompletionOrder simulates workers completing
// out of source order to confirm the manifest still ends up in source order.
func TestPutChunksParallelAcceptsAnyCompletionOrder(t *testing.T) {
	const total = 8
	const chunkSize = 64
	src := bytes.Repeat([]byte{'x'}, total*chunkSize)
	// Sleep schedule: even-idx chunks are slow, odd-idx are fast → odd
	// completes first, even later. Manifest must still be in idx order.
	put := func(ctx context.Context, idx int, body []byte) (data.ChunkRef, error) {
		if idx%2 == 0 {
			time.Sleep(15 * time.Millisecond)
		}
		return data.ChunkRef{OID: fmt.Sprintf("oid.%05d", idx), Size: int64(len(body))}, nil
	}
	m, err := putChunksParallelWithChunkSize(context.Background(), bytes.NewReader(src), "STANDARD", 8, chunkSize, put)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Chunks) != total {
		t.Fatalf("chunks: %d want %d", len(m.Chunks), total)
	}
	for i, ref := range m.Chunks {
		want := fmt.Sprintf("oid.%05d", i)
		if ref.OID != want {
			t.Fatalf("chunks[%d].OID = %q want %q (out of source order)", i, ref.OID, want)
		}
	}

	// Sanity: the OIDs are exactly the set 0..total-1.
	got := make([]string, 0, len(m.Chunks))
	for _, ref := range m.Chunks {
		got = append(got, ref.OID)
	}
	want := make([]string, total)
	for i := range total {
		want[i] = fmt.Sprintf("oid.%05d", i)
	}
	sort.Strings(got)
	sort.Strings(want)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("oid set mismatch at %d: got %q want %q", i, got[i], want[i])
		}
	}
}

// erroringReader reads `after` bytes successfully, then returns err on the
// next Read call.
type erroringReader struct {
	body  io.Reader
	after int
	err   error
	read  int
}

func (e *erroringReader) Read(p []byte) (int, error) {
	if e.read >= e.after {
		return 0, e.err
	}
	rem := e.after - e.read
	if rem < len(p) {
		p = p[:rem]
	}
	n, _ := e.body.Read(p)
	e.read += n
	return n, nil
}
