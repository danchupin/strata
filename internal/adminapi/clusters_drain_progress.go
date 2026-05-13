package adminapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/promclient"
	"github.com/danchupin/strata/internal/rebalance"
	"github.com/danchupin/strata/internal/s3api"
)

// PromQL backing the ETA half of GET /admin/v1/clusters/{id}/drain-progress.
// `from=<id>` is the source-cluster label stamped by the rebalance worker
// on every successful chunk copy (US-005 placement-rebalance), so the
// expression measures chunks-leaving-the-cluster — exactly the rate that
// drains it.
const drainMoveRateExprFmt = `sum(rate(strata_rebalance_chunks_moved_total{from="%s"}[5m]))`

// ClusterDrainProgressResponse is the wire shape returned by
// GET /admin/v1/clusters/{id}/drain-progress (US-003 drain-lifecycle).
//
// Pointer fields go *null* in the JSON output when no value applies. The
// shape is deliberately narrow: the UI consumes one record per draining
// cluster and the operator console reads the same JSON straight via curl.
type ClusterDrainProgressResponse struct {
	State            string   `json:"state"`
	Mode             string   `json:"mode"`
	ChunksOnCluster  *int64   `json:"chunks_on_cluster"`
	BytesOnCluster   *int64   `json:"bytes_on_cluster"`
	BaseChunks       *int64   `json:"base_chunks_at_start"`
	LastScanAt       *string  `json:"last_scan_at"`
	ETASeconds       *int64   `json:"eta_seconds"`
	DeregisterReady  *bool    `json:"deregister_ready"`
	Warnings         []string `json:"warnings,omitempty"`
}

// handleClusterDrainProgress serves GET /admin/v1/clusters/{id}/drain-
// progress. Reads from the in-process ProgressTracker populated by the
// rebalance worker — never scans synchronously per request. ETA is
// derived from the Prom rate of `strata_rebalance_chunks_moved_total
// {from=<id>}` over the last 5 minutes; Prom unavailable or rate=0
// surfaces eta_seconds=null without failing the request.
func (s *Server) handleClusterDrainProgress(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "cluster id is required")
		return
	}
	if len(s.KnownClusters) > 0 {
		if _, ok := s.KnownClusters[id]; !ok {
			writeJSONError(w, http.StatusBadRequest, "UnknownCluster",
				"cluster id is not configured (check STRATA_RADOS_CLUSTERS / STRATA_S3_CLUSTERS)")
			return
		}
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	if s.RebalanceProgress == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ProgressUnavailable",
			"drain-progress requires the rebalance worker — start `strata server --workers=rebalance`")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:GetClusterDrainProgress", "cluster:"+id, "-", owner)

	rows, err := s.Meta.ListClusterStates(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	row, ok := rows[id]
	if !ok || row.State == "" {
		row = meta.ClusterStateRow{State: meta.ClusterStateLive}
	}
	resp := ClusterDrainProgressResponse{State: row.State, Mode: row.Mode}
	if !meta.IsDrainingForWrite(row.State) {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	snap, scanOK := s.RebalanceProgress.Snapshot(id)
	resp = buildDrainProgressResponse(ctx, s.Prom, id, row, snap, scanOK, s.RebalanceProgress.Interval, time.Now())
	writeJSON(w, http.StatusOK, resp)
}

// buildDrainProgressResponse is the testable core of the handler. Pure
// over its inputs — no IO besides the optional Prom query.
func buildDrainProgressResponse(ctx context.Context, prom *promclient.Client, id string, row meta.ClusterStateRow, snap rebalance.ProgressSnapshot, ok bool, interval time.Duration, now time.Time) ClusterDrainProgressResponse {
	out := ClusterDrainProgressResponse{State: row.State, Mode: row.Mode}
	if !ok || snap.LastScanAt.IsZero() {
		out.Warnings = append(out.Warnings, "progress scan pending; rebalance worker has not yet committed a tick")
		return out
	}
	chunks := snap.Chunks
	bytes := snap.Bytes
	out.ChunksOnCluster = &chunks
	out.BytesOnCluster = &bytes
	if snap.BaseChunks > 0 {
		base := snap.BaseChunks
		out.BaseChunks = &base
	}
	scan := snap.LastScanAt.UTC().Format(time.RFC3339)
	out.LastScanAt = &scan
	deregReady := snap.Chunks == 0
	out.DeregisterReady = &deregReady

	if eta, ok := drainETASeconds(ctx, prom, id, snap.Chunks); ok {
		out.ETASeconds = &eta
	}

	if interval > 0 && now.Sub(snap.LastScanAt) > 2*interval {
		out.Warnings = append(out.Warnings, "progress data stale")
	}
	return out
}

// drainETASeconds queries Prom for the chunk-out rate and returns the
// projected seconds-to-empty. Returns (0, false) on any Prom error, on
// rate=0 (no traffic → undefined ETA), or when chunks is already 0
// (the dereg chip carries the signal, ETA stays null).
func drainETASeconds(ctx context.Context, prom *promclient.Client, id string, chunks int64) (int64, bool) {
	if prom == nil || !prom.Available() || chunks <= 0 {
		return 0, false
	}
	samples, err := prom.Query(ctx, fmt.Sprintf(drainMoveRateExprFmt, id))
	if err != nil || len(samples) == 0 {
		return 0, false
	}
	rate := samples[0].Value
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
		return 0, false
	}
	eta := float64(chunks) / rate
	if math.IsNaN(eta) || math.IsInf(eta, 0) || eta <= 0 {
		return 0, false
	}
	return int64(math.Ceil(eta)), true
}
