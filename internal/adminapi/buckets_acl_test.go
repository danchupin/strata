package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danchupin/strata/internal/meta"
)

func seedACLBucket(t *testing.T, s *Server, name, owner string) {
	t.Helper()
	if _, err := s.Meta.CreateBucket(context.Background(), name, owner, "STANDARD"); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
}

func TestBucketACL_GetDefault(t *testing.T) {
	s := newTestServer()
	seedACLBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/acl", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ACLConfigJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Canned != cannedACLPrivate {
		t.Errorf("canned=%q want private", got.Canned)
	}
	if len(got.Grants) != 0 {
		t.Errorf("grants=%v want empty", got.Grants)
	}
}

func TestBucketACL_GetBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/missing/acl", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q want NoSuchBucket", er.Code)
	}
}

func TestBucketACL_PutCannedAndGrantsRoundTrip(t *testing.T) {
	s := newTestServer()
	seedACLBucket(t, s, "bkt", "alice")
	body := ACLConfigJSON{
		Canned: "public-read",
		Grants: []ACLGrantJSON{
			{GranteeType: "CanonicalUser", ID: "user-bob", DisplayName: "Bob", Permission: "READ"},
			{GranteeType: "Group", URI: "http://acs.amazonaws.com/groups/global/AllUsers", Permission: "WRITE"},
			{GranteeType: "AmazonCustomerByEmail", Email: "carol@example.com", Permission: "FULL_CONTROL"},
		},
	}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/acl", body)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/acl", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ACLConfigJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Canned != "public-read" {
		t.Errorf("canned=%q want public-read", got.Canned)
	}
	if len(got.Grants) != 3 {
		t.Fatalf("grants len=%d want 3", len(got.Grants))
	}
	if got.Grants[0].ID != "user-bob" || got.Grants[0].Permission != "READ" {
		t.Errorf("grant[0]=%+v", got.Grants[0])
	}
	if got.Grants[1].URI != "http://acs.amazonaws.com/groups/global/AllUsers" || got.Grants[1].Permission != "WRITE" {
		t.Errorf("grant[1]=%+v", got.Grants[1])
	}
	if got.Grants[2].Email != "carol@example.com" || got.Grants[2].Permission != "FULL_CONTROL" {
		t.Errorf("grant[2]=%+v", got.Grants[2])
	}
}

func TestBucketACL_PutLogDeliveryWriteCanned(t *testing.T) {
	s := newTestServer()
	seedACLBucket(t, s, "bkt", "alice")
	body := ACLConfigJSON{Canned: "log-delivery-write"}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/acl", body)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	b, err := s.Meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	if b.ACL != "log-delivery-write" {
		t.Errorf("ACL=%q want log-delivery-write", b.ACL)
	}
}

func TestBucketACL_PutBadCanned(t *testing.T) {
	s := newTestServer()
	seedACLBucket(t, s, "bkt", "alice")
	body := ACLConfigJSON{Canned: "world-domination"}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/acl", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "InvalidArgument" {
		t.Errorf("code=%q want InvalidArgument", er.Code)
	}
}

func TestBucketACL_PutBadGranteeType(t *testing.T) {
	s := newTestServer()
	seedACLBucket(t, s, "bkt", "alice")
	body := ACLConfigJSON{Grants: []ACLGrantJSON{
		{GranteeType: "Alien", ID: "x", Permission: "READ"},
	}}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/acl", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketACL_PutCanonicalRequiresID(t *testing.T) {
	s := newTestServer()
	seedACLBucket(t, s, "bkt", "alice")
	body := ACLConfigJSON{Grants: []ACLGrantJSON{
		{GranteeType: "CanonicalUser", Permission: "READ"},
	}}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/acl", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketACL_PutGroupRequiresURI(t *testing.T) {
	s := newTestServer()
	seedACLBucket(t, s, "bkt", "alice")
	body := ACLConfigJSON{Grants: []ACLGrantJSON{
		{GranteeType: "Group", Permission: "READ"},
	}}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/acl", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketACL_PutEmailRequiresEmail(t *testing.T) {
	s := newTestServer()
	seedACLBucket(t, s, "bkt", "alice")
	body := ACLConfigJSON{Grants: []ACLGrantJSON{
		{GranteeType: "AmazonCustomerByEmail", Permission: "READ"},
	}}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/acl", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketACL_PutBadPermission(t *testing.T) {
	s := newTestServer()
	seedACLBucket(t, s, "bkt", "alice")
	body := ACLConfigJSON{Grants: []ACLGrantJSON{
		{GranteeType: "CanonicalUser", ID: "x", Permission: "DELETE"},
	}}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/acl", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketACL_PutBucketNotFound(t *testing.T) {
	s := newTestServer()
	body := ACLConfigJSON{Canned: "private"}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/missing/acl", body)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q want NoSuchBucket", er.Code)
	}
}

func TestBucketACL_PutEmptyGrantsClears(t *testing.T) {
	s := newTestServer()
	seedACLBucket(t, s, "bkt", "alice")
	// First populate.
	body := ACLConfigJSON{Canned: "private", Grants: []ACLGrantJSON{
		{GranteeType: "CanonicalUser", ID: "user-bob", Permission: "READ"},
	}}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/acl", body)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("seed put status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Then clear.
	body = ACLConfigJSON{Canned: "private", Grants: []ACLGrantJSON{}}
	rr = putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/acl", body)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("clear put status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/acl", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d", rr.Code)
	}
	var got ACLConfigJSON
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Grants) != 0 {
		t.Errorf("grants=%v want empty", got.Grants)
	}
}

func TestBucketACL_PutCannedNormalizedLower(t *testing.T) {
	s := newTestServer()
	seedACLBucket(t, s, "bkt", "alice")
	body := ACLConfigJSON{Canned: "  Public-Read  "}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/acl", body)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	b, err := s.Meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	if b.ACL != "public-read" {
		t.Errorf("ACL=%q want public-read", b.ACL)
	}
}

func TestBucketACL_PutMalformedJSON(t *testing.T) {
	s := newTestServer()
	seedACLBucket(t, s, "bkt", "alice")
	rr := rawAdmin(t, s, http.MethodPut, "/admin/v1/buckets/bkt/acl", "{not json")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketACL_GrantsPersistedInMeta(t *testing.T) {
	s := newTestServer()
	seedACLBucket(t, s, "bkt", "alice")
	b, _ := s.Meta.GetBucket(context.Background(), "bkt")
	body := ACLConfigJSON{Canned: "private", Grants: []ACLGrantJSON{
		{GranteeType: "Group", URI: "http://acs.amazonaws.com/groups/s3/LogDelivery", Permission: "WRITE"},
	}}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/acl", body)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	got, err := s.Meta.GetBucketGrants(context.Background(), b.ID)
	if err != nil {
		t.Fatalf("get grants: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("grants len=%d want 1", len(got))
	}
	want := meta.Grant{
		GranteeType: "Group",
		URI:         "http://acs.amazonaws.com/groups/s3/LogDelivery",
		Permission:  "WRITE",
	}
	if got[0] != want {
		t.Errorf("grant=%+v want %+v", got[0], want)
	}
}
