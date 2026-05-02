//go:build ceph

package rados

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	goceph "github.com/ceph/go-ceph/rados"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/data"
)

type Backend struct {
	clusters map[string]ClusterSpec
	classes  map[string]ClassSpec
	logger   *slog.Logger
	metrics  Metrics
	tracer   trace.Tracer

	mu      sync.Mutex
	conns   map[string]*goceph.Conn
	ioctxes map[string]*goceph.IOContext // key: cluster|pool|ns
}

var ErrUnknownStorageClass = errors.New("unknown storage class")

func New(cfg Config) (data.Backend, error) {
	classes := cfg.Classes
	if len(classes) == 0 {
		if cfg.Pool == "" && len(cfg.Clusters) == 0 {
			return nil, errors.New("rados: either Classes or Pool must be set")
		}
		if cfg.Pool != "" {
			classes = map[string]ClassSpec{
				"STANDARD": {Cluster: DefaultCluster, Pool: cfg.Pool, Namespace: cfg.Namespace},
			}
		}
	}
	clusters, err := BuildClusters(cfg)
	if err != nil {
		return nil, err
	}
	if err := ValidateClusterRefs(classes, clusters); err != nil {
		return nil, err
	}
	return &Backend{
		clusters: clusters,
		classes:  classes,
		logger:   cfg.Logger,
		metrics:  cfg.Metrics,
		tracer:   cfg.Tracer,
		conns:    make(map[string]*goceph.Conn),
		ioctxes:  make(map[string]*goceph.IOContext),
	}, nil
}

// dialCluster opens + connects a fresh *goceph.Conn for one cluster spec.
// Lifted out of connFor so it has no lock dependencies.
func dialCluster(spec ClusterSpec) (*goceph.Conn, error) {
	user := spec.User
	if user == "" {
		user = "admin"
	}
	conn, err := goceph.NewConnWithUser(user)
	if err != nil {
		return nil, fmt.Errorf("new conn: %w", err)
	}
	if spec.ConfigFile != "" {
		if err := conn.ReadConfigFile(spec.ConfigFile); err != nil {
			return nil, fmt.Errorf("read config %s: %w", spec.ConfigFile, err)
		}
	} else if err := conn.ReadDefaultConfigFile(); err != nil {
		return nil, fmt.Errorf("read default config: %w", err)
	}
	if spec.Keyring != "" {
		if err := conn.SetConfigOption("keyring", spec.Keyring); err != nil {
			return nil, fmt.Errorf("set keyring: %w", err)
		}
	}
	if err := conn.Connect(); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return conn, nil
}

// connFor lazy-dials the cluster on first use. Caller must hold b.mu.
// Logs INFO + retries once on the first dial failure so a transient
// outage at startup doesn't poison the cached map.
func (b *Backend) connFor(ctx context.Context, id string) (*goceph.Conn, error) {
	if id == "" {
		id = DefaultCluster
	}
	if c, ok := b.conns[id]; ok {
		return c, nil
	}
	spec, ok := b.clusters[id]
	if !ok {
		return nil, fmt.Errorf("rados: unknown cluster %q", id)
	}
	c, err := dialCluster(spec)
	if err != nil {
		if b.logger != nil {
			b.logger.InfoContext(ctx, "rados: cluster dial failed; retrying once",
				"cluster", id, "error", err.Error())
		}
		var rerr error
		c, rerr = dialCluster(spec)
		if rerr != nil {
			return nil, fmt.Errorf("rados: cluster %q dial: %w", id, rerr)
		}
	}
	b.conns[id] = c
	return c, nil
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

func (b *Backend) ioctx(ctx context.Context, cluster, pool, ns string) (*goceph.IOContext, error) {
	if cluster == "" {
		cluster = DefaultCluster
	}
	key := cluster + "|" + pool + "|" + ns
	b.mu.Lock()
	defer b.mu.Unlock()
	if x, ok := b.ioctxes[key]; ok {
		return x, nil
	}
	conn, err := b.connFor(ctx, cluster)
	if err != nil {
		return nil, err
	}
	x, err := conn.OpenIOContext(pool)
	if err != nil {
		return nil, fmt.Errorf("rados: open ioctx %s/%s: %w", cluster, pool, err)
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
	ioctx, err := b.ioctx(ctx, spec.Cluster, spec.Pool, spec.Namespace)
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
			b.cleanupManifest(ctx, m.Chunks)
			return nil, err
		}
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			oid := fmt.Sprintf("%s.%05d", objID, idx)
			chunk := buf[:n]
			start := time.Now()
			werr := writeChunk(ioctx, oid, chunk)
			ObserveOp(ctx, b.logger, b.metrics, b.tracer, spec.Pool, "put", oid, start, werr)
			if werr != nil {
				b.cleanupManifest(ctx, m.Chunks)
				return nil, werr
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
			b.cleanupManifest(ctx, m.Chunks)
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

func (b *Backend) cleanupManifest(ctx context.Context, chunks []data.ChunkRef) {
	for _, c := range chunks {
		ioctx, err := b.ioctx(ctx, c.Cluster, c.Pool, c.Namespace)
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
			ioctx, err := r.b.ioctx(r.ctx, c.Cluster, c.Pool, c.Namespace)
			if err != nil {
				return err
			}
			off := r.pos - base
			remaining := c.Size - off
			buf := make([]byte, remaining)
			start := time.Now()
			n, rerr := ioctx.Read(c.OID, buf, uint64(off))
			ObserveOp(r.ctx, r.b.logger, r.b.metrics, r.b.tracer, c.Pool, "get", c.OID, start, rerr)
			if rerr != nil {
				return fmt.Errorf("rados: read %s: %w", c.OID, rerr)
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

func (b *Backend) Delete(ctx context.Context, m *data.Manifest) error {
	if m == nil {
		return nil
	}
	var firstErr error
	for _, c := range m.Chunks {
		ioctx, err := b.ioctx(ctx, c.Cluster, c.Pool, c.Namespace)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		start := time.Now()
		derr := ioctx.Delete(c.OID)
		ObserveOp(ctx, b.logger, b.metrics, b.tracer, c.Pool, "del", c.OID, start, derr)
		if derr != nil && firstErr == nil {
			firstErr = derr
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
	for _, c := range b.conns {
		c.Shutdown()
	}
	b.conns = nil
	return nil
}

// Probe stats a canary OID in the STANDARD-class pool to confirm RADOS is
// reachable. ENOENT (canary missing) still proves connectivity and counts as
// success — only transport / auth errors fail. Honours ctx via a goroutine
// + select; the underlying librados Stat itself is blocking.
func (b *Backend) Probe(ctx context.Context, oid string) error {
	if b == nil {
		return errors.New("rados backend closed")
	}
	if oid == "" {
		return errors.New("rados probe: oid required")
	}
	spec, err := b.resolveClass("STANDARD")
	if err != nil {
		return err
	}
	ioctx, err := b.ioctx(ctx, spec.Cluster, spec.Pool, spec.Namespace)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		_, sErr := ioctx.Stat(oid)
		if sErr != nil && !errors.Is(sErr, goceph.ErrNotFound) {
			done <- sErr
			return
		}
		done <- nil
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}
