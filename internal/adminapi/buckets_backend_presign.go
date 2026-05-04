package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// handleBucketSetBackendPresign serves PUT /admin/v1/buckets/{bucket}/
// backend-presign (US-020). Body: SetBackendPresignRequest. Flips the
// per-bucket s3-over-s3 presign-passthrough flag via
// meta.Store.SetBucketBackendPresign. The toggle is meaningful only when
// the gateway runs on the s3-over-s3 data backend; the UI greys the card
// out otherwise — server-side we still persist the flag so the toggle
// state survives a backend swap. 404 NoSuchBucket if missing. Audit row:
// admin:SetBucketBackendPresign.
func (s *Server) handleBucketSetBackendPresign(w http.ResponseWriter, r *http.Request) {
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
	var req SetBackendPresignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "malformed JSON body")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:SetBucketBackendPresign", "bucket:"+name, name, owner)

	if err := s.Meta.SetBucketBackendPresign(ctx, name, req.Enabled); err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": req.Enabled})
}
