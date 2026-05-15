package s3api

import (
	"context"
	"log/slog"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/data/placement"
	"github.com/danchupin/strata/internal/logging"
	"github.com/danchupin/strata/internal/meta"
)

// dataCtxForPut wraps the request context with the bucket id, object key,
// per-bucket placement policy, and (US-006) the live draining-cluster
// set so the data backend's PutChunks can route chunks via
// placement.PickClusterExcluding.
//
// GetBucketPlacement is consulted ONCE per PutChunks invocation. The
// memory note tracks that meta.Bucket.Placement is NOT populated by
// GetBucket — the hot-path GetBucket stays a single buckets-table read —
// so the policy must be fetched explicitly here.
//
// Errors fetching the policy are logged at WARN and treated as
// "no placement" so a transient meta hiccup never breaks the PUT path.
// The draining-cluster set is read from s.DrainCache (in-process,
// 30s TTL) so the meta backend is not burdened per request.
func (s *Server) dataCtxForPut(ctx context.Context, b *meta.Bucket, key string) context.Context {
	return dataCtxForPutWith(ctx, s.Meta, b, key, s.drainingClusters(ctx), s.defaultPlacement(ctx), s.clusterStates(ctx))
}

func (s *Server) drainingClusters(ctx context.Context) map[string]bool {
	if s.DrainCache == nil {
		return nil
	}
	return s.DrainCache.Get(ctx)
}

// defaultPlacement reads the cluster_state snapshot from the shared
// DrainCache and synthesises the default-routing policy from per-
// cluster weights (US-002 cluster-weights). Returns nil when the cache
// is unwired or no live cluster has positive weight — backends fall
// back to per-class spec.Cluster.
func (s *Server) defaultPlacement(ctx context.Context) map[string]int {
	if s.DrainCache == nil {
		return nil
	}
	return placement.DefaultPolicy(s.DrainCache.States(ctx))
}

// clusterStates returns the raw cluster_state snapshot from the shared
// DrainCache. Backends thread it through ctx so PutChunks resolves the
// effective routing policy with the full live/draining/pending/removed
// filter (US-003 effective-placement) rather than just the draining
// boolean set.
func (s *Server) clusterStates(ctx context.Context) map[string]meta.ClusterStateRow {
	if s.DrainCache == nil {
		return nil
	}
	return s.DrainCache.States(ctx)
}

func dataCtxForPutWith(ctx context.Context, m meta.Store, b *meta.Bucket, key string, draining map[string]bool, defaultPolicy map[string]int, states map[string]meta.ClusterStateRow) context.Context {
	ctx = data.WithBucketID(ctx, b.ID)
	ctx = data.WithObjectKey(ctx, key)
	policy, err := m.GetBucketPlacement(ctx, b.Name)
	if err != nil {
		if lg := logging.LoggerFromContext(ctx); lg != nil {
			lg.WarnContext(ctx, "placement: GetBucketPlacement failed; routing per class default",
				"bucket", b.Name, "error", err.Error())
		} else {
			slog.WarnContext(ctx, "placement: GetBucketPlacement failed; routing per class default",
				"bucket", b.Name, "error", err.Error())
		}
	} else if policy != nil {
		ctx = data.WithPlacement(ctx, policy)
	}
	if b.PlacementMode != "" {
		ctx = data.WithPlacementMode(ctx, b.PlacementMode)
	}
	if len(draining) > 0 {
		ctx = data.WithDrainingClusters(ctx, draining)
	}
	if len(defaultPolicy) > 0 {
		ctx = data.WithDefaultPlacement(ctx, defaultPolicy)
	}
	if len(states) > 0 {
		ctx = placement.WithClusterStates(ctx, states)
	}
	return ctx
}
