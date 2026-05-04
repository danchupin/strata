package adminapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// iamPolicyNamePattern enforces the AWS managed-policy name charset: 1..128
// chars from [A-Za-z0-9+=,.@_-]. Mirrors the IAM CreatePolicy contract.
var iamPolicyNamePattern = regexp.MustCompile(`^[A-Za-z0-9_+=,.@-]{1,128}$`)

// strataPolicyArnPrefix is the ARN namespace minted for managed policies
// owned by this gateway. Mirrors the user ARN convention in
// internal/s3api/iam.go (`arn:aws:iam::strata:user<path><name>`).
const strataPolicyArnPrefix = "arn:aws:iam::strata:policy"

// ManagedPolicySummary is the wire shape for one entry in
// GET /admin/v1/iam/policies. AttachmentCount is computed via
// meta.Store.ListPolicyUsers per row.
type ManagedPolicySummary struct {
	Arn             string `json:"arn"`
	Name            string `json:"name"`
	Path            string `json:"path"`
	Description     string `json:"description,omitempty"`
	Document        string `json:"document"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
	AttachmentCount int    `json:"attachment_count"`
}

// ManagedPoliciesListResponse wraps the list rows.
type ManagedPoliciesListResponse struct {
	Policies []ManagedPolicySummary `json:"policies"`
}

// CreateManagedPolicyRequest is the body for POST /admin/v1/iam/policies.
// Path defaults to "/" (AWS IAM convention). Document is the raw IAM-policy
// JSON; the admin layer canonicalises it before persisting.
type CreateManagedPolicyRequest struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Description string `json:"description"`
	Document    string `json:"document"`
}

// UpdateManagedPolicyDocumentRequest is the body for
// PUT /admin/v1/iam/policies/{arn...}.
type UpdateManagedPolicyDocumentRequest struct {
	Document string `json:"document"`
}

// PolicyAttachmentsResponse is the body wrapped inside a 409 PolicyAttached
// error so the UI can render the list of users blocking the delete.
type PolicyAttachmentsResponse struct {
	Code        string   `json:"code"`
	Message     string   `json:"message"`
	AttachedTo  []string `json:"attached_to"`
}

// handleIAMPoliciesList serves GET /admin/v1/iam/policies.
//
// AttachmentCount is computed via meta.Store.ListPolicyUsers per row. Operator-
// scope cardinality is small; if managed-policy count grows, switch to an
// indexed COUNT(*) per partition.
func (s *Server) handleIAMPoliciesList(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	policies, err := s.Meta.ListManagedPolicies(r.Context(), "")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	out := ManagedPoliciesListResponse{Policies: make([]ManagedPolicySummary, 0, len(policies))}
	for _, p := range policies {
		users, _ := s.Meta.ListPolicyUsers(r.Context(), p.Arn)
		out.Policies = append(out.Policies, ManagedPolicySummary{
			Arn:             p.Arn,
			Name:            p.Name,
			Path:            p.Path,
			Description:     p.Description,
			Document:        string(p.Document),
			CreatedAt:       p.CreatedAt.Unix(),
			UpdatedAt:       p.UpdatedAt.Unix(),
			AttachmentCount: len(users),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleIAMPolicyCreate serves POST /admin/v1/iam/policies. Mints the ARN
// from name + path under the strata namespace and validates the document via
// the gateway's IAM-policy parser (s3api.ValidateBucketPolicyBlob — same
// parser is used at request-time for bucket policies; managed policies share
// the IAM document schema).
//
// Audit row admin:CreateManagedPolicy, resource iam-policy:<arn>.
func (s *Server) handleIAMPolicyCreate(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req CreateManagedPolicyRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if !iamPolicyNamePattern.MatchString(name) {
		writeJSONError(w, http.StatusBadRequest, "InvalidPolicyName",
			"name must match ^[A-Za-z0-9_+=,.@-]{1,128}$")
		return
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") || !strings.HasSuffix(path, "/") {
		writeJSONError(w, http.StatusBadRequest, "InvalidPath",
			"path must begin and end with '/'")
		return
	}
	docRaw := []byte(req.Document)
	if vErr := s3api.ValidateBucketPolicyBlob(docRaw); vErr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedPolicy", vErr.Error())
		return
	}
	canonical := canonicalisePolicyJSON(docRaw)
	arn := strataPolicyArnPrefix + path + name

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:CreateManagedPolicy", "iam-policy:"+arn, "", owner)

	now := time.Now().UTC()
	p := &meta.ManagedPolicy{
		Arn:         arn,
		Name:        name,
		Path:        path,
		Description: strings.TrimSpace(req.Description),
		Document:    canonical,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if cerr := s.Meta.CreateManagedPolicy(ctx, p); cerr != nil {
		if errors.Is(cerr, meta.ErrManagedPolicyAlreadyExists) {
			writeJSONError(w, http.StatusConflict, "EntityAlreadyExists",
				fmt.Sprintf("managed policy %q already exists", arn))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", cerr.Error())
		return
	}
	writeJSON(w, http.StatusCreated, ManagedPolicySummary{
		Arn:             p.Arn,
		Name:            p.Name,
		Path:            p.Path,
		Description:     p.Description,
		Document:        string(p.Document),
		CreatedAt:       p.CreatedAt.Unix(),
		UpdatedAt:       p.UpdatedAt.Unix(),
		AttachmentCount: 0,
	})
}

// handleIAMPolicyUpdate serves PUT /admin/v1/iam/policies/{arn...}. Replaces
// the Document blob and bumps UpdatedAt. The arn segment is captured via a
// trailing wildcard so the slash inside the strata-minted ARN
// (`arn:aws:iam::strata:policy/<path><name>`) does not break Go mux pattern
// matching.
//
// Audit row admin:UpdateManagedPolicy, resource iam-policy:<arn>.
func (s *Server) handleIAMPolicyUpdate(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	arn := strings.TrimSpace(r.PathValue("arn"))
	if arn == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "arn is required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req UpdateManagedPolicyDocumentRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	docRaw := []byte(req.Document)
	if vErr := s3api.ValidateBucketPolicyBlob(docRaw); vErr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedPolicy", vErr.Error())
		return
	}
	canonical := canonicalisePolicyJSON(docRaw)
	updatedAt := time.Now().UTC()

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:UpdateManagedPolicy", "iam-policy:"+arn, "", owner)

	if uerr := s.Meta.UpdateManagedPolicyDocument(ctx, arn, canonical, updatedAt); uerr != nil {
		if errors.Is(uerr, meta.ErrManagedPolicyNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "managed policy not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", uerr.Error())
		return
	}
	got, gerr := s.Meta.GetManagedPolicy(ctx, arn)
	if gerr != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", gerr.Error())
		return
	}
	users, _ := s.Meta.ListPolicyUsers(ctx, arn)
	writeJSON(w, http.StatusOK, ManagedPolicySummary{
		Arn:             got.Arn,
		Name:            got.Name,
		Path:            got.Path,
		Description:     got.Description,
		Document:        string(got.Document),
		CreatedAt:       got.CreatedAt.Unix(),
		UpdatedAt:       got.UpdatedAt.Unix(),
		AttachmentCount: len(users),
	})
}

// handleIAMPolicyDelete serves DELETE /admin/v1/iam/policies/{arn...}.
// Returns 409 PolicyAttached with the attached-user list when the policy has
// any attachments — the operator must detach (US-014) before deleting.
//
// Audit row admin:DeleteManagedPolicy, resource iam-policy:<arn>.
func (s *Server) handleIAMPolicyDelete(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	arn := strings.TrimSpace(r.PathValue("arn"))
	if arn == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "arn is required")
		return
	}
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:DeleteManagedPolicy", "iam-policy:"+arn, "", owner)

	if derr := s.Meta.DeleteManagedPolicy(ctx, arn); derr != nil {
		if errors.Is(derr, meta.ErrManagedPolicyNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "managed policy not found")
			return
		}
		if errors.Is(derr, meta.ErrPolicyAttached) {
			users, _ := s.Meta.ListPolicyUsers(ctx, arn)
			writeJSON(w, http.StatusConflict, PolicyAttachmentsResponse{
				Code:       "PolicyAttached",
				Message:    "managed policy is attached to one or more users; detach before delete",
				AttachedTo: users,
			})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", derr.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
