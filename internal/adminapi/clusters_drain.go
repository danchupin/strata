package adminapi

import (
	"net/http"
	"sort"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/data/placement"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// ClusterStateEntry is one row in the operator-facing
// GET /admin/v1/clusters response (US-006 placement-rebalance).
// State is one of "live" / "draining" / "removed"; backend is
// "rados" or "s3" depending on which env produced the cluster id.
type ClusterStateEntry struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Backend string `json:"backend"`
}

// ClustersListResponse is the wire shape for GET /admin/v1/clusters.
type ClustersListResponse struct {
	Clusters []ClusterStateEntry `json:"clusters"`
}

// handleClustersList serves GET /admin/v1/clusters. Returns every
// configured cluster id (from KnownClusters + KnownClusterBackends)
// joined against the persisted cluster_state rows. Clusters without a
// row default to meta.ClusterStateLive.
func (s *Server) handleClustersList(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	rows, err := s.Meta.ListClusterStates(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	seen := map[string]struct{}{}
	out := ClustersListResponse{Clusters: []ClusterStateEntry{}}
	add := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		state := rows[id]
		if state == "" {
			state = meta.ClusterStateLive
		}
		out.Clusters = append(out.Clusters, ClusterStateEntry{
			ID:      id,
			State:   state,
			Backend: s.ClusterBackends[id],
		})
	}
	ids := make([]string, 0, len(s.KnownClusters)+len(rows))
	for id := range s.KnownClusters {
		ids = append(ids, id)
	}
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		add(id)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleClusterDrain serves POST /admin/v1/clusters/{id}/drain.
// Flips state to meta.ClusterStateDraining; subsequent PUTs route
// around the cluster (after the drain cache TTL elapses) and the
// rebalance worker migrates existing chunks off it.
func (s *Server) handleClusterDrain(w http.ResponseWriter, r *http.Request) {
	s.flipClusterState(w, r, meta.ClusterStateDraining, "admin:DrainCluster", true)
}

// handleClusterUndrain serves POST /admin/v1/clusters/{id}/undrain.
// Drops the cluster_state row entirely (absence == live).
func (s *Server) handleClusterUndrain(w http.ResponseWriter, r *http.Request) {
	s.flipClusterState(w, r, "", "admin:UndrainCluster", false)
}

// flipClusterState is the shared body of drain + undrain. desiredState
// == "" calls DeleteClusterState; non-empty calls SetClusterState.
// invalidateOnly invalidates the in-process drain cache so the change
// is visible to the PUT hot path without waiting out the TTL.
func (s *Server) flipClusterState(w http.ResponseWriter, r *http.Request, desiredState, action string, isDrain bool) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "cluster id is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	if len(s.KnownClusters) > 0 {
		if _, ok := s.KnownClusters[id]; !ok {
			writeJSONError(w, http.StatusBadRequest, "UnknownCluster",
				"cluster id is not configured (check STRATA_RADOS_CLUSTERS / STRATA_S3_CLUSTERS)")
			return
		}
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, action, "cluster:"+id, "-", owner)

	var err error
	if desiredState == "" {
		err = s.Meta.DeleteClusterState(ctx, id)
	} else {
		err = s.Meta.SetClusterState(ctx, id, desiredState)
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if s.DrainCache != nil {
		s.DrainCache.Invalidate()
	}
	_ = isDrain
	w.WriteHeader(http.StatusNoContent)
}

// ensure placement import is used (kept for type reference in Server).
var _ = placement.DefaultDrainCacheTTL
