package adminapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/promclient"
	"github.com/danchupin/strata/internal/s3api"
)

// PromQL backing GET /admin/v1/clusters/{id}/rebalance-progress. The
// rebalance worker emits both counters with a {to=…} (moved) / {target=…}
// (refused) label naming the destination cluster. Sparkline uses a 5m
// rate window over the 1h history range so the points line up with the
// per-minute cadence of the chip.
const (
	rebalanceMovedTotalExprFmt   = `sum(strata_rebalance_chunks_moved_total{to="%s"})`
	rebalanceRefusedTotalExprFmt = `sum(strata_rebalance_refused_total{target="%s"})`
	rebalanceMovedRateExprFmt    = `sum(rate(strata_rebalance_chunks_moved_total{to="%s"}[5m]))`

	rebalanceProgressRange = time.Hour
	rebalanceProgressStep  = time.Minute
)

// ClusterRebalanceProgressResponse is the wire shape returned by
// GET /admin/v1/clusters/{id}/rebalance-progress.
//
// MetricsAvailable=false signals the UI to render "(metrics unavailable)"
// in place of the chip body; Series stays nil/empty in that case.
type ClusterRebalanceProgressResponse struct {
	MetricsAvailable bool          `json:"metrics_available"`
	MovedTotal       float64       `json:"moved_total"`
	RefusedTotal     float64       `json:"refused_total"`
	Series           []MetricPoint `json:"series"`
}

// handleClusterRebalanceProgress serves the per-cluster rebalance chip
// driving US-005 placement-ui. Degrades to a 200 with metrics_available=
// false when Prom is unset or returns an error, mirroring the
// /admin/v1/metrics/timeseries graceful-degradation contract — the chip
// never fails the cluster card render.
func (s *Server) handleClusterRebalanceProgress(w http.ResponseWriter, r *http.Request) {
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
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:GetClusterRebalanceProgress", "cluster:"+id, "-", owner)

	resp := ClusterRebalanceProgressResponse{Series: []MetricPoint{}}
	if !s.Prom.Available() {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp = buildClusterRebalanceProgress(ctx, s.Prom, id, time.Now())
	writeJSON(w, http.StatusOK, resp)
}

// buildClusterRebalanceProgress is the testable core of the handler.
// Returns metrics_available=false on any Prom error so the UI never
// hard-fails on an upstream outage.
func buildClusterRebalanceProgress(ctx context.Context, prom *promclient.Client, id string, now time.Time) ClusterRebalanceProgressResponse {
	out := ClusterRebalanceProgressResponse{Series: []MetricPoint{}}

	movedSamples, err := prom.Query(ctx, fmt.Sprintf(rebalanceMovedTotalExprFmt, id))
	if err != nil {
		return out
	}
	refusedSamples, err := prom.Query(ctx, fmt.Sprintf(rebalanceRefusedTotalExprFmt, id))
	if err != nil {
		return out
	}

	end := now.UTC()
	start := end.Add(-rebalanceProgressRange)
	series, err := prom.QueryRange(ctx, fmt.Sprintf(rebalanceMovedRateExprFmt, id), start, end, rebalanceProgressStep)
	if err != nil {
		return out
	}

	out.MetricsAvailable = true
	if v, ok := firstFiniteValue(movedSamples); ok {
		out.MovedTotal = v
	}
	if v, ok := firstFiniteValue(refusedSamples); ok {
		out.RefusedTotal = v
	}

	if len(series) > 0 {
		points := make([]MetricPoint, 0, len(series[0].Points))
		for _, p := range series[0].Points {
			if math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
				continue
			}
			points = append(points, MetricPoint{float64(p.Timestamp.UnixMilli()), p.Value})
		}
		sort.Slice(points, func(i, j int) bool { return points[i][0] < points[j][0] })
		out.Series = points
	}
	return out
}

// firstFiniteValue returns the first non-NaN / non-Inf value in the slice.
// Prom's sum() returns at most one sample on an instant query — we still
// guard against NaN edges (empty window, counter reset right at the edge).
func firstFiniteValue(samples []promclient.Sample) (float64, bool) {
	for _, s := range samples {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			continue
		}
		return s.Value, true
	}
	return 0, false
}
