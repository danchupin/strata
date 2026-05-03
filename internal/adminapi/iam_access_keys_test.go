package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

// iamRequestRaw drives an admin request through routes() using a raw bytes
// body — used by the "malformed JSON" tests where iamRequest's
// json.NewEncoder would refuse the input.
func iamRequestRaw(t *testing.T, s *Server, method, path, owner string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if owner != "" {
		req = req.WithContext(auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: owner, Owner: owner}))
	}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	return rr
}

func TestIAMAccessKeyList_Empty(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	rr := iamRequest(t, s, http.MethodGet, "/admin/v1/iam/users/alice/access-keys", "ops", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got IAMAccessKeyListResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AccessKeys == nil {
		t.Fatalf("AccessKeys must be a non-nil slice for the React empty-state branch")
	}
	if len(got.AccessKeys) != 0 {
		t.Errorf("want 0 keys; got %d", len(got.AccessKeys))
	}
}

func TestIAMAccessKeyList_Populated(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	seedIAMAccessKey(t, s, "alice", "AKIATESTAAA")
	seedIAMAccessKey(t, s, "alice", "AKIATESTBBB")
	// Disable one to verify the bool round-trips.
	if _, err := s.Meta.UpdateIAMAccessKeyDisabled(context.Background(), "AKIATESTBBB", true); err != nil {
		t.Fatalf("disable: %v", err)
	}

	rr := iamRequest(t, s, http.MethodGet, "/admin/v1/iam/users/alice/access-keys", "ops", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got IAMAccessKeyListResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.AccessKeys) != 2 {
		t.Fatalf("want 2 keys; got %d", len(got.AccessKeys))
	}
	if got.AccessKeys[0].AccessKeyID != "AKIATESTAAA" || got.AccessKeys[1].AccessKeyID != "AKIATESTBBB" {
		t.Errorf("ordering wrong: %+v", got.AccessKeys)
	}
	if got.AccessKeys[0].Disabled || !got.AccessKeys[1].Disabled {
		t.Errorf("disabled flag mismatch: %+v", got.AccessKeys)
	}
	// Secret must NEVER appear on a list response — sanity-check the JSON body.
	if got, want := rr.Body.String(), "secret"; len(got) > 0 && bytesContainsCI([]byte(got), []byte(want)) {
		t.Errorf("list response leaked secret-shaped substring: %s", got)
	}
}

func TestIAMAccessKeyList_UserMissing(t *testing.T) {
	s := newTestServer()
	rr := iamRequest(t, s, http.MethodGet, "/admin/v1/iam/users/ghost/access-keys", "ops", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIAMAccessKeyCreate_Happy(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/users/alice/access-keys", "ops", nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got IAMAccessKeyCreateResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AccessKeyID == "" || got.SecretAccessKey == "" {
		t.Fatalf("missing fields in response: %+v", got)
	}
	if got.UserName != "alice" {
		t.Errorf("user_name=%q want alice", got.UserName)
	}
	if got.Disabled {
		t.Errorf("freshly minted key must be enabled")
	}
	// Verify the key actually persisted and can be re-read.
	keys, err := s.Meta.ListIAMAccessKeys(context.Background(), "alice")
	if err != nil || len(keys) != 1 || keys[0].AccessKeyID != got.AccessKeyID {
		t.Fatalf("post-create list: %v %+v", err, keys)
	}
}

func TestIAMAccessKeyCreate_UserMissing(t *testing.T) {
	s := newTestServer()
	rr := iamRequest(t, s, http.MethodPost, "/admin/v1/iam/users/ghost/access-keys", "ops", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIAMAccessKeyUpdate_DisableThenEnable(t *testing.T) {
	s := newTestServer()
	var (
		mu          sync.Mutex
		invalidated []string
	)
	s.InvalidateCredential = func(ak string) {
		mu.Lock()
		defer mu.Unlock()
		invalidated = append(invalidated, ak)
	}
	seedIAMUser(t, s, "alice")
	seedIAMAccessKey(t, s, "alice", "AKIAFLIPKEY1")

	rr := iamRequest(t, s, http.MethodPatch, "/admin/v1/iam/access-keys/AKIAFLIPKEY1", "ops",
		IAMAccessKeyUpdateRequest{Disabled: true})
	if rr.Code != http.StatusOK {
		t.Fatalf("disable: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got IAMAccessKeySummary
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Disabled {
		t.Errorf("response Disabled=false")
	}
	read, err := s.Meta.GetIAMAccessKey(context.Background(), "AKIAFLIPKEY1")
	if err != nil || !read.Disabled {
		t.Fatalf("post-disable read: err=%v disabled=%v", err, read != nil && read.Disabled)
	}

	// Now enable.
	rr = iamRequest(t, s, http.MethodPatch, "/admin/v1/iam/access-keys/AKIAFLIPKEY1", "ops",
		IAMAccessKeyUpdateRequest{Disabled: false})
	if rr.Code != http.StatusOK {
		t.Fatalf("enable: status=%d body=%s", rr.Code, rr.Body.String())
	}
	read, err = s.Meta.GetIAMAccessKey(context.Background(), "AKIAFLIPKEY1")
	if err != nil || read.Disabled {
		t.Fatalf("post-enable read: err=%v disabled=%v", err, read != nil && read.Disabled)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(invalidated) != 2 || invalidated[0] != "AKIAFLIPKEY1" || invalidated[1] != "AKIAFLIPKEY1" {
		t.Errorf("InvalidateCredential not called twice with the access key: %v", invalidated)
	}
}

func TestIAMAccessKeyUpdate_Missing(t *testing.T) {
	s := newTestServer()
	rr := iamRequest(t, s, http.MethodPatch, "/admin/v1/iam/access-keys/AKIA-MISSING", "ops",
		IAMAccessKeyUpdateRequest{Disabled: true})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIAMAccessKeyUpdate_MalformedJSON(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	seedIAMAccessKey(t, s, "alice", "AKIAMALFORMEDJ")
	// raw string body is sent verbatim by iamRequest only when it can be
	// JSON-encoded; build the recorder ourselves.
	rr := iamRequestRaw(t, s, http.MethodPatch, "/admin/v1/iam/access-keys/AKIAMALFORMEDJ",
		"ops", []byte("not json"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestIAMAccessKeyDelete_Happy(t *testing.T) {
	s := newTestServer()
	var (
		mu          sync.Mutex
		invalidated []string
	)
	s.InvalidateCredential = func(ak string) {
		mu.Lock()
		defer mu.Unlock()
		invalidated = append(invalidated, ak)
	}
	seedIAMUser(t, s, "alice")
	seedIAMAccessKey(t, s, "alice", "AKIAGONESOON")

	rr := iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/access-keys/AKIAGONESOON", "ops", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := s.Meta.GetIAMAccessKey(context.Background(), "AKIAGONESOON"); err != meta.ErrIAMAccessKeyNotFound {
		t.Errorf("post-delete get: got %v want ErrIAMAccessKeyNotFound", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(invalidated) != 1 || invalidated[0] != "AKIAGONESOON" {
		t.Errorf("InvalidateCredential not called with the deleted key: %v", invalidated)
	}
}

func TestIAMAccessKeyDelete_Missing(t *testing.T) {
	s := newTestServer()
	rr := iamRequest(t, s, http.MethodDelete, "/admin/v1/iam/access-keys/AKIA-NEVER-EXISTED", "ops", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// bytesContainsCI is a case-insensitive substring check that does not
// allocate. Used for the "no secret leaked into the list response" sanity
// check above.
func bytesContainsCI(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	if len(needle) > len(haystack) {
		return false
	}
outer:
	for i := 0; i+len(needle) <= len(haystack); i++ {
		for j := 0; j < len(needle); j++ {
			h := haystack[i+j]
			n := needle[j]
			if h >= 'A' && h <= 'Z' {
				h += 'a' - 'A'
			}
			if n >= 'A' && n <= 'Z' {
				n += 'a' - 'A'
			}
			if h != n {
				continue outer
			}
		}
		return true
	}
	return false
}
