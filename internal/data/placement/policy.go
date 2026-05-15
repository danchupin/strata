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
