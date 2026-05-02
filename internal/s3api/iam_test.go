package s3api_test

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/s3api"
)

func iamForm(action string, kv ...string) string {
	v := url.Values{}
	v.Set("Action", action)
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v.Encode()
}

func iamCall(t *testing.T, h *testHarness, action string, principal string, kv ...string) *http.Response {
	t.Helper()
	body := iamForm(action, kv...)
	headers := []string{"Content-Type", "application/x-www-form-urlencoded"}
	if principal != "" {
		headers = append(headers, testPrincipalHeader, principal)
	}
	return h.do("POST", "/", strings.NewReader(body), headers...)
}

type iamUserBody struct {
	UserName string `xml:"UserName"`
	UserID   string `xml:"UserId"`
	Path     string `xml:"Path"`
	Arn      string `xml:"Arn"`
}

type createUserResp struct {
	XMLName xml.Name `xml:"CreateUserResponse"`
	Result  struct {
		User iamUserBody `xml:"User"`
	} `xml:"CreateUserResult"`
}

type getUserResp struct {
	XMLName xml.Name `xml:"GetUserResponse"`
	Result  struct {
		User iamUserBody `xml:"User"`
	} `xml:"GetUserResult"`
}

type listUsersResp struct {
	XMLName xml.Name `xml:"ListUsersResponse"`
	Result  struct {
		Users struct {
			Members []iamUserBody `xml:"member"`
		} `xml:"Users"`
	} `xml:"ListUsersResult"`
}

func decodeXML(t *testing.T, body io.Reader, v any) {
	t.Helper()
	if err := xml.NewDecoder(body).Decode(v); err != nil {
		t.Fatalf("decode xml: %v", err)
	}
}

func TestIAM_AnonymousDenied(t *testing.T) {
	h := newHarness(t)
	resp := iamCall(t, h, "ListUsers", "")
	h.mustStatus(resp, http.StatusForbidden)
}

func TestIAM_NonRootPrincipalDenied(t *testing.T) {
	h := newHarness(t)
	resp := iamCall(t, h, "ListUsers", "alice")
	h.mustStatus(resp, http.StatusForbidden)
}

func TestIAM_RoundTrip(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal

	resp := iamCall(t, h, "CreateUser", root, "UserName", "alice", "Path", "/team/")
	h.mustStatus(resp, http.StatusOK)
	var created createUserResp
	decodeXML(t, resp.Body, &created)
	resp.Body.Close()
	if created.Result.User.UserName != "alice" {
		t.Fatalf("UserName: got %q", created.Result.User.UserName)
	}
	if created.Result.User.Path != "/team/" {
		t.Fatalf("Path: got %q", created.Result.User.Path)
	}
	if !strings.HasPrefix(created.Result.User.UserID, "AID") {
		t.Fatalf("UserID prefix: got %q", created.Result.User.UserID)
	}
	if !strings.Contains(created.Result.User.Arn, "user/team/alice") {
		t.Fatalf("Arn: got %q", created.Result.User.Arn)
	}

	resp = iamCall(t, h, "GetUser", root, "UserName", "alice")
	h.mustStatus(resp, http.StatusOK)
	var got getUserResp
	decodeXML(t, resp.Body, &got)
	resp.Body.Close()
	if got.Result.User.UserName != "alice" {
		t.Fatalf("GetUser UserName: got %q", got.Result.User.UserName)
	}
	if got.Result.User.UserID != created.Result.User.UserID {
		t.Fatalf("GetUser UserID: got %q want %q", got.Result.User.UserID, created.Result.User.UserID)
	}

	// Add a second user and list.
	h.mustStatus(iamCall(t, h, "CreateUser", root, "UserName", "bob"), http.StatusOK)

	resp = iamCall(t, h, "ListUsers", root)
	h.mustStatus(resp, http.StatusOK)
	var ls listUsersResp
	decodeXML(t, resp.Body, &ls)
	resp.Body.Close()
	if len(ls.Result.Users.Members) != 2 {
		t.Fatalf("ListUsers: got %d users want 2", len(ls.Result.Users.Members))
	}

	// Delete and verify GetUser 404.
	h.mustStatus(iamCall(t, h, "DeleteUser", root, "UserName", "alice"), http.StatusOK)
	resp = iamCall(t, h, "GetUser", root, "UserName", "alice")
	h.mustStatus(resp, http.StatusNotFound)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "NoSuchEntity") {
		t.Fatalf("expected NoSuchEntity in body, got: %s", body)
	}
}

func TestIAM_DoubleCreateConflicts(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	h.mustStatus(iamCall(t, h, "CreateUser", root, "UserName", "alice"), http.StatusOK)
	resp := iamCall(t, h, "CreateUser", root, "UserName", "alice")
	h.mustStatus(resp, http.StatusConflict)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "EntityAlreadyExists") {
		t.Fatalf("expected EntityAlreadyExists, got: %s", body)
	}
}

func TestIAM_DeleteMissingNotFound(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	resp := iamCall(t, h, "DeleteUser", root, "UserName", "ghost")
	h.mustStatus(resp, http.StatusNotFound)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "NoSuchEntity") {
		t.Fatalf("expected NoSuchEntity, got: %s", body)
	}
}

func TestIAM_MissingUserNameValidation(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	resp := iamCall(t, h, "CreateUser", root)
	h.mustStatus(resp, http.StatusBadRequest)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "ValidationError") {
		t.Fatalf("expected ValidationError, got: %s", body)
	}
}

func TestIAM_UnknownActionRejected(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	resp := iamCall(t, h, "Frobulate", root)
	h.mustStatus(resp, http.StatusBadRequest)
}
