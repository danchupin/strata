package adminapi

import (
	"net/http"
)

// handleDiagnosticsTrace serves GET /admin/v1/diagnostics/trace/{requestID}
// (US-005). Looks up the trace by request id in the in-process ring buffer
// and returns the waterfall-friendly JSON shape consumed by the trace
// browser UI (US-006). 404 NotFound when the id is unknown (typical when
// the trace has aged out).
func (s *Server) handleDiagnosticsTrace(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("requestID")
	if requestID == "" {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "request id required")
		return
	}
	if s.TraceRingbuf == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "RingbufUnavailable",
			"in-process trace ring buffer is disabled (STRATA_OTEL_RINGBUF=off)")
		return
	}
	stampAuditOverride(r, "admin:GetTrace", "diagnostics:trace", "")

	if doc, ok := s.TraceRingbuf.GetByRequestID(requestID); ok {
		writeJSON(w, http.StatusOK, doc)
		return
	}
	if doc, ok := s.TraceRingbuf.GetByTraceID(requestID); ok {
		writeJSON(w, http.StatusOK, doc)
		return
	}
	writeJSONError(w, http.StatusNotFound, "NotFound",
		"no trace retained for the supplied request-id (it may have aged out of the in-process ring buffer)")
}
