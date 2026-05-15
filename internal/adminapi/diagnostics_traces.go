package adminapi

import (
	"net/http"
	"strconv"

	"github.com/danchupin/strata/internal/otel/ringbuf"
)

// diagnosticsTracesResponse is the wire shape returned by the recent-traces
// list endpoint. `total` is the live retained count (not affected by limit /
// offset) so the UI can render "showing N of M".
type diagnosticsTracesResponse struct {
	Traces []ringbuf.TraceSummary `json:"traces"`
	Total  int                    `json:"total"`
}

const (
	defaultDiagnosticsTracesLimit = 50
	maxDiagnosticsTracesLimit     = 200
)

// handleDiagnosticsTraces serves GET /admin/v1/diagnostics/traces (US-008).
// Returns the most-recent N retained trace summaries from the in-process
// ring buffer so the trace browser UI can render a list view without forcing
// the operator to know a request_id upfront. limit caps at 200 to bound the
// response cost; offset enables paging via the same query-string convention
// used by other diagnostics endpoints.
func (s *Server) handleDiagnosticsTraces(w http.ResponseWriter, r *http.Request) {
	if s.TraceRingbuf == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "RingbufUnavailable",
			"in-process trace ring buffer is disabled (STRATA_OTEL_RINGBUF=off)")
		return
	}
	limit := defaultDiagnosticsTracesLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxDiagnosticsTracesLimit {
		limit = maxDiagnosticsTracesLimit
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	stampAuditOverride(r, "admin:GetDiagnosticsTraces", "diagnostics", "-")

	traces := s.TraceRingbuf.List(limit, offset)
	if traces == nil {
		traces = []ringbuf.TraceSummary{}
	}
	writeJSON(w, http.StatusOK, diagnosticsTracesResponse{
		Traces: traces,
		Total:  s.TraceRingbuf.TraceCount(),
	})
}
