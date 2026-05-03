package adminapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// handleBucketGetPolicy serves GET /admin/v1/buckets/{bucket}/policy (US-006).
// Returns 200 + the raw policy JSON, 404 NoSuchBucketPolicy when no policy is
// stored, 404 NoSuchBucket when the bucket is missing.
func (s *Server) handleBucketGetPolicy(w http.ResponseWriter, r *http.Request) {
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
	blob, err := s.Meta.GetBucketPolicy(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchBucketPolicy) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucketPolicy",
				"no policy configured for bucket")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

// handleBucketSetPolicy serves PUT /admin/v1/buckets/{bucket}/policy (US-006).
// Validates the document via s3api.ValidateBucketPolicyBlob (the same parser
// the gateway runs at request-time) before persisting via SetBucketPolicy.
// Audit row: admin:SetBucketPolicy.
func (s *Server) handleBucketSetPolicy(w http.ResponseWriter, r *http.Request) {
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
	blob, err := io.ReadAll(io.LimitReader(r.Body, 20<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	if vErr := s3api.ValidateBucketPolicyBlob(blob); vErr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedPolicy", vErr.Error())
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:SetBucketPolicy", "bucket:"+name, name, owner)

	b, err := s.Meta.GetBucket(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	canonical := canonicalisePolicyJSON(blob)
	if err := s.Meta.SetBucketPolicy(ctx, b.ID, canonical); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(canonical)
}

// handleBucketDeletePolicy serves DELETE /admin/v1/buckets/{bucket}/policy
// (US-006). Idempotent — returns 204 even when no policy exists. Audit row:
// admin:DeleteBucketPolicy.
func (s *Server) handleBucketDeletePolicy(w http.ResponseWriter, r *http.Request) {
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
	s3api.SetAuditOverride(ctx, "admin:DeleteBucketPolicy", "bucket:"+name, name, owner)
	b, err := s.Meta.GetBucket(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err := s.Meta.DeleteBucketPolicy(ctx, b.ID); err != nil {
		if errors.Is(err, meta.ErrNoSuchBucketPolicy) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PolicyDryRunResponse is the wire shape of POST /admin/v1/buckets/{bucket}/policy/dry-run.
// Valid=true → the document parses and every Effect is Allow|Deny. Valid=false
// carries the parse error message but the response status is still 400 so
// non-200 surfaces as an error in fetch wrappers.
type PolicyDryRunResponse struct {
	Valid   bool   `json:"valid"`
	Message string `json:"message,omitempty"`
}

// handleBucketDryRunPolicy serves POST /admin/v1/buckets/{bucket}/policy/dry-run
// (US-006). Runs the same validation as PUT but does NOT persist. Returns 200
// {valid:true} on accept, 400 {valid:false, message} on parse error.
func (s *Server) handleBucketDryRunPolicy(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	blob, err := io.ReadAll(io.LimitReader(r.Body, 20<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	if vErr := s3api.ValidateBucketPolicyBlob(blob); vErr != nil {
		writeJSON(w, http.StatusBadRequest, PolicyDryRunResponse{
			Valid:   false,
			Message: vErr.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, PolicyDryRunResponse{Valid: true})
}

// canonicalisePolicyJSON re-emits the document with a stable 2-space indent so
// readbacks are deterministic and the Monaco editor doesn't surprise the
// operator with whitespace-only diffs after Save → reload. Falls back to the
// original blob when re-marshal fails (validation already passed, so this is
// belt-and-braces).
func canonicalisePolicyJSON(blob []byte) []byte {
	var v any
	if err := json.Unmarshal(blob, &v); err != nil {
		return blob
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return blob
	}
	out := buf.Bytes()
	// strip trailing newline emitted by Encode
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out
}
