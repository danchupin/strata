package s3api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// authModeHarness runs s3api.Server behind the real auth.Middleware in a
// configurable Mode. Used to verify US-024 behaviour end-to-end (anonymous
// gating depends on both the middleware and the policy/ACL stack).
type authModeHarness struct {
	t  *testing.T
	ts *httptest.Server
}

const (
	modeOwnerAK = "AKIATESTOWNER0000000"
	modeOwnerSK = "ownersecretownersecretownerse00"
	modeOwnerID = "owner-mode"
	// Second, distinct principal — used to prove a valid signature from a
	// NON-owner is still denied by the ACL/policy stack (authn != authz).
	modeOtherAK = "AKIATESTOTHER0000000"
	modeOtherSK = "othersecretothersecretotherse00"
	modeOtherID = "other-mode"
)

func newAuthModeHarness(t *testing.T, mode auth.Mode) *authModeHarness {
	t.Helper()
	ms := metamem.New()
	api := s3api.New(datamem.New(), ms)
	api.Region = "us-east-1"

	store := auth.NewStaticStore(map[string]*auth.Credential{
		modeOwnerAK: {AccessKey: modeOwnerAK, Secret: modeOwnerSK, Owner: modeOwnerID},
		modeOtherAK: {AccessKey: modeOtherAK, Secret: modeOtherSK, Owner: modeOtherID},
	})
	multi := auth.NewMultiStore(time.Minute, store)
	mw := &auth.Middleware{Store: multi, Mode: mode}
	ts := httptest.NewServer(mw.Wrap(api, s3api.WriteAuthDenied))
	t.Cleanup(ts.Close)
	return &authModeHarness{t: t, ts: ts}
}

func (h *authModeHarness) signed(method, path, body string, headers ...string) *http.Response {
	return h.signedAs(modeOwnerAK, modeOwnerSK, method, path, body, headers...)
}

// signedAs signs the request with the given access key + secret, letting tests
// drive a non-owner (or wrong-secret) principal through the same path.
func (h *authModeHarness) signedAs(ak, sk, method, path, body string, headers ...string) *http.Response {
	h.t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	var req *http.Request
	var err error
	if rdr == nil {
		req, err = http.NewRequest(method, h.ts.URL+path, nil)
	} else {
		req, err = http.NewRequest(method, h.ts.URL+path, rdr)
	}
	if err != nil {
		h.t.Fatalf("new req: %v", err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	signRequest(h.t, req, ak, sk, "us-east-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("do: %v", err)
	}
	return resp
}

// badSig signs with a registered access key but the WRONG secret, so the
// gateway recomputes a mismatching signature and must deny.
func (h *authModeHarness) badSig(method, path string) *http.Response {
	return h.signedAs(modeOwnerAK, "wrongsecretwrongsecretwrongsec00", method, path, "")
}

// anonBody is anon() with a request body (for write verbs).
func (h *authModeHarness) anonBody(method, path, body string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.ts.URL+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("new req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("do: %v", err)
	}
	return resp
}

func (h *authModeHarness) anon(method, path string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.ts.URL+path, nil)
	if err != nil {
		h.t.Fatalf("new req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("do: %v", err)
	}
	return resp
}

// US-024: optional mode + public-read bucket → anonymous GET succeeds.
func TestAuthMode_Optional_PublicBucket_AnonAllowed(t *testing.T) {
	h := newAuthModeHarness(t, auth.ModeOptional)

	mustStatus(t, h.signed("PUT", "/bkt", ""), http.StatusOK)
	mustStatus(t, h.signed("PUT", "/bkt?acl=", "", "x-amz-acl", "public-read"), http.StatusOK)
	mustStatus(t, h.signed("PUT", "/bkt/k", "hello"), http.StatusOK)

	resp := h.anon("GET", "/bkt/k")
	mustStatus(t, resp, http.StatusOK)
}

// US-024: optional mode + private bucket → anonymous GET denied (gated by ACL).
func TestAuthMode_Optional_PrivateBucket_AnonDenied(t *testing.T) {
	h := newAuthModeHarness(t, auth.ModeOptional)

	mustStatus(t, h.signed("PUT", "/bkt", ""), http.StatusOK)
	mustStatus(t, h.signed("PUT", "/bkt/k", "hello"), http.StatusOK)

	resp := h.anon("GET", "/bkt/k")
	mustStatus(t, resp, http.StatusForbidden)
}

// US-024: required mode + no auth header → 403, regardless of ACL.
func TestAuthMode_Required_AnonAlwaysDenied(t *testing.T) {
	h := newAuthModeHarness(t, auth.ModeRequired)

	mustStatus(t, h.signed("PUT", "/bkt", ""), http.StatusOK)
	mustStatus(t, h.signed("PUT", "/bkt?acl=", "", "x-amz-acl", "public-read"), http.StatusOK)
	mustStatus(t, h.signed("PUT", "/bkt/k", "hello"), http.StatusOK)

	mustStatus(t, h.anon("GET", "/bkt/k"), http.StatusForbidden)
}

// off mode is the dev-only "no auth at all" posture: every request is
// full-access and bypasses the policy/ACL stack. Anonymous writes/deletes/
// multipart-init against a bucket succeed regardless of ownership — this is
// the case the admin-console e2e relies on (UI creates the bucket, the S3
// surface writes anonymously).
func TestAuthMode_Off_AnonFullAccess(t *testing.T) {
	h := newAuthModeHarness(t, auth.ModeOff)

	mustStatus(t, h.anon("PUT", "/bkt"), http.StatusOK)              // create
	mustStatus(t, h.anonBody("PUT", "/bkt/k", "hello"), http.StatusOK) // write
	mustStatus(t, h.anon("GET", "/bkt/k"), http.StatusOK)           // read
	mustStatus(t, h.anon("POST", "/bkt/mp?uploads"), http.StatusOK) // multipart init
	mustStatus(t, h.anon("DELETE", "/bkt/k"), http.StatusNoContent) // delete
}

// optional mode + public-read bucket → anonymous WRITE still denied (the
// canned ACL grants read, not write).
func TestAuthMode_Optional_PublicRead_AnonWriteDenied(t *testing.T) {
	h := newAuthModeHarness(t, auth.ModeOptional)

	mustStatus(t, h.signed("PUT", "/bkt", ""), http.StatusOK)
	mustStatus(t, h.signed("PUT", "/bkt?acl=", "", "x-amz-acl", "public-read"), http.StatusOK)

	mustStatus(t, h.anonBody("PUT", "/bkt/k", "hello"), http.StatusForbidden)
}

// optional mode + public-read-write bucket → anonymous WRITE allowed.
func TestAuthMode_Optional_PublicReadWrite_AnonWriteAllowed(t *testing.T) {
	h := newAuthModeHarness(t, auth.ModeOptional)

	mustStatus(t, h.signed("PUT", "/bkt", ""), http.StatusOK)
	mustStatus(t, h.signed("PUT", "/bkt?acl=", "", "x-amz-acl", "public-read-write"), http.StatusOK)

	mustStatus(t, h.anonBody("PUT", "/bkt/k", "hello"), http.StatusOK)
	mustStatus(t, h.anon("GET", "/bkt/k"), http.StatusOK)
}

// authn != authz: a VALID signature from a non-owner is still denied on the
// owner's private bucket (optional + required alike).
func TestAuthMode_Optional_SignedNonOwner_PrivateDenied(t *testing.T) {
	h := newAuthModeHarness(t, auth.ModeOptional)

	mustStatus(t, h.signed("PUT", "/bkt", ""), http.StatusOK)
	mustStatus(t, h.signed("PUT", "/bkt/k", "hello"), http.StatusOK)

	mustStatus(t, h.signedAs(modeOtherAK, modeOtherSK, "GET", "/bkt/k", ""), http.StatusForbidden)
	mustStatus(t, h.signedAs(modeOtherAK, modeOtherSK, "PUT", "/bkt/k2", "x"), http.StatusForbidden)
}

// optional mode + Authorization header with a bad signature → 403 (a present
// header is validated, never treated as anonymous).
func TestAuthMode_Optional_BadSignature_Denied(t *testing.T) {
	h := newAuthModeHarness(t, auth.ModeOptional)

	mustStatus(t, h.signed("PUT", "/bkt", ""), http.StatusOK)
	mustStatus(t, h.badSig("GET", "/bkt"), http.StatusForbidden)
}

// required mode + valid owner signature → allowed across verbs.
func TestAuthMode_Required_ValidOwner_Allowed(t *testing.T) {
	h := newAuthModeHarness(t, auth.ModeRequired)

	mustStatus(t, h.signed("PUT", "/bkt", ""), http.StatusOK)
	mustStatus(t, h.signed("PUT", "/bkt/k", "hello"), http.StatusOK)
	mustStatus(t, h.signed("GET", "/bkt/k", ""), http.StatusOK)
}

// required mode + bad signature → 403.
func TestAuthMode_Required_BadSignature_Denied(t *testing.T) {
	h := newAuthModeHarness(t, auth.ModeRequired)

	mustStatus(t, h.signed("PUT", "/bkt", ""), http.StatusOK)
	mustStatus(t, h.badSig("GET", "/bkt"), http.StatusForbidden)
}

// required mode + valid non-owner signature → still denied on a private bucket.
func TestAuthMode_Required_SignedNonOwner_PrivateDenied(t *testing.T) {
	h := newAuthModeHarness(t, auth.ModeRequired)

	mustStatus(t, h.signed("PUT", "/bkt", ""), http.StatusOK)
	mustStatus(t, h.signed("PUT", "/bkt/k", "hello"), http.StatusOK)

	mustStatus(t, h.signedAs(modeOtherAK, modeOtherSK, "GET", "/bkt/k", ""), http.StatusForbidden)
}

func mustStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status: got %d want %d", resp.StatusCode, want)
	}
	resp.Body.Close()
}
