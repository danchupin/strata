package adminapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// handleBucketCreate serves POST /admin/v1/buckets — the Create-bucket
// dialog from the operator console (US-001). Body: CreateBucketRequest.
// Returns 201 + BucketDetail on success, 400 InvalidBucketName on a name
// that fails s3api.ValidBucketName, 400 InvalidArgument when versioning is
// "Disabled" or object_lock_enabled=true with non-Enabled versioning, 409
// BucketAlreadyExists on name collision.
//
// Audit row: action=admin:CreateBucket, resource=bucket:<name>. Stamped via
// s3api.SetAuditOverride so the AuditMiddleware wrapping /admin/v1/* on the
// gateway emits the row with the operator-meaningful label rather than the
// path-derived "PostBucket".
func (s *Server) handleBucketCreate(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req CreateBucketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "malformed JSON body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if !s3api.ValidBucketName(name) {
		writeJSONError(w, http.StatusBadRequest, "InvalidBucketName",
			"bucket name violates S3 naming rules or collides with a reserved gateway path")
		return
	}

	versioning, ok := normalizeCreateVersioning(req.Versioning)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"versioning must be Enabled or Suspended")
		return
	}
	if req.ObjectLockEnabled && versioning != meta.VersioningEnabled {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"object_lock_enabled requires versioning=Enabled")
		return
	}

	region := strings.TrimSpace(req.Region)
	if region == "" {
		region = s.Region
	}

	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:CreateBucket", "bucket:"+name, name, owner)

	b, err := s.Meta.CreateBucket(ctx, name, owner, "STANDARD")
	if err != nil {
		if errors.Is(err, meta.ErrBucketAlreadyExists) {
			writeJSONError(w, http.StatusConflict, "BucketAlreadyExists",
				fmt.Sprintf("bucket %q already exists", name))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	if region != "" && region != s.Region {
		if err := s.Meta.SetBucketRegion(ctx, name, region); err != nil {
			s.Logger.Printf("adminapi: SetBucketRegion %q: %v", name, err)
		}
	}
	if versioning == meta.VersioningEnabled {
		if err := s.Meta.SetBucketVersioning(ctx, name, meta.VersioningEnabled); err != nil {
			s.Logger.Printf("adminapi: SetBucketVersioning %q: %v", name, err)
		}
		b.Versioning = meta.VersioningEnabled
	}
	if req.ObjectLockEnabled {
		if err := s.Meta.SetBucketObjectLockEnabled(ctx, name, true); err != nil {
			s.Logger.Printf("adminapi: SetBucketObjectLockEnabled %q: %v", name, err)
		}
		b.ObjectLockEnabled = true
	}

	resp := BucketDetail{
		Name:           b.Name,
		Owner:          b.Owner,
		Region:         region,
		CreatedAt:      b.CreatedAt.Unix(),
		Versioning:     versioningLabel(b.Versioning),
		ObjectLock:     b.ObjectLockEnabled,
		SizeBytes:      0,
		ObjectCount:    0,
		BackendPresign: b.BackendPresign,
		ShardCount:     b.ShardCount,
	}
	writeJSON(w, http.StatusCreated, resp)
}

// normalizeCreateVersioning maps the request body's loose versioning string
// to a meta-store enum. Empty, "suspended" → Suspended; "enabled" → Enabled.
// Anything else (including "Disabled", "Off") is rejected — a freshly
// created bucket already starts Disabled, and the dialog never asks for it.
func normalizeCreateVersioning(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "suspended":
		return meta.VersioningSuspended, true
	case "enabled":
		return meta.VersioningEnabled, true
	default:
		return "", false
	}
}
