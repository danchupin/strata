package adminapi

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// handleBucketSetVersioning serves PUT /admin/v1/buckets/{bucket}/versioning
// (US-003). Body: SetVersioningRequest. Accepts "Enabled" or "Suspended"
// (case-insensitive); "Disabled" is rejected with 400. 404 NoSuchBucket if
// the bucket is missing. Audit row: admin:SetBucketVersioning.
func (s *Server) handleBucketSetVersioning(w http.ResponseWriter, r *http.Request) {
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
	var req SetVersioningRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "malformed JSON body")
		return
	}
	state, ok := normalizeVersioningState(req.State)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"state must be Enabled or Suspended")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:SetBucketVersioning", "bucket:"+name, name, owner)

	if err := s.Meta.SetBucketVersioning(ctx, name, state); err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": state})
}

// normalizeVersioningState maps the request body's loose state string to a
// meta-store enum. "enabled" → Enabled; "suspended" → Suspended. Anything
// else (including "Disabled" and the empty string) returns ok=false — the
// console explicitly does not let operators revert to Disabled.
func normalizeVersioningState(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "enabled":
		return meta.VersioningEnabled, true
	case "suspended":
		return meta.VersioningSuspended, true
	default:
		return "", false
	}
}

// handleBucketGetObjectLock serves GET /admin/v1/buckets/{bucket}/object-lock.
// Returns 200 + ObjectLockConfigJSON. 404 NoSuchObjectLockConfiguration when
// no default rule is configured (the bucket may still have ObjectLockEnabled
// at the bucket level — the response then carries object_lock_enabled with no
// Rule, matching AWS shape).
func (s *Server) handleBucketGetObjectLock(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
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

	resp := ObjectLockConfigJSON{}
	if b.ObjectLockEnabled {
		resp.ObjectLockEnabled = "Enabled"
	}

	blob, err := s.Meta.GetBucketObjectLockConfig(r.Context(), b.ID)
	if err != nil && !errors.Is(err, meta.ErrNoSuchObjectLockConfig) {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err == nil {
		var xmlCfg objectLockXML
		if uerr := xml.Unmarshal(blob, &xmlCfg); uerr == nil {
			if xmlCfg.ObjectLockEnabled != "" {
				resp.ObjectLockEnabled = xmlCfg.ObjectLockEnabled
			}
			if xmlCfg.Rule != nil && xmlCfg.Rule.DefaultRetention != nil {
				dr := xmlCfg.Rule.DefaultRetention
				resp.Rule = &ObjectLockRuleJSON{
					DefaultRetention: &ObjectLockDefaultRetentionJSON{
						Mode:  dr.Mode,
						Days:  dr.Days,
						Years: dr.Years,
					},
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleBucketSetObjectLock serves PUT /admin/v1/buckets/{bucket}/object-lock
// (US-003). Body: ObjectLockConfigJSON. Validates Mode + Days/Years, marshals
// to the AWS XML shape stored by meta.Store.SetBucketObjectLockConfig (so the
// existing s3api consumers — putObject default-retention resolution — read it
// back via xml.Unmarshal without code changes). Returns 200 on success, 400
// InvalidArgument on bad shape, 404 NoSuchBucket if missing, 409 Conflict if
// the bucket has Object-Lock disabled at the bucket level. Audit row:
// admin:SetBucketObjectLockConfig.
func (s *Server) handleBucketSetObjectLock(w http.ResponseWriter, r *http.Request) {
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
	var req ObjectLockConfigJSON
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "malformed JSON body")
		return
	}
	if req.Rule != nil && req.Rule.DefaultRetention != nil {
		dr := req.Rule.DefaultRetention
		if dr.Mode != "" && dr.Mode != meta.LockModeGovernance && dr.Mode != meta.LockModeCompliance {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
				"mode must be GOVERNANCE or COMPLIANCE")
			return
		}
		if dr.Days != nil && *dr.Days <= 0 {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "days must be > 0")
			return
		}
		if dr.Years != nil && *dr.Years <= 0 {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "years must be > 0")
			return
		}
		if dr.Days != nil && dr.Years != nil {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
				"days and years are mutually exclusive")
			return
		}
		if dr.Mode != "" && dr.Days == nil && dr.Years == nil {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
				"mode requires days or years")
			return
		}
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:SetBucketObjectLockConfig", "bucket:"+name, name, owner)

	b, err := s.Meta.GetBucket(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if !b.ObjectLockEnabled {
		writeJSONError(w, http.StatusConflict, "ObjectLockNotEnabled",
			"bucket has Object-Lock disabled; enable it at bucket creation")
		return
	}

	xmlCfg := objectLockXML{
		ObjectLockEnabled: req.ObjectLockEnabled,
	}
	if xmlCfg.ObjectLockEnabled == "" {
		xmlCfg.ObjectLockEnabled = "Enabled"
	}
	if req.Rule != nil && req.Rule.DefaultRetention != nil {
		dr := req.Rule.DefaultRetention
		xmlCfg.Rule = &objectLockRuleXML{
			DefaultRetention: &objectLockRetentionXML{
				Mode:  dr.Mode,
				Days:  dr.Days,
				Years: dr.Years,
			},
		}
	}
	blob, err := xml.Marshal(xmlCfg)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err := s.Meta.SetBucketObjectLockConfig(ctx, b.ID, blob); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// objectLockXML is the on-the-wire AWS PutObjectLockConfiguration XML shape.
// Mirrors the struct in internal/s3api/objectlock.go — duplicated here so the
// admin handler does not depend on s3api's unexported parser and the
// SetBucketObjectLockConfig consumer (resolveDefaultRetention) reads back the
// same XML it expects.
type objectLockXML struct {
	XMLName           xml.Name           `xml:"ObjectLockConfiguration"`
	ObjectLockEnabled string             `xml:"ObjectLockEnabled,omitempty"`
	Rule              *objectLockRuleXML `xml:"Rule,omitempty"`
}

type objectLockRuleXML struct {
	DefaultRetention *objectLockRetentionXML `xml:"DefaultRetention,omitempty"`
}

type objectLockRetentionXML struct {
	Mode  string `xml:"Mode,omitempty"`
	Days  *int   `xml:"Days,omitempty"`
	Years *int   `xml:"Years,omitempty"`
}
