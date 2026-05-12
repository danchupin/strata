// Package rebalance — RADOS-side mover (US-004).
//
// The mover is an in-package, librados-free type: it talks to RADOS
// only through the RadosCluster interface, which the ceph-tagged
// rados.Backend implements via a thin facade. Keeping the interface
// here lets unit tests fake the cluster surface entirely in-memory
// without the `ceph` build tag, while the cmd binary plugs in the
// real librados-backed facade when ceph is available.
//
// One Mover owns the entire RADOS cluster family — i.e. every cluster
// in the rados.Backend's cluster map. Move(plan) groups the plan by
// (BucketID, ObjectKey, VersionID), copies each chunk to a freshly-
// minted OID on the target cluster, then issues a single manifest CAS
// per object via meta.Store.SetObjectStorage. On CAS success the old
// (source) chunks land in the GC queue; on CAS reject the new
// (target) chunks land in the GC queue instead so the unused copy
// gets cleaned up by the existing GC worker.
package rebalance

import (
	"context"
	"errors"
	"fmt"
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

// RadosCluster is the minimum surface the mover needs from a RADOS
// cluster. The ceph-tagged rados.Backend implements it via librados
// ioctx; unit tests can plug an in-memory fake.
//
// All methods take pool + namespace explicitly because rebalance
// inherits the source chunk's pool layout — there is no per-cluster
// pool-rewrite. ID() returns the cluster's operator label and surfaces
// in Owns()/metric labels.
type RadosCluster interface {
	ID() string
	Read(ctx context.Context, pool, namespace, oid string) ([]byte, error)
	Write(ctx context.Context, pool, namespace, oid string, body []byte) error
}

// RadosMover executes a plan slice against a set of RADOS clusters.
// Clusters is keyed by ClusterSpec.ID; Meta is the shared metadata
// store; Region is the GC partition the old chunks land in (matches
// the gateway's STRATA_REGION). Throttle gates byte-rate across the
// whole iteration; Inflight bounds the per-Move(plan) errgroup at
// per-object concurrency.
type RadosMover struct {
	Clusters map[string]RadosCluster
	Meta     meta.Store
	Region   string
	Logger   *slog.Logger
	Metrics  MoverMetrics
	Tracer   trace.Tracer
	Throttle *Throttle
	Inflight int
}

// Owns reports whether target is one of the mover's known clusters.
func (m *RadosMover) Owns(target string) bool {
	if m == nil {
		return false
	}
	_, ok := m.Clusters[target]
	return ok
}

// Move batches plan by (BucketID, ObjectKey, VersionID) and dispatches
// each object group through a bounded errgroup. ctx cancellation
// drains in-flight groups. Per-group failures log + drop the affected
// chunks; a fully-failed group leaves the manifest untouched and the
// worker will replan next tick.
func (m *RadosMover) Move(ctx context.Context, plan []Move) error {
	if m == nil || len(plan) == 0 {
		return nil
	}
	if m.Meta == nil {
		return errors.New("rebalance: rados mover meta required")
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
			m.moveObject(gctx, logger, metrics, tracer, region, group)
			return nil
		})
	}
	return g.Wait()
}

func (m *RadosMover) moveObject(ctx context.Context, logger *slog.Logger, metrics MoverMetrics, tracer trace.Tracer, region string, group []Move) {
	if len(group) == 0 {
		return
	}
	head := group[0]
	bucketID := uuid.UUID(head.BucketID)

	copies, err := m.copyChunks(ctx, logger, metrics, tracer, group)
	if err != nil {
		logger.WarnContext(ctx, "rebalance: copy chunks aborted",
			"bucket", head.Bucket, "key", head.ObjectKey, "error", err.Error())
		// best-effort GC for whatever copies landed before the abort
		m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, newRefsFrom(copies))
		return
	}
	if len(copies) == 0 {
		// every chunk read or write failed; original manifest stays
		return
	}

	obj, err := m.Meta.GetObject(ctx, bucketID, head.ObjectKey, head.VersionID)
	if err != nil {
		logger.WarnContext(ctx, "rebalance: fetch manifest",
			"bucket", head.Bucket, "key", head.ObjectKey, "error", err.Error())
		m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, newRefsFrom(copies))
		return
	}
	if obj == nil || obj.Manifest == nil {
		logger.InfoContext(ctx, "rebalance: object disappeared mid-flight, discarding new chunks",
			"bucket", head.Bucket, "key", head.ObjectKey)
		m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, newRefsFrom(copies))
		return
	}

	updated, oldRefs, ok := buildUpdatedManifest(obj.Manifest, copies)
	if !ok {
		// the manifest changed under us — chunk indices no longer
		// match the planned source refs. Treat as a CAS conflict.
		metrics.IncCASConflict(head.Bucket)
		newRefs := newRefsFrom(copies)
		m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, newRefs)
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
		m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, newRefsFrom(copies))
		return
	}
	if !applied {
		metrics.IncCASConflict(head.Bucket)
		logger.InfoContext(ctx, "rebalance: CAS conflict, discarding new chunks",
			"bucket", head.Bucket, "key", head.ObjectKey)
		m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, newRefsFrom(copies))
		return
	}
	for _, c := range copies {
		metrics.IncChunksMoved(c.move.FromCluster, c.move.ToCluster, head.Bucket)
	}
	m.enqueueGC(ctx, region, head.Bucket, head.ObjectKey, oldRefs)
}

// chunkCopy is the in-memory record of one successfully-copied chunk.
// move carries the original plan entry (for metric labels + matching
// against the live manifest); newRef is the freshly-written target
// chunk locator.
type chunkCopy struct {
	move   Move
	newRef data.ChunkRef
}

func (m *RadosMover) copyChunks(ctx context.Context, logger *slog.Logger, metrics MoverMetrics, tracer trace.Tracer, group []Move) ([]chunkCopy, error) {
	copies := make([]chunkCopy, 0, len(group))
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
			copy, err := m.copyOne(gctx, metrics, tracer, mv)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				logger.WarnContext(gctx, "rebalance: copy chunk",
					"bucket", mv.Bucket, "key", mv.ObjectKey, "chunk_idx", mv.ChunkIdx,
					"from", mv.FromCluster, "to", mv.ToCluster, "error", err.Error())
				return nil
			}
			copiesMu.Lock()
			copies = append(copies, copy)
			copiesMu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return copies, err
	}
	return copies, nil
}

func (m *RadosMover) copyOne(ctx context.Context, metrics MoverMetrics, tracer trace.Tracer, mv Move) (chunkCopy, error) {
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
		return chunkCopy{}, err
	}
	tgt, ok := m.Clusters[mv.ToCluster]
	if !ok {
		err := fmt.Errorf("target cluster %q not configured", mv.ToCluster)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return chunkCopy{}, err
	}

	if err := m.Throttle.Wait(ctx, mv.SrcRef.Size); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return chunkCopy{}, err
	}
	body, err := src.Read(ctx, mv.SrcRef.Pool, mv.SrcRef.Namespace, mv.SrcRef.OID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return chunkCopy{}, fmt.Errorf("read %s/%s: %w", mv.SrcRef.Pool, mv.SrcRef.OID, err)
	}
	if err := m.Throttle.Wait(ctx, int64(len(body))); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return chunkCopy{}, err
	}
	newOID := fmt.Sprintf("%s.%05d", uuid.NewString(), mv.ChunkIdx)
	if err := tgt.Write(ctx, mv.SrcRef.Pool, mv.SrcRef.Namespace, newOID, body); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return chunkCopy{}, fmt.Errorf("write %s/%s: %w", mv.SrcRef.Pool, newOID, err)
	}
	metrics.IncBytesMoved(mv.FromCluster, mv.ToCluster, int64(len(body)))
	return chunkCopy{
		move: mv,
		newRef: data.ChunkRef{
			Cluster:   mv.ToCluster,
			Pool:      mv.SrcRef.Pool,
			Namespace: mv.SrcRef.Namespace,
			OID:       newOID,
			Size:      int64(len(body)),
		},
	}, nil
}

func (m *RadosMover) enqueueGC(ctx context.Context, region, bucket, key string, refs []data.ChunkRef) {
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

// groupMovesByObject groups plan entries by (BucketID, ObjectKey,
// VersionID) so each output slice can be turned into one manifest CAS.
// Group order is not deterministic — callers MUST NOT depend on it.
func groupMovesByObject(plan []Move) [][]Move {
	type objectKey struct {
		bucketID  [16]byte
		key       string
		versionID string
	}
	idx := map[objectKey]int{}
	var groups [][]Move
	for _, mv := range plan {
		k := objectKey{bucketID: mv.BucketID, key: mv.ObjectKey, versionID: mv.VersionID}
		if pos, ok := idx[k]; ok {
			groups[pos] = append(groups[pos], mv)
			continue
		}
		idx[k] = len(groups)
		groups = append(groups, []Move{mv})
	}
	return groups
}

// buildUpdatedManifest returns a clone of `live` with each copied
// chunk's locator replaced by the freshly-written target ref. ok=false
// when the live manifest no longer matches the plan (chunk index out
// of range, or the OID at that index has already been rewritten) —
// the caller treats this as a CAS conflict. oldRefs returns the
// pre-update chunk locators in the order they were copied so the
// caller can enqueue them into the GC queue once the CAS lands.
func buildUpdatedManifest(live *data.Manifest, copies []chunkCopy) (*data.Manifest, []data.ChunkRef, bool) {
	if live == nil {
		return nil, nil, false
	}
	updated := *live
	updated.Chunks = append([]data.ChunkRef(nil), live.Chunks...)
	oldRefs := make([]data.ChunkRef, 0, len(copies))
	for _, c := range copies {
		if c.move.ChunkIdx < 0 || c.move.ChunkIdx >= len(updated.Chunks) {
			return nil, nil, false
		}
		current := updated.Chunks[c.move.ChunkIdx]
		if current.OID != c.move.SrcRef.OID || current.Cluster != c.move.FromCluster {
			return nil, nil, false
		}
		oldRefs = append(oldRefs, current)
		updated.Chunks[c.move.ChunkIdx] = c.newRef
	}
	return &updated, oldRefs, true
}

func newRefsFrom(copies []chunkCopy) []data.ChunkRef {
	refs := make([]data.ChunkRef, 0, len(copies))
	for _, c := range copies {
		refs = append(refs, c.newRef)
	}
	return refs
}
