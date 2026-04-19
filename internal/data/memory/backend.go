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
				Cluster: "mem",
				Pool:    "mem",
				OID:     oid,
				Size:    int64(n),
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
			data, ok := r.b.chunks[c.OID]
			r.b.mu.RUnlock()
			if !ok {
				return fmt.Errorf("chunk %s missing", c.OID)
			}
			off := r.pos - base
			r.cur = bytes.NewReader(data[off:])
			r.curIdx = i
			r.curBase = base
			return nil
		}
		base += c.Size
	}
	return io.EOF
}

func (r *memReader) Close() error { return nil }
