package adminapi

import "net/http"

// handleClusterStatus serves GET /admin/v1/cluster/status. Phase 1 stub —
// US-006 fills the remaining derived fields (uptime, node counts, backends).
func (s *Server) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ClusterStatus{
		Status:      "ok",
		Version:     s.Version,
		StartedAt:   s.Started.Unix(),
		ClusterName: s.ClusterName,
	})
}

// handleClusterNodes serves GET /admin/v1/cluster/nodes. Phase 1 stub —
// returns an empty list until the heartbeat table (US-006) is wired.
func (s *Server) handleClusterNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ClusterNodesResponse{Nodes: []ClusterNode{}})
}
