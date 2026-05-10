package rados

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strconv"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/danchupin/strata/internal/data"
)

const (
	defaultPutConcurrency = 32
	maxPutConcurrency     = 256
)

// chunkPutFn writes one chunk's body to the data backend and returns the
// ChunkRef that names it. Implementations are responsible for OID
// derivation, ioctx resolution, and per-op observability. The coordinator
// dispatches calls to a bounded worker pool but feeds chunks to it in
// strict source byte order; idx is the submission index and is unique per
// PutChunks invocation.
type chunkPutFn func(ctx context.Context, idx int, body []byte) (data.ChunkRef, error)

// putChunksParallel splits r into chunkSize-sized buffers and dispatches each
// to putOne via a bounded worker pool of `concurrency` goroutines. The MD5
// hasher is fed chunk bytes in strict source order so the resulting ETag
// matches the byte-stream MD5 regardless of worker completion order. The
// returned manifest's Chunks slice is in source order. On any worker error,
// gctx is cancelled, in-flight workers drain, and the partial Chunks slice
// is returned alongside the error so the caller can run cleanupManifest
// over OIDs that landed in the data tier.
func putChunksParallel(ctx context.Context, r io.Reader, class string, concurrency int, putOne chunkPutFn) (*data.Manifest, error) {
	return putChunksParallelWithChunkSize(ctx, r, class, concurrency, data.DefaultChunkSize, putOne)
}

func putChunksParallelWithChunkSize(ctx context.Context, r io.Reader, class string, concurrency int, chunkSize int64, putOne chunkPutFn) (*data.Manifest, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	if chunkSize <= 0 {
		chunkSize = data.DefaultChunkSize
	}
	if class == "" {
		class = "STANDARD"
	}
	m := &data.Manifest{
		Class:     class,
		ChunkSize: chunkSize,
	}
	hash := md5.New()

	type job struct {
		idx  int
		body []byte
	}
	work := make(chan job)

	g, gctx := errgroup.WithContext(ctx)

	var landedMu sync.Mutex
	var landed []data.ChunkRef
	placeChunk := func(idx int, ref data.ChunkRef) {
		landedMu.Lock()
		defer landedMu.Unlock()
		for len(landed) <= idx {
			landed = append(landed, data.ChunkRef{})
		}
		landed[idx] = ref
	}

	for range concurrency {
		g.Go(func() error {
			for j := range work {
				if err := gctx.Err(); err != nil {
					return err
				}
				ref, err := putOne(gctx, j.idx, j.body)
				if err != nil {
					return err
				}
				placeChunk(j.idx, ref)
			}
			return nil
		})
	}

	g.Go(func() error {
		defer close(work)
		idx := 0
		for {
			if err := gctx.Err(); err != nil {
				return err
			}
			buf := make([]byte, chunkSize)
			n, rerr := io.ReadFull(r, buf)
			if n > 0 {
				chunk := buf[:n]
				hash.Write(chunk)
				m.Size += int64(n)
				select {
				case work <- job{idx: idx, body: chunk}:
				case <-gctx.Done():
					return gctx.Err()
				}
				idx++
			}
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				return nil
			}
			if rerr != nil {
				return rerr
			}
		}
	})

	werr := g.Wait()

	landedMu.Lock()
	final := make([]data.ChunkRef, 0, len(landed))
	for _, ref := range landed {
		if ref.OID != "" {
			final = append(final, ref)
		}
	}
	landedMu.Unlock()
	m.Chunks = final

	if werr != nil {
		return m, werr
	}
	m.ETag = hex.EncodeToString(hash.Sum(nil))
	return m, nil
}

// putConcurrencyFromEnv reads STRATA_RADOS_PUT_CONCURRENCY and clamps to
// [1, 256]; unset or unparseable falls back to defaultPutConcurrency (32).
func putConcurrencyFromEnv() int {
	return clampPutConcurrency(intFromEnv("STRATA_RADOS_PUT_CONCURRENCY", defaultPutConcurrency))
}

func clampPutConcurrency(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxPutConcurrency {
		return maxPutConcurrency
	}
	return n
}

func intFromEnv(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
