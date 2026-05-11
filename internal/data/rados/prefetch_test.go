package rados

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
)

// chunkedManifest builds an in-memory manifest from src by splitting it into
// chunkSize-sized pieces, plus a stored map keyed by OID for the test
// chunkGetFn to read from.
func chunkedManifest(src []byte, chunkSize int64) (*data.Manifest, map[string][]byte) {
	stored := make(map[string][]byte)
	m := &data.Manifest{Size: int64(len(src)), ChunkSize: chunkSize}
	idx := 0
	for off := int64(0); off < int64(len(src)); off += chunkSize {
		hi := off + chunkSize
		if hi > int64(len(src)) {
			hi = int64(len(src))
		}
		oid := fmt.Sprintf("oid.%05d", idx)
		stored[oid] = append([]byte(nil), src[off:hi]...)
		m.Chunks = append(m.Chunks, data.ChunkRef{
			Cluster: "default",
			Pool:    "p",
			OID:     oid,
			Size:    hi - off,
		})
		idx++
	}
	return m, stored
}

func storedGetFn(stored map[string][]byte) chunkGetFn {
	return func(_ context.Context, c data.ChunkRef, off uint64, length int64) ([]byte, error) {
		body, ok := stored[c.OID]
		if !ok {
			return nil, fmt.Errorf("missing %s", c.OID)
		}
		end := int64(off) + length
		if end > int64(len(body)) {
			end = int64(len(body))
		}
		return append([]byte(nil), body[off:end]...), nil
	}
}

func TestPrefetchReaderFullRead(t *testing.T) {
	const chunkSize = 64
	src := bytes.Repeat([]byte{'A'}, chunkSize)
	src = append(src, bytes.Repeat([]byte{'B'}, chunkSize)...)
	src = append(src, bytes.Repeat([]byte{'C'}, chunkSize)...)
	src = append(src, []byte("tail-bytes")...)
	m, stored := chunkedManifest(src, chunkSize)

	r, err := newPrefetchReader(context.Background(), m, 0, m.Size, 4, storedGetFn(stored))
	if err != nil {
		t.Fatalf("newPrefetchReader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Fatalf("byte mismatch: got %d bytes (first 16 = %q) want %d", len(got), got[:min(16, len(got))], len(src))
	}
}

func TestPrefetchReaderRangeGet(t *testing.T) {
	const chunkSize = 32
	const chunks = 5
	src := make([]byte, chunkSize*chunks)
	for i := range src {
		src[i] = byte(i)
	}
	m, stored := chunkedManifest(src, chunkSize)

	cases := []struct {
		name           string
		offset, length int64
	}{
		{"full", 0, m.Size},
		{"first-chunk-only", 0, chunkSize},
		{"mid-chunk-prefix", 16, chunkSize / 2},
		{"crosses-boundary", 24, 24}, // last 8 of chunk 0 + first 16 of chunk 1
		{"single-byte", 17, 1},
		{"trailing-tail", chunkSize * 3, chunkSize * 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := newPrefetchReader(context.Background(), m, tc.offset, tc.length, 4, storedGetFn(stored))
			if err != nil {
				t.Fatalf("newPrefetchReader: %v", err)
			}
			t.Cleanup(func() { _ = r.Close() })
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			want := src[tc.offset : tc.offset+tc.length]
			if !bytes.Equal(got, want) {
				t.Fatalf("bytes mismatch:\n got  %v\n want %v", got, want)
			}
		})
	}
}

// TestPrefetchReaderGoesParallel: with per-chunk fetch latency injected,
// wall-clock for a 32-chunk full-read at depth=4 should be well below 32×
// single-chunk latency (the sequential ceiling).
func TestPrefetchReaderGoesParallel(t *testing.T) {
	const chunkSize = 64
	const chunks = 32
	const perOp = 5 * time.Millisecond

	src := bytes.Repeat([]byte{'x'}, chunks*chunkSize)
	m, stored := chunkedManifest(src, chunkSize)
	get := func(ctx context.Context, c data.ChunkRef, off uint64, length int64) ([]byte, error) {
		select {
		case <-time.After(perOp):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		body := stored[c.OID]
		end := int64(off) + length
		if end > int64(len(body)) {
			end = int64(len(body))
		}
		return append([]byte(nil), body[off:end]...), nil
	}

	start := time.Now()
	r, err := newPrefetchReader(context.Background(), m, 0, m.Size, 4, get)
	if err != nil {
		t.Fatalf("newPrefetchReader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	got, err := io.ReadAll(r)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != len(src) {
		t.Fatalf("length: got %d want %d", len(got), len(src))
	}
	// Sequential lower bound is chunks × perOp = 160 ms; depth=4 should
	// finish near ceil(32/4) × perOp = 40 ms. Assert < half-sequential
	// = 80 ms with generous slack for scheduler jitter.
	if elapsed >= time.Duration(chunks)*perOp/2 {
		t.Fatalf("wall-clock %v suggests sequential fetch (expected ≪ %v)", elapsed, time.Duration(chunks)*perOp)
	}
}

// TestPrefetchReaderClientCancel: closing the reader mid-stream cancels
// in-flight fetches and lets goroutines exit within a bounded window.
func TestPrefetchReaderClientCancel(t *testing.T) {
	const chunkSize = 64
	const chunks = 32
	src := bytes.Repeat([]byte{'x'}, chunks*chunkSize)
	m, stored := chunkedManifest(src, chunkSize)

	var inflight atomic.Int32
	get := func(ctx context.Context, c data.ChunkRef, off uint64, length int64) ([]byte, error) {
		inflight.Add(1)
		defer inflight.Add(-1)
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		body := stored[c.OID]
		end := int64(off) + length
		if end > int64(len(body)) {
			end = int64(len(body))
		}
		return append([]byte(nil), body[off:end]...), nil
	}

	baseline := runtime.NumGoroutine()
	r, err := newPrefetchReader(context.Background(), m, 0, m.Size, 4, get)
	if err != nil {
		t.Fatalf("newPrefetchReader: %v", err)
	}
	// Drain a small prefix so dispatcher is mid-flight.
	prefix := make([]byte, chunkSize/2)
	if _, err := io.ReadFull(r, prefix); err != nil {
		t.Fatalf("ReadFull prefix: %v", err)
	}

	closeStart := time.Now()
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closeDur := time.Since(closeStart)
	if closeDur > 500*time.Millisecond {
		t.Fatalf("Close took %v (>500ms — goroutines did not exit promptly)", closeDur)
	}

	// Allow up to 100ms for scheduler to reclaim exited goroutines, then
	// assert delta returns to ~baseline (allow small slack for transient
	// runtime goroutines unrelated to the prefetcher).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine()-baseline <= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 2 {
		t.Fatalf("goroutine leak: baseline=%d now=%d delta=%d", baseline, runtime.NumGoroutine(), delta)
	}
	if inflight.Load() != 0 {
		t.Fatalf("inflight fetches: %d (expected 0 after Close)", inflight.Load())
	}
}

func TestPrefetchReaderFetchError(t *testing.T) {
	const chunkSize = 64
	src := bytes.Repeat([]byte{'x'}, chunkSize*4)
	m, _ := chunkedManifest(src, chunkSize)
	boom := errors.New("rados ENOENT")
	get := func(_ context.Context, c data.ChunkRef, _ uint64, _ int64) ([]byte, error) {
		if c.OID == "oid.00002" {
			return nil, boom
		}
		return bytes.Repeat([]byte{'x'}, int(c.Size)), nil
	}
	r, err := newPrefetchReader(context.Background(), m, 0, m.Size, 4, get)
	if err != nil {
		t.Fatalf("newPrefetchReader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	_, err = io.ReadAll(r)
	if !errors.Is(err, boom) {
		t.Fatalf("ReadAll: got %v want %v", err, boom)
	}
}

func TestPrefetchReaderHonoursParentCtxCancel(t *testing.T) {
	const chunkSize = 64
	src := bytes.Repeat([]byte{'x'}, chunkSize*8)
	m, stored := chunkedManifest(src, chunkSize)
	ctx, cancel := context.WithCancel(context.Background())

	get := func(opCtx context.Context, c data.ChunkRef, off uint64, length int64) ([]byte, error) {
		select {
		case <-time.After(50 * time.Millisecond):
		case <-opCtx.Done():
			return nil, opCtx.Err()
		}
		body := stored[c.OID]
		end := int64(off) + length
		if end > int64(len(body)) {
			end = int64(len(body))
		}
		return append([]byte(nil), body[off:end]...), nil
	}

	r, err := newPrefetchReader(ctx, m, 0, m.Size, 4, get)
	if err != nil {
		t.Fatalf("newPrefetchReader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err = io.ReadAll(r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadAll: got %v want context.Canceled", err)
	}
}

// TestPrefetchReaderEmptyRange: zero-length range or empty manifest returns
// EOF immediately and produces no fetches.
func TestPrefetchReaderEmptyRange(t *testing.T) {
	const chunkSize = 32
	src := bytes.Repeat([]byte{'x'}, chunkSize*2)
	m, _ := chunkedManifest(src, chunkSize)

	called := atomic.Int32{}
	get := func(context.Context, data.ChunkRef, uint64, int64) ([]byte, error) {
		called.Add(1)
		return nil, errors.New("should not fetch")
	}

	r, err := newPrefetchReader(context.Background(), m, m.Size, 0, 4, get)
	if err != nil {
		t.Fatalf("newPrefetchReader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 bytes, got %d", len(got))
	}
	if called.Load() != 0 {
		t.Fatalf("getOne called %d times for empty range", called.Load())
	}
}

// TestPrefetchReaderOutOfRangeOffset: offset > m.Size returns an error.
func TestPrefetchReaderOutOfRangeOffset(t *testing.T) {
	m, stored := chunkedManifest([]byte("hello world"), 4)
	_, err := newPrefetchReader(context.Background(), m, 100, 1, 4, storedGetFn(stored))
	if err == nil {
		t.Fatal("expected error for offset > m.Size")
	}
}

// TestPrefetchReaderRangeStopsBeforeUnneededChunks: with a range that stops
// before the manifest tail, fetches must be limited to the touched chunks
// only. Validates "prefetch window stops at the chunk containing
// offset+length" from the PRD.
func TestPrefetchReaderRangeStopsBeforeUnneededChunks(t *testing.T) {
	const chunkSize = 32
	const chunks = 8
	src := bytes.Repeat([]byte{'x'}, chunks*chunkSize)
	m, stored := chunkedManifest(src, chunkSize)

	var fetched atomic.Int32
	get := func(ctx context.Context, c data.ChunkRef, off uint64, length int64) ([]byte, error) {
		fetched.Add(1)
		body := stored[c.OID]
		end := int64(off) + length
		if end > int64(len(body)) {
			end = int64(len(body))
		}
		return append([]byte(nil), body[off:end]...), nil
	}
	// Range covers chunks 1 + 2 (indices) only.
	r, err := newPrefetchReader(context.Background(), m, chunkSize, chunkSize*2, 4, get)
	if err != nil {
		t.Fatalf("newPrefetchReader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if got := fetched.Load(); got != 2 {
		t.Fatalf("fetched %d chunks, want 2 (range was strictly within chunks 1+2)", got)
	}
}

func TestClampGetPrefetch(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{-5, 1},
		{0, 1},
		{1, 1},
		{4, 4},
		{64, 64},
		{1000, 64},
	}
	for _, tc := range cases {
		if got := clampGetPrefetch(tc.in); got != tc.want {
			t.Errorf("clampGetPrefetch(%d) = %d want %d", tc.in, got, tc.want)
		}
	}
}

func TestGetPrefetchFromEnv(t *testing.T) {
	t.Setenv("STRATA_RADOS_GET_PREFETCH", "")
	if got := getPrefetchFromEnv(); got != defaultGetPrefetch {
		t.Fatalf("default: got %d want %d", got, defaultGetPrefetch)
	}
	t.Setenv("STRATA_RADOS_GET_PREFETCH", "8")
	if got := getPrefetchFromEnv(); got != 8 {
		t.Fatalf("explicit 8: got %d", got)
	}
	t.Setenv("STRATA_RADOS_GET_PREFETCH", "9999")
	if got := getPrefetchFromEnv(); got != maxGetPrefetch {
		t.Fatalf("clamp high: got %d want %d", got, maxGetPrefetch)
	}
	t.Setenv("STRATA_RADOS_GET_PREFETCH", "garbage")
	if got := getPrefetchFromEnv(); got != defaultGetPrefetch {
		t.Fatalf("garbage falls back to default: got %d", got)
	}
}

// TestPrefetchReaderMemoryBoundedBacked: the inflight count never exceeds
// depth even when the producer is fast and the consumer is slow.
func TestPrefetchReaderMemoryBounded(t *testing.T) {
	const chunkSize = 64
	const chunks = 32
	const depth = 4
	src := bytes.Repeat([]byte{'x'}, chunks*chunkSize)
	m, stored := chunkedManifest(src, chunkSize)

	var inflight atomic.Int32
	var maxInflight atomic.Int32
	get := func(_ context.Context, c data.ChunkRef, off uint64, length int64) ([]byte, error) {
		now := inflight.Add(1)
		for {
			cur := maxInflight.Load()
			if now <= cur || maxInflight.CompareAndSwap(cur, now) {
				break
			}
		}
		defer inflight.Add(-1)
		// Fast fetch — body returns immediately.
		body := stored[c.OID]
		return append([]byte(nil), body[off:int64(off)+length]...), nil
	}

	r, err := newPrefetchReader(context.Background(), m, 0, m.Size, depth, get)
	if err != nil {
		t.Fatalf("newPrefetchReader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	// Slow consumer: read 1 byte at a time with a tiny pause so producer
	// would overrun if unbounded.
	buf := make([]byte, 1)
	for {
		_, err := r.Read(buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		time.Sleep(time.Microsecond)
	}
	// maxInflight reflects concurrent get() calls. The semaphore (capacity
	// depth) gates dispatcher launches; reader releases tokens after
	// consuming each chunk. Inflight never exceeds depth.
	if got := maxInflight.Load(); int(got) > depth {
		t.Fatalf("maxInflight=%d exceeds depth=%d", got, depth)
	}
}
