// Package cephimpl is the ceph-linked RADOS data backend, split into a
// separate Go module so the main module's go.mod stays free of
// github.com/ceph/go-ceph. Only operators building with librados on the
// host (and the matching docker images in CI) pull in this module; the
// main module's internal/data/rados/stub.go covers the default-tag
// build path.
package cephimpl

import (
	"context"
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
	"github.com/danchupin/strata/internal/data/placement"
	"github.com/danchupin/strata/internal/data/rados"
)

// Backend is the librados-backed implementation of data.Backend.
type Backend struct {
	clusters map[string]rados.ClusterSpec
	classes  map[string]rados.ClassSpec
	logger   *slog.Logger
	metrics  rados.Metrics
	tracer   trace.Tracer

	// putConcurrency caps the per-PutChunks worker pool that dispatches
	// chunk writes to RADOS. Read once at New from
	// STRATA_RADOS_PUT_CONCURRENCY (default 32, clamped to [1, 256]).
	putConcurrency int

	// getPrefetch caps the per-GetChunks prefetch depth: at most this many
	// chunk fetches are in flight while the caller drains the current
	// chunk. Read once at New from STRATA_RADOS_GET_PREFETCH (default 4,
	// clamped to [1, 64]). Memory budget per request is roughly
	// getPrefetch × chunk_size.
	getPrefetch int

	// batchOps toggles the PUT/GET hot path between the per-op default
	// (ioctx.WriteFull-shaped writeChunk + ioctx.Read in getOne) and the
	// WriteOp / ReadOp batched helpers in ops.go.
	batchOps bool

	// backref stamps a self-describing owner pointer (data.BackrefXattrName)
	// on each chunk in the SAME WriteOp as the body (US-001
	// metadata-data-reconcile). Read once at New from
	// STRATA_CHUNK_BACKREF (default on). When on, PutChunks routes through
	// writeChunkBatched regardless of batchOps so the xattr rides one
	// Operate; when off (or the ctx carries no identity) the legacy
	// no-xattr path is byte-identical.
	backref bool

	// poolSize is the conn-pool depth per cluster. Read once at New from
	// STRATA_RADOS_POOL_SIZE (default 1 = legacy single-conn, clamped to
	// [1, MaxPoolSize]).
	poolSize int

	mu    sync.Mutex
	pools map[string]*connPool // key: clusterID
}

// ErrUnknownStorageClass mirrors the original rados-package sentinel.
var ErrUnknownStorageClass = errors.New("unknown storage class")

// New builds a ceph-linked Backend from the same Config shape the main
// rados package exposes. Returned data.Backend identity matches *Backend
// so callers (cmd/strata/workers/rebalance) can type-assert when the
// build tag is set.
func New(cfg rados.Config) (data.Backend, error) {
	classes := cfg.Classes
	if len(classes) == 0 {
		if cfg.Pool == "" && len(cfg.Clusters) == 0 {
			return nil, errors.New("rados: either Classes or Pool must be set")
		}
		if cfg.Pool != "" {
			classes = map[string]rados.ClassSpec{
				"STANDARD": {Cluster: rados.DefaultCluster, Pool: cfg.Pool, Namespace: cfg.Namespace},
			}
		}
	}
	clusters, err := rados.BuildClusters(cfg)
	if err != nil {
		return nil, err
	}
	if err := rados.ValidateClusterRefs(classes, clusters); err != nil {
		return nil, err
	}
	putConc := cfg.PutConcurrency
	if putConc == 0 {
		putConc = rados.PutConcurrencyFromEnv()
	}
	getPre := cfg.GetPrefetch
	if getPre == 0 {
		getPre = rados.GetPrefetchFromEnv()
	}
	poolSize := cfg.PoolSize
	if poolSize == 0 {
		poolSize = rados.PoolSizeFromEnv(cfg.Logger)
	}
	batchOps := cfg.BatchOps
	if !batchOps {
		batchOps = rados.BatchOpsFromEnv()
	}
	return &Backend{
		clusters:       clusters,
		classes:        classes,
		logger:         cfg.Logger,
		metrics:        cfg.Metrics,
		tracer:         cfg.Tracer,
		putConcurrency: putConc,
		getPrefetch:    getPre,
		batchOps:       batchOps,
		backref:        data.BackrefEnabledFromEnv(),
		poolSize:       poolSize,
		pools:          make(map[string]*connPool),
	}, nil
}

// dialCluster opens + connects a fresh *goceph.Conn for one cluster spec.
func dialCluster(spec rados.ClusterSpec) (*goceph.Conn, error) {
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

func (b *Backend) poolFor(ctx context.Context, id string) (*connPool, error) {
	if id == "" {
		id = rados.DefaultCluster
	}
	if p, ok := b.pools[id]; ok {
		return p, nil
	}
	spec, ok := b.clusters[id]
	if !ok {
		return nil, fmt.Errorf("rados: unknown cluster %q", id)
	}
	p, err := newConnPool(spec, b.poolSize)
	if err != nil {
		if b.logger != nil {
			b.logger.InfoContext(ctx, "rados: pool dial failed; retrying once",
				"cluster", id, "pool_size", b.poolSize, "error", err.Error())
		}
		var rerr error
		p, rerr = newConnPool(spec, b.poolSize)
		if rerr != nil {
			return nil, fmt.Errorf("rados: cluster %q pool dial: %w", id, rerr)
		}
	}
	b.pools[id] = p
	return p, nil
}

func (b *Backend) resolveClass(class string) (rados.ClassSpec, error) {
	if class == "" {
		class = "STANDARD"
	}
	spec, ok := b.classes[class]
	if !ok {
		return rados.ClassSpec{}, fmt.Errorf("%w: %s", ErrUnknownStorageClass, class)
	}
	return spec, nil
}

func (b *Backend) ioctx(ctx context.Context, cluster, pool, ns string) (*goceph.IOContext, error) {
	if cluster == "" {
		cluster = rados.DefaultCluster
	}
	b.mu.Lock()
	p, err := b.poolFor(ctx, cluster)
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	x, err := p.IOContext(pool, ns)
	if err != nil {
		return nil, fmt.Errorf("rados: %s/%s: %w", cluster, pool, err)
	}
	return x, nil
}

func (b *Backend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	spec, err := b.resolveClass(class)
	if err != nil {
		return nil, err
	}
	bucketPolicy, _ := data.PlacementFromContext(ctx)
	defaultPolicy, _ := data.DefaultPlacementFromContext(ctx)
	bucketID, _ := data.BucketIDFromContext(ctx)
	objKey, _ := data.ObjectKeyFromContext(ctx)
	draining, _ := data.DrainingClustersFromContext(ctx)
	mode, _ := data.PlacementModeFromContext(ctx)
	states, _ := placement.ClusterStatesFromContext(ctx)
	activePolicy := placement.LiveSubset(bucketPolicy, states)
	if activePolicy == nil {
		strictRefuse := mode == "strict" && len(bucketPolicy) > 0
		switch {
		case strictRefuse:
		case spec.ClusterPinned:
		default:
			activePolicy = placement.LiveSubset(defaultPolicy, states)
		}
	}
	pickCluster := func(idx int) (string, error) {
		picked := placement.PickClusterExcluding(bucketID, objKey, idx, activePolicy, draining)
		if picked == "" {
			if draining[spec.Cluster] {
				return "", data.NewDrainRefusedError(spec.Cluster)
			}
			return spec.Cluster, nil
		}
		if _, ok := b.clusters[picked]; !ok {
			if b.logger != nil {
				b.logger.WarnContext(ctx, "rados placement: picked cluster not configured; falling back",
					"picked", picked, "fallback", spec.Cluster)
			}
			if draining[spec.Cluster] {
				return "", data.NewDrainRefusedError(spec.Cluster)
			}
			return spec.Cluster, nil
		}
		return picked, nil
	}
	objID := uuid.NewString()
	putOne := func(opCtx context.Context, idx int, body []byte) (data.ChunkRef, error) {
		cluster, pickErr := pickCluster(idx)
		if pickErr != nil {
			return data.ChunkRef{}, pickErr
		}
		ioctx, err := b.ioctx(opCtx, cluster, spec.Pool, spec.Namespace)
		if err != nil {
			return data.ChunkRef{}, err
		}
		oid := fmt.Sprintf("%s.%05d", objID, idx)
		start := time.Now()
		var xattrs map[string]string
		if b.backref {
			if a, ok := data.BackrefFromContext(opCtx); ok {
				enc := data.EncodeBackref(data.Backref{
					BucketID:  a.BucketID,
					Key:       a.Key,
					VersionID: a.VersionID,
					ChunkIdx:  idx,
					Mtime:     a.Mtime,
				})
				xattrs = map[string]string{data.BackrefXattrName: string(enc)}
			}
		}
		var werr error
		switch {
		case xattrs != nil:
			// Stamp the back-reference in the SAME WriteOp as the body.
			werr = writeChunkBatched(ioctx, oid, body, xattrs)
		case b.batchOps:
			werr = writeChunkBatched(ioctx, oid, body, nil)
		default:
			werr = writeChunk(ioctx, oid, body)
		}
		rados.ObserveOp(opCtx, b.logger, b.metrics, b.tracer, spec.Pool, "put", oid, start, werr)
		if werr != nil {
			return data.ChunkRef{}, werr
		}
		return data.ChunkRef{
			Cluster:   cluster,
			Pool:      spec.Pool,
			Namespace: spec.Namespace,
			OID:       oid,
			Size:      int64(len(body)),
		}, nil
	}

	m, err := rados.PutChunksParallel(ctx, r, class, b.putConcurrency, putOne)
	if err != nil {
		b.cleanupManifest(ctx, m.Chunks)
		return nil, err
	}
	if ec, ok := data.ECPolicyFromContext(ctx); ok {
		m.ECParams = &data.ECParams{K: ec.K, M: ec.M}
	}
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
	getOne := func(opCtx context.Context, c data.ChunkRef, off uint64, segLen int64) ([]byte, error) {
		ioctx, err := b.ioctx(opCtx, c.Cluster, c.Pool, c.Namespace)
		if err != nil {
			return nil, err
		}
		start := time.Now()
		if b.batchOps {
			body, _, rerr := readChunkBatched(ioctx, c.OID, off, segLen, false)
			rados.ObserveOp(opCtx, b.logger, b.metrics, b.tracer, c.Pool, "get", c.OID, start, rerr)
			if rerr != nil {
				return nil, rerr
			}
			return body, nil
		}
		buf := make([]byte, segLen)
		n, rerr := ioctx.Read(c.OID, buf, off)
		rados.ObserveOp(opCtx, b.logger, b.metrics, b.tracer, c.Pool, "get", c.OID, start, rerr)
		if rerr != nil {
			return nil, fmt.Errorf("rados: read %s: %w", c.OID, rerr)
		}
		return buf[:n], nil
	}
	return rados.NewPrefetchReader(ctx, m, offset, length, b.getPrefetch, getOne)
}

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
		rados.ObserveOp(ctx, b.logger, b.metrics, b.tracer, c.Pool, "del", c.OID, start, derr)
		if derr != nil && errors.Is(derr, goceph.ErrNotFound) {
			derr = fmt.Errorf("chunk %s: %w", c.OID, data.ErrChunkNotFound)
		}
		if derr != nil && firstErr == nil {
			firstErr = derr
		}
	}
	return firstErr
}

func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.pools {
		p.Close()
	}
	b.pools = nil
	return nil
}

// Probe stats a canary OID in the STANDARD-class pool to confirm RADOS is
// reachable. ENOENT (canary missing) still proves connectivity and counts
// as success — only transport / auth errors fail.
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
