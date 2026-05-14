package serverapp

import (
	"context"
	"log/slog"
	"sort"

	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/data/rados"
	"github.com/danchupin/strata/internal/data/s3"
	"github.com/danchupin/strata/internal/meta"
)

// ReconcileInput is the resolved env-side input to ReconcileClusters.
// EnvClusters is the ordered list of cluster ids configured via
// STRATA_RADOS_CLUSTERS / STRATA_S3_CLUSTERS. ClassDefaults is the set
// of cluster ids referenced by the per-class env (post-`@` pin); these
// ids are where chunks land for nil-policy buckets historically.
// HasData reports whether any bucket has UsedBytes>0 or UsedObjects>0
// — the proxy for "is this gateway already serving traffic".
type ReconcileInput struct {
	EnvClusters   []string
	ClassDefaults map[string]bool
	HasData       bool
}

// ReconcileClusters materialises a cluster_state row for every
// cluster in EnvClusters that does not yet have one. The rule
// (US-001 cluster-weights):
//
//   - row already exists → leave alone (idempotent re-run)
//   - HasData && (id ∈ ClassDefaults || id ∈ Placement refs) →
//     state=live weight=100 (existing-live; preserves routing)
//   - otherwise → state=pending weight=0 (new-pending; operator must
//     /activate)
//
// Returns counts for the summary log line. Errors short-circuit; partial
// reconcile is acceptable (next boot retries).
func ReconcileClusters(ctx context.Context, m meta.Store, in ReconcileInput, logger *slog.Logger) (autoPending, existingLive int, err error) {
	if m == nil || len(in.EnvClusters) == 0 {
		return 0, 0, nil
	}
	existing, err := m.ListClusterStates(ctx)
	if err != nil {
		return 0, 0, err
	}
	// Walk env in stable (sorted) order so log output is deterministic.
	env := append([]string(nil), in.EnvClusters...)
	sort.Strings(env)

	// Collect Placement refs from buckets — bucket is referenced if any
	// bucket explicitly lists it in its Placement policy. ListBuckets
	// with empty owner returns every bucket (memory + cassandra + tikv
	// honour this convention).
	placementRefs := map[string]bool{}
	buckets, lerr := m.ListBuckets(ctx, "")
	if lerr == nil {
		for _, b := range buckets {
			for cid := range b.Placement {
				placementRefs[cid] = true
			}
		}
	}

	for _, id := range env {
		if _, ok := existing[id]; ok {
			continue
		}
		referenced := in.ClassDefaults[id] || placementRefs[id]
		if in.HasData && referenced {
			if serr := m.SetClusterState(ctx, id, meta.ClusterStateLive, "", 100); serr != nil {
				return autoPending, existingLive, serr
			}
			existingLive++
			if logger != nil {
				logger.Info("cluster auto-init",
					"cluster_id", id,
					"state", meta.ClusterStateLive,
					"weight", 100,
					"reason", "existing-live")
			}
			continue
		}
		if serr := m.SetClusterState(ctx, id, meta.ClusterStatePending, "", 0); serr != nil {
			return autoPending, existingLive, serr
		}
		autoPending++
		if logger != nil {
			logger.Info("cluster auto-init",
				"cluster_id", id,
				"state", meta.ClusterStatePending,
				"weight", 0,
				"reason", "new-pending")
		}
	}
	if logger != nil && (autoPending+existingLive) > 0 {
		logger.Info("cluster reconcile",
			"auto_pending", autoPending,
			"existing_live", existingLive)
	}
	return autoPending, existingLive, nil
}

// classDefaultClusters returns the set of cluster ids the data backend
// resolves the per-class default to (the post-`@` pin in
// STRATA_RADOS_CLASSES / STRATA_S3_CLASSES). Any historic chunk for a
// nil-Placement bucket has landed on one of these ids, so they qualify
// as "existing-live" candidates at boot reconcile.
func classDefaultClusters(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	switch cfg.DataBackend {
	case "rados":
		classes, err := rados.ParseClasses(cfg.RADOS.Classes)
		if err != nil {
			return out
		}
		for _, spec := range classes {
			if spec.Cluster != "" {
				out[spec.Cluster] = true
			}
		}
	case "s3":
		classes, err := s3.ParseClasses(cfg.S3.Classes)
		if err != nil {
			return out
		}
		for _, spec := range classes {
			if spec.Cluster != "" {
				out[spec.Cluster] = true
			}
		}
	}
	return out
}

// reconcileHasData reports whether any bucket carries non-zero
// bucket_stats. Caller-tolerant: any error or no buckets → false.
func reconcileHasData(ctx context.Context, m meta.Store) bool {
	buckets, err := m.ListBuckets(ctx, "")
	if err != nil {
		return false
	}
	for _, b := range buckets {
		stats, _ := m.GetBucketStats(ctx, b.ID)
		if stats.UsedBytes > 0 || stats.UsedObjects > 0 {
			return true
		}
	}
	return false
}
