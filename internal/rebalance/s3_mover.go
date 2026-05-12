// Package rebalance — S3-side mover (US-005).
//
// Mirrors the RADOS mover shape: librados-free interfaces, in-package
// type-asserted by unit tests against fakes, real minio/aws S3 traffic
// handled by the facade in internal/data/s3/rebalance.go. One S3Mover
// owns the whole S3 cluster family registered on the underlying
// s3.Backend.
//
// Move(plan) groups the plan by (BucketID, ObjectKey, VersionID) using
// the shared groupMovesByObject helper, copies each "chunk" — for
// BackendRef-shape manifests each Strata object is a single virtual
// chunk — onto a freshly-minted key on the target cluster, then issues
// a single manifest CAS per object via meta.Store.SetObjectStorage. On
// CAS success the old (source) backend object lands in the GC queue as
// a chunk-shape entry (Cluster/Pool/OID encode the (cluster, bucket,
// key) tuple); the s3 backend's Delete dispatches chunk-shape entries
// with a non-empty Cluster to the matching cluster. On CAS reject the
// new (target) backend object lands in the GC queue instead.
//
// Endpoint+region match short-circuits to awss3.CopyObject (server-side
// copy — no bytes through the rebalance process). Endpoint/region
// mismatch falls back to GetObject → manager.Uploader.Upload via an
// io.Pipe so the body never fully materialises in memory.
package rebalance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	strataotel "github.com/danchupin/strata/internal/otel"
)

// S3Cluster is the minimum surface the S3 mover needs from one S3
// cluster. The s3.Backend implements it via a thin per-cluster facade
// returned by s3.RebalanceClusters; unit tests can plug an in-memory
// fake. Endpoint + Region drive the same-endpoint short-circuit (where
// awss3.CopyObject is cheaper than Get+Put). Get/Put/Copy are scoped to
// (bucket, key) because the source bucket and target bucket may differ
// (per-cluster bucket routing in s3.ClassSpec).
type S3Cluster interface {
	ID() string
	Endpoint() string
	Region() string
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, int64, error)
	Put(ctx context.Context, bucket, key string, body io.Reader, size int64) error
	Copy(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) error
}

// BucketResolver maps (storage class, target clusterID) to the bucket
// name on that cluster. The s3.Backend.BucketOnCluster method matches
// this signature; unit tests can plug a literal map lookup.
type BucketResolver func(class, clusterID string) string

// S3Mover executes a plan slice against a set of S3 clusters. Clusters
// is keyed by S3ClusterSpec.ID; Meta is the shared metadata store;
// Region is the GC partition the old (or losing new) keys land in.
type S3Mover struct {
	Clusters      map[string]S3Cluster
	BucketBy      BucketResolver
	Meta          meta.Store
	Region        string
	Logger        *slog.Logger
	Metrics       MoverMetrics
	Tracer        trace.Tracer
	Throttle      *Throttle
	Inflight      int
	// KeyMint synthesises the target backend key. Defaults to a
	// uuid-prefixed shape mirroring s3.Backend.objectKey. Exposed for
	// tests so they can assert determinism.
	KeyMint func(bucketID [16]byte) string
}

// Owns reports whether target is one of the mover's known clusters.
func (m *S3Mover) Owns(target string) bool {
	if m == nil {
		return false
	}
	_, ok := m.Clusters[target]
	return ok
}

// Move batches plan by (BucketID, ObjectKey, VersionID) and dispatches
// each object group through a bounded errgroup. ctx cancellation drains
// in-flight groups. Per-group failures log + drop the affected object;
// the worker will replan next tick.
func (m *S3Mover) Move(ctx context.Context, plan []Move) error {
	if m == nil || len(plan) == 0 {
		return nil
	}
	if m.Meta == nil {
		return errors.New("rebalance: s3 mover meta required")
	}
	logger := m.Logger
	if logger == nil {
		logger = slog.Default()
	}
	metrics := m.Metrics
	if metrics == nil {
		metrics = nopMoverMetrics{}
	}
	tracer := m.Tracer
	if tracer == nil {
		tracer = strataotel.NoopTracer()
	}
	region := m.Region
	if region == "" {
		region = "default"
	}
	mint := m.KeyMint
	if mint == nil {
		mint = defaultS3KeyMint
	}

	groups := groupMovesByObject(plan)

	inflight := m.Inflight
	if inflight <= 0 {
		inflight = 1
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(inflight)
	for _, group := range groups {
		g.Go(func() error {
			if gctx.Err() != nil {
				return gctx.Err()
			}
			m.moveObject(gctx, logger, metrics, tracer, region, mint, group)
			return nil
		})
	}
	return g.Wait()
}

func (m *S3Mover) moveObject(ctx context.Context, logger *slog.Logger, metrics MoverMetrics, tracer trace.Tracer, region string, mint func([16]byte) string, group []Move) {
	if len(group) == 0 {
		return
	}
	head := group[0]
	bucketID := uuid.UUID(head.BucketID)

	copies, err := m.copyObjects(ctx, logger, metrics, tracer, mint, group)
	if err != nil {
		logger.WarnContext(ctx, "rebalance: copy backend objects aborted",
			"bucket", head.Bucket, "key", head.ObjectKey, "error", err.Error())
		m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, newRefsFromS3(copies))
		return
	}
	if len(copies) == 0 {
		return
	}

	obj, err := m.Meta.GetObject(ctx, bucketID, head.ObjectKey, head.VersionID)
	if err != nil {
		logger.WarnContext(ctx, "rebalance: fetch manifest",
			"bucket", head.Bucket, "key", head.ObjectKey, "error", err.Error())
		m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, newRefsFromS3(copies))
		return
	}
	if obj == nil || obj.Manifest == nil {
		logger.InfoContext(ctx, "rebalance: object disappeared mid-flight, discarding new backend objects",
			"bucket", head.Bucket, "key", head.ObjectKey)
		m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, newRefsFromS3(copies))
		return
	}

	updated, oldRef, ok := buildUpdatedBackendManifest(obj.Manifest, copies[0])
	if !ok {
		metrics.IncCASConflict(head.Bucket)
		m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, newRefsFromS3(copies))
		return
	}

	expectedClass := obj.StorageClass
	if expectedClass == "" {
		expectedClass = head.Class
	}
	applied, err := m.Meta.SetObjectStorage(ctx, bucketID, head.ObjectKey, obj.VersionID, expectedClass, expectedClass, updated)
	if err != nil {
		logger.WarnContext(ctx, "rebalance: manifest CAS",
			"bucket", head.Bucket, "key", head.ObjectKey, "error", err.Error())
		m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, newRefsFromS3(copies))
		return
	}
	if !applied {
		metrics.IncCASConflict(head.Bucket)
		logger.InfoContext(ctx, "rebalance: CAS conflict, discarding new backend objects",
			"bucket", head.Bucket, "key", head.ObjectKey)
		m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, newRefsFromS3(copies))
		return
	}
	for _, c := range copies {
		metrics.IncChunksMoved(c.move.FromCluster, c.move.ToCluster, head.Bucket)
	}
	m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, []data.ChunkRef{oldRef})
}

// s3Copy carries one successfully-copied backend object: the original
// plan entry (for label + manifest-match), the source + target bucket
// names (resolved at copy time so the GC enqueue and manifest update
// don't have to re-query the BucketResolver), and the freshly-minted
// target key.
type s3Copy struct {
	move      Move
	srcBucket string
	dstBucket string
	dstKey    string
	size      int64
}

func (m *S3Mover) copyObjects(ctx context.Context, logger *slog.Logger, metrics MoverMetrics, tracer trace.Tracer, mint func([16]byte) string, group []Move) ([]s3Copy, error) {
	copies := make([]s3Copy, 0, len(group))
	var copiesMu sync.Mutex

	inflight := m.Inflight
	if inflight <= 0 {
		inflight = 1
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(inflight)
	for _, mv := range group {
		g.Go(func() error {
			if gctx.Err() != nil {
				return gctx.Err()
			}
			cp, err := m.copyOne(gctx, metrics, tracer, mint, mv)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				logger.WarnContext(gctx, "rebalance: copy backend object",
					"bucket", mv.Bucket, "key", mv.ObjectKey,
					"from", mv.FromCluster, "to", mv.ToCluster, "error", err.Error())
				return nil
			}
			copiesMu.Lock()
			copies = append(copies, cp)
			copiesMu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return copies, err
	}
	return copies, nil
}

func (m *S3Mover) copyOne(ctx context.Context, metrics MoverMetrics, tracer trace.Tracer, mint func([16]byte) string, mv Move) (s3Copy, error) {
	ctx, span := tracer.Start(ctx, "rebalance.move_chunk",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			strataotel.AttrComponentWorker,
			attribute.String(strataotel.WorkerKey, "rebalance"),
			attribute.String("strata.rebalance.bucket", mv.Bucket),
			attribute.String("strata.rebalance.key", mv.ObjectKey),
			attribute.String("strata.rebalance.from", mv.FromCluster),
			attribute.String("strata.rebalance.to", mv.ToCluster),
			attribute.Int("strata.rebalance.chunk_idx", mv.ChunkIdx),
		),
	)
	defer span.End()

	src, ok := m.Clusters[mv.FromCluster]
	if !ok {
		err := fmt.Errorf("source cluster %q not configured", mv.FromCluster)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return s3Copy{}, err
	}
	tgt, ok := m.Clusters[mv.ToCluster]
	if !ok {
		err := fmt.Errorf("target cluster %q not configured", mv.ToCluster)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return s3Copy{}, err
	}

	srcBucket := m.bucket(mv.Class, mv.FromCluster)
	if srcBucket == "" {
		// Fall back to SrcRef.Pool — the rebalance worker plants the
		// source bucket name there for S3 manifests (see worker.go).
		srcBucket = mv.SrcRef.Pool
	}
	dstBucket := m.bucket(mv.Class, mv.ToCluster)
	if dstBucket == "" {
		err := fmt.Errorf("no bucket registered on cluster %q for class %q", mv.ToCluster, mv.Class)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return s3Copy{}, err
	}
	dstKey := mint(mv.BucketID)

	if err := m.Throttle.Wait(ctx, mv.SrcRef.Size); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return s3Copy{}, err
	}

	// Same endpoint+region → server-side copy (zero bytes through the
	// rebalance process). Else stream Get from src into Put on tgt.
	if sameEndpoint(src, tgt) {
		if err := src.Copy(ctx, srcBucket, mv.SrcRef.OID, dstBucket, dstKey); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return s3Copy{}, fmt.Errorf("copy %s/%s -> %s/%s: %w", srcBucket, mv.SrcRef.OID, dstBucket, dstKey, err)
		}
		metrics.IncBytesMoved(mv.FromCluster, mv.ToCluster, mv.SrcRef.Size)
		return s3Copy{move: mv, srcBucket: srcBucket, dstBucket: dstBucket, dstKey: dstKey, size: mv.SrcRef.Size}, nil
	}

	body, size, err := src.Get(ctx, srcBucket, mv.SrcRef.OID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return s3Copy{}, fmt.Errorf("get %s/%s: %w", srcBucket, mv.SrcRef.OID, err)
	}
	defer body.Close()
	if err := m.Throttle.Wait(ctx, size); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return s3Copy{}, err
	}
	if err := tgt.Put(ctx, dstBucket, dstKey, body, size); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return s3Copy{}, fmt.Errorf("put %s/%s: %w", dstBucket, dstKey, err)
	}
	metrics.IncBytesMoved(mv.FromCluster, mv.ToCluster, size)
	return s3Copy{move: mv, srcBucket: srcBucket, dstBucket: dstBucket, dstKey: dstKey, size: size}, nil
}

func (m *S3Mover) bucket(class, clusterID string) string {
	if m.BucketBy == nil {
		return ""
	}
	return m.BucketBy(class, clusterID)
}

func (m *S3Mover) enqueueGC(ctx context.Context, region, bucket, key string, refs []data.ChunkRef) {
	if len(refs) == 0 {
		return
	}
	if err := m.Meta.EnqueueChunkDeletion(ctx, region, refs); err != nil {
		logger := m.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.WarnContext(ctx, "rebalance: enqueue GC",
			"bucket", bucket, "key", key, "chunks", len(refs), "error", err.Error())
	}
}

func sameEndpoint(a, b S3Cluster) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Endpoint() == b.Endpoint() && a.Region() == b.Region()
}

// buildUpdatedBackendManifest returns a clone of `live` with BackendRef
// rewritten to point at the freshly-written target object. ok=false
// when the live manifest no longer matches the plan (BackendRef gone,
// or Key/Cluster has been rewritten by a concurrent client) — the
// caller treats this as a CAS conflict. oldRef returns the pre-update
// (cluster, bucket, key) tuple as a ChunkRef so the caller can enqueue
// it into the GC queue once the CAS lands.
func buildUpdatedBackendManifest(live *data.Manifest, cp s3Copy) (*data.Manifest, data.ChunkRef, bool) {
	if live == nil || live.BackendRef == nil {
		return nil, data.ChunkRef{}, false
	}
	br := live.BackendRef
	if br.Key != cp.move.SrcRef.OID || br.Cluster != cp.move.FromCluster {
		return nil, data.ChunkRef{}, false
	}
	updated := *live
	newRef := *br
	newRef.Cluster = cp.move.ToCluster
	newRef.Key = cp.dstKey
	// VersionID does not survive a copy; the target object has its own.
	newRef.VersionID = ""
	updated.BackendRef = &newRef
	oldRef := data.ChunkRef{
		Cluster: cp.move.FromCluster,
		Pool:    cp.srcBucket,
		OID:     br.Key,
		Size:    br.Size,
	}
	return &updated, oldRef, true
}

func newRefsFromS3(copies []s3Copy) []data.ChunkRef {
	refs := make([]data.ChunkRef, 0, len(copies))
	for _, c := range copies {
		refs = append(refs, data.ChunkRef{
			Cluster: c.move.ToCluster,
			Pool:    c.dstBucket,
			OID:     c.dstKey,
			Size:    c.size,
		})
	}
	return refs
}

// defaultS3KeyMint synthesises a target backend key. Mirrors the
// s3.Backend.objectKey shape (<bucket-uuid>/<object-uuid>) so operators
// see consistent key shapes across PUT-time and rebalance-time writes.
func defaultS3KeyMint(bucketID [16]byte) string {
	return uuid.UUID(bucketID).String() + "/" + uuid.NewString()
}
