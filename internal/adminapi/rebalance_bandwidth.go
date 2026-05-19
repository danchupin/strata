package adminapi

import (
	"context"
	"net/http"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/promclient"
	"github.com/danchupin/strata/internal/s3api"
)

// Aggregate PromQL backing GET /admin/v1/rebalance-bandwidth. Sums the
// rebalance worker counters across every {to=...} cluster label so the
// Cluster Overview rebalance card shows one cluster-wide bandwidth row
// regardless of how many destination clusters are currently moving.
const (
	rebalanceBytesRate1mAllExpr  = `sum(rate(strata_rebalance_bytes_moved_total[1m]))`
	rebalanceChunksRate1mAllExpr = `sum(rate(strata_rebalance_chunks_moved_total[1m]))`
)

// RebalanceBandwidthResponse is the wire shape returned by
// GET /admin/v1/rebalance-bandwidth (US-003 drain-rebalance-transparency).
// MetricsAvailable=false signals the UI to hide the live-bandwidth row;
// callers fall back to the static rate * replicas math.
type RebalanceBandwidthResponse struct {
	MetricsAvailable bool    `json:"metrics_available"`
	BytesPerSec      float64 `json:"bytes_per_sec"`
	ChunksPerSec     float64 `json:"chunks_per_sec"`
}

// handleGetRebalanceBandwidth serves the Cluster Overview rebalance card's
// live-bandwidth row. Degrades to 200 + metrics_available=false when Prom
// is unset or returns an error — mirrors the rebalance-progress contract
// so the UI never hard-fails on an upstream outage.
func (s *Server) handleGetRebalanceBandwidth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:GetRebalanceBandwidth", "rebalance-bandwidth", "-", owner)

	resp := RebalanceBandwidthResponse{}
	if !s.Prom.Available() {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp = buildRebalanceBandwidth(ctx, s.Prom)
	writeJSON(w, http.StatusOK, resp)
}

// buildRebalanceBandwidth issues the two instant-vector queries and rolls
// them up. Returns metrics_available=false on any Prom error.
func buildRebalanceBandwidth(ctx context.Context, prom *promclient.Client) RebalanceBandwidthResponse {
	out := RebalanceBandwidthResponse{}
	bytesSamples, err := prom.Query(ctx, rebalanceBytesRate1mAllExpr)
	if err != nil {
		return out
	}
	chunksSamples, err := prom.Query(ctx, rebalanceChunksRate1mAllExpr)
	if err != nil {
		return out
	}
	out.MetricsAvailable = true
	if v, ok := firstFiniteValue(bytesSamples); ok {
		out.BytesPerSec = v
	}
	if v, ok := firstFiniteValue(chunksSamples); ok {
		out.ChunksPerSec = v
	}
	return out
}
