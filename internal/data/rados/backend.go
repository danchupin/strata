//go:build ceph

package rados

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
)

type Backend struct {
	clusters map[string]ClusterSpec
	classes  map[string]ClassSpec
	logger   *slog.Logger
	metrics  Metrics
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
		clusters:       clusters,
		classes:        classes,
		logger:         cfg.Logger,
		metrics:        cfg.Metrics,
		tracer:         cfg.Tracer,
		putConcurrency: putConcurrencyFromEnv(),
		getPrefetch:    getPrefetchFromEnv(),
		conns:          make(map[string]*goceph.Conn),
		ioctxes:        make(map[string]*goceph.IOContext),
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
	// Placement routing (US-003 effective-placement): per-chunk hash-mod
	// stable picker over the EffectivePolicy verdict, which folds three
	// layers:
	//   1. bucket.Placement (data.PlacementFromContext) filtered to live
	//      clusters — wins outright when at least one entry survives.
	//   2. Else if the class has an explicit `@cluster` pin
	//      (spec.ClusterPinned), route to spec.Cluster — class env wins
	//      over default-routing synthesis.
	//   3. Else the synthesised cluster-weight policy
	//      (data.DefaultPlacementFromContext) filtered to live clusters.
	//   4. Else fall back to spec.Cluster (existing strict-refuse
	//      semantic still applies when spec.Cluster is itself draining).
	// PlacementMode toggles the auto-fallback to step 3: mode=strict
	// keeps the bucket pinned to its (now-empty) Placement instead of
	// silently routing via cluster.weights.
	// Pool + namespace inherit from the class spec — operators are expected
	// to use the same pool layout across clusters within one class.
	bucketPolicy, _ := data.PlacementFromContext(ctx)
	defaultPolicy, _ := data.DefaultPlacementFromContext(ctx)
	bucketID, _ := data.BucketIDFromContext(ctx)
	objKey, _ := data.ObjectKeyFromContext(ctx)
	draining, _ := data.DrainingClustersFromContext(ctx)
	mode, _ := data.PlacementModeFromContext(ctx)
	states, _ := placement.ClusterStatesFromContext(ctx)
	// Effective resolution per US-003: bucket Placement's live subset
	// wins; if empty under mode=strict (and bucket had a policy) → no
	// auto-fallback so we surface the "compliance refuse" path through
	// spec.Cluster; if empty under weighted → fall to the class-pin
	// shortcut OR the synthesised cluster-weights policy (live subset).
	activePolicy := placement.LiveSubset(bucketPolicy, states)
	if activePolicy == nil {
		strictRefuse := mode == "strict" && len(bucketPolicy) > 0
		switch {
		case strictRefuse:
			// Leave activePolicy nil — strict + all-drained bucket
			// falls to spec.Cluster (which may strict-refuse if drained).
		case spec.ClusterPinned:
			// Class env wins over default-routing synthesis.
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
		werr := writeChunk(ioctx, oid, body)
		ObserveOp(opCtx, b.logger, b.metrics, b.tracer, spec.Pool, "put", oid, start, werr)
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
