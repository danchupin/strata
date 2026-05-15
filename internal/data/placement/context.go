package placement

import (
	"context"

	"github.com/danchupin/strata/internal/meta"
)

// clusterStatesKey carries the full cluster_state snapshot onto the data-
// plane ctx so PutChunks can call EffectivePolicy with state-aware
// filtering (US-003 effective-placement). Lives in the placement package
// rather than internal/data because the value carries a meta type and
// internal/data must not import meta (cycle with internal/meta/tikv).
type clusterStatesKey struct{}

// WithClusterStates stores the cluster_state snapshot on ctx. Empty /
// nil map returns ctx unchanged — backends treat absence as "every
// cluster live" (the absence==live semantic from CLAUDE.md).
func WithClusterStates(ctx context.Context, states map[string]meta.ClusterStateRow) context.Context {
	if len(states) == 0 {
		return ctx
	}
	return context.WithValue(ctx, clusterStatesKey{}, states)
}

// ClusterStatesFromContext returns the cluster_state map stored via
// WithClusterStates. Second return is false when none recorded — callers
// pass nil to EffectivePolicy and the absence==live default kicks in.
func ClusterStatesFromContext(ctx context.Context) (map[string]meta.ClusterStateRow, bool) {
	if ctx == nil {
		return nil, false
	}
	v, ok := ctx.Value(clusterStatesKey{}).(map[string]meta.ClusterStateRow)
	if !ok || len(v) == 0 {
		return nil, false
	}
	return v, true
}
