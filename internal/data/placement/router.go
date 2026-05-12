// Package placement implements stable per-chunk routing across data
// clusters under an operator-supplied weight policy. The hash function is
// fnv32a over "<bucketID>/<key>/<chunkIdx>"; the result modulo the weight
// sum walks a weight-wheel laid out over a sorted-cluster-id slice, so the
// same chunk always lands on the same cluster across retries and the
// rebalance worker can recompute target distribution offline.
package placement

import (
	"fmt"
	"hash/fnv"
	"sort"

	"github.com/google/uuid"
)

// PickCluster returns the cluster id that should host the chunk identified
// by (bucketID, key, chunkIdx) under the supplied weight policy.
//
// Empty/nil policy returns "" so the caller falls back to its existing
// $defaultCluster behaviour (no breaking change for unconfigured buckets).
// Zero-weight clusters are never picked. Cluster ids are walked in sorted
// order to make routing independent of Go map iteration order — without
// this the picker would be non-deterministic across processes.
func PickCluster(bucketID uuid.UUID, key string, chunkIdx int, policy map[string]int) string {
	return PickClusterExcluding(bucketID, key, chunkIdx, policy, nil)
}

// PickClusterExcluding is the placement picker with an explicit
// excluded-clusters set. Entries in `excluded` are treated as weight=0:
// they never receive new chunks. Used by the rebalance worker to honour
// the drain sentinel (US-006). If every cluster in the policy is excluded
// the function returns "" so the caller falls back to its default
// cluster.
func PickClusterExcluding(bucketID uuid.UUID, key string, chunkIdx int, policy map[string]int, excluded map[string]bool) string {
	if len(policy) == 0 {
		return ""
	}
	ids := make([]string, 0, len(policy))
	for id := range policy {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var total uint32
	for _, id := range ids {
		w := policy[id]
		if w <= 0 {
			continue
		}
		if excluded != nil && excluded[id] {
			continue
		}
		total += uint32(w)
	}
	if total == 0 {
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(fmt.Sprintf("%s/%s/%d", bucketID.String(), key, chunkIdx)))
	pick := h.Sum32() % total
	var cum uint32
	for _, id := range ids {
		w := policy[id]
		if w <= 0 {
			continue
		}
		if excluded != nil && excluded[id] {
			continue
		}
		cum += uint32(w)
		if pick < cum {
			return id
		}
	}
	return ids[len(ids)-1]
}
