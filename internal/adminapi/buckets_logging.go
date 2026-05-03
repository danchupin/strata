package adminapi

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// LoggingConfigJSON is the operator-console wire shape for bucket access-log
// configuration (US-009). Mirrors AWS BucketLoggingStatus.LoggingEnabled. The
// admin endpoint translates JSON↔XML so the s3api consumer / access-log
// worker keep reading the AWS XML shape unchanged.
//
// LoggingEnabled=false (no body) means "disable logging" and the handler
// routes to DELETE-equivalent semantics.
type LoggingConfigJSON struct {
	TargetBucket string             `json:"target_bucket"`
	TargetPrefix string             `json:"target_prefix"`
	TargetGrants []LoggingGrantJSON `json:"target_grants,omitempty"`
}

// LoggingGrantJSON mirrors the AWS Grant shape inside TargetGrants. Permission
// is restricted to FULL_CONTROL | READ | WRITE per AWS spec.
type LoggingGrantJSON struct {
	GranteeType string `json:"grantee_type"`
	ID          string `json:"id,omitempty"`
	URI         string `json:"uri,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Permission  string `json:"permission"`
}

// validLoggingPermissions is the AWS-spec subset for TargetGrants — narrower
// than the bucket-ACL permission set (no READ_ACP / WRITE_ACP).
var validLoggingPermissions = map[string]struct{}{
	"FULL_CONTROL": {},
	"READ":         {},
	"WRITE":        {},
}

// loggingStatusXML is the AWS BucketLoggingStatus XML wire shape. Duplicated
// here so the s3api package keeps its parser unexported.
type loggingStatusXML struct {
	XMLName        xml.Name             `xml:"BucketLoggingStatus"`
	XMLNS          string               `xml:"xmlns,attr,omitempty"`
	LoggingEnabled *loggingEnabledXML   `xml:"LoggingEnabled,omitempty"`
}

type loggingEnabledXML struct {
	TargetBucket string             `xml:"TargetBucket"`
	TargetPrefix string             `xml:"TargetPrefix"`
	TargetGrants *loggingGrantsXML  `xml:"TargetGrants,omitempty"`
}

type loggingGrantsXML struct {
	Grant []loggingGrantXML `xml:"Grant"`
}

type loggingGrantXML struct {
	Grantee    loggingGranteeXML `xml:"Grantee"`
	Permission string            `xml:"Permission"`
}

type loggingGranteeXML struct {
	XSIType     string `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr,omitempty"`
	ID          string `xml:"ID,omitempty"`
	URI         string `xml:"URI,omitempty"`
	DisplayName string `xml:"DisplayName,omitempty"`
	Email       string `xml:"EmailAddress,omitempty"`
}

func encodeLoggingXML(cfg *LoggingConfigJSON) ([]byte, error) {
	out := loggingStatusXML{
		XMLNS: "http://s3.amazonaws.com/doc/2006-03-01/",
		LoggingEnabled: &loggingEnabledXML{
			TargetBucket: cfg.TargetBucket,
			TargetPrefix: cfg.TargetPrefix,
		},
	}
	if len(cfg.TargetGrants) > 0 {
		grants := make([]loggingGrantXML, 0, len(cfg.TargetGrants))
		for _, g := range cfg.TargetGrants {
			grants = append(grants, loggingGrantXML{
				Grantee: loggingGranteeXML{
					XSIType:     g.GranteeType,
					ID:          g.ID,
					URI:         g.URI,
					DisplayName: g.DisplayName,
					Email:       g.Email,
				},
				Permission: g.Permission,
			})
		}
		out.LoggingEnabled.TargetGrants = &loggingGrantsXML{Grant: grants}
	}
	return xml.Marshal(out)
}

func decodeLoggingXML(blob []byte) (*LoggingConfigJSON, error) {
	var x loggingStatusXML
	if err := xml.Unmarshal(blob, &x); err != nil {
		return nil, err
	}
	if x.LoggingEnabled == nil {
		return nil, nil
	}
	out := &LoggingConfigJSON{
		TargetBucket: x.LoggingEnabled.TargetBucket,
		TargetPrefix: x.LoggingEnabled.TargetPrefix,
	}
	if x.LoggingEnabled.TargetGrants != nil {
		for _, g := range x.LoggingEnabled.TargetGrants.Grant {
			out.TargetGrants = append(out.TargetGrants, LoggingGrantJSON{
				GranteeType: g.Grantee.XSIType,
				ID:          g.Grantee.ID,
				URI:         g.Grantee.URI,
				DisplayName: g.Grantee.DisplayName,
				Email:       g.Grantee.Email,
				Permission:  g.Permission,
			})
		}
	}
	return out, nil
}

func validateLoggingConfig(cfg *LoggingConfigJSON) error {
	if strings.TrimSpace(cfg.TargetBucket) == "" {
		return errors.New("target_bucket is required")
	}
	for i, g := range cfg.TargetGrants {
		gt := strings.TrimSpace(g.GranteeType)
		if _, ok := allowedGranteeTypes[gt]; !ok {
			return fmt.Errorf("target_grants[%d]: grantee_type must be CanonicalUser | Group | AmazonCustomerByEmail", i)
		}
		perm := strings.TrimSpace(g.Permission)
		if _, ok := validLoggingPermissions[perm]; !ok {
			return fmt.Errorf("target_grants[%d]: permission must be FULL_CONTROL | READ | WRITE", i)
		}
		switch gt {
		case "CanonicalUser":
			if strings.TrimSpace(g.ID) == "" {
				return fmt.Errorf("target_grants[%d]: CanonicalUser grant requires id", i)
			}
		case "Group":
			if strings.TrimSpace(g.URI) == "" {
				return fmt.Errorf("target_grants[%d]: Group grant requires uri", i)
			}
		case "AmazonCustomerByEmail":
			if strings.TrimSpace(g.Email) == "" {
				return fmt.Errorf("target_grants[%d]: AmazonCustomerByEmail grant requires email", i)
			}
		}
	}
	return nil
}

// handleBucketGetLogging serves GET /admin/v1/buckets/{bucket}/logging.
// Returns 200 + LoggingConfigJSON when configured, 404
// NoSuchBucketLoggingConfiguration when no logging set, 404 NoSuchBucket
// when the bucket itself is missing.
func (s *Server) handleBucketGetLogging(w http.ResponseWriter, r *http.Request) {
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
	blob, err := s.Meta.GetBucketLogging(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchLogging) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucketLoggingConfiguration",
				"no logging configuration on bucket")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	cfg, derr := decodeLoggingXML(blob)
	if derr != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal",
			"stored logging blob is not valid XML: "+derr.Error())
		return
	}
	if cfg == nil {
		// Stored blob has no LoggingEnabled element — equivalent to absent.
		writeJSONError(w, http.StatusNotFound, "NoSuchBucketLoggingConfiguration",
			"no logging configuration on bucket")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handleBucketSetLogging serves PUT /admin/v1/buckets/{bucket}/logging.
// Body: LoggingConfigJSON. Audit row admin:SetBucketLogging.
func (s *Server) handleBucketSetLogging(w http.ResponseWriter, r *http.Request) {
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req LoggingConfigJSON
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	if vErr := validateLoggingConfig(&req); vErr != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", vErr.Error())
		return
	}
	xmlBlob, eerr := encodeLoggingXML(&req)
	if eerr != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", eerr.Error())
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:SetBucketLogging", "bucket:"+name, name, owner)

	b, err := s.Meta.GetBucket(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err := s.Meta.SetBucketLogging(ctx, b.ID, xmlBlob); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// handleBucketDeleteLogging serves DELETE /admin/v1/buckets/{bucket}/logging.
// Idempotent: missing config returns 204. Audit row admin:DeleteBucketLogging.
func (s *Server) handleBucketDeleteLogging(w http.ResponseWriter, r *http.Request) {
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
	s3api.SetAuditOverride(ctx, "admin:DeleteBucketLogging", "bucket:"+name, name, owner)
	b, err := s.Meta.GetBucket(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err := s.Meta.DeleteBucketLogging(ctx, b.ID); err != nil {
		if errors.Is(err, meta.ErrNoSuchLogging) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
