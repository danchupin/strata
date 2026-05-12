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
// runtime stats (BytesUsed/ObjectCount/State) before pushing the inner
// status into the wire report.
type pendingPoolStatus struct {
	group  poolGroup
	status data.PoolStatus
}

// buildPendingPoolStatuses groups the configured classes map by
// (cluster, pool, namespace) and emits one pendingPoolStatus per group,
// pre-populating Name, Class (comma-joined sorted class list), and
// Cluster (cluster id with DefaultCluster substituted for empty).
// Output is sorted by (cluster, pool, ns) for stable wire output.
// Lives in a build-tag-free file so the grouping logic is testable
// without a librados linkage.
func buildPendingPoolStatuses(classes map[string]ClassSpec) []pendingPoolStatus {
	classByPool := make(map[poolGroup][]string)
	for class, spec := range classes {
		cluster := spec.Cluster
		if cluster == "" {
			cluster = DefaultCluster
		}
		k := poolGroup{cluster: cluster, pool: spec.Pool, ns: spec.Namespace}
		classByPool[k] = append(classByPool[k], class)
	}
	keys := make([]poolGroup, 0, len(classByPool))
	for k := range classByPool {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].cluster != keys[j].cluster {
			return keys[i].cluster < keys[j].cluster
		}
		if keys[i].pool != keys[j].pool {
			return keys[i].pool < keys[j].pool
		}
		return keys[i].ns < keys[j].ns
	})
	out := make([]pendingPoolStatus, 0, len(keys))
	for _, k := range keys {
		cls := append([]string(nil), classByPool[k]...)
		sort.Strings(cls)
		out = append(out, pendingPoolStatus{
			group: k,
			status: data.PoolStatus{
				Name:    k.pool,
				Class:   strings.Join(cls, ","),
				Cluster: k.cluster,
			},
		})
	}
	return out
}
