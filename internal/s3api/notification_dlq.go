package s3api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

// notifyDLQResponse is the JSON envelope for the [iam root]-gated
// /?notify-dlq endpoint. Records are ordered as the meta backend returns
// them (newest day partition first; within a partition, TimeUUID ascending);
// pagination uses an opaque continuation token equal to the EventID of the
// last record on the previous page.
type notifyDLQResponse struct {
	Records               []notifyDLQRecord `json:"records"`
	NextContinuationToken string            `json:"next_continuation_token,omitempty"`
}

type notifyDLQRecord struct {
	BucketID   string          `json:"bucket_id"`
	Bucket     string          `json:"bucket"`
	Key        string          `json:"key"`
	EventID    string          `json:"event_id"`
	EventName  string          `json:"event_name"`
	EventTime  time.Time       `json:"event_time"`
	ConfigID   string          `json:"config_id"`
	TargetType string          `json:"target_type"`
	TargetARN  string          `json:"target_arn"`
	Payload    json.RawMessage `json:"payload"`
	Attempts   int             `json:"attempts"`
	Reason     string          `json:"reason"`
	EnqueuedAt time.Time       `json:"enqueued_at"`
}

// listNotificationDLQ serves GET /?notify-dlq&bucket=<name>&limit=<n>&continuation=<event_id>.
// Authentication: caller must be the [iam root] principal. Other principals
// (or anonymous) yield 403 AccessDenied. Missing bucket query param yields
// 400 InvalidArgument; unknown bucket yields the standard NoSuchBucket flow.
func (s *Server) listNotificationDLQ(w http.ResponseWriter, r *http.Request) {
	info := auth.FromContext(r.Context())
	if info == nil || info.IsAnonymous || info.Owner != IAMRootPrincipal {
		writeError(w, r, ErrAccessDenied)
		return
	}
	q := r.URL.Query()
	bucketName := q.Get("bucket")
	if bucketName == "" {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucketName)
	if err != nil {
		mapMetaErr(w, r, err)
		return
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
	cont := q.Get("continuation")

	// Pull more than `limit` so we can drop everything up to and including
	// `continuation` and still return a full page. ListNotificationDLQ caps
	// at 1000; if a deployment fills more than 1000 DLQ rows ahead of the
	// page cursor on a single bucket the operator is already past the point
	// of "inspect manually" and should drain to log first.
	pull := limit + 1
	if cont != "" {
		pull = 1000
	}
	rows, err := s.Meta.ListNotificationDLQ(r.Context(), b.ID, pull)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}

	if cont != "" {
		idx := -1
		for i, row := range rows {
			if row.EventID == cont {
				idx = i
				break
			}
		}
		if idx >= 0 {
			rows = rows[idx+1:]
		}
	}

	out := notifyDLQResponse{Records: make([]notifyDLQRecord, 0, len(rows))}
	for i, row := range rows {
		if i >= limit {
			out.NextContinuationToken = out.Records[limit-1].EventID
			break
		}
		out.Records = append(out.Records, toNotifyDLQRecord(row))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

func toNotifyDLQRecord(row meta.NotificationDLQEntry) notifyDLQRecord {
	payload := row.Payload
	if !json.Valid(payload) {
		// Defensive: persist as a JSON string if the upstream payload is not
		// valid JSON. The notification hook always emits JSON, so this branch
		// is for a future sink that emits something else (binary/MsgPack).
		quoted, _ := json.Marshal(string(payload))
		payload = quoted
	}
	return notifyDLQRecord{
		BucketID:   row.BucketID.String(),
		Bucket:     row.Bucket,
		Key:        row.Key,
		EventID:    row.EventID,
		EventName:  row.EventName,
		EventTime:  row.EventTime,
		ConfigID:   row.ConfigID,
		TargetType: row.TargetType,
		TargetARN:  row.TargetARN,
		Payload:    payload,
		Attempts:   row.Attempts,
		Reason:     row.Reason,
		EnqueuedAt: row.EnqueuedAt,
	}
}

