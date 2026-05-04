package adminapi

import (
	"encoding/base64"
	"encoding/csv"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// auditRecordJSON is the wire shape for one audit row served by
// GET /admin/v1/audit. UserAgent is empty for rows written before US-018
// added the column — UI tolerates empty.
type auditRecordJSON struct {
	BucketID  string    `json:"bucket_id"`
	Bucket    string    `json:"bucket"`
	EventID   string    `json:"event_id"`
	Time      time.Time `json:"time"`
	Principal string    `json:"principal"`
	Action    string    `json:"action"`
	Resource  string    `json:"resource"`
	Result    string    `json:"result"`
	RequestID string    `json:"request_id"`
	SourceIP  string    `json:"source_ip"`
	UserAgent string    `json:"user_agent"`
}

// auditListResponse is the JSON envelope for the operator audit-log viewer
// (US-018). NextPageToken is base64(EventID) of the last record returned;
// empty when no more rows match.
type auditListResponse struct {
	Records       []auditRecordJSON `json:"records"`
	NextPageToken string            `json:"next_page_token"`
}

const (
	// auditAdminPageSize is the default page size for the JSON list endpoint.
	auditAdminPageSize = 100
	// auditAdminPageMax caps the per-call rows requested via &limit=.
	auditAdminPageMax = 500
	// auditCSVRowCap caps the total rows the CSV exporter streams to the
	// operator before truncating — protects the gateway from runaway scans
	// when the operator forgets a time-range filter.
	auditCSVRowCap = 100_000
	// auditCSVScanPageSize is the per-fanout page size used internally by the
	// CSV exporter. Small enough for memory pressure to stay bounded;
	// large enough to keep the partition iteration count down.
	auditCSVScanPageSize = 500
)

// handleAuditList serves GET /admin/v1/audit (US-018). Filters: since/until
// (RFC3339), action (comma-separated multi-select), principal, bucket
// (resolved to BucketID), page_token (opaque base64). Returns up to
// auditAdminPageSize matching rows + a base64 next-page-token.
//
// Action filtering happens AFTER ListAuditFiltered returns — the meta layer
// has no native action filter, and the post-filter is the same shape as the
// IAM ?audit endpoint's filter chain (Principal/time-window).
func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	q := r.URL.Query()
	filter, actions, errCode, errMsg := s.parseAuditQuery(r, q)
	if errCode != "" {
		writeJSONError(w, http.StatusBadRequest, errCode, errMsg)
		return
	}

	limit := auditAdminPageSize
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "limit must be a positive integer")
			return
		}
		if n > auditAdminPageMax {
			n = auditAdminPageMax
		}
		limit = n
	}

	rows, nextRaw, err := s.collectAuditPage(r, filter, actions, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	stampAuditOverride(r, "admin:ListAudit", "audit", "")

	out := auditListResponse{
		Records:       make([]auditRecordJSON, 0, len(rows)),
		NextPageToken: encodePageToken(nextRaw),
	}
	for _, e := range rows {
		out.Records = append(out.Records, auditRecordToJSON(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAuditCSV serves GET /admin/v1/audit.csv (US-018). Same filter shape
// as handleAuditList. Streams CSV (with header row) to the operator capped
// at auditCSVRowCap rows total — when reached, the response trailer logs a
// truncation marker row and ends. Server-side paginated through
// ListAuditFiltered so memory stays bounded.
func (s *Server) handleAuditCSV(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	q := r.URL.Query()
	filter, actions, errCode, errMsg := s.parseAuditQuery(r, q)
	if errCode != "" {
		writeJSONError(w, http.StatusBadRequest, errCode, errMsg)
		return
	}

	stampAuditOverride(r, "admin:ExportAuditCSV", "audit", "")

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\"audit.csv\"")
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{
		"time", "request_id", "principal", "action", "resource",
		"result", "bucket", "source_ip", "user_agent", "event_id",
	})

	emitted := 0
	cont := filter.Continuation
	for emitted < auditCSVRowCap {
		page := filter
		page.Continuation = cont
		page.Limit = auditCSVScanPageSize
		batch, next, err := s.Meta.ListAuditFiltered(r.Context(), page)
		if err != nil {
			// Mid-stream error — record a truncation marker and stop. We've
			// already sent 200 + headers, so a clean abort is the best we can do.
			_ = cw.Write([]string{"#ERROR", err.Error(), "", "", "", "", "", "", "", ""})
			return
		}
		for _, e := range batch {
			if !matchAction(e.Action, actions) {
				continue
			}
			_ = cw.Write([]string{
				e.Time.UTC().Format(time.RFC3339Nano),
				e.RequestID,
				e.Principal,
				e.Action,
				e.Resource,
				e.Result,
				e.Bucket,
				e.SourceIP,
				e.UserAgent,
				e.EventID,
			})
			emitted++
			if emitted >= auditCSVRowCap {
				break
			}
		}
		if next == "" || len(batch) == 0 {
			return
		}
		cont = next
	}
	// Cap hit — emit a marker row so the operator knows the export was
	// truncated and they should narrow filters or fetch the next page via the
	// JSON endpoint.
	_ = cw.Write([]string{"#TRUNCATED", strconv.Itoa(auditCSVRowCap), "", "", "", "", "", "", "", ""})
}

// parseAuditQuery turns the raw URL query into a meta.AuditFilter + parsed
// action set. Returns (errCode, errMsg) when validation fails — caller sends
// a 400 with the supplied code/message.
func (s *Server) parseAuditQuery(r *http.Request, q map[string][]string) (meta.AuditFilter, []string, string, string) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	filter := meta.AuditFilter{
		Principal: get("principal"),
	}

	if v := get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return filter, nil, "InvalidArgument", "since must be RFC3339"
		}
		filter.Start = t.UTC()
	}
	if v := get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return filter, nil, "InvalidArgument", "until must be RFC3339"
		}
		filter.End = t.UTC()
	}

	if name := get("bucket"); name != "" {
		b, err := s.Meta.GetBucket(r.Context(), name)
		if err != nil {
			if errors.Is(err, meta.ErrBucketNotFound) {
				return filter, nil, "NoSuchBucket", "bucket not found"
			}
			return filter, nil, "Internal", err.Error()
		}
		filter.BucketID = b.ID
		filter.BucketScoped = true
	}

	if v := get("page_token"); v != "" {
		raw, err := decodePageToken(v)
		if err != nil {
			return filter, nil, "InvalidArgument", "page_token is not valid base64"
		}
		filter.Continuation = raw
	}

	actions := splitActions(get("action"))
	return filter, actions, "", ""
}

// collectAuditPage drives ListAuditFiltered across multiple inner pages when
// the action filter shrinks the page below the requested limit. Returns the
// raw (un-encoded) continuation token of the last yielded row when the page
// is full; "" when the underlying scan has no more rows.
func (s *Server) collectAuditPage(r *http.Request, filter meta.AuditFilter, actions []string, limit int) ([]meta.AuditEvent, string, error) {
	out := make([]meta.AuditEvent, 0, limit)
	cont := filter.Continuation
	scanLimit := limit
	if len(actions) > 0 && scanLimit < auditCSVScanPageSize {
		// When post-filtering by action, scan a wider window per round so we
		// don't burn an extra round-trip just to get one more matching row.
		scanLimit = auditCSVScanPageSize
	}
	// Cap inner-scan iterations to keep one HTTP call from running forever
	// against an unbounded backend; an action filter that matches zero rows
	// across the entire window terminates here rather than spinning on
	// continuation tokens.
	const maxInnerScans = 10
	var lastEventID string
	for i := 0; i < maxInnerScans; i++ {
		page := filter
		page.Continuation = cont
		page.Limit = scanLimit
		batch, next, err := s.Meta.ListAuditFiltered(r.Context(), page)
		if err != nil {
			return nil, "", err
		}
		for _, e := range batch {
			if !matchAction(e.Action, actions) {
				continue
			}
			out = append(out, e)
			lastEventID = e.EventID
			if len(out) >= limit {
				return out, lastEventID, nil
			}
		}
		if next == "" || len(batch) == 0 {
			return out, "", nil
		}
		cont = next
	}
	// Hit the inner-scan cap with rows still buffered — return what we have
	// and surface the underlying continuation so the operator can keep paging.
	return out, cont, nil
}

func auditRecordToJSON(e meta.AuditEvent) auditRecordJSON {
	bucketID := ""
	if e.BucketID != uuid.Nil {
		bucketID = e.BucketID.String()
	}
	return auditRecordJSON{
		BucketID:  bucketID,
		Bucket:    e.Bucket,
		EventID:   e.EventID,
		Time:      e.Time,
		Principal: e.Principal,
		Action:    e.Action,
		Resource:  e.Resource,
		Result:    e.Result,
		RequestID: e.RequestID,
		SourceIP:  e.SourceIP,
		UserAgent: e.UserAgent,
	}
}

// splitActions parses the comma-separated action filter into a normalised
// slice. Empty input → nil (matches everything). Whitespace and empty
// segments are dropped.
func splitActions(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// matchAction returns true when the event action satisfies the filter — exact
// case-insensitive match against any entry. nil filter matches every action.
func matchAction(eventAction string, filter []string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, a := range filter {
		if strings.EqualFold(eventAction, a) {
			return true
		}
	}
	return false
}

// encodePageToken / decodePageToken wrap base64.URLEncoding with no padding
// so the page_token is URL-safe and stays opaque to the caller.
func encodePageToken(raw string) string {
	if raw == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodePageToken(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		// Tolerate padded base64 from clients that don't strip '='.
		b2, err2 := base64.URLEncoding.DecodeString(token)
		if err2 != nil {
			return "", err
		}
		b = b2
	}
	return string(b), nil
}

// stampAuditOverride records the operator-meaningful audit row on the request
// context. Principal defaults to the authenticated owner so the audit row
// stays attributable even when the override doesn't carry one explicitly.
func stampAuditOverride(r *http.Request, action, resource, bucket string) {
	owner := ""
	if info := auth.FromContext(r.Context()); info != nil {
		owner = info.Owner
	}
	s3api.SetAuditOverride(r.Context(), action, resource, bucket, owner)
}

