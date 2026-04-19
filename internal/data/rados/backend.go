//go:build ceph

package rados

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"

	goceph "github.com/ceph/go-ceph/rados"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
)

type Backend struct {
	conn    *goceph.Conn
	classes map[string]ClassSpec

	mu      sync.Mutex
	ioctxes map[string]*goceph.IOContext
}

var ErrUnknownStorageClass = errors.New("unknown storage class")

func New(cfg Config) (data.Backend, error) {
	classes := cfg.Classes
	if len(classes) == 0 {
		if cfg.Pool == "" {
			return nil, errors.New("rados: either Classes or Pool must be set")
		}
		classes = map[string]ClassSpec{
			"STANDARD": {Cluster: DefaultCluster, Pool: cfg.Pool, Namespace: cfg.Namespace},
		}
	}

	user := cfg.User
	if user == "" {
		user = "admin"
	}
	conn, err := goceph.NewConnWithUser(user)
	if err != nil {
		return nil, fmt.Errorf("rados: new conn: %w", err)
	}

	if cfg.ConfigFile != "" {
		if err := conn.ReadConfigFile(cfg.ConfigFile); err != nil {
			return nil, fmt.Errorf("rados: read config %s: %w", cfg.ConfigFile, err)
		}
	} else if err := conn.ReadDefaultConfigFile(); err != nil {
		return nil, fmt.Errorf("rados: read default config: %w", err)
	}
	if cfg.Keyring != "" {
		if err := conn.SetConfigOption("keyring", cfg.Keyring); err != nil {
			return nil, fmt.Errorf("rados: set keyring: %w", err)
		}
	}
	if err := conn.Connect(); err != nil {
		return nil, fmt.Errorf("rados: connect: %w", err)
	}
	return &Backend{
		conn:    conn,
		classes: classes,
		ioctxes: make(map[string]*goceph.IOContext),
	}, nil
}

func (b *Backend) resolveClass(class string) (ClassSpec, error) {
	if class == "" {
		class = "STANDARD"
	}
	spec, ok := b.classes[class]
	if !ok {
		return ClassSpec{}, fmt.Errorf("%w: %s", ErrUnknownStorageClass, class)
	}
	return spec, nil
}

func (b *Backend) ioctx(pool, ns string) (*goceph.IOContext, error) {
	key := pool + "|" + ns
	b.mu.Lock()
	defer b.mu.Unlock()
	if x, ok := b.ioctxes[key]; ok {
		return x, nil
	}
	x, err := b.conn.OpenIOContext(pool)
	if err != nil {
		return nil, fmt.Errorf("rados: open ioctx %s: %w", pool, err)
	}
	if ns != "" {
		x.SetNamespace(ns)
	}
	b.ioctxes[key] = x
	return x, nil
}

func (b *Backend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	spec, err := b.resolveClass(class)
	if err != nil {
		return nil, err
	}
	ioctx, err := b.ioctx(spec.Pool, spec.Namespace)
	if err != nil {
		return nil, err
	}
	if class == "" {
		class = "STANDARD"
	}
	m := &data.Manifest{
		Class:     class,
		ChunkSize: data.DefaultChunkSize,
	}
	hash := md5.New()
	buf := make([]byte, data.DefaultChunkSize)
	objID := uuid.NewString()
	idx := 0
	for {
		if err := ctx.Err(); err != nil {
			b.cleanupManifest(m.Chunks)
			return nil, err
		}
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			oid := fmt.Sprintf("%s.%05d", objID, idx)
			chunk := buf[:n]
			if err := writeChunk(ioctx, oid, chunk); err != nil {
				b.cleanupManifest(m.Chunks)
				return nil, err
			}
			hash.Write(chunk)
			m.Chunks = append(m.Chunks, data.ChunkRef{
				Cluster:   spec.Cluster,
				Pool:      spec.Pool,
				Namespace: spec.Namespace,
				OID:       oid,
				Size:      int64(n),
			})
			m.Size += int64(n)
			idx++
		}
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			break
		}
		if rerr != nil {
			b.cleanupManifest(m.Chunks)
			return nil, rerr
		}
	}
	m.ETag = hex.EncodeToString(hash.Sum(nil))
	return m, nil
}

func writeChunk(ioctx *goceph.IOContext, oid string, chunk []byte) error {
	op := goceph.CreateWriteOp()
	defer op.Release()
	op.WriteFull(chunk)
	if err := op.Operate(ioctx, oid, goceph.OperationNoFlag); err != nil {
		return fmt.Errorf("rados: write %s: %w", oid, err)
	}
	return nil
}

func (b *Backend) cleanupManifest(chunks []data.ChunkRef) {
	for _, c := range chunks {
		ioctx, err := b.ioctx(c.Pool, c.Namespace)
		if err != nil {
			continue
		}
		_ = ioctx.Delete(c.OID)
	}
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
	return &radosReader{
		ctx:    ctx,
		b:      b,
		chunks: m.Chunks,
		pos:    offset,
		end:    offset + length,
	}, nil
}

type radosReader struct {
	ctx      context.Context
	b        *Backend
	chunks   []data.ChunkRef
	pos, end int64
	buf      []byte
	bufPos   int
}

func (r *radosReader) Read(p []byte) (int, error) {
	if r.pos >= r.end {
		return 0, io.EOF
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.bufPos >= len(r.buf) {
		if err := r.loadNextChunk(); err != nil {
			return 0, err
		}
	}
	remaining := int(r.end - r.pos)
	avail := len(r.buf) - r.bufPos
	n := len(p)
	if n > avail {
		n = avail
	}
	if n > remaining {
		n = remaining
	}
	copy(p[:n], r.buf[r.bufPos:r.bufPos+n])
	r.bufPos += n
	r.pos += int64(n)
	return n, nil
}

func (r *radosReader) loadNextChunk() error {
	var base int64
	for _, c := range r.chunks {
		if r.pos < base+c.Size {
			ioctx, err := r.b.ioctx(c.Pool, c.Namespace)
			if err != nil {
				return err
			}
			off := r.pos - base
			remaining := c.Size - off
			buf := make([]byte, remaining)
			n, err := ioctx.Read(c.OID, buf, uint64(off))
			if err != nil {
				return fmt.Errorf("rados: read %s: %w", c.OID, err)
			}
			r.buf = buf[:n]
			r.bufPos = 0
			return nil
		}
		base += c.Size
	}
	return io.EOF
}

func (r *radosReader) Close() error { return nil }

func (b *Backend) Delete(_ context.Context, m *data.Manifest) error {
	if m == nil {
		return nil
	}
	var firstErr error
	for _, c := range m.Chunks {
		ioctx, err := b.ioctx(c.Pool, c.Namespace)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := ioctx.Delete(c.OID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, x := range b.ioctxes {
		x.Destroy()
	}
	b.ioctxes = nil
	if b.conn != nil {
		b.conn.Shutdown()
		b.conn = nil
	}
	return nil
}
