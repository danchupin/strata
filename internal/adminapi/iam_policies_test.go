package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

// seedManagedPolicy persists a fresh ManagedPolicy with the strata-minted ARN.
func seedManagedPolicy(t *testing.T, s *Server, name, path, doc string) string {
	t.Helper()
	if path == "" {
		path = "/"
	}
	arn := strataPolicyArnPrefix + path + name
	if err := s.Meta.CreateManagedPolicy(context.Background(), &meta.ManagedPolicy{
		Arn:       arn,
		Name:      name,
		Path:      path,
		Document:  []byte(doc),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed managed policy %q: %v", name, err)
	}
	return arn
}

// validPolicyDoc is the smallest IAM-policy that passes ValidateBucketPolicyBlob.
const validPolicyDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::*"}]}`

// validPolicyDocAlt is a different IAM-policy used to exercise document
// rotation under UpdateManagedPolicyDocument.
const validPolicyDocAlt = `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"s3:*","Resource":"arn:aws:s3:::secret/*"}]}`

func TestIAMPoliciesList_Empty(t *testing.T) {
	s := newTestServer()
	rr := iamRequest(t, s, http.MethodGet, "/admin/v1/iam/policies", "ops", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ManagedPoliciesListResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Policies == nil {
		t.Fatalf("Policies must be a non-nil slice for the React empty-state branch")
	}
	if len(got.Policies) != 0 {
		t.Errorf("want 0 policies; got %d", len(got.Policies))
	}
}

func TestIAMPoliciesList_PopulatedWithAttachmentCount(t *testing.T) {
	s := newTestServer()
	arn1 := seedManagedPolicy(t, s, "AdminAccess", "/", validPolicyDoc)
	_ = seedManagedPolicy(t, s, "ReadOnly", "/", validPolicyDoc)
	seedIAMUser(t, s, "alice")
	seedIAMUser(t, s, "bob")
	if err := s.Meta.AttachUserPolicy(context.Background(), "alice", arn1); err != nil {
		t.Fatalf("attach alice: %v", err)
	}
	if err := s.Meta.AttachUserPolicy(context.Background(), "bob", arn1); err != nil {
		t.Fatalf("attach bob: %v", err)
	}

	rr := iamRequest(t, s, http.MethodGet, "/admin/v1/iam/policies", "ops", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ManagedPoliciesListResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if len(got.Policies) != 2 {
		t.Fatalf("want 2 policies, got %d", len(got.Policies))
	}
	byName := map[string]ManagedPolicySummary{}
	for _, p := range got.Policies {
		byName[p.Name] = p
	}
	if byName["AdminAccess"].AttachmentCount != 2 {
		t.Errorf("AdminAccess attachment_count=%d want 2", byName["AdminAccess"].AttachmentCount)
	}
	if byName["ReadOnly"].AttachmentCount != 0 {
		t.Errorf("ReadOnly attachment_count=%d want 0", byName["ReadOnly"].AttachmentCount)
	}
	if byName["AdminAccess"].Document != validPolicyDoc {
		t.Errorf("AdminAccess document round-trip drifted: %s", byName["AdminAccess"].Document)
	}
}

func TestIAMPolicyCreate_HappyAndCanonicaliseDoc(t *testing.T) {
	s := newTestServer()
	body := CreateManagedPolicyRequest{
		Name:        "AdminAccess",
		Path:        "/",
		Description: "operator",
		Document:    validPolicyDoc,
	}
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/policies", "ops", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ManagedPolicySummary
	_ = json.NewDecoder(rr.Body).Decode(&got)
	wantArn := "arn:aws:iam::strata:policy/AdminAccess"
	if got.Arn != wantArn {
		t.Errorf("arn=%q want %q", got.Arn, wantArn)
	}
	// Canonical re-emit: indented (newlines + 2-space indent), no trailing newline.
	if !strings.Contains(got.Document, "\n  ") {
		t.Errorf("document not canonicalised (no indent): %q", got.Document)
	}
	persisted, _ := s.Meta.GetManagedPolicy(context.Background(), wantArn)
	if persisted.Description != "operator" {
		t.Errorf("description=%q want %q", persisted.Description, "operator")
	}
}

func TestIAMPolicyCreate_DefaultPath(t *testing.T) {
	s := newTestServer()
	body := CreateManagedPolicyRequest{Name: "AnyName", Document: validPolicyDoc}
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/policies", "ops", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ManagedPolicySummary
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Path != "/" {
		t.Errorf("path=%q want /", got.Path)
	}
}

func TestIAMPolicyCreate_InvalidName(t *testing.T) {
	s := newTestServer()
	for _, bad := range []string{"", "has space", strings.Repeat("a", 129), "/leading-slash"} {
		body := CreateManagedPolicyRequest{Name: bad, Document: validPolicyDoc}
		rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/policies", "ops", body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("name=%q status=%d want 400", bad, rr.Code)
		}
	}
}

func TestIAMPolicyCreate_InvalidPath(t *testing.T) {
	s := newTestServer()
	body := CreateManagedPolicyRequest{Name: "X", Path: "team/", Document: validPolicyDoc}
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/policies", "ops", body)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for path missing leading slash", rr.Code)
	}
}

func TestIAMPolicyCreate_MalformedDoc(t *testing.T) {
	s := newTestServer()
	body := CreateManagedPolicyRequest{Name: "X", Document: "not-json"}
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/policies", "ops", body)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIAMPolicyCreate_EmptyDocRejected(t *testing.T) {
	s := newTestServer()
	body := CreateManagedPolicyRequest{Name: "X", Document: "  "}
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/policies", "ops", body)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d body=%s want 400 for empty doc", rr.Code, rr.Body.String())
	}
}

func TestIAMPolicyCreate_Conflict(t *testing.T) {
	s := newTestServer()
	_ = seedManagedPolicy(t, s, "AdminAccess", "/", validPolicyDoc)
	body := CreateManagedPolicyRequest{Name: "AdminAccess", Document: validPolicyDoc}
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/policies", "ops", body)
	if rr.Code != http.StatusConflict {
		t.Errorf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "EntityAlreadyExists" {
		t.Errorf("code=%q want EntityAlreadyExists", er.Code)
	}
}

func TestIAMPolicyCreate_MalformedJSON(t *testing.T) {
	s := newTestServer()
	rr := iamRequestRaw(t, s, http.MethodPost, "/admin/v1/iam/policies", "ops", []byte("{not valid"))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIAMPolicyUpdate_HappyAndBumpsUpdatedAt(t *testing.T) {
	s := newTestServer()
	arn := seedManagedPolicy(t, s, "AdminAccess", "/", validPolicyDoc)
	before, _ := s.Meta.GetManagedPolicy(context.Background(), arn)

	body := UpdateManagedPolicyDocumentRequest{Document: validPolicyDocAlt}
	rr := iamRequest(t, s, http.MethodPut, "/admin/v1/iam/policies/"+arn, "ops", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ManagedPolicySummary
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if !strings.Contains(got.Document, "Deny") {
		t.Errorf("document not updated: %q", got.Document)
	}
	if got.UpdatedAt < before.UpdatedAt.Unix() {
		t.Errorf("updated_at not bumped: %d < %d", got.UpdatedAt, before.UpdatedAt.Unix())
	}
}

func TestIAMPolicyUpdate_NotFound(t *testing.T) {
	s := newTestServer()
	body := UpdateManagedPolicyDocumentRequest{Document: validPolicyDoc}
	rr := iamRequest(t, s, http.MethodPut,
		"/admin/v1/iam/policies/arn:aws:iam::strata:policy/Ghost",
		"ops", body)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIAMPolicyUpdate_MalformedDoc(t *testing.T) {
	s := newTestServer()
	arn := seedManagedPolicy(t, s, "AdminAccess", "/", validPolicyDoc)
	body := UpdateManagedPolicyDocumentRequest{Document: "garbage"}
	rr := iamRequest(t, s, http.MethodPut, "/admin/v1/iam/policies/"+arn, "ops", body)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIAMPolicyDelete_Happy(t *testing.T) {
	s := newTestServer()
	arn := seedManagedPolicy(t, s, "AdminAccess", "/", validPolicyDoc)
	rr := iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/policies/"+arn, "ops", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := s.Meta.GetManagedPolicy(context.Background(), arn); err != meta.ErrManagedPolicyNotFound {
		t.Errorf("post-delete get: got %v want ErrManagedPolicyNotFound", err)
	}
}

func TestIAMPolicyDelete_NotFound(t *testing.T) {
	s := newTestServer()
	rr := iamRequest(t, s, http.MethodDelete,
		"/admin/v1/iam/policies/arn:aws:iam::strata:policy/Ghost",
		"ops", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIAMPolicyDelete_AttachedReturns409WithUserList(t *testing.T) {
	s := newTestServer()
	arn := seedManagedPolicy(t, s, "AdminAccess", "/", validPolicyDoc)
	seedIAMUser(t, s, "alice")
	seedIAMUser(t, s, "bob")
	if err := s.Meta.AttachUserPolicy(context.Background(), "alice", arn); err != nil {
		t.Fatalf("attach alice: %v", err)
	}
	if err := s.Meta.AttachUserPolicy(context.Background(), "bob", arn); err != nil {
		t.Fatalf("attach bob: %v", err)
	}

	rr := iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/policies/"+arn, "ops", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got PolicyAttachmentsResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Code != "PolicyAttached" {
		t.Errorf("code=%q want PolicyAttached", got.Code)
	}
	want := map[string]bool{"alice": true, "bob": true}
	for _, u := range got.AttachedTo {
		if !want[u] {
			t.Errorf("unexpected attached user %q", u)
		}
		delete(want, u)
	}
	if len(want) != 0 {
		t.Errorf("missing attached users: %v", want)
	}
	// Policy must remain present after the conflict — deletion failed.
	if _, err := s.Meta.GetManagedPolicy(context.Background(), arn); err != nil {
		t.Errorf("policy gone after 409: %v", err)
	}
}

func TestIAMPolicyArnRoundTripWithSlashes(t *testing.T) {
	// The trailing-wildcard route must accept ARNs that contain slashes
	// (e.g. /team/ path). Without a tail wildcard Go's mux would split the
	// path on '/' and 404.
	s := newTestServer()
	arn := seedManagedPolicy(t, s, "ReadOnly", "/team/", validPolicyDoc)
	rr := iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/policies/"+arn, "ops", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}
