package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestIAMUserPoliciesList_Empty(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	rr := iamRequest(t, s, http.MethodGet, "/admin/v1/iam/users/alice/policies", "ops", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got UserPoliciesListResponse
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

func TestIAMUserPoliciesList_PopulatedWithMetadata(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	arn1 := seedManagedPolicy(t, s, "AdminAccess", "/", validPolicyDoc)
	arn2 := seedManagedPolicy(t, s, "ReadOnly", "/team/", validPolicyDoc)
	if err := s.Meta.AttachUserPolicy(context.Background(), "alice", arn1); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := s.Meta.AttachUserPolicy(context.Background(), "alice", arn2); err != nil {
		t.Fatalf("attach: %v", err)
	}

	rr := iamRequest(t, s, http.MethodGet, "/admin/v1/iam/users/alice/policies", "ops", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got UserPoliciesListResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if len(got.Policies) != 2 {
		t.Fatalf("want 2 attachments; got %d (%+v)", len(got.Policies), got.Policies)
	}
	byArn := map[string]UserPolicyAttachment{}
	for _, p := range got.Policies {
		byArn[p.Arn] = p
	}
	if byArn[arn1].Name != "AdminAccess" || byArn[arn1].Path != "/" {
		t.Errorf("arn1 metadata wrong: %+v", byArn[arn1])
	}
	if byArn[arn2].Name != "ReadOnly" || byArn[arn2].Path != "/team/" {
		t.Errorf("arn2 metadata wrong: %+v", byArn[arn2])
	}
}

func TestIAMUserPoliciesList_UserMissing(t *testing.T) {
	s := newTestServer()
	rr := iamRequest(t, s, http.MethodGet, "/admin/v1/iam/users/ghost/policies", "ops", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "NoSuchEntity" {
		t.Errorf("code=%q want NoSuchEntity", er.Code)
	}
}

func TestIAMUserPolicyAttach_Happy(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	arn := seedManagedPolicy(t, s, "AdminAccess", "/", validPolicyDoc)
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/users/alice/policies", "ops",
		AttachUserPolicyRequest{PolicyArn: arn})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	got, err := s.Meta.ListUserPolicies(context.Background(), "alice")
	if err != nil || len(got) != 1 || got[0] != arn {
		t.Fatalf("post-attach list: err=%v %+v", err, got)
	}
}

func TestIAMUserPolicyAttach_UserMissing(t *testing.T) {
	s := newTestServer()
	arn := seedManagedPolicy(t, s, "AdminAccess", "/", validPolicyDoc)
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/users/ghost/policies", "ops",
		AttachUserPolicyRequest{PolicyArn: arn})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "NoSuchEntity" {
		t.Errorf("code=%q want NoSuchEntity", er.Code)
	}
}

func TestIAMUserPolicyAttach_PolicyMissing(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/users/alice/policies", "ops",
		AttachUserPolicyRequest{PolicyArn: "arn:aws:iam::strata:policy/Ghost"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "NoSuchEntity" {
		t.Errorf("code=%q want NoSuchEntity", er.Code)
	}
}

func TestIAMUserPolicyAttach_AlreadyAttached(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	arn := seedManagedPolicy(t, s, "AdminAccess", "/", validPolicyDoc)
	if err := s.Meta.AttachUserPolicy(context.Background(), "alice", arn); err != nil {
		t.Fatalf("attach: %v", err)
	}
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/users/alice/policies", "ops",
		AttachUserPolicyRequest{PolicyArn: arn})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "EntityAlreadyExists" {
		t.Errorf("code=%q want EntityAlreadyExists", er.Code)
	}
}

func TestIAMUserPolicyAttach_MissingArn(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/users/alice/policies", "ops",
		AttachUserPolicyRequest{PolicyArn: ""})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIAMUserPolicyAttach_MalformedJSON(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	rr := iamRequestRaw(t, s, http.MethodPost, "/admin/v1/iam/users/alice/policies",
		"ops", []byte("{not json"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIAMUserPolicyDetach_Happy(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	arn := seedManagedPolicy(t, s, "AdminAccess", "/", validPolicyDoc)
	if err := s.Meta.AttachUserPolicy(context.Background(), "alice", arn); err != nil {
		t.Fatalf("attach: %v", err)
	}
	rr := iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/users/alice/policies/"+arn, "ops", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	got, _ := s.Meta.ListUserPolicies(context.Background(), "alice")
	if len(got) != 0 {
		t.Errorf("post-detach list non-empty: %+v", got)
	}
}

func TestIAMUserPolicyDetach_NotAttached(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	arn := seedManagedPolicy(t, s, "AdminAccess", "/", validPolicyDoc)
	rr := iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/users/alice/policies/"+arn, "ops", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "NoSuchEntity" {
		t.Errorf("code=%q want NoSuchEntity", er.Code)
	}
}

func TestIAMUserPolicyDetach_ArnRoundTripWithSlashes(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	arn := seedManagedPolicy(t, s, "ReadOnly", "/team/", validPolicyDoc)
	if err := s.Meta.AttachUserPolicy(context.Background(), "alice", arn); err != nil {
		t.Fatalf("attach: %v", err)
	}
	rr := iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/users/alice/policies/"+arn, "ops", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIAMUserPolicyAttachThenDeleteManagedPolicyBlocked(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	arn := seedManagedPolicy(t, s, "AdminAccess", "/", validPolicyDoc)
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/users/alice/policies", "ops",
		AttachUserPolicyRequest{PolicyArn: arn})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("attach status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/policies/"+arn, "ops", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete-policy status=%d body=%s want 409", rr.Code, rr.Body.String())
	}
	var got PolicyAttachmentsResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Code != "PolicyAttached" || len(got.AttachedTo) != 1 || got.AttachedTo[0] != "alice" {
		t.Errorf("want PolicyAttached + [alice]; got %+v", got)
	}
	rr = iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/users/alice/policies/"+arn, "ops", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("detach status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/policies/"+arn, "ops", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete-policy after detach: status=%d body=%s", rr.Code, rr.Body.String())
	}
}
