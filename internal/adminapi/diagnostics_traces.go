package adminapi

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/danchupin/strata/internal/otel/ringbuf"
)

// diagnosticsTracesResponse is the wire shape returned by the recent-traces
// list endpoint. `total` is the filtered count (matches the cursor walked
// by limit/offset) so the UI can render "showing N of M" and page through
// the filtered subset without surprising jumps.
type diagnosticsTracesResponse struct {
	Traces []ringbuf.TraceSummary `json:"traces"`
	Total  int                    `json:"total"`
}

const (
	defaultDiagnosticsTracesLimit = 50
	maxDiagnosticsTracesLimit     = 200
	maxDiagnosticsPathSubstrLen   = 256
)

var allowedDiagnosticsMethods = []string{
	http.MethodPut, http.MethodGet, http.MethodDelete, http.MethodPost,
	http.MethodHead, http.MethodOptions, http.MethodPatch,
}

var allowedDiagnosticsStatuses = []string{"Error", "OK"}

// handleDiagnosticsTraces serves GET /admin/v1/diagnostics/traces (US-008,
// extended in US-001 drain-followup with filter query params). Returns the
// most-recent N retained trace summaries from the in-process ring buffer
// filtered by the supplied query params, so the trace browser UI can narrow
// the list without fetching everything. limit caps at 200 to bound the
// response cost; offset enables paging via the same query-string convention
// used by other diagnostics endpoints.
func (s *Server) handleDiagnosticsTraces(w http.ResponseWriter, r *http.Request) {
	if s.TraceRingbuf == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "RingbufUnavailable",
			"in-process trace ring buffer is disabled (STRATA_OTEL_RINGBUF=off)")
		return
	}
	q := r.URL.Query()

	limit := defaultDiagnosticsTracesLimit
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxDiagnosticsTracesLimit {
		limit = maxDiagnosticsTracesLimit
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}

	opts, err := parseDiagnosticsFilter(q)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidFilter", err.Error())
		return
	}

	stampAuditOverride(r, "admin:GetDiagnosticsTraces", "diagnostics", "-")

	filtered := s.TraceRingbuf.Filter(opts)
	total := len(filtered)

	page := pageSummaries(filtered, limit, offset)
	if page == nil {
		page = []ringbuf.TraceSummary{}
	}
	writeJSON(w, http.StatusOK, diagnosticsTracesResponse{
		Traces: page,
		Total:  total,
	})
}

func pageSummaries(in []ringbuf.TraceSummary, limit, offset int) []ringbuf.TraceSummary {
	if offset >= len(in) {
		return []ringbuf.TraceSummary{}
	}
	end := min(offset+limit, len(in))
	return in[offset:end]
}

func parseDiagnosticsFilter(q map[string][]string) (ringbuf.FilterOpts, error) {
	var opts ringbuf.FilterOpts

	if raw := strings.TrimSpace(firstQuery(q, "method")); raw != "" {
		upper := strings.ToUpper(raw)
		if !slices.Contains(allowedDiagnosticsMethods, upper) {
			return opts, fmt.Errorf("method must be one of %s",
				strings.Join(allowedDiagnosticsMethods, ", "))
		}
		opts.Method = upper
	}

	if raw := strings.TrimSpace(firstQuery(q, "status")); raw != "" {
		matched := ""
		for _, allowed := range allowedDiagnosticsStatuses {
			if strings.EqualFold(raw, allowed) {
				matched = allowed
				break
			}
		}
		if matched == "" {
			return opts, fmt.Errorf("status must be one of %s",
				strings.Join(allowedDiagnosticsStatuses, ", "))
		}
		opts.Status = matched
	}

	pathRaw := firstQuery(q, "path_substr")
	if pathRaw == "" {
		pathRaw = firstQuery(q, "path")
	}
	if pathRaw != "" {
		if len(pathRaw) > maxDiagnosticsPathSubstrLen {
			return opts, fmt.Errorf("path filter exceeds %d-character limit", maxDiagnosticsPathSubstrLen)
		}
		opts.PathSubstr = pathRaw
	}

	min, err := parseOptionalInt(q, "min_duration_ms")
	if err != nil {
		return opts, err
	}
	max, err := parseOptionalInt(q, "max_duration_ms")
	if err != nil {
		return opts, err
	}
	if min != nil && *min < 0 {
		return opts, fmt.Errorf("min_duration_ms must be non-negative")
	}
	if max != nil && *max < 0 {
		return opts, fmt.Errorf("max_duration_ms must be non-negative")
	}
	if min != nil && max != nil && *min > *max {
		return opts, fmt.Errorf("min_duration_ms (%d) must not exceed max_duration_ms (%d)", *min, *max)
	}
	opts.MinDurationMs = min
	opts.MaxDurationMs = max

	return opts, nil
}

func parseOptionalInt(q map[string][]string, key string) (*int64, error) {
	raw := strings.TrimSpace(firstQuery(q, key))
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return &n, nil
}

func firstQuery(q map[string][]string, key string) string {
	if vs, ok := q[key]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}

