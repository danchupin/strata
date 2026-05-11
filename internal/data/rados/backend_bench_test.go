package rados

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
)

// Benchmarks exercise the tag-free schedulers (putChunksParallel +
// newPrefetchReader) against fake chunk-put / chunk-get callbacks that
// inject a per-op latency. Bench parameters mirror the realistic shape an
// operator sees in production: 5 ms per OSD round-trip, 4 MiB chunks,
// 64 MiB and 256 MiB object sizes.
//
// Running: `go test -bench=. -benchmem -run=^$ ./internal/data/rados/...`
// Recorded numbers land in
// `docs/site/content/architecture/benchmarks/parallel-chunks.md`.

const (
	benchPerOp     = 5 * time.Millisecond
	benchChunkSize = int64(4 * 1024 * 1024) // 4 MiB
)

func benchPutLatencyFn(perOp time.Duration) chunkPutFn {
	return func(ctx context.Context, idx int, body []byte) (data.ChunkRef, error) {
		select {
		case <-time.After(perOp):
		case <-ctx.Done():
			return data.ChunkRef{}, ctx.Err()
		}
		return data.ChunkRef{
			Cluster: "default",
			Pool:    "p",
			OID:     fmt.Sprintf("oid.%05d", idx),
			Size:    int64(len(body)),
		}, nil
	}
}

func benchGetLatencyFn(stored map[string][]byte, perOp time.Duration) chunkGetFn {
	return func(ctx context.Context, c data.ChunkRef, off uint64, length int64) ([]byte, error) {
		select {
		case <-time.After(perOp):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		body := stored[c.OID]
		end := min(int64(off)+length, int64(len(body)))
		out := make([]byte, end-int64(off))
		copy(out, body[off:end])
		return out, nil
	}
}

func benchPutChunks(b *testing.B, sizeMiB int, concurrency int) {
	src := make([]byte, sizeMiB*1024*1024)
	put := benchPutLatencyFn(benchPerOp)
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for b.Loop() {
		_, err := putChunksParallelWithChunkSize(
			context.Background(),
			bytes.NewReader(src),
			"STANDARD",
			concurrency,
			benchChunkSize,
			put,
		)
		if err != nil {
			b.Fatalf("putChunksParallel: %v", err)
		}
	}
}

func benchGetChunks(b *testing.B, sizeMiB int, depth int) {
	src := make([]byte, sizeMiB*1024*1024)
	m, stored := chunkedManifest(src, benchChunkSize)
	get := benchGetLatencyFn(stored, benchPerOp)
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for b.Loop() {
		r, err := newPrefetchReader(context.Background(), m, 0, m.Size, depth, get)
		if err != nil {
			b.Fatalf("newPrefetchReader: %v", err)
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			b.Fatalf("read: %v", err)
		}
		_ = r.Close()
	}
}

func BenchmarkPutChunks_64MiB_Sequential(b *testing.B)  { benchPutChunks(b, 64, 1) }
func BenchmarkPutChunks_64MiB_Concurrent(b *testing.B)  { benchPutChunks(b, 64, 32) }
func BenchmarkPutChunks_256MiB_Sequential(b *testing.B) { benchPutChunks(b, 256, 1) }
func BenchmarkPutChunks_256MiB_Concurrent(b *testing.B) { benchPutChunks(b, 256, 32) }

func BenchmarkGetChunks_64MiB_Sequential(b *testing.B)  { benchGetChunks(b, 64, 1) }
func BenchmarkGetChunks_64MiB_Prefetch(b *testing.B)    { benchGetChunks(b, 64, 4) }
func BenchmarkGetChunks_256MiB_Sequential(b *testing.B) { benchGetChunks(b, 256, 1) }
func BenchmarkGetChunks_256MiB_Prefetch(b *testing.B)   { benchGetChunks(b, 256, 4) }
