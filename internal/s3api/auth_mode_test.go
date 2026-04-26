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
)

func newAuthModeHarness(t *testing.T, mode auth.Mode) *authModeHarness {
	t.Helper()
	ms := metamem.New()
	api := s3api.New(datamem.New(), ms)
	api.Region = "us-east-1"

	store := auth.NewStaticStore(map[string]*auth.Credential{
		modeOwnerAK: {AccessKey: modeOwnerAK, Secret: modeOwnerSK, Owner: modeOwnerID},
	})
	multi := auth.NewMultiStore(time.Minute, store)
	mw := &auth.Middleware{Store: multi, Mode: mode}
	ts := httptest.NewServer(mw.Wrap(api, s3api.WriteAuthDenied))
	t.Cleanup(ts.Close)
	return &authModeHarness{t: t, ts: ts}
}

func (h *authModeHarness) signed(method, path, body string, headers ...string) *http.Response {
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
	signRequest(h.t, req, modeOwnerAK, modeOwnerSK, "us-east-1")
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

func mustStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status: got %d want %d", resp.StatusCode, want)
	}
	resp.Body.Close()
}
