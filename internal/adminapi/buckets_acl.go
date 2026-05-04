package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// Canned ACL values accepted by the admin endpoint. Mirrors S3 spec; the
// gateway s3api package recognises only the first four — log-delivery-write is
// preserved verbatim in the ACL column for buckets that opt into S3 access
// logging.
const (
	cannedACLPrivate           = "private"
	cannedACLPublicRead        = "public-read"
	cannedACLPublicReadWrite   = "public-read-write"
	cannedACLAuthenticatedRead = "authenticated-read"
	cannedACLLogDeliveryWrite  = "log-delivery-write"
)

var allowedCannedACLs = map[string]struct{}{
	cannedACLPrivate:           {},
	cannedACLPublicRead:        {},
	cannedACLPublicReadWrite:   {},
	cannedACLAuthenticatedRead: {},
	cannedACLLogDeliveryWrite:  {},
}

var allowedGranteeTypes = map[string]struct{}{
	"CanonicalUser":         {},
	"Group":                 {},
	"AmazonCustomerByEmail": {},
}

var allowedGrantPermissions = map[string]struct{}{
	"FULL_CONTROL": {},
	"READ":         {},
	"WRITE":        {},
	"READ_ACP":     {},
	"WRITE_ACP":    {},
}

// ACLGrantJSON is the wire shape for one Grant.
type ACLGrantJSON struct {
	GranteeType string `json:"grantee_type"`
	ID          string `json:"id,omitempty"`
	URI         string `json:"uri,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Permission  string `json:"permission"`
}

// ACLConfigJSON is the GET/PUT body shape for /admin/v1/buckets/{bucket}/acl.
type ACLConfigJSON struct {
	Canned string         `json:"canned"`
	Grants []ACLGrantJSON `json:"grants"`
}

// handleBucketGetACL serves GET /admin/v1/buckets/{bucket}/acl (US-007).
// Returns 200 + {canned, grants}. canned is the bucket-level header value
// ("private" by default); grants is the explicit grant list (may be empty).
func (s *Server) handleBucketGetACL(w http.ResponseWriter, r *http.Request) {
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
	canned := b.ACL
	if canned == "" {
		canned = cannedACLPrivate
	}
	grants, gerr := s.Meta.GetBucketGrants(r.Context(), b.ID)
	if gerr != nil && !errors.Is(gerr, meta.ErrNoSuchGrants) {
		writeJSONError(w, http.StatusInternalServerError, "Internal", gerr.Error())
		return
	}
	out := ACLConfigJSON{Canned: canned, Grants: make([]ACLGrantJSON, 0, len(grants))}
	for _, g := range grants {
		out.Grants = append(out.Grants, ACLGrantJSON{
			GranteeType: g.GranteeType,
			ID:          g.ID,
			URI:         g.URI,
			DisplayName: g.DisplayName,
			Email:       g.Email,
			Permission:  g.Permission,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleBucketSetACL serves PUT /admin/v1/buckets/{bucket}/acl (US-007).
// Persists canned + grants in two store calls. Audit row admin:SetBucketACL.
func (s *Server) handleBucketSetACL(w http.ResponseWriter, r *http.Request) {
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req ACLConfigJSON
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	canned := strings.ToLower(strings.TrimSpace(req.Canned))
	if canned == "" {
		canned = cannedACLPrivate
	}
	if _, ok := allowedCannedACLs[canned]; !ok {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"canned must be one of private | public-read | public-read-write | authenticated-read | log-delivery-write")
		return
	}
	grants, gerr := normaliseACLGrants(req.Grants)
	if gerr != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", gerr.Error())
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:SetBucketACL", "bucket:"+name, name, owner)

	b, err := s.Meta.GetBucket(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err := s.Meta.SetBucketACL(ctx, b.Name, canned); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err := s.Meta.SetBucketGrants(ctx, b.ID, grants); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// normaliseACLGrants validates each grant and returns the meta.Grant slice
// ready for SetBucketGrants. Empty input is valid (returns empty slice — the
// caller wipes the explicit grant list, falling back to canned semantics).
func normaliseACLGrants(in []ACLGrantJSON) ([]meta.Grant, error) {
	out := make([]meta.Grant, 0, len(in))
	for i, g := range in {
		gt := strings.TrimSpace(g.GranteeType)
		if _, ok := allowedGranteeTypes[gt]; !ok {
			return nil, errInvalidGrant(i, "grantee_type must be CanonicalUser | Group | AmazonCustomerByEmail")
		}
		perm := strings.TrimSpace(g.Permission)
		if _, ok := allowedGrantPermissions[perm]; !ok {
			return nil, errInvalidGrant(i, "permission must be FULL_CONTROL | READ | WRITE | READ_ACP | WRITE_ACP")
		}
		switch gt {
		case "CanonicalUser":
			if strings.TrimSpace(g.ID) == "" {
				return nil, errInvalidGrant(i, "CanonicalUser grant requires id")
			}
		case "Group":
			if strings.TrimSpace(g.URI) == "" {
				return nil, errInvalidGrant(i, "Group grant requires uri")
			}
		case "AmazonCustomerByEmail":
			if strings.TrimSpace(g.Email) == "" {
				return nil, errInvalidGrant(i, "AmazonCustomerByEmail grant requires email")
			}
		}
		out = append(out, meta.Grant{
			GranteeType: gt,
			ID:          strings.TrimSpace(g.ID),
			URI:         strings.TrimSpace(g.URI),
			DisplayName: strings.TrimSpace(g.DisplayName),
			Email:       strings.TrimSpace(g.Email),
			Permission:  perm,
		})
	}
	return out, nil
}

func errInvalidGrant(idx int, msg string) error {
	return errors.New("grants[" + strconv.Itoa(idx) + "]: " + msg)
}
