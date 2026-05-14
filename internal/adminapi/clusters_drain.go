package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/data/placement"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// ClusterStateEntry is one row in the operator-facing
// GET /admin/v1/clusters response (US-006 placement-rebalance; mode
// added in US-001 drain-transparency). State is one of "live" /
// "draining_readonly" / "evacuating" / "removed"; Mode is "readonly"
// for draining_readonly, "evacuate" for evacuating, "" otherwise.
// Backend is "rados" or "s3" depending on which env produced the
// cluster id.
type ClusterStateEntry struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Mode    string `json:"mode"`
	// Weight is the per-cluster default-routing share (0..100). Only
	// consulted when state == live; reported as 0 for every other
	// state regardless of stored value (US-001 cluster-weights).
	Weight  int    `json:"weight"`
	Backend string `json:"backend"`
}

// ClustersListResponse is the wire shape for GET /admin/v1/clusters.
// drain_strict was removed in US-007 drain-transparency — drain is now
// unconditionally strict so the flag is no longer surfaced.
type ClustersListResponse struct {
	Clusters []ClusterStateEntry `json:"clusters"`
}

// drainRequestBody is the JSON shape accepted by POST
// /admin/v1/clusters/{id}/drain. Mode is required (no default) and
// must be one of {"readonly", "evacuate"} (US-001 drain-transparency).
type drainRequestBody struct {
	Mode string `json:"mode"`
}

// invalidTransitionResponse is the 409 Conflict payload returned when a
// drain request asks for a transition the 4-state machine refuses.
type invalidTransitionResponse struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	CurrentState   string `json:"current_state"`
	RequestedMode  string `json:"requested_mode"`
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
		row, ok := rows[id]
		if !ok || row.State == "" {
			row = meta.ClusterStateRow{State: meta.ClusterStateLive}
		}
		weight := 0
		if row.State == meta.ClusterStateLive {
			weight = row.Weight
		}
		out.Clusters = append(out.Clusters, ClusterStateEntry{
			ID:      id,
			State:   row.State,
			Mode:    row.Mode,
			Weight:  weight,
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

// handleClusterDrain serves POST /admin/v1/clusters/{id}/drain. The
// request body MUST carry {"mode":"readonly"} or {"mode":"evacuate"} —
// there is no default. Transitions are enforced per the 4-state machine
// (US-001 drain-transparency):
//
//	live → draining_readonly                 (mode=readonly)
//	live → evacuating                        (mode=evacuate)
//	draining_readonly → evacuating           (mode=evacuate, upgrade)
//	any other (state, mode) combination → 409 Conflict.
func (s *Server) handleClusterDrain(w http.ResponseWriter, r *http.Request) {
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

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "read body: "+err.Error())
		return
	}
	if len(body) == 0 {
		writeJSONError(w, http.StatusBadRequest, "BadRequest",
			"request body required: {\"mode\":\"readonly\"|\"evacuate\"}")
		return
	}
	var req drainRequestBody
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "decode body: "+err.Error())
		return
	}
	if req.Mode != meta.ClusterModeReadonly && req.Mode != meta.ClusterModeEvacuate {
		writeJSONError(w, http.StatusBadRequest, "BadRequest",
			"mode must be one of: readonly, evacuate")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:DrainCluster", "cluster:"+id, "-", owner)

	currentRow, _, err := s.Meta.GetClusterState(ctx, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	currentState := currentRow.State
	if currentState == "" {
		currentState = meta.ClusterStateLive
	}

	targetState := stateForMode(req.Mode)
	if !isLegalDrainTransition(currentState, req.Mode) {
		writeJSON(w, http.StatusConflict, invalidTransitionResponse{
			Code: "InvalidTransition",
			Message: "transition from " + currentState + " via mode=" + req.Mode +
				" is not permitted",
			CurrentState:  currentState,
			RequestedMode: req.Mode,
		})
		return
	}

	if err := s.Meta.SetClusterState(ctx, id, targetState, req.Mode, currentRow.Weight); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if s.DrainCache != nil {
		s.DrainCache.Invalidate()
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleClusterUndrain serves POST /admin/v1/clusters/{id}/undrain.
// Drops the cluster_state row entirely (absence == live). Works from
// both draining_readonly and evacuating; refused from live or removed
// states (409). Moved chunks stay on the target cluster — undrain does
// NOT reverse a migration in flight (US-001 drain-transparency).
func (s *Server) handleClusterUndrain(w http.ResponseWriter, r *http.Request) {
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
	s3api.SetAuditOverride(ctx, "admin:UndrainCluster", "cluster:"+id, "-", owner)

	currentRow, _, err := s.Meta.GetClusterState(ctx, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	currentState := currentRow.State
	if currentState == "" {
		currentState = meta.ClusterStateLive
	}
	if currentState != meta.ClusterStateDrainingReadonly && currentState != meta.ClusterStateEvacuating {
		writeJSON(w, http.StatusConflict, invalidTransitionResponse{
			Code:          "InvalidTransition",
			Message:       "undrain only permitted from draining_readonly or evacuating",
			CurrentState:  currentState,
			RequestedMode: "",
		})
		return
	}

	if err := s.Meta.DeleteClusterState(ctx, id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if s.DrainCache != nil {
		s.DrainCache.Invalidate()
	}
	w.WriteHeader(http.StatusNoContent)
}

// stateForMode maps the operator-supplied mode onto the target state.
// The (state, mode) pair is the canonical wire shape for cluster_state.
func stateForMode(mode string) string {
	switch mode {
	case meta.ClusterModeReadonly:
		return meta.ClusterStateDrainingReadonly
	case meta.ClusterModeEvacuate:
		return meta.ClusterStateEvacuating
	default:
		return ""
	}
}

// isLegalDrainTransition enforces the 4-state machine:
//
//	live              + readonly  → draining_readonly  ✓
//	live              + evacuate  → evacuating          ✓
//	draining_readonly + evacuate  → evacuating (upgrade) ✓
//	draining_readonly + readonly  → (no-op refused)
//	evacuating        + *         → (no transition; undrain is the way back)
//	removed           + *         → (terminal)
func isLegalDrainTransition(currentState, mode string) bool {
	switch currentState {
	case meta.ClusterStateLive:
		return mode == meta.ClusterModeReadonly || mode == meta.ClusterModeEvacuate
	case meta.ClusterStateDrainingReadonly:
		return mode == meta.ClusterModeEvacuate
	default:
		return false
	}
}

// ensure imports are used.
var (
	_ = placement.DefaultDrainCacheTTL
	_ = errors.New
)
