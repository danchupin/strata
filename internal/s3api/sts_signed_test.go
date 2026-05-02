package s3api_test

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// stsHarness adds an auth.STSStore to the credential chain and exposes it so
// tests can drive expiry via SetClock.
type stsHarness struct {
	t   *testing.T
	ts  *httptest.Server
	sts *auth.STSStore
}

func newSTSHarness(t *testing.T) *stsHarness {
	t.Helper()
	ms := metamem.New()
	api := s3api.New(datamem.New(), ms)
	api.Region = "us-east-1"

	sts := auth.NewSTSStore()
	api.STS = sts

	stores := []auth.CredentialsStore{
		sts,
		auth.NewStaticStore(map[string]*auth.Credential{
			iamRootAK: {AccessKey: iamRootAK, Secret: iamRootSK, Owner: s3api.IAMRootPrincipal},
		}),
		metamem.NewCredentialStore(ms),
	}
	multi := auth.NewMultiStore(time.Minute, stores...)
	api.InvalidateCredential = multi.Invalidate

	mw := &auth.Middleware{Store: multi, Mode: auth.ModeRequired}
	ts := httptest.NewServer(mw.Wrap(api, s3api.WriteAuthDenied))
	t.Cleanup(ts.Close)
	return &stsHarness{t: t, ts: ts, sts: sts}
}

type assumeRoleResp struct {
	XMLName xml.Name `xml:"AssumeRoleResponse"`
	Result  struct {
		Credentials struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
			SessionToken    string `xml:"SessionToken"`
			Expiration      string `xml:"Expiration"`
		} `xml:"Credentials"`
		AssumedRoleUser struct {
			AssumedRoleID string `xml:"AssumedRoleId"`
			Arn           string `xml:"Arn"`
		} `xml:"AssumedRoleUser"`
	} `xml:"AssumeRoleResult"`
}

func stsAssumeRoleSigned(t *testing.T, ts *httptest.Server, ak, sk, roleArn, sessionName string) *http.Response {
	t.Helper()
	v := url.Values{}
	v.Set("Action", "AssumeRole")
	v.Set("RoleArn", roleArn)
	v.Set("RoleSessionName", sessionName)
	body := v.Encode()
	req, err := http.NewRequest("POST", ts.URL+"/", strings.NewReader(""))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.URL.RawQuery = body
	req.ContentLength = 0
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signRequest(t, req, ak, sk, "us-east-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// signedGetWithToken signs a GET / and adds x-amz-security-token outside the
// signed-header set (the middleware reads the header independently of SigV4
// signed headers, matching the AC: "honors x-amz-security-token").
func signedGetWithToken(t *testing.T, ts *httptest.Server, ak, sk, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", ts.URL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	signRequest(t, req, ak, sk, "us-east-1")
	if token != "" {
		req.Header.Set("X-Amz-Security-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// TestSTS_AssumeRole_Roundtrip covers: AssumeRole returns AccessKeyId,
// SecretAccessKey, SessionToken, Expiration; the temp creds authenticate a
// signed request; expiration leads to 403 ExpiredToken.
func TestSTS_AssumeRole_Roundtrip(t *testing.T) {
	h := newSTSHarness(t)

	// Pin clock so we can advance past expiry deterministically.
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := now
	h.sts.SetClock(func() time.Time { return clk })

	resp := stsAssumeRoleSigned(t, h.ts, iamRootAK, iamRootSK, "arn:aws:iam::strata:role/test", "session1")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("AssumeRole: status=%d body=%s", resp.StatusCode, body)
	}
	var ar assumeRoleResp
	if err := xml.NewDecoder(resp.Body).Decode(&ar); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()

	creds := ar.Result.Credentials
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" || creds.SessionToken == "" {
		t.Fatalf("missing fields in response: %+v", creds)
	}
	if !strings.HasPrefix(creds.AccessKeyID, "ASIA") {
		t.Errorf("expected ASIA-prefixed AccessKeyId, got %q", creds.AccessKeyID)
	}
	if creds.Expiration == "" {
		t.Errorf("missing Expiration")
	}
	if ar.Result.AssumedRoleUser.Arn == "" {
		t.Errorf("missing AssumedRoleUser.Arn")
	}

	// Use the temp credential.
	resp = signedGetWithToken(t, h.ts, creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)
	if resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("temp creds should authenticate; got 403 body=%s", body)
	}
	resp.Body.Close()

	// Missing token while cred has SessionToken → InvalidToken 403.
	resp = signedGetWithToken(t, h.ts, creds.AccessKeyID, creds.SecretAccessKey, "")
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 403 without token; got status=%d body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "InvalidToken") {
		t.Fatalf("expected InvalidToken, got: %s", body)
	}

	// Wrong token → InvalidToken.
	resp = signedGetWithToken(t, h.ts, creds.AccessKeyID, creds.SecretAccessKey, "not-the-token")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on wrong token; got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Advance the clock past the default 1h expiry.
	clk = now.Add(2 * time.Hour)
	resp = signedGetWithToken(t, h.ts, creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 403 after expiry; got status=%d body=%s", resp.StatusCode, body)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "ExpiredToken") {
		t.Fatalf("expected ExpiredToken, got: %s", body)
	}
}

func TestSTS_AssumeRole_RequiresIAMRoot(t *testing.T) {
	h := newSTSHarness(t)
	// Anonymous (no auth) cannot reach AssumeRole. Hit ?Action=AssumeRole
	// without signing — middleware rejects before the handler runs.
	resp, err := http.PostForm(h.ts.URL+"/", url.Values{
		"Action":          {"AssumeRole"},
		"RoleArn":         {"arn:aws:iam::strata:role/x"},
		"RoleSessionName": {"s"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSTS_AssumeRole_ValidationErrors(t *testing.T) {
	h := newSTSHarness(t)

	// Missing RoleArn.
	resp := stsAssumeRoleSigned(t, h.ts, iamRootAK, iamRootSK, "", "session1")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing RoleArn, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Missing RoleSessionName.
	resp = stsAssumeRoleSigned(t, h.ts, iamRootAK, iamRootSK, "arn:aws:iam::strata:role/x", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing RoleSessionName, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
