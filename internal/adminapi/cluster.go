package adminapi

import (
	"net/http"
	"time"

	"github.com/danchupin/strata/internal/heartbeat"
)

// handleClusterStatus serves GET /admin/v1/cluster/status. Aggregates the
// heartbeat table to surface live node counts + derived health.
//
//	healthy   — every listed node heartbeat ≤ 30s AND node_count >= 1
//	degraded  — at least one node listed but some are stale (heartbeat > 30s)
//	unhealthy — heartbeat table empty or unreadable
func (s *Server) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	uptime := int64(now.Sub(s.Started).Seconds())
	if uptime < 0 {
		uptime = 0
	}

	var (
		nodes   []heartbeat.Node
		listErr error
	)
	if s.Heartbeat != nil {
		nodes, listErr = s.Heartbeat.ListNodes(r.Context())
		if listErr != nil {
			s.Logger.Printf("adminapi: cluster/status list nodes: %v", listErr)
		}
	}

	healthy := 0
	for _, n := range nodes {
		if heartbeat.IsAlive(n, heartbeat.DefaultTTL) {
			healthy++
		}
	}

	status := "unhealthy"
	switch {
	case listErr != nil:
		status = "unhealthy"
	case len(nodes) == 0:
		status = "unhealthy"
	case healthy == len(nodes):
		status = "healthy"
	default:
		status = "degraded"
	}

	writeJSON(w, http.StatusOK, ClusterStatus{
		Status:           status,
		Version:          s.Version,
		StartedAt:        s.Started.Unix(),
		UptimeSec:        uptime,
		ClusterName:      s.ClusterName,
		NodeCount:        len(nodes),
		NodeCountHealthy: healthy,
		MetaBackend:      s.MetaBackend,
		DataBackend:      s.DataBackend,
	})
}

// handleClusterNodes serves GET /admin/v1/cluster/nodes. Returns the live
// heartbeat rows; an empty list is valid (single-replica dev stack before
// the heartbeat goroutine has had time to write).
func (s *Server) handleClusterNodes(w http.ResponseWriter, r *http.Request) {
	out := ClusterNodesResponse{Nodes: []ClusterNode{}}
	if s.Heartbeat == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	nodes, err := s.Heartbeat.ListNodes(r.Context())
	if err != nil {
		s.Logger.Printf("adminapi: cluster/nodes list: %v", err)
		writeJSON(w, http.StatusOK, out)
		return
	}
	now := time.Now().UTC()
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, ClusterNode{
			ID:            n.ID,
			Address:       n.Address,
			Version:       n.Version,
			StartedAt:     n.StartedAt.Unix(),
			UptimeSec:     int64(now.Sub(n.StartedAt).Seconds()),
			Status:        nodeStatus(n, now),
			Workers:       nilSliceToEmpty(n.Workers),
			LeaderFor:     nilSliceToEmpty(n.LeaderFor),
			LastHeartbeat: n.LastHeartbeat.Unix(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// nodeStatus maps a heartbeat freshness to a per-node badge:
//   - healthy  : last heartbeat within the writer interval (10s)
//   - degraded : 10s < age <= 30s (one missed write but still inside TTL)
//   - unhealthy: age > 30s (in practice the row is gone, but defensive)
func nodeStatus(n heartbeat.Node, now time.Time) string {
	age := now.Sub(n.LastHeartbeat)
	switch {
	case age <= heartbeat.DefaultInterval:
		return "healthy"
	case age <= heartbeat.DefaultTTL:
		return "degraded"
	default:
		return "unhealthy"
	}
}

func nilSliceToEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
