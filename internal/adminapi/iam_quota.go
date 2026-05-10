package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// handleIAMUserGetQuota serves GET /admin/v1/iam/users/{userName}/quota.
// Returns 200 + UserQuotaJSON when configured, 404 NoSuchUserQuota when no
// quota row exists, 404 NoSuchEntity when the user itself is missing.
func (s *Server) handleIAMUserGetQuota(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("userName")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "user_name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	if _, err := s.Meta.GetIAMUser(r.Context(), name); err != nil {
		if errors.Is(err, meta.ErrIAMUserNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "user not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	q, ok, err := s.Meta.GetUserQuota(r.Context(), name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "NoSuchUserQuota",
			"no quota configuration for user")
		return
	}
	writeJSON(w, http.StatusOK, UserQuotaJSON{
		MaxBuckets:    q.MaxBuckets,
		TotalMaxBytes: q.TotalMaxBytes,
	})
}

// handleIAMUserSetQuota serves PUT /admin/v1/iam/users/{userName}/quota. Body
// is UserQuotaJSON. Audit: admin:PutUserQuota.
func (s *Server) handleIAMUserSetQuota(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	name := r.PathValue("userName")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "user_name is required")
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
	var req UserQuotaJSON
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	if req.MaxBuckets < 0 || req.TotalMaxBytes < 0 {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"quota fields must be non-negative (zero ⇒ unlimited)")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:PutUserQuota", "iam:"+name, "", owner)

	if _, err := s.Meta.GetIAMUser(ctx, name); err != nil {
		if errors.Is(err, meta.ErrIAMUserNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "user not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	q := meta.UserQuota{
		MaxBuckets:    req.MaxBuckets,
		TotalMaxBytes: req.TotalMaxBytes,
	}
	if err := s.Meta.SetUserQuota(ctx, name, q); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleIAMUserDeleteQuota serves DELETE /admin/v1/iam/users/{userName}/quota.
// Idempotent. Audit: admin:DeleteUserQuota.
func (s *Server) handleIAMUserDeleteQuota(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("userName")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "user_name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:DeleteUserQuota", "iam:"+name, "", owner)
	if _, err := s.Meta.GetIAMUser(ctx, name); err != nil {
		if errors.Is(err, meta.ErrIAMUserNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "user not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err := s.Meta.DeleteUserQuota(ctx, name); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleIAMUserGetUsage serves GET /admin/v1/iam/users/{userName}/usage. Lists
// per-(bucket, day, storage_class) rows across every bucket the user owns and
// returns cross-row totals so the billing summary can render without
// re-summing client-side.
func (s *Server) handleIAMUserGetUsage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("userName")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "user_name is required")
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
	if _, err := s.Meta.GetIAMUser(r.Context(), name); err != nil {
		if errors.Is(err, meta.ErrIAMUserNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "user not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	buckets, err := s.Meta.ListBuckets(r.Context(), name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	out := UserUsageResponse{Rows: make([]UsageRowJSON, 0)}
	for _, b := range buckets {
		rows, lerr := s.Meta.ListUsageAggregates(r.Context(), b.ID, "", dayFrom, dayTo)
		if lerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "Internal", lerr.Error())
			return
		}
		for _, agg := range rows {
			out.Rows = append(out.Rows, UsageRowJSON{
				Bucket:         b.Name,
				Day:            agg.Day.UTC().Format("2006-01-02"),
				StorageClass:   agg.StorageClass,
				ByteSeconds:    agg.ByteSeconds,
				ObjectCountAvg: agg.ObjectCountAvg,
				ObjectCountMax: agg.ObjectCountMax,
			})
			out.Totals.ByteSeconds += agg.ByteSeconds
			out.Totals.Objects += agg.ObjectCountMax
		}
	}
	writeJSON(w, http.StatusOK, out)
}
