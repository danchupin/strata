package memory

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
)

type Backend struct {
	mu     sync.RWMutex
	chunks map[string][]byte
}

func New() *Backend {
	return &Backend{chunks: make(map[string][]byte)}
}

// PutChunks ignores the chunk back-reference (US-001 metadata-data-reconcile)
// on ctx: back-references are a data-tier-durability concept (RADOS xattr /
// S3 object metadata) so a reconcile / rebuild can run from data alone. The
// in-memory backend holds chunks in a process-local map with no persistence
// to reconcile, so it is a deliberate no-op.
func (b *Backend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	if class == "" {
		class = "STANDARD"
	}
	m := &data.Manifest{
		Class:     class,
		ChunkSize: data.DefaultChunkSize,
	}
	hash := md5.New()
	buf := make([]byte, data.DefaultChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			oid := fmt.Sprintf("%s.%04d", uuid.NewString(), len(m.Chunks))
			b.mu.Lock()
			b.chunks[oid] = chunk
			b.mu.Unlock()
			hash.Write(chunk)
			m.Chunks = append(m.Chunks, data.ChunkRef{
				Cluster:  "mem",
				Pool:     "mem",
				OID:      oid,
				Size:     int64(n),
				Checksum: data.ComputeChunkCRC(chunk),
			})
			m.Size += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	m.ETag = hex.EncodeToString(hash.Sum(nil))
	if ec, ok := data.ECPolicyFromContext(ctx); ok {
		m.ECParams = &data.ECParams{K: ec.K, M: ec.M}
	}
	return m, nil
}

func (b *Backend) GetChunks(ctx context.Context, m *data.Manifest, offset, length int64) (io.ReadCloser, error) {
	if m == nil {
		return nil, errors.New("nil manifest")
	}
	if offset < 0 || offset > m.Size {
		return nil, fmt.Errorf("offset %d out of range (size %d)", offset, m.Size)
	}
	if length <= 0 || offset+length > m.Size {
		length = m.Size - offset
	}
	return &memReader{b: b, m: m, pos: offset, end: offset + length}, nil
}

func (b *Backend) Delete(ctx context.Context, m *data.Manifest) error {
	if m == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range m.Chunks {
		delete(b.chunks, c.OID)
	}
	return nil
}

func (b *Backend) Close() error { return nil }

// ChunkExists implements data.ChunkStater: reports whether the chunk OID is
// held by the backend. Drives the reconcile dangling-manifest pass (US-003).
func (b *Backend) ChunkExists(ctx context.Context, ref data.ChunkRef) (bool, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.chunks[ref.OID]
	return ok, nil
}

var _ data.ChunkStater = (*Backend)(nil)

// ClusterECCapability satisfies data.ClusterECCapability. The memory
// backend has no underlying erasure-coded pool, so every cluster is
// reported as replicated (US-007 EC-aware manifests).
func (b *Backend) ClusterECCapability(ctx context.Context, clusterID string) (bool, int, int, error) {
	return false, 0, 0, nil
}

// DataHealth implements data.HealthProbe — returns a single in-process pool
// row whose BytesUsed is the sum of currently-held chunk bytes (process-RSS
// proxy) and ChunkCount is the chunk count. Class is "*" since the memory
// backend serves every storage class out of the same map.
func (b *Backend) DataHealth(ctx context.Context) (*data.DataHealthReport, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var bytes uint64
	for _, c := range b.chunks {
		bytes += uint64(len(c))
	}
	return &data.DataHealthReport{
		Backend: "memory",
		Pools: []data.PoolStatus{{
			Name:        "memory",
			Class:       "*",
			Cluster:     "",
			BytesUsed:   bytes,
			ChunkCount:  uint64(len(b.chunks)),
			NumReplicas: 1,
			State:       "reachable",
		}},
	}, nil
}

var _ data.HealthProbe = (*Backend)(nil)

// ChunkOIDs returns the set of OIDs currently held by the backend. Test-only
// helper used by the race harness to assert no chunk lands outside a manifest
// or the GC queue.
func (b *Backend) ChunkOIDs() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.chunks))
	for oid := range b.chunks {
		out = append(out, oid)
	}
	return out
}

// CorruptFirstChunk flips a byte in an arbitrary stored chunk and returns
// true. Test-only helper used by SSE round-trip tests to simulate at-rest
// tampering; returns false if there are no chunks. NOTE: chunks is a map, so
// "first" is whichever chunk map iteration yields — non-deterministic. For a
// test that must corrupt a SPECIFIC chunk (e.g. the one a range window
// touches), use CorruptChunkByOID instead.
func (b *Backend) CorruptFirstChunk() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for oid, data := range b.chunks {
		if len(data) == 0 {
			continue
		}
		data[0] ^= 0xFF
		b.chunks[oid] = data
		return true
	}
	return false
}

// CorruptChunkByOID deterministically flips a byte in the chunk named by oid
// and returns true; false if the OID is absent or the chunk is empty.
// Test-only helper for CRC tests that must corrupt the exact chunk a read
// window covers (US-009 range-boundary verification).
func (b *Backend) CorruptChunkByOID(oid string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.chunks[oid]
	if !ok || len(data) == 0 {
		return false
	}
	data[0] ^= 0xFF
	b.chunks[oid] = data
	return true
}

type memReader struct {
	b        *Backend
	m        *data.Manifest
	pos, end int64
	cur      *bytes.Reader
	curIdx   int
	curBase  int64
}

func (r *memReader) Read(p []byte) (int, error) {
	if r.pos >= r.end {
		return 0, io.EOF
	}
	if r.cur == nil || r.cur.Len() == 0 {
		if err := r.seekToChunk(); err != nil {
			return 0, err
		}
	}
	remaining := r.end - r.pos
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.cur.Read(p)
	r.pos += int64(n)
	if err == io.EOF && r.pos < r.end {
		err = nil
	}
	return n, err
}

func (r *memReader) seekToChunk() error {
	var base int64
	for i, c := range r.m.Chunks {
		if r.pos < base+c.Size {
			r.b.mu.RLock()
			chunkBytes, ok := r.b.chunks[c.OID]
			r.b.mu.RUnlock()
			if !ok {
				return fmt.Errorf("chunk %s missing", c.OID)
			}
			// US-009: verify the WHOLE chunk against its stored CRC32C
			// before slicing the range window out of it (option a:
			// full-chunk verification on range boundaries). VerifyChunk
			// is a no-op for legacy zero-checksum rows or when disabled.
			if err := data.VerifyChunk(c, chunkBytes); err != nil {
				return err
			}
			off := r.pos - base
			r.cur = bytes.NewReader(chunkBytes[off:])
			r.curIdx = i
			r.curBase = base
			return nil
		}
		base += c.Size
	}
	return io.EOF
}

func (r *memReader) Close() error { return nil }
