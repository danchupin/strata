package adminapi

import "net/http"

// handleConsumersTop serves GET /admin/v1/consumers/top. Phase 1 stub.
func (s *Server) handleConsumersTop(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ConsumersTopResponse{Consumers: []ConsumerTop{}})
}
