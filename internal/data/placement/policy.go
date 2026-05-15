package placement

import "github.com/danchupin/strata/internal/meta"

// DefaultPolicy synthesises the default-routing weight policy from a
// snapshot of cluster_state rows (US-002 cluster-weights). Only clusters
// with state=live AND weight>0 contribute. pending / draining_readonly /
// evacuating / removed / draining are excluded — the same exclusion set
// IsDrainingForWrite uses for the drain sentinel, extended with pending
// (no row → cluster not eligible).
//
// Callers feed the result into PickClusterExcluding the same way they
// feed bucket Placement policy. An empty map (no live clusters with
// positive weight) signals "no default routing available" — the PUT
// hot path falls back to the per-class spec.Cluster pin.
func DefaultPolicy(states map[string]meta.ClusterStateRow) map[string]int {
	if len(states) == 0 {
		return nil
	}
	out := make(map[string]int, len(states))
	for id, row := range states {
		if row.State != meta.ClusterStateLive {
			continue
		}
		if row.Weight <= 0 {
			continue
		}
		out[id] = row.Weight
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EffectivePolicy resolves the routing policy that drives both PUT
// placement and the rebalance worker's distribution classifier for a
// given bucket (US-002 effective-placement). The same helper is consulted
// from both call sites so categorisation matches actual routing.
//
// Resolution order:
//
//  1. Filter bucketPolicy to entries whose cluster is live — state == live
//     (or absent from clusterStates, since absence == live per the
//     cluster_state semantic) AND weight > 0. The result is the
//     "live subset" of the bucket's explicit Placement.
//  2. If the live subset is non-empty, return it. The bucket's Placement
//     still has at least one viable target; honour it.
//  3. If bucketPolicy was non-empty (operator configured a policy) but
//     no entry survived the live filter AND mode == strict, return nil.
//     The operator opted into compliance stickiness; surface the
//     no-target case through the caller's strict-refuse path
//     (503 DrainRefused on PUT, stuck_single_policy in the worker).
//  4. Otherwise — mode != strict, or bucketPolicy was nil entirely —
//     fall through to the synthesised cluster-weights policy filtered
//     to live + weight>0. nil bucketPolicy + strict is intentionally
//     handled here: strict requires an explicit policy to be meaningful,
//     so an unconfigured bucket falls back like a weighted bucket.
//  5. If the synthesised policy is empty too, return nil — genuine
//     no-target case; caller observes 503 / stuck_no_policy.
//
// mode == "" is treated as PlacementModeWeighted (backwards-compat for
// legacy buckets without a stored placement_mode column).
func EffectivePolicy(
	bucketPolicy map[string]int,
	mode string,
	clusterWeights map[string]int,
	clusterStates map[string]meta.ClusterStateRow,
) map[string]int {
	norm := meta.NormalizePlacementMode(mode)
	live := liveSubset(bucketPolicy, clusterStates)
	if len(live) > 0 {
		return live
	}
	if len(bucketPolicy) > 0 && norm == meta.PlacementModeStrict {
		return nil
	}
	return liveSubset(clusterWeights, clusterStates)
}

// LiveSubset returns the entries of policy whose cluster is live (per
// clusterStates; absence == live) AND weight > 0. Returns nil for empty
// inputs or when no entry survives the filter. Exported so call sites
// outside this package can apply the same live-state filter to either
// bucket.Placement or the synthesised cluster-weights map without
// re-implementing the predicate.
func LiveSubset(policy map[string]int, states map[string]meta.ClusterStateRow) map[string]int {
	return liveSubset(policy, states)
}

// liveSubset returns the entries of policy whose cluster is live (per
// clusterStates; absence == live) AND weight > 0. Returns nil for empty
// inputs or when no entry survives the filter.
func liveSubset(policy map[string]int, states map[string]meta.ClusterStateRow) map[string]int {
	if len(policy) == 0 {
		return nil
	}
	out := make(map[string]int, len(policy))
	for id, w := range policy {
		if w <= 0 {
			continue
		}
		if row, ok := states[id]; ok && row.State != meta.ClusterStateLive {
			continue
		}
		out[id] = w
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
