package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// BucketQuotaJSON is the operator-console wire shape for per-bucket quota
// (US-009). Zero on any field means "unlimited" — matches the underlying
// meta.BucketQuota semantics.
type BucketQuotaJSON struct {
	MaxBytes          int64 `json:"max_bytes"`
	MaxObjects        int64 `json:"max_objects"`
	MaxBytesPerObject int64 `json:"max_bytes_per_object"`
}

// UserQuotaJSON is the operator-console wire shape for per-user quota
// (US-009). Zero means "unlimited".
type UserQuotaJSON struct {
	MaxBuckets    int32 `json:"max_buckets"`
	TotalMaxBytes int64 `json:"total_max_bytes"`
}

// UsageRowJSON is one row of per-(bucket, day, storage_class) usage history.
type UsageRowJSON struct {
	Bucket         string `json:"bucket,omitempty"`
	Day            string `json:"day"`
	StorageClass   string `json:"storage_class"`
	ByteSeconds    int64  `json:"byte_seconds"`
	ObjectCountAvg int64  `json:"object_count_avg"`
	ObjectCountMax int64  `json:"object_count_max"`
}

// BucketUsageResponse is the response shape for the per-bucket usage endpoint.
type BucketUsageResponse struct {
	Rows []UsageRowJSON `json:"rows"`
}

// UserUsageTotals carries the cross-row sums returned alongside per-user
// usage rows so the UI can render the billing summary without re-summing.
type UserUsageTotals struct {
	ByteSeconds int64 `json:"byte_seconds"`
	Objects     int64 `json:"objects"`
}

// UserUsageResponse is the response shape for the per-user usage endpoint.
type UserUsageResponse struct {
	Rows   []UsageRowJSON  `json:"rows"`
	Totals UserUsageTotals `json:"totals"`
}

// handleBucketGetQuota serves GET /admin/v1/buckets/{bucket}/quota.
// Returns 200 + BucketQuotaJSON when configured, 404 NoSuchBucketQuota when
// no quota row exists, 404 NoSuchBucket when the bucket itself is missing.
func (s *Server) handleBucketGetQuota(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	q, ok, err := s.Meta.GetBucketQuota(r.Context(), b.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "NoSuchBucketQuota",
			"no quota configuration on bucket")
		return
	}
	writeJSON(w, http.StatusOK, BucketQuotaJSON{
		MaxBytes:          q.MaxBytes,
		MaxObjects:        q.MaxObjects,
		MaxBytesPerObject: q.MaxBytesPerObject,
	})
}

// handleBucketSetQuota serves PUT /admin/v1/buckets/{bucket}/quota. Body is
// BucketQuotaJSON. Audit: admin:PutBucketQuota.
func (s *Server) handleBucketSetQuota(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req BucketQuotaJSON
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	if req.MaxBytes < 0 || req.MaxObjects < 0 || req.MaxBytesPerObject < 0 {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"quota fields must be non-negative (zero ⇒ unlimited)")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:PutBucketQuota", "bucket:"+name, name, owner)

	b, err := s.Meta.GetBucket(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	q := meta.BucketQuota{
		MaxBytes:          req.MaxBytes,
		MaxObjects:        req.MaxObjects,
		MaxBytesPerObject: req.MaxBytesPerObject,
	}
	if err := s.Meta.SetBucketQuota(ctx, b.ID, q); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBucketDeleteQuota serves DELETE /admin/v1/buckets/{bucket}/quota.
// Idempotent: returns 204 even when no quota was configured.
// Audit: admin:DeleteBucketQuota.
func (s *Server) handleBucketDeleteQuota(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:DeleteBucketQuota", "bucket:"+name, name, owner)
	b, err := s.Meta.GetBucket(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err := s.Meta.DeleteBucketQuota(ctx, b.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBucketGetUsage serves GET /admin/v1/buckets/{bucket}/usage?start=YYYY-MM-DD&end=YYYY-MM-DD.
// Returns rows from meta.ListUsageAggregates folded across every recorded
// storage_class, sorted by (day ASC, storage_class ASC).
func (s *Server) handleBucketGetUsage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	dayFrom, dayTo, err := parseUsageDayRange(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", err.Error())
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	rows, err := s.Meta.ListUsageAggregates(r.Context(), b.ID, "", dayFrom, dayTo)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	out := BucketUsageResponse{Rows: make([]UsageRowJSON, 0, len(rows))}
	for _, agg := range rows {
		out.Rows = append(out.Rows, UsageRowJSON{
			Day:            agg.Day.UTC().Format("2006-01-02"),
			StorageClass:   agg.StorageClass,
			ByteSeconds:    agg.ByteSeconds,
			ObjectCountAvg: agg.ObjectCountAvg,
			ObjectCountMax: agg.ObjectCountMax,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// parseUsageDayRange reads start / end query params (YYYY-MM-DD inclusive)
// and returns a half-open [from, to) day range. Defaults: end = today UTC,
// start = end - 30d.
func parseUsageDayRange(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	now := time.Now().UTC()
	endDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	startDay := endDay.AddDate(0, 0, -30)

	if v := q.Get("start"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, time.UTC)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("start must be YYYY-MM-DD")
		}
		startDay = t
	}
	if v := q.Get("end"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, time.UTC)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("end must be YYYY-MM-DD")
		}
		endDay = t
	}
	if endDay.Before(startDay) {
		return time.Time{}, time.Time{}, errors.New("end must be on or after start")
	}
	// Caller's `end` is inclusive in the operator's intent (last day shown);
	// ListUsageAggregates is half-open [from, to), so widen by one day. This
	// also makes `start == end` a single-day window rather than empty.
	return startDay, endDay.AddDate(0, 0, 1), nil
}
