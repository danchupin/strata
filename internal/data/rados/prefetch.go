package rados

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/danchupin/strata/internal/data"
)

const (
	defaultGetPrefetch = 4
	maxGetPrefetch     = 64
)

// chunkGetFn fetches the bytes [off, off+length) of a single chunk from the
// data backend. Implementations are responsible for ioctx resolution and
// per-op observability. The reader dispatches calls to a bounded prefetch
// pool (depth concurrent fetches) and consumes results in source order.
type chunkGetFn func(ctx context.Context, ref data.ChunkRef, off uint64, length int64) ([]byte, error)

// chunkSegment is one slice of a manifest chunk that contributes to the
// requested range. For the first selected chunk, off is the in-chunk offset
// of the first byte to emit; for the last selected chunk, length tracks the
// trailing byte count to read so range GETs do not over-fetch the tail.
type chunkSegment struct {
	ref    data.ChunkRef
	off    uint64
	length int64
}

type getResult struct {
	body []byte
	err  error
}

// prefetchReader implements io.ReadCloser by walking a contiguous slice of
// a manifest with a bounded-concurrency prefetch pool. Up to depth chunk
// fetches are in flight at any moment; the reader emits bytes in strict
// source order regardless of fetch completion order.
type prefetchReader struct {
	ctx    context.Context
	cancel context.CancelFunc

	futures []chan getResult
	sem     chan struct{}
	wg      sync.WaitGroup

	remain int64
	cur    int
	buf    []byte
	bufPos int

	closeOnce sync.Once
}

// newPrefetchReader builds a reader over the bytes [offset, offset+length)
// of the manifest. depth bounds the count of concurrent in-flight chunk
// fetches and the in-flight memory budget (depth × chunk payload). Fetch
// errors propagate to Read; Close cancels in-flight fetches and waits for
// dispatch + fetch goroutines to exit.
func newPrefetchReader(ctx context.Context, m *data.Manifest, offset, length int64, depth int, getOne chunkGetFn) (*prefetchReader, error) {
	if m == nil {
		return nil, errors.New("nil manifest")
	}
	if offset < 0 || offset > m.Size {
		return nil, fmt.Errorf("offset %d out of range (size %d)", offset, m.Size)
	}
	if length <= 0 || offset+length > m.Size {
		length = m.Size - offset
	}
	depth = clampGetPrefetch(depth)

	pr := &prefetchReader{remain: length}
	if length == 0 {
		return pr, nil
	}

	end := offset + length
	var (
		segments []chunkSegment
		base     int64
	)
	for _, c := range m.Chunks {
		chunkEnd := base + c.Size
		if chunkEnd <= offset {
			base = chunkEnd
			continue
		}
		if base >= end {
			break
		}
		readOff := int64(0)
		if base < offset {
			readOff = offset - base
		}
		readEnd := c.Size
		if chunkEnd > end {
			readEnd = end - base
		}
		segments = append(segments, chunkSegment{
			ref:    c,
			off:    uint64(readOff),
			length: readEnd - readOff,
		})
		base = chunkEnd
	}
	if len(segments) == 0 {
		// Manifest reports m.Size that the chunks slice cannot satisfy. Mirror
		// the existing radosReader by returning EOF on first Read instead of
		// failing here.
		return pr, nil
	}

	rctx, cancel := context.WithCancel(ctx)
	futures := make([]chan getResult, len(segments))
	for i := range futures {
		futures[i] = make(chan getResult, 1)
	}
	pr.ctx = rctx
	pr.cancel = cancel
	pr.futures = futures
	pr.sem = make(chan struct{}, depth)

	pr.wg.Add(1)
	go pr.dispatch(rctx, segments, getOne)
	return pr, nil
}

// dispatch launches one fetch goroutine per segment, gating launches on a
// semaphore of capacity depth. The reader releases a token after consuming
// each chunk, so in-flight count + buffered-but-unconsumed count never
// exceeds depth — bounding memory to depth × chunk payload.
func (r *prefetchReader) dispatch(ctx context.Context, segments []chunkSegment, getOne chunkGetFn) {
	defer r.wg.Done()
	for i, seg := range segments {
		select {
		case r.sem <- struct{}{}:
		case <-ctx.Done():
			err := ctx.Err()
			for j := i; j < len(segments); j++ {
				select {
				case r.futures[j] <- getResult{err: err}:
				default:
				}
			}
			return
		}
		idx := i
		s := seg
		r.wg.Go(func() {
			body, err := getOne(ctx, s.ref, s.off, s.length)
			// futures[idx] is buffered size-1 and only ever written here;
			// the send never blocks.
			r.futures[idx] <- getResult{body: body, err: err}
		})
	}
}

func (r *prefetchReader) Read(p []byte) (int, error) {
	if r.remain <= 0 {
		return 0, io.EOF
	}
	if r.ctx != nil {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
	}
	if r.bufPos >= len(r.buf) {
		if r.cur >= len(r.futures) {
			return 0, io.EOF
		}
		var res getResult
		select {
		case res = <-r.futures[r.cur]:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
		if res.err != nil {
			return 0, res.err
		}
		r.buf = res.body
		r.bufPos = 0
		r.cur++
		select {
		case <-r.sem:
		default:
		}
	}
	avail := len(r.buf) - r.bufPos
	n := min(len(p), avail)
	if int64(n) > r.remain {
		n = int(r.remain)
	}
	copy(p[:n], r.buf[r.bufPos:r.bufPos+n])
	r.bufPos += n
	r.remain -= int64(n)
	return n, nil
}

func (r *prefetchReader) Close() error {
	r.closeOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		for i := r.cur; i < len(r.futures); i++ {
			select {
			case <-r.futures[i]:
			default:
			}
		}
		r.wg.Wait()
	})
	return nil
}

// getPrefetchFromEnv reads STRATA_RADOS_GET_PREFETCH and clamps to [1, 64];
// unset or unparseable falls back to defaultGetPrefetch (4).
func getPrefetchFromEnv() int {
	return clampGetPrefetch(intFromEnv("STRATA_RADOS_GET_PREFETCH", defaultGetPrefetch))
}

func clampGetPrefetch(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxGetPrefetch {
		return maxGetPrefetch
	}
	return n
}
