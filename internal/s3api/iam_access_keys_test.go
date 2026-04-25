package s3api_test

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/s3api"
)

type iamAccessKeyBody struct {
	UserName        string `xml:"UserName"`
	AccessKeyID     string `xml:"AccessKeyId"`
	Status          string `xml:"Status"`
	SecretAccessKey string `xml:"SecretAccessKey"`
}

type createAccessKeyResp struct {
	XMLName xml.Name `xml:"CreateAccessKeyResponse"`
	Result  struct {
		AccessKey iamAccessKeyBody `xml:"AccessKey"`
	} `xml:"CreateAccessKeyResult"`
}

type accessKeyMetaBody struct {
	UserName    string `xml:"UserName"`
	AccessKeyID string `xml:"AccessKeyId"`
	Status      string `xml:"Status"`
}

type listAccessKeysResp struct {
	XMLName xml.Name `xml:"ListAccessKeysResponse"`
	Result  struct {
		UserName          string `xml:"UserName"`
		AccessKeyMetadata struct {
			Members []accessKeyMetaBody `xml:"member"`
		} `xml:"AccessKeyMetadata"`
	} `xml:"ListAccessKeysResult"`
}

func TestIAMAccessKey_AnonymousDenied(t *testing.T) {
	h := newHarness(t)
	resp := iamCall(t, h, "CreateAccessKey", "", "UserName", "alice")
	h.mustStatus(resp, http.StatusForbidden)
	resp.Body.Close()
}

func TestIAMAccessKey_NonRootDenied(t *testing.T) {
	h := newHarness(t)
	resp := iamCall(t, h, "ListAccessKeys", "alice", "UserName", "alice")
	h.mustStatus(resp, http.StatusForbidden)
	resp.Body.Close()
}

func TestIAMAccessKey_RoundTrip(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	h.mustStatus(iamCall(t, h, "CreateUser", root, "UserName", "alice"), http.StatusOK)

	resp := iamCall(t, h, "CreateAccessKey", root, "UserName", "alice")
	h.mustStatus(resp, http.StatusOK)
	var created createAccessKeyResp
	decodeXML(t, resp.Body, &created)
	resp.Body.Close()
	if created.Result.AccessKey.UserName != "alice" {
		t.Fatalf("UserName: got %q", created.Result.AccessKey.UserName)
	}
	if !strings.HasPrefix(created.Result.AccessKey.AccessKeyID, "AKIA") {
		t.Fatalf("AccessKeyId prefix: got %q", created.Result.AccessKey.AccessKeyID)
	}
	if created.Result.AccessKey.SecretAccessKey == "" {
		t.Fatalf("SecretAccessKey empty in CreateAccessKey response")
	}
	if created.Result.AccessKey.Status != "Active" {
		t.Fatalf("Status: got %q want Active", created.Result.AccessKey.Status)
	}

	// Mint a second key so list returns >1 entries.
	h.mustStatus(iamCall(t, h, "CreateAccessKey", root, "UserName", "alice"), http.StatusOK)

	resp = iamCall(t, h, "ListAccessKeys", root, "UserName", "alice")
	h.mustStatus(resp, http.StatusOK)
	var list listAccessKeysResp
	decodeXML(t, resp.Body, &list)
	resp.Body.Close()
	if list.Result.UserName != "alice" {
		t.Fatalf("ListAccessKeys.UserName: got %q", list.Result.UserName)
	}
	if len(list.Result.AccessKeyMetadata.Members) != 2 {
		t.Fatalf("ListAccessKeys members: got %d want 2", len(list.Result.AccessKeyMetadata.Members))
	}
	for _, m := range list.Result.AccessKeyMetadata.Members {
		if m.UserName != "alice" {
			t.Fatalf("member UserName: got %q", m.UserName)
		}
		if !strings.HasPrefix(m.AccessKeyID, "AKIA") {
			t.Fatalf("member AccessKeyId: got %q", m.AccessKeyID)
		}
		if m.Status != "Active" {
			t.Fatalf("member Status: got %q", m.Status)
		}
	}

	// Delete one and confirm the count drops.
	target := list.Result.AccessKeyMetadata.Members[0].AccessKeyID
	h.mustStatus(iamCall(t, h, "DeleteAccessKey", root, "AccessKeyId", target), http.StatusOK)
	resp = iamCall(t, h, "ListAccessKeys", root, "UserName", "alice")
	h.mustStatus(resp, http.StatusOK)
	var after listAccessKeysResp
	decodeXML(t, resp.Body, &after)
	resp.Body.Close()
	if len(after.Result.AccessKeyMetadata.Members) != 1 {
		t.Fatalf("after delete: got %d want 1", len(after.Result.AccessKeyMetadata.Members))
	}
	if after.Result.AccessKeyMetadata.Members[0].AccessKeyID == target {
		t.Fatalf("deleted access key still listed")
	}
}

func TestIAMAccessKey_CreateForMissingUser(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	resp := iamCall(t, h, "CreateAccessKey", root, "UserName", "ghost")
	h.mustStatus(resp, http.StatusNotFound)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "NoSuchEntity") {
		t.Fatalf("expected NoSuchEntity, got: %s", body)
	}
}

func TestIAMAccessKey_ListForMissingUser(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	resp := iamCall(t, h, "ListAccessKeys", root, "UserName", "ghost")
	h.mustStatus(resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestIAMAccessKey_DeleteMissing(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	resp := iamCall(t, h, "DeleteAccessKey", root, "AccessKeyId", "AKIAGHOST")
	h.mustStatus(resp, http.StatusNotFound)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "NoSuchEntity") {
		t.Fatalf("expected NoSuchEntity, got: %s", body)
	}
}

func TestIAMAccessKey_CreateMissingUserName(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	resp := iamCall(t, h, "CreateAccessKey", root)
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestIAMAccessKey_DeleteMissingId(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	resp := iamCall(t, h, "DeleteAccessKey", root)
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()
}
