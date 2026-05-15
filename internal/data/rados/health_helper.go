package rados

import (
	"sort"
	"strings"

	"github.com/danchupin/strata/internal/data"
)

// poolGroup is a unique (cluster, pool, namespace) tuple — one PoolStatus
// row in DataHealthReport corresponds to one poolGroup.
type poolGroup struct {
	cluster, pool, ns string
}

// pendingPoolStatus pairs the source group key with the pre-populated
// row (Name/Class/Cluster filled). The ceph-tagged DataHealth folds in
// runtime stats (BytesUsed/ChunkCount/State) before pushing the inner
// status into the wire report.
type pendingPoolStatus struct {
	group  poolGroup
	status data.PoolStatus
}

// buildPendingPoolStatuses emits one pendingPoolStatus per (cluster, pool,
// namespace) cell of the cross-product between every registered cluster
// and every distinct (pool, namespace) tuple referenced by the configured
// classes. The Class field is the sorted comma-joined list of classes
// mapped to that (pool, namespace). Lab shape: 2 clusters × 3 distinct
// pools → 6 rows, so the Pools table reflects actual per-cluster
// distribution instead of class env routing config.
//
// Output is sorted ascending by (Cluster, Name) with empty Cluster sorted
// last; the helper substitutes DefaultCluster for any "" cluster id in
// the input clusters map.
//
// Lives in a build-tag-free file so the grouping logic is testable
// without a librados linkage.
func buildPendingPoolStatuses(classes map[string]ClassSpec, clusters map[string]ClusterSpec) []pendingPoolStatus {
	type poolKey struct {
		pool, ns string
	}
	classByPool := make(map[poolKey][]string)
	for class, spec := range classes {
		k := poolKey{pool: spec.Pool, ns: spec.Namespace}
		classByPool[k] = append(classByPool[k], class)
	}
	poolKeys := make([]poolKey, 0, len(classByPool))
	for k := range classByPool {
		poolKeys = append(poolKeys, k)
	}
	sort.Slice(poolKeys, func(i, j int) bool {
		if poolKeys[i].pool != poolKeys[j].pool {
			return poolKeys[i].pool < poolKeys[j].pool
		}
		return poolKeys[i].ns < poolKeys[j].ns
	})

	clusterIDs := make([]string, 0, len(clusters))
	for id := range clusters {
		clusterIDs = append(clusterIDs, id)
	}
	sort.Slice(clusterIDs, func(i, j int) bool {
		ci, cj := clusterIDs[i], clusterIDs[j]
		if ci == "" {
			return false
		}
		if cj == "" {
			return true
		}
		return ci < cj
	})

	out := make([]pendingPoolStatus, 0, len(clusterIDs)*len(poolKeys))
	for _, cid := range clusterIDs {
		cluster := cid
		if cluster == "" {
			cluster = DefaultCluster
		}
		for _, pk := range poolKeys {
			cls := append([]string(nil), classByPool[pk]...)
			sort.Strings(cls)
			out = append(out, pendingPoolStatus{
				group: poolGroup{cluster: cluster, pool: pk.pool, ns: pk.ns},
				status: data.PoolStatus{
					Name:    pk.pool,
					Class:   strings.Join(cls, ","),
					Cluster: cluster,
				},
			})
		}
	}
	return out
}
