package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// UserPolicyAttachment is the wire shape for one entry in
// GET /admin/v1/iam/users/{userName}/policies. Name+Path are looked up via
// GetManagedPolicy so the UI can render the operator-friendly name without
// parsing the ARN.
type UserPolicyAttachment struct {
	Arn  string `json:"arn"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// UserPoliciesListResponse wraps the per-user attached-policy list.
type UserPoliciesListResponse struct {
	Policies []UserPolicyAttachment `json:"policies"`
}

// AttachUserPolicyRequest is the body for POST .../policies.
type AttachUserPolicyRequest struct {
	PolicyArn string `json:"policy_arn"`
}

// handleIAMUserPoliciesList serves GET /admin/v1/iam/users/{userName}/policies.
// Returns the policy ARNs attached to the user, enriched with name+path via a
// GetManagedPolicy lookup per row so the UI can render an operator-friendly
// list without re-parsing the ARN string.
func (s *Server) handleIAMUserPoliciesList(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	name := r.PathValue("userName")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "user_name is required")
		return
	}
	arns, err := s.Meta.ListUserPolicies(r.Context(), name)
	if err != nil {
		if errors.Is(err, meta.ErrIAMUserNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "user not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	out := UserPoliciesListResponse{Policies: make([]UserPolicyAttachment, 0, len(arns))}
	for _, arn := range arns {
		row := UserPolicyAttachment{Arn: arn}
		if p, gerr := s.Meta.GetManagedPolicy(r.Context(), arn); gerr == nil && p != nil {
			row.Name = p.Name
			row.Path = p.Path
		}
		out.Policies = append(out.Policies, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleIAMUserPolicyAttach serves POST /admin/v1/iam/users/{userName}/policies.
// Body {policy_arn} → meta.Store.AttachUserPolicy. Returns 404 NoSuchEntity if
// either the user or the policy is missing, 409 EntityAlreadyExists if the
// attachment row already exists.
//
// Audit row admin:AttachUserPolicy, resource iam:<userName>.
func (s *Server) handleIAMUserPolicyAttach(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	name := r.PathValue("userName")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "user_name is required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req AttachUserPolicyRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	arn := strings.TrimSpace(req.PolicyArn)
	if arn == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "policy_arn is required")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:AttachUserPolicy", "iam:"+name, "", owner)

	if aerr := s.Meta.AttachUserPolicy(ctx, name, arn); aerr != nil {
		switch {
		case errors.Is(aerr, meta.ErrIAMUserNotFound):
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "user not found")
		case errors.Is(aerr, meta.ErrManagedPolicyNotFound):
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "managed policy not found")
		case errors.Is(aerr, meta.ErrUserPolicyAlreadyAttached):
			writeJSONError(w, http.StatusConflict, "EntityAlreadyExists",
				"managed policy is already attached to user")
		default:
			writeJSONError(w, http.StatusInternalServerError, "Internal", aerr.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleIAMUserPolicyDetach serves
// DELETE /admin/v1/iam/users/{userName}/policies/{policyArn...}. The arn
// segment is captured via a trailing wildcard so slashes inside the strata-
// minted ARN don't break Go mux pattern matching (mirrors the iam_policies
// PUT/DELETE shape, US-013).
//
// Audit row admin:DetachUserPolicy, resource iam:<userName>.
func (s *Server) handleIAMUserPolicyDetach(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	name := r.PathValue("userName")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "user_name is required")
		return
	}
	arn := strings.TrimSpace(r.PathValue("policyArn"))
	if arn == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "policy_arn is required")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:DetachUserPolicy", "iam:"+name, "", owner)

	if derr := s.Meta.DetachUserPolicy(ctx, name, arn); derr != nil {
		if errors.Is(derr, meta.ErrUserPolicyNotAttached) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity",
				"managed policy is not attached to user")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", derr.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
