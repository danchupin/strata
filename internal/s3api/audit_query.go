package s3api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

// auditResponse is the JSON envelope for the [iam root]-gated /?audit
// endpoint (US-023). Records are newest-first across all matching partitions;
// pagination uses an opaque continuation token equal to the EventID of the
// last record returned. start/end accept RFC3339 (ISO-8601) timestamps.
type auditResponse struct {
	Records               []auditRecord `json:"records"`
	NextContinuationToken string        `json:"next_continuation_token,omitempty"`
}

type auditRecord struct {
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
}

// listAudit serves
// GET /?audit&start=<ISO8601>&end=<ISO8601>&principal=<id>&bucket=<name>&limit=<n>&continuation=<token>.
// Authentication: header-signed [iam root] only. Anonymous, non-root and
// presigned-URL callers all yield 403 AccessDenied. Filters compose; pagination
// uses an opaque continuation token equal to the EventID of the last record on
// the previous page.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	// Refuse presigned-URL requests outright — admin endpoints require a
	// fresh header-signed call so the token can't be replayed from a URL.
	if r.URL.Query().Get("X-Amz-Signature") != "" {
		writeError(w, r, ErrAccessDenied)
		return
	}
	info := auth.FromContext(r.Context())
	if info == nil || info.IsAnonymous || info.Owner != IAMRootPrincipal {
		writeError(w, r, ErrAccessDenied)
		return
	}
	q := r.URL.Query()

	filter := meta.AuditFilter{
		Principal:    q.Get("principal"),
		Continuation: q.Get("continuation"),
	}

	if v := q.Get("start"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		filter.Start = t.UTC()
	}
	if v := q.Get("end"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		filter.End = t.UTC()
	}

	if v := q.Get("bucket"); v != "" {
		b, err := s.Meta.GetBucket(r.Context(), v)
		if err != nil {
			mapMetaErr(w, r, err)
			return
		}
		filter.BucketID = b.ID
		filter.BucketScoped = true
	}

	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		if n > 1000 {
			n = 1000
		}
		limit = n
	}
	filter.Limit = limit

	rows, next, err := s.Meta.ListAuditFiltered(r.Context(), filter)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}

	out := auditResponse{
		Records:               make([]auditRecord, 0, len(rows)),
		NextContinuationToken: next,
	}
	for _, row := range rows {
		out.Records = append(out.Records, auditRecord{
			BucketID:  row.BucketID.String(),
			Bucket:    row.Bucket,
			EventID:   row.EventID,
			Time:      row.Time,
			Principal: row.Principal,
			Action:    row.Action,
			Resource:  row.Resource,
			Result:    row.Result,
			RequestID: row.RequestID,
			SourceIP:  row.SourceIP,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}
