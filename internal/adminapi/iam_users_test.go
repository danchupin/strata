package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

// putAdminBody runs an arbitrary admin request through routes() with an
// owner stamped on the context, mirroring the helper used by other
// adminapi tests.
func iamRequest(t *testing.T, s *Server, method, path, owner string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if owner != "" {
		req = req.WithContext(auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: owner, Owner: owner}))
	}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	return rr
}

func seedIAMUser(t *testing.T, s *Server, name string) {
	t.Helper()
	if err := s.Meta.CreateIAMUser(context.Background(), &meta.IAMUser{
		UserName:  name,
		UserID:    "AID" + strings.ToUpper(name),
		Path:      "/",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed user %q: %v", name, err)
	}
}

func seedIAMAccessKey(t *testing.T, s *Server, userName, accessKeyID string) {
	t.Helper()
	if err := s.Meta.CreateIAMAccessKey(context.Background(), &meta.IAMAccessKey{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: "secret-" + accessKeyID,
		UserName:        userName,
		CreatedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed access key %q: %v", accessKeyID, err)
	}
}

func TestIAMUsersList_Empty(t *testing.T) {
	s := newTestServer()
	rr := iamRequest(t, s, http.MethodGet, "/admin/v1/iam/users", "ops", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got IAMUsersListResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 0 || len(got.Users) != 0 {
		t.Errorf("want empty list; got total=%d len=%d", got.Total, len(got.Users))
	}
	if got.Users == nil {
		t.Errorf("Users must be a non-nil slice for the React empty-state branch")
	}
}

func TestIAMUsersList_PopulatedAndKeyCounts(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	seedIAMUser(t, s, "bob")
	seedIAMAccessKey(t, s, "alice", "AKIAALICE1")
	seedIAMAccessKey(t, s, "alice", "AKIAALICE2")

	rr := iamRequest(t, s, http.MethodGet, "/admin/v1/iam/users", "ops", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got IAMUsersListResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Total != 2 {
		t.Fatalf("total=%d want 2", got.Total)
	}
	byName := map[string]IAMUserSummary{}
	for _, u := range got.Users {
		byName[u.UserName] = u
	}
	if byName["alice"].AccessKeyCount != 2 {
		t.Errorf("alice access_key_count=%d want 2", byName["alice"].AccessKeyCount)
	}
	if byName["bob"].AccessKeyCount != 0 {
		t.Errorf("bob access_key_count=%d want 0", byName["bob"].AccessKeyCount)
	}
}

func TestIAMUsersList_QueryFilter(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	seedIAMUser(t, s, "bob")
	rr := iamRequest(t, s, http.MethodGet, "/admin/v1/iam/users?query=ali", "ops", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got IAMUsersListResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Total != 1 || got.Users[0].UserName != "alice" {
		t.Errorf("want one row alice, got total=%d users=%+v", got.Total, got.Users)
	}
}

func TestIAMUsersList_Pagination(t *testing.T) {
	s := newTestServer()
	for _, n := range []string{"alice", "bob", "carol", "dave"} {
		seedIAMUser(t, s, n)
	}
	rr := iamRequest(t, s, http.MethodGet, "/admin/v1/iam/users?page_size=2&page=2", "ops", nil)
	var got IAMUsersListResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Total != 4 {
		t.Errorf("total=%d want 4", got.Total)
	}
	if len(got.Users) != 2 || got.Users[0].UserName != "carol" || got.Users[1].UserName != "dave" {
		t.Errorf("page 2 rows wrong: %+v", got.Users)
	}
}

func TestIAMUserCreate_Happy(t *testing.T) {
	s := newTestServer()
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/users", "ops", map[string]string{
		"user_name": "alice",
		"path":      "/",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got IAMUserSummary
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.UserName != "alice" || got.Path != "/" {
		t.Errorf("response shape wrong: %+v", got)
	}
	if got.UserID == "" {
		t.Errorf("UserID must be populated; got empty string")
	}
	u, err := s.Meta.GetIAMUser(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetIAMUser after create: %v", err)
	}
	if u.UserID != got.UserID {
		t.Errorf("persisted UserID=%q response=%q", u.UserID, got.UserID)
	}
}

func TestIAMUserCreate_DefaultPath(t *testing.T) {
	s := newTestServer()
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/users", "ops", map[string]string{
		"user_name": "alice",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got IAMUserSummary
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Path != "/" {
		t.Errorf("default path=%q want '/'", got.Path)
	}
}

func TestIAMUserCreate_InvalidName(t *testing.T) {
	s := newTestServer()
	for _, bad := range []string{"", " ", "ab cd", "thisnameislongerthansixtyfourcharacterswhichistheawsiamupperboundforsure"} {
		t.Run(bad, func(t *testing.T) {
			rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/users", "ops",
				map[string]string{"user_name": bad})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400", rr.Code)
			}
			var er errorResponse
			_ = json.NewDecoder(rr.Body).Decode(&er)
			if er.Code != "InvalidUserName" {
				t.Errorf("code=%q want InvalidUserName", er.Code)
			}
		})
	}
}

func TestIAMUserCreate_InvalidPath(t *testing.T) {
	s := newTestServer()
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/users", "ops", map[string]string{
		"user_name": "alice",
		"path":      "/no-trailing",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "InvalidPath" {
		t.Errorf("code=%q want InvalidPath", er.Code)
	}
}

func TestIAMUserCreate_Conflict(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/users", "ops",
		map[string]string{"user_name": "alice"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "EntityAlreadyExists" {
		t.Errorf("code=%q want EntityAlreadyExists", er.Code)
	}
}

func TestIAMUserCreate_MalformedJSON(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/iam/users", strings.NewReader("{not json"))
	req = req.WithContext(auth.WithAuth(req.Context(), &auth.AuthInfo{Owner: "ops"}))
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIAMUserDelete_Happy(t *testing.T) {
	s := newTestServerWithLocker(t)
	seedIAMUser(t, s, "alice")
	rr := iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/users/alice", "ops", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := s.Meta.GetIAMUser(context.Background(), "alice"); !errors.Is(err, meta.ErrIAMUserNotFound) {
		t.Errorf("GetIAMUser after delete: %v want ErrIAMUserNotFound", err)
	}
}

func TestIAMUserDelete_CascadeAccessKeys(t *testing.T) {
	s := newTestServerWithLocker(t)
	seedIAMUser(t, s, "alice")
	seedIAMAccessKey(t, s, "alice", "AKIAALICE1")
	seedIAMAccessKey(t, s, "alice", "AKIAALICE2")

	rr := iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/users/alice", "ops", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	keys, err := s.Meta.ListIAMAccessKeys(context.Background(), "alice")
	if err != nil {
		t.Fatalf("ListIAMAccessKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("after cascade, len(keys)=%d want 0; %+v", len(keys), keys)
	}
}

func TestIAMUserDelete_NotFound(t *testing.T) {
	s := newTestServerWithLocker(t)
	rr := iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/users/missing", "ops", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "NoSuchEntity" {
		t.Errorf("code=%q want NoSuchEntity", er.Code)
	}
}

func TestIAMUserDelete_LockerUnavailable(t *testing.T) {
	// newTestServer (no locker) — DELETE must 503 LockerUnavailable.
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	rr := iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/users/alice", "ops", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "LockerUnavailable" {
		t.Errorf("code=%q want LockerUnavailable", er.Code)
	}
}
