package adminapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// objectKeyBodyLimit caps the JSON body size for the per-object PUT
// endpoints (tags / retention / legal-hold). Tags maps can be arbitrarily
// large but the AWS-S3 spec caps them at 10 entries × 256-char keys ×
// 256-char values, so 64 KiB is more than enough headroom.
const objectKeyBodyLimit = 64 << 10

// SetObjectTagsRequest is the JSON body accepted by PUT
// /admin/v1/buckets/{bucket}/object-tags. Key is mandatory, version_id is
// optional (latest when empty), tags map is the full replacement (not a
// patch — to clear tags pass an empty object).
type SetObjectTagsRequest struct {
	Key       string            `json:"key"`
	VersionID string            `json:"version_id,omitempty"`
	Tags      map[string]string `json:"tags"`
}

// SetObjectRetentionRequest is the JSON body accepted by PUT
// /admin/v1/buckets/{bucket}/object-retention. Mode must be GOVERNANCE,
// COMPLIANCE, or empty/None (which clears the retention). RetainUntil is
// an RFC 3339 timestamp; required when Mode is set, ignored when None.
type SetObjectRetentionRequest struct {
	Key         string `json:"key"`
	VersionID   string `json:"version_id,omitempty"`
	Mode        string `json:"mode"`
	RetainUntil string `json:"retain_until,omitempty"`
}

// SetObjectLegalHoldRequest is the JSON body accepted by PUT
// /admin/v1/buckets/{bucket}/object-legal-hold.
type SetObjectLegalHoldRequest struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id,omitempty"`
	Enabled   bool   `json:"enabled"`
}

// ObjectDetailResponse is the GET shape for /admin/v1/buckets/{bucket}/
// object?key=... — the side-panel Overview tab payload. Mirrors the
// gateway-side meta.Object shape, projecting only the fields the operator
// console renders.
type ObjectDetailResponse struct {
	Key            string            `json:"key"`
	VersionID      string            `json:"version_id,omitempty"`
	IsLatest       bool              `json:"is_latest"`
	IsDeleteMarker bool              `json:"is_delete_marker"`
	Size           int64             `json:"size"`
	ETag           string            `json:"etag"`
	ContentType    string            `json:"content_type,omitempty"`
	StorageClass   string            `json:"storage_class,omitempty"`
	LastModified   int64             `json:"last_modified"`
	Tags           map[string]string `json:"tags"`
	RetainMode     string            `json:"retain_mode,omitempty"`
	RetainUntil    int64             `json:"retain_until,omitempty"`
	LegalHold      bool              `json:"legal_hold"`
}

// ObjectVersionsResponse is the GET shape for /admin/v1/buckets/{bucket}/
// object-versions?key=... — the side-panel Versions tab payload. One entry
// per stored row (latest first via ListObjectVersions ordering).
type ObjectVersionsResponse struct {
	Versions []ObjectVersionEntry `json:"versions"`
}

type ObjectVersionEntry struct {
	VersionID      string `json:"version_id"`
	IsLatest       bool   `json:"is_latest"`
	IsDeleteMarker bool   `json:"is_delete_marker"`
	Size           int64  `json:"size"`
	ETag           string `json:"etag"`
	StorageClass   string `json:"storage_class,omitempty"`
	LastModified   int64  `json:"last_modified"`
}

// handleObjectDelete serves DELETE /admin/v1/buckets/{bucket}/objects/
// {key...}?versionId=<id>. The trailing wildcard means subroute siblings
// (tags/retention/legal-hold) cannot live under /objects/{key}/... — they
// take their key via JSON body. Forwards through s.S3Handler so all the
// existing object-lock / replication / notification / GC enqueue logic
// stays the single source of truth.
//
// Audit row: admin:DeleteObject, resource=object:<bucket>/<key>.
func (s *Server) handleObjectDelete(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	key := r.PathValue("key")
	if bucket == "" || key == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket and key are required")
		return
	}
	if s.S3Handler == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "s3 handler not wired")
		return
	}
	versionID := r.URL.Query().Get("versionId")

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:DeleteObject", "object:"+bucket+"/"+key, bucket, owner)

	innerURL := "/" + bucket + "/" + key
	if versionID != "" {
		innerURL += "?versionId=" + url.QueryEscape(versionID)
	}
	inner := httptest.NewRequest(http.MethodDelete, innerURL, nil)
	inner = inner.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.S3Handler.ServeHTTP(rec, inner)

	if rec.Code >= 200 && rec.Code < 300 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSONError(w, rec.Code, "DeleteFailed", strings.TrimSpace(rec.Body.String()))
}

// handleObjectGet serves GET /admin/v1/buckets/{bucket}/object?key=&
// versionId=. Returns 200 + ObjectDetailResponse (the side-panel Overview
// tab payload), 404 NoSuchBucket / NoSuchKey when missing.
func (s *Server) handleObjectGet(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	key := r.URL.Query().Get("key")
	versionID := r.URL.Query().Get("versionId")
	if bucket == "" || key == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket and key are required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	o, err := s.Meta.GetObject(r.Context(), b.ID, key, versionID)
	if err != nil {
		if errors.Is(err, meta.ErrObjectNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchKey", "object not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, objectToDetail(o))
}

// handleObjectVersions serves GET /admin/v1/buckets/{bucket}/object-versions
// ?key=. Returns 200 + ObjectVersionsResponse (latest version first; delete
// markers included). 404 when the bucket is missing; an empty versions list
// is a valid response when no rows are stored under the key.
func (s *Server) handleObjectVersions(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	key := r.URL.Query().Get("key")
	if bucket == "" || key == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket and key are required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	res, err := s.Meta.ListObjectVersions(r.Context(), b.ID, meta.ListOptions{
		Prefix: key,
		Limit:  1000,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	out := ObjectVersionsResponse{Versions: []ObjectVersionEntry{}}
	for _, v := range res.Versions {
		// Prefix-listing returns every key starting with `key` — filter to
		// the exact key. ListObjectVersions does not expose an "exact" mode.
		if v.Key != key {
			continue
		}
		out.Versions = append(out.Versions, ObjectVersionEntry{
			VersionID:      v.VersionID,
			IsLatest:       v.IsLatest,
			IsDeleteMarker: v.IsDeleteMarker,
			Size:           v.Size,
			ETag:           v.ETag,
			StorageClass:   v.StorageClass,
			LastModified:   v.Mtime.Unix(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleObjectTags serves PUT /admin/v1/buckets/{bucket}/object-tags. Body:
// SetObjectTagsRequest. Empty tags map clears all tags. Audit row:
// admin:SetObjectTags, resource=object:<bucket>/<key>.
func (s *Server) handleObjectTags(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	bucket := r.PathValue("bucket")
	if bucket == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket is required")
		return
	}
	body, berr := io.ReadAll(io.LimitReader(r.Body, objectKeyBodyLimit))
	if berr != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req SetObjectTagsRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "key is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:SetObjectTags", "object:"+bucket+"/"+key, bucket, owner)

	tags := req.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	if err := s.Meta.SetObjectTags(ctx, b.ID, key, req.VersionID, tags); err != nil {
		if errors.Is(err, meta.ErrObjectNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchKey", "object not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleObjectRetention serves PUT /admin/v1/buckets/{bucket}/object-retention.
// Mode "" or "None" clears the retention; otherwise GOVERNANCE / COMPLIANCE
// require RetainUntil (RFC 3339). Refuses to set retention when the bucket
// has Object-Lock disabled (400 ObjectLockNotEnabled). Audit row:
// admin:SetObjectRetention.
func (s *Server) handleObjectRetention(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	bucket := r.PathValue("bucket")
	if bucket == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket is required")
		return
	}
	body, berr := io.ReadAll(io.LimitReader(r.Body, objectKeyBodyLimit))
	if berr != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req SetObjectRetentionRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "key is required")
		return
	}
	mode := strings.ToUpper(strings.TrimSpace(req.Mode))
	var retainMode string
	var retainUntil time.Time
	switch mode {
	case "", "NONE":
		retainMode = ""
		retainUntil = time.Time{}
	case meta.LockModeGovernance, meta.LockModeCompliance:
		retainMode = mode
		if strings.TrimSpace(req.RetainUntil) == "" {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
				"retain_until is required when mode is "+mode)
			return
		}
		t, perr := time.Parse(time.RFC3339, req.RetainUntil)
		if perr != nil {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
				"retain_until must be RFC 3339: "+perr.Error())
			return
		}
		retainUntil = t.UTC()
	default:
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"mode must be GOVERNANCE, COMPLIANCE, or None")
		return
	}

	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if retainMode != "" && !b.ObjectLockEnabled {
		writeJSONError(w, http.StatusBadRequest, "ObjectLockNotEnabled",
			"bucket does not have object-lock enabled")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:SetObjectRetention", "object:"+bucket+"/"+key, bucket, owner)

	if err := s.Meta.SetObjectRetention(ctx, b.ID, key, req.VersionID, retainMode, retainUntil); err != nil {
		if errors.Is(err, meta.ErrObjectNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchKey", "object not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleObjectLegalHold serves PUT /admin/v1/buckets/{bucket}/object-legal-hold.
// Refuses to set when the bucket has Object-Lock disabled (400
// ObjectLockNotEnabled). Audit row: admin:SetObjectLegalHold.
func (s *Server) handleObjectLegalHold(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	bucket := r.PathValue("bucket")
	if bucket == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket is required")
		return
	}
	body, berr := io.ReadAll(io.LimitReader(r.Body, objectKeyBodyLimit))
	if berr != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req SetObjectLegalHoldRequest
	if jerr := json.Unmarshal(bytes.TrimSpace(body), &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "key is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if req.Enabled && !b.ObjectLockEnabled {
		writeJSONError(w, http.StatusBadRequest, "ObjectLockNotEnabled",
			"bucket does not have object-lock enabled")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:SetObjectLegalHold", "object:"+bucket+"/"+key, bucket, owner)

	if err := s.Meta.SetObjectLegalHold(ctx, b.ID, key, req.VersionID, req.Enabled); err != nil {
		if errors.Is(err, meta.ErrObjectNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchKey", "object not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// objectToDetail projects a meta.Object onto the side-panel Overview wire
// shape. Tags map is always non-nil (so the JSON output is `{}` not `null`).
func objectToDetail(o *meta.Object) ObjectDetailResponse {
	tags := o.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	out := ObjectDetailResponse{
		Key:            o.Key,
		VersionID:      o.VersionID,
		IsLatest:       o.IsLatest,
		IsDeleteMarker: o.IsDeleteMarker,
		Size:           o.Size,
		ETag:           o.ETag,
		ContentType:    o.ContentType,
		StorageClass:   o.StorageClass,
		LastModified:   o.Mtime.Unix(),
		Tags:           tags,
		RetainMode:     o.RetainMode,
		LegalHold:      o.LegalHold,
	}
	if !o.RetainUntil.IsZero() {
		out.RetainUntil = o.RetainUntil.Unix()
	}
	return out
}
