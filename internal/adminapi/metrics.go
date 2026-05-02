package adminapi

import "net/http"

// handleMetricsTimeseries serves GET /admin/v1/metrics/timeseries. Phase 1 stub.
func (s *Server) handleMetricsTimeseries(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, MetricsTimeseriesResponse{Series: []MetricSeries{}})
}
