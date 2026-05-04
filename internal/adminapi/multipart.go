package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// MultipartActiveRow is one in-flight multipart upload row surfaced by the
// watchdog page (US-017). Initiator falls back to the bucket owner since the
// gateway does not record the originating principal on the multipart row.
// BytesUploaded is the sum of currently-uploaded part sizes.
type MultipartActiveRow struct {
	Bucket        string `json:"bucket"`
	Key           string `json:"key"`
	UploadID      string `json:"upload_id"`
	InitiatedAt   int64  `json:"initiated_at"`
	AgeSeconds    int64  `json:"age_seconds"`
	StorageClass  string `json:"storage_class"`
	Initiator     string `json:"initiator"`
	BytesUploaded int64  `json:"bytes_uploaded"`
}

// MultipartActiveResponse is the response shape for GET
// /admin/v1/multipart/active. Total is the row count BEFORE pagination so
// the client can render page chips.
type MultipartActiveResponse struct {
	Uploads []MultipartActiveRow `json:"uploads"`
	Total   int                  `json:"total"`
}

// multipartListMaxPerBucket bounds the per-bucket ListMultipartUploads page
// the watchdog walks. Operators rarely accumulate more than a handful of
// stalled uploads per bucket; the page is paginated client-side after the
// fan-out so this keeps a single request bounded.
const multipartListMaxPerBucket = 1000

// handleMultipartActive serves GET /admin/v1/multipart/active. Fans out
// across every bucket and returns a paginated, filtered, age-sorted list of
// in-flight multipart uploads.
//
// Query params: bucket (exact match), min_age_hours (>=N hours; default 0),
// initiator (exact-match access key / owner), page (1-based, default 1),
// page_size (1..500, default 50).
func (s *Server) handleMultipartActive(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSON(w, http.StatusOK, MultipartActiveResponse{Uploads: []MultipartActiveRow{}})
		return
	}
	q := r.URL.Query()
	bucketFilter := strings.TrimSpace(q.Get("bucket"))
	initiatorFilter := strings.TrimSpace(q.Get("initiator"))
	minAgeHours := 0
	if v := strings.TrimSpace(q.Get("min_age_hours")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "BadRequest", "min_age_hours must be a non-negative integer")
			return
		}
		minAgeHours = n
	}
	page := parsePositive(q.Get("page"), 1)
	pageSize := parseRange(q.Get("page_size"), 50, 1, 500)

	buckets, err := s.Meta.ListBuckets(r.Context(), "")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	now := time.Now().UTC()
	minAge := time.Duration(minAgeHours) * time.Hour
	rows := make([]MultipartActiveRow, 0)
	for _, b := range buckets {
		if bucketFilter != "" && b.Name != bucketFilter {
			continue
		}
		ups, lerr := s.Meta.ListMultipartUploads(r.Context(), b.ID, "", multipartListMaxPerBucket)
		if lerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "Internal", lerr.Error())
			return
		}
		for _, mu := range ups {
			if mu == nil {
				continue
			}
			age := now.Sub(mu.InitiatedAt.UTC())
			if age < minAge {
				continue
			}
			initiator := b.Owner
			if initiatorFilter != "" && initiator != initiatorFilter {
				continue
			}
			var bytesUploaded int64
			parts, perr := s.Meta.ListParts(r.Context(), b.ID, mu.UploadID)
			if perr == nil {
				for _, p := range parts {
					if p != nil {
						bytesUploaded += p.Size
					}
				}
			}
			rows = append(rows, MultipartActiveRow{
				Bucket:        b.Name,
				Key:           mu.Key,
				UploadID:      mu.UploadID,
				InitiatedAt:   mu.InitiatedAt.Unix(),
				AgeSeconds:    int64(age.Seconds()),
				StorageClass:  mu.StorageClass,
				Initiator:     initiator,
				BytesUploaded: bytesUploaded,
			})
		}
	}

	// Oldest first — operators want the stalled uploads at the top.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].AgeSeconds != rows[j].AgeSeconds {
			return rows[i].AgeSeconds > rows[j].AgeSeconds
		}
		if rows[i].Bucket != rows[j].Bucket {
			return rows[i].Bucket < rows[j].Bucket
		}
		return rows[i].UploadID < rows[j].UploadID
	})

	total := len(rows)
	resp := MultipartActiveResponse{Uploads: []MultipartActiveRow{}, Total: total}
	start := (page - 1) * pageSize
	if start >= total {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	end := min(start+pageSize, total)
	resp.Uploads = rows[start:end]
	writeJSON(w, http.StatusOK, resp)
}

// MultipartAbortTarget identifies one upload to cancel.
type MultipartAbortTarget struct {
	Bucket   string `json:"bucket"`
	UploadID string `json:"upload_id"`
}

// MultipartAbortRequest is the JSON body for POST /admin/v1/multipart/abort.
type MultipartAbortRequest struct {
	Uploads []MultipartAbortTarget `json:"uploads"`
}

// MultipartAbortResult reports the per-row outcome of a batch abort. Status
// is "aborted" on success, "error" otherwise; Code carries an AWS-style
// short error code on failure ("NoSuchBucket", "NoSuchUpload", "AbortFailed").
type MultipartAbortResult struct {
	Bucket   string `json:"bucket"`
	UploadID string `json:"upload_id"`
	Status   string `json:"status"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}

// MultipartAbortResponse aggregates the per-target outcomes.
type MultipartAbortResponse struct {
	Results []MultipartAbortResult `json:"results"`
}

// handleMultipartAbort serves POST /admin/v1/multipart/abort. Forwards each
// abort through the s3api handler so chunk GC / metrics / the existing
// multipart finalisation logic stay the single source of truth. Emits one
// admin:AbortMultipartUpload audit row per aborted upload via the cascaded
// emitAuditRow helper (the AuditMiddleware override only emits one row per
// HTTP request, but the AC requires one audit row per aborted upload).
func (s *Server) handleMultipartAbort(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	if s.S3Handler == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "s3 handler not wired")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req MultipartAbortRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	if len(req.Uploads) == 0 {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "uploads list is empty")
		return
	}
	if len(req.Uploads) > 500 {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "uploads list exceeds 500 entries")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	// Stamp a top-level audit override so the request-scoped audit row carries
	// the operator-meaningful action; per-upload rows below are emitted via
	// emitAuditRow so each aborted upload still gets its own row.
	s3api.SetAuditOverride(ctx, "admin:AbortMultipartUpload",
		"multipart:batch:"+strconv.Itoa(len(req.Uploads)), "-", owner)

	results := make([]MultipartAbortResult, 0, len(req.Uploads))
	for _, tgt := range req.Uploads {
		bucket := strings.TrimSpace(tgt.Bucket)
		uploadID := strings.TrimSpace(tgt.UploadID)
		res := MultipartAbortResult{Bucket: bucket, UploadID: uploadID}
		if bucket == "" || uploadID == "" {
			res.Status = "error"
			res.Code = "BadRequest"
			res.Message = "bucket and upload_id are required"
			results = append(results, res)
			continue
		}
		b, gerr := s.Meta.GetBucket(ctx, bucket)
		if gerr != nil {
			res.Status = "error"
			if errors.Is(gerr, meta.ErrBucketNotFound) {
				res.Code = "NoSuchBucket"
				res.Message = "bucket not found"
			} else {
				res.Code = "Internal"
				res.Message = gerr.Error()
			}
			results = append(results, res)
			continue
		}
		mu, mErr := s.Meta.GetMultipartUpload(ctx, b.ID, uploadID)
		if mErr != nil {
			res.Status = "error"
			if errors.Is(mErr, meta.ErrMultipartNotFound) {
				res.Code = "NoSuchUpload"
				res.Message = "multipart upload not found"
			} else {
				res.Code = "Internal"
				res.Message = mErr.Error()
			}
			results = append(results, res)
			continue
		}
		innerPath := "/" + bucket + "/" + mu.Key
		innerURL := innerPath + "?uploadId=" + url.QueryEscape(uploadID)
		inner := httptest.NewRequest(http.MethodDelete, innerURL, nil)
		inner = inner.WithContext(ctx)
		rec := httptest.NewRecorder()
		s.S3Handler.ServeHTTP(rec, inner)
		if rec.Code >= 200 && rec.Code < 300 {
			res.Status = "aborted"
			results = append(results, res)
			s.emitAuditRow(ctx, r, &meta.AuditEvent{
				Time:      time.Now().UTC(),
				Principal: owner,
				Action:    "admin:AbortMultipartUpload",
				Resource:  "object:" + bucket + "/" + mu.Key,
				Bucket:    bucket,
				Result:    strconv.Itoa(http.StatusNoContent),
			})
			continue
		}
		res.Status = "error"
		res.Code = "AbortFailed"
		res.Message = strings.TrimSpace(rec.Body.String())
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, MultipartAbortResponse{Results: results})
}
