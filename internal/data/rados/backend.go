//go:build ceph

package rados

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	goceph "github.com/ceph/go-ceph/rados"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/data"
)

type Backend struct {
	classes map[string]ClassSpec
	logger  *slog.Logger
	metrics Metrics
	tracer  trace.Tracer

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

	mu       sync.Mutex
	clusters map[string]ClusterSpec
	conns    map[string]*goceph.Conn
	ioctxes  map[string]*goceph.IOContext // key: cluster|pool|ns
	closed   bool

	watcher *RegistryWatcher
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
	if err != nil && cfg.Catalog == nil {
		return nil, err
	}
	if clusters == nil {
		clusters = map[string]ClusterSpec{}
	}
	if err := ValidateClusterRefs(classes, clusters); err != nil && cfg.Catalog == nil {
		return nil, err
	}
	b := &Backend{
		clusters:       clusters,
		classes:        classes,
		logger:         cfg.Logger,
		metrics:        cfg.Metrics,
		tracer:         cfg.Tracer,
		putConcurrency: putConcurrencyFromEnv(),
		getPrefetch:    getPrefetchFromEnv(),
		conns:          make(map[string]*goceph.Conn),
		ioctxes:        make(map[string]*goceph.IOContext),
	}
	if cfg.Catalog != nil {
		b.watcher = NewRegistryWatcher(cfg.Catalog, b, cfg.Logger, cfg.RegistryMetrics)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := b.watcher.SyncOnce(ctx); err != nil && cfg.Logger != nil {
			cfg.Logger.WarnContext(ctx, "rados cluster registry initial sync failed; watcher will retry",
				"error", err.Error())
		}
		cancel()
		b.watcher.Start(context.Background())
	}
	return b, nil
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

// ClassesUsingCluster implements data.ClusterReferenceChecker. Returns
// the sorted list of class names whose ClassSpec.Cluster resolves to
// clusterID. Empty id is normalised to DefaultCluster — matches the
// connFor / ioctx routing rule.
func (b *Backend) ClassesUsingCluster(clusterID string) []string {
	if clusterID == "" {
		clusterID = DefaultCluster
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var refs []string
	for name, spec := range b.classes {
		id := spec.Cluster
		if id == "" {
			id = DefaultCluster
		}
		if id == clusterID {
			refs = append(refs, name)
		}
	}
	sort.Strings(refs)
	return refs
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
	objID := uuid.NewString()
	putOne := func(opCtx context.Context, idx int, body []byte) (data.ChunkRef, error) {
		oid := fmt.Sprintf("%s.%05d", objID, idx)
		start := time.Now()
		werr := writeChunk(ioctx, oid, body)
		ObserveOp(opCtx, b.logger, b.metrics, b.tracer, spec.Pool, "put", oid, start, werr)
		if werr != nil {
			return data.ChunkRef{}, werr
		}
		return data.ChunkRef{
			Cluster:   spec.Cluster,
			Pool:      spec.Pool,
			Namespace: spec.Namespace,
			OID:       oid,
			Size:      int64(len(body)),
		}, nil
	}

	m, err := putChunksParallel(ctx, r, class, b.putConcurrency, putOne)
	if err != nil {
		b.cleanupManifest(ctx, m.Chunks)
		return nil, err
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
		buf := make([]byte, segLen)
		start := time.Now()
		n, rerr := ioctx.Read(c.OID, buf, off)
		ObserveOp(opCtx, b.logger, b.metrics, b.tracer, c.Pool, "get", c.OID, start, rerr)
		if rerr != nil {
			return nil, fmt.Errorf("rados: read %s: %w", c.OID, rerr)
		}
		return buf[:n], nil
	}
	return newPrefetchReader(ctx, m, offset, length, b.getPrefetch, getOne)
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
		ObserveOp(ctx, b.logger, b.metrics, b.tracer, c.Pool, "del", c.OID, start, derr)
		if derr != nil && firstErr == nil {
			firstErr = derr
		}
	}
	return firstErr
}

// ReconcileClusters implements ClusterReconciler. Computes a set diff
// against the in-memory cluster catalogue and reconciles dial state:
//   - Added id → insert spec; connFor lazy-dials on next traffic.
//   - Removed id → drop cached conn + ioctxes for the cluster, remove from map.
//   - Updated id (same id, fresh Version OR semantically different spec) →
//     replace + drop cached state so the next traffic re-dials.
//
// Rows whose backend discriminator is not "rados" are skipped (the s3
// watcher consumer will own those once US-007's follow-up cycle lands).
// Rows whose spec is unparseable are dropped with a WARN and treated as if
// absent — the operator must republish a valid spec.
func (b *Backend) ReconcileClusters(ctx context.Context, latest map[string]CatalogEntry) ReconcileSummary {
	next := make(map[string]ClusterSpec, len(latest))
	for id, e := range latest {
		if e.Backend != "" && e.Backend != "rados" {
			continue
		}
		var spec ClusterSpec
		if len(e.Spec) > 0 {
			if err := json.Unmarshal(e.Spec, &spec); err != nil {
				if b.logger != nil {
					b.logger.WarnContext(ctx, "rados cluster registry spec decode failed; skipping",
						"cluster", id, "error", err.Error())
				}
				continue
			}
		}
		spec.ID = id
		next[id] = spec
	}

	var summary ReconcileSummary
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return summary
	}
	for id := range b.clusters {
		if _, ok := next[id]; !ok {
			b.closeClusterLocked(id)
			delete(b.clusters, id)
			summary.Removed++
		}
	}
	for id, spec := range next {
		prev, existed := b.clusters[id]
		if !existed {
			b.clusters[id] = spec
			summary.Added++
			continue
		}
		if prev != spec {
			b.closeClusterLocked(id)
			b.clusters[id] = spec
			summary.Updated++
		}
	}
	return summary
}

// closeClusterLocked drops cached conn + ioctxes for one cluster id. Caller
// must hold b.mu. Idempotent — missing entries are a no-op so concurrent
// Close + reconcile cannot double-destroy.
func (b *Backend) closeClusterLocked(id string) {
	prefix := id + "|"
	for key, x := range b.ioctxes {
		if strings.HasPrefix(key, prefix) {
			x.Destroy()
			delete(b.ioctxes, key)
		}
	}
	if c, ok := b.conns[id]; ok {
		c.Shutdown()
		delete(b.conns, id)
	}
}

// Close stops the cluster-registry watcher and drains the cached
// connection + ioctx pool. Idempotent — double-close is a no-op.
func (b *Backend) Close(context.Context) error {
	if b == nil {
		return nil
	}
	// Stop the watcher BEFORE taking b.mu — its in-flight Reconcile may be
	// blocked on b.mu and Stop() waits for the loop to drain.
	b.watcher.Stop()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
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
