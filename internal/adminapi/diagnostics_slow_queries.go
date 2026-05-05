package adminapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// slowQueryRow is the wire shape for one row served by GET
// /admin/v1/diagnostics/slow-queries (US-003). The object key is truncated
// to 100 characters so a partition with pathological keys can't blow the
// JSON payload; the truncation marker is the trailing ellipsis "…".
type slowQueryRow struct {
	Time      time.Time `json:"ts"`
	Bucket    string    `json:"bucket"`
	BucketID  string    `json:"bucket_id"`
	Op        string    `json:"op"`
	LatencyMS int       `json:"latency_ms"`
	Status    int       `json:"status"`
	RequestID string    `json:"request_id"`
	Principal string    `json:"principal"`
	SourceIP  string    `json:"source_ip"`
	ObjectKey string    `json:"object_key"`
}

type slowQueriesResponse struct {
	Rows          []slowQueryRow `json:"rows"`
	NextPageToken string         `json:"next_page_token"`
}

const slowQueriesObjectKeyMax = 100

// handleDiagnosticsSlowQueries serves GET /admin/v1/diagnostics/slow-queries
// (US-003). Defaults: since=15m, min_ms=100. Page-token is the opaque base64
// continuation token produced by a prior page.
func (s *Server) handleDiagnosticsSlowQueries(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	q := r.URL.Query()

	since := 15 * time.Minute
	if v := q.Get("since"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "since must be a positive Go duration")
			return
		}
		since = d
	}

	minMs := 100
	if v := q.Get("min_ms"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "min_ms must be a non-negative integer")
			return
		}
		minMs = n
	}

	pageToken := ""
	if v := q.Get("page_token"); v != "" {
		raw, err := decodePageToken(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "page_token is not valid base64")
			return
		}
		pageToken = raw
	}

	rows, next, err := s.Meta.ListSlowQueries(r.Context(), since, minMs, pageToken)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	stampAuditOverride(r, "admin:ListSlowQueries", "diagnostics:slow-queries", "")

	out := slowQueriesResponse{
		Rows:          make([]slowQueryRow, 0, len(rows)),
		NextPageToken: encodePageToken(next),
	}
	for _, e := range rows {
		out.Rows = append(out.Rows, toSlowQueryRow(e))
	}
	writeJSON(w, http.StatusOK, out)
}

func toSlowQueryRow(e meta.AuditEvent) slowQueryRow {
	statusInt := 0
	if e.Result != "" {
		if n, err := strconv.Atoi(e.Result); err == nil {
			statusInt = n
		}
	}
	bucketID := ""
	if e.BucketID != uuid.Nil {
		bucketID = e.BucketID.String()
	}
	return slowQueryRow{
		Time:      e.Time,
		Bucket:    e.Bucket,
		BucketID:  bucketID,
		Op:        e.Action,
		LatencyMS: e.TotalTimeMS,
		Status:    statusInt,
		RequestID: e.RequestID,
		Principal: e.Principal,
		SourceIP:  e.SourceIP,
		ObjectKey: truncateObjectKey(deriveSlowQueryObjectKey(e), slowQueriesObjectKeyMax),
	}
}

// deriveSlowQueryObjectKey strips the leading "/<bucket>/" prefix from the
// stored Resource string so the row carries just the object key. Returns ""
// for bucket-scoped or non-bucket (iam:*) audit rows.
func deriveSlowQueryObjectKey(e meta.AuditEvent) string {
	r := e.Resource
	if r == "" {
		return ""
	}
	if strings.HasPrefix(r, "iam:") {
		return ""
	}
	if e.Bucket == "" || e.Bucket == "-" {
		return ""
	}
	prefix := "/" + e.Bucket + "/"
	if strings.HasPrefix(r, prefix) {
		return r[len(prefix):]
	}
	return ""
}

func truncateObjectKey(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
