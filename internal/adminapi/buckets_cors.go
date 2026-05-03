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

// handleBucketGetCORS serves GET /admin/v1/buckets/{bucket}/cors (US-005).
// Returns 200 + CORSConfigJSON, 404 NoSuchCORSConfiguration when no rules
// stored, 404 NoSuchBucket when the bucket itself is missing.
//
// The stored blob is XML (s3api.parseCORSConfig consumes it back); the admin
// endpoint translates to AWS-shape JSON for the operator console.
func (s *Server) handleBucketGetCORS(w http.ResponseWriter, r *http.Request) {
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
	blob, err := s.Meta.GetBucketCORS(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchCORS) {
			writeJSONError(w, http.StatusNotFound, "NoSuchCORSConfiguration",
				"no cors configuration on bucket")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	cfg, derr := decodeCORSXML(blob)
	if derr != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal",
			"stored cors blob is not valid XML: "+derr.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handleBucketSetCORS serves PUT /admin/v1/buckets/{bucket}/cors (US-005).
// Body: CORSConfigJSON. Validates each rule (≥1 method, ≥1 origin, recognized
// methods), marshals to XML, runs the s3api parser to confirm round-trip, then
// persists via SetBucketCORS. Audit row: admin:SetBucketCORS.
func (s *Server) handleBucketSetCORS(w http.ResponseWriter, r *http.Request) {
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
	var req CORSConfigJSON
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "malformed JSON body")
		return
	}
	if len(req.Rules) == 0 {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"at least one rule is required")
		return
	}
	for i := range req.Rules {
		if vErr := validateCORSRule(&req.Rules[i]); vErr != nil {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument", vErr.Error())
			return
		}
	}

	xmlBlob, err := encodeCORSXML(&req)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	// Confirm the s3api consumer accepts the rendered XML before we persist.
	if !s3api.ValidCORSBlob(xmlBlob) {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"cors configuration failed validation")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:SetBucketCORS", "bucket:"+name, name, owner)

	b, err := s.Meta.GetBucket(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err := s.Meta.SetBucketCORS(ctx, b.ID, xmlBlob); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// handleBucketDeleteCORS serves DELETE /admin/v1/buckets/{bucket}/cors
// (US-005). Audit row: admin:DeleteBucketCORS.
func (s *Server) handleBucketDeleteCORS(w http.ResponseWriter, r *http.Request) {
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
	s3api.SetAuditOverride(ctx, "admin:DeleteBucketCORS", "bucket:"+name, name, owner)
	b, err := s.Meta.GetBucket(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err := s.Meta.DeleteBucketCORS(ctx, b.ID); err != nil {
		if errors.Is(err, meta.ErrNoSuchCORS) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validCORSMethods is the set s3api.parseCORSConfig accepts implicitly (the
// upstream parser only checks ≥1 method/origin); we tighten to AWS spec to
// stop typos like POSTS landing on disk.
var validCORSMethods = map[string]struct{}{
	"GET": {}, "PUT": {}, "POST": {}, "DELETE": {}, "HEAD": {},
}

func validateCORSRule(r *CORSRuleJSON) error {
	if len(r.AllowedMethods) == 0 {
		return fmt.Errorf("rule %q: at least one allowed_methods entry is required", r.ID)
	}
	if len(r.AllowedOrigins) == 0 {
		return fmt.Errorf("rule %q: at least one allowed_origins entry is required", r.ID)
	}
	for i, m := range r.AllowedMethods {
		up := strings.ToUpper(strings.TrimSpace(m))
		if _, ok := validCORSMethods[up]; !ok {
			return fmt.Errorf("rule %q: allowed_methods[%d]=%q must be GET|PUT|POST|DELETE|HEAD",
				r.ID, i, m)
		}
		r.AllowedMethods[i] = up
	}
	if r.MaxAgeSeconds < 0 {
		return fmt.Errorf("rule %q: max_age_seconds must be >= 0", r.ID)
	}
	return nil
}

// CORSConfigJSON is the operator-console wire shape for bucket CORS. Mirrors
// the AWS JSON form of CORSConfiguration.
type CORSConfigJSON struct {
	Rules []CORSRuleJSON `json:"rules"`
}

type CORSRuleJSON struct {
	ID             string   `json:"id,omitempty"`
	AllowedMethods []string `json:"allowed_methods"`
	AllowedOrigins []string `json:"allowed_origins"`
	AllowedHeaders []string `json:"allowed_headers,omitempty"`
	ExposeHeaders  []string `json:"expose_headers,omitempty"`
	MaxAgeSeconds  int      `json:"max_age_seconds,omitempty"`
}

// corsConfigXML is the AWS CORSConfiguration XML wire shape — adminapi-local
// duplicate so the s3api package keeps its parser unexported.
type corsConfigXML struct {
	XMLName xml.Name       `xml:"CORSConfiguration"`
	Rules   []corsRuleXML  `xml:"CORSRule"`
}

type corsRuleXML struct {
	XMLName        xml.Name `xml:"CORSRule"`
	ID             string   `xml:"ID,omitempty"`
	AllowedMethods []string `xml:"AllowedMethod"`
	AllowedOrigins []string `xml:"AllowedOrigin"`
	AllowedHeaders []string `xml:"AllowedHeader,omitempty"`
	ExposeHeaders  []string `xml:"ExposeHeader,omitempty"`
	MaxAgeSeconds  int      `xml:"MaxAgeSeconds,omitempty"`
}

func encodeCORSXML(cfg *CORSConfigJSON) ([]byte, error) {
	out := corsConfigXML{}
	for _, r := range cfg.Rules {
		out.Rules = append(out.Rules, corsRuleXML{
			ID:             r.ID,
			AllowedMethods: append([]string(nil), r.AllowedMethods...),
			AllowedOrigins: append([]string(nil), r.AllowedOrigins...),
			AllowedHeaders: append([]string(nil), r.AllowedHeaders...),
			ExposeHeaders:  append([]string(nil), r.ExposeHeaders...),
			MaxAgeSeconds:  r.MaxAgeSeconds,
		})
	}
	return xml.Marshal(out)
}

func decodeCORSXML(blob []byte) (*CORSConfigJSON, error) {
	var x corsConfigXML
	if err := xml.Unmarshal(blob, &x); err != nil {
		return nil, err
	}
	out := &CORSConfigJSON{}
	for _, r := range x.Rules {
		out.Rules = append(out.Rules, CORSRuleJSON{
			ID:             r.ID,
			AllowedMethods: append([]string(nil), r.AllowedMethods...),
			AllowedOrigins: append([]string(nil), r.AllowedOrigins...),
			AllowedHeaders: append([]string(nil), r.AllowedHeaders...),
			ExposeHeaders:  append([]string(nil), r.ExposeHeaders...),
			MaxAgeSeconds:  r.MaxAgeSeconds,
		})
	}
	return out, nil
}
