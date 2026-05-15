package adminapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// weightRequestBody is the JSON shape accepted by both
// POST /admin/v1/clusters/{id}/activate and
// PUT  /admin/v1/clusters/{id}/weight (US-001 cluster-weights).
type weightRequestBody struct {
	Weight int `json:"weight"`
}

// handleClusterActivate serves POST /admin/v1/clusters/{id}/activate.
// Promotes a pending cluster to live with the operator-supplied
// weight. Returns 409 Conflict when state != pending; 400 BadRequest
// when weight is outside [0, 100]. Synchronously invalidates the
// shared DrainCache so default routing picks up the freshly-live
// cluster on the next PUT (US-001 cluster-weights).
func (s *Server) handleClusterActivate(w http.ResponseWriter, r *http.Request) {
	id, weight, ok := s.parseClusterWeightRequest(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:ActivateCluster", "cluster:"+id, "-", owner)

	currentRow, _, err := s.Meta.GetClusterState(ctx, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if currentRow.State != meta.ClusterStatePending {
		writeJSON(w, http.StatusConflict, errorResponse{
			Code:    "InvalidTransition",
			Message: "activate only permitted from pending state, current=" + currentRow.State,
		})
		return
	}
	if err := s.Meta.SetClusterState(ctx, id, meta.ClusterStateLive, "", weight); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if s.DrainCache != nil {
		s.DrainCache.Invalidate()
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleClusterUpdateWeight serves PUT /admin/v1/clusters/{id}/weight.
// Adjusts the per-cluster default-routing weight while the cluster is
// live. Returns 409 Conflict when state != live; 400 BadRequest when
// weight is outside [0, 100]. Cache invalidates synchronously so
// operator drags propagate to routing within seconds (US-001
// cluster-weights).
func (s *Server) handleClusterUpdateWeight(w http.ResponseWriter, r *http.Request) {
	id, weight, ok := s.parseClusterWeightRequest(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:UpdateClusterWeight", "cluster:"+id, "-", owner)

	currentRow, exists, err := s.Meta.GetClusterState(ctx, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	currentState := currentRow.State
	if !exists || currentState == "" {
		currentState = meta.ClusterStateLive
	}
	if currentState != meta.ClusterStateLive {
		writeJSON(w, http.StatusConflict, errorResponse{
			Code:    "InvalidTransition",
			Message: "weight update only permitted in live state, current=" + currentState,
		})
		return
	}
	if err := s.Meta.SetClusterState(ctx, id, meta.ClusterStateLive, "", weight); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if s.DrainCache != nil {
		s.DrainCache.Invalidate()
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseClusterWeightRequest extracts {id, weight} from the URL path +
// JSON body for the activate / weight handlers. Validates id presence,
// known-cluster membership, body decode, and the [0, 100] range.
// Writes the appropriate 4xx response and returns ok=false when any
// check fails.
func (s *Server) parseClusterWeightRequest(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "cluster id is required")
		return "", 0, false
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return "", 0, false
	}
	if len(s.KnownClusters) > 0 {
		if _, ok := s.KnownClusters[id]; !ok {
			writeJSONError(w, http.StatusBadRequest, "UnknownCluster",
				"cluster id is not configured (check STRATA_RADOS_CLUSTERS / STRATA_S3_CLUSTERS)")
			return "", 0, false
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "read body: "+err.Error())
		return "", 0, false
	}
	if len(body) == 0 {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", `request body required: {"weight": <0..100>}`)
		return "", 0, false
	}
	var req weightRequestBody
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "decode body: "+err.Error())
		return "", 0, false
	}
	if err := meta.ValidateClusterWeight(req.Weight); err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "weight must be in [0, 100]")
		return "", 0, false
	}
	return id, req.Weight, true
}
