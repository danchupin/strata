package s3api_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// signedHarness wraps the s3api.Server with the real auth.Middleware backed by
// an in-memory MultiStore that knows about the [iam root] credential and any
// IAM-minted access keys (via memory.CredentialStore over the same meta.Store).
type signedHarness struct {
	t     *testing.T
	ts    *httptest.Server
	multi *auth.MultiStore
}

const (
	iamRootAK = "AKIATESTROOT00000000"
	iamRootSK = "rootsecretrootsecretrootsecret00"
)

func newSignedHarness(t *testing.T) *signedHarness {
	t.Helper()
	ms := metamem.New()
	api := s3api.New(datamem.New(), ms)
	api.Region = "us-east-1"

	stores := []auth.CredentialsStore{
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
	return &signedHarness{t: t, ts: ts, multi: multi}
}

// signRequest applies AWS SigV4 to req using the canonical "list-buckets" GET
// shape (no query, no body). It mutates req.Header to add Authorization and
// X-Amz-Date.
func signRequest(t *testing.T, req *http.Request, accessKey, secret, region string) {
	t.Helper()
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	if req.Header.Get("Host") == "" {
		req.Host = req.URL.Host
	}

	bodyHash := sha256Hex(nil)
	req.Header.Set("X-Amz-Content-Sha256", bodyHash)

	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonical := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL.EscapedPath()),
		canonicalQuery(req.URL),
		canonicalHeaderBlock(req, signedHeaders),
		strings.Join(signedHeaders, ";"),
		bodyHash,
	}, "\n")

	scope := day + "/" + region + "/s3/aws4_request"
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonical)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(day))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	sig := hex.EncodeToString(hmacSHA256(kSigning, []byte(sts)))

	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
			", SignedHeaders="+strings.Join(signedHeaders, ";")+
			", Signature="+sig,
	)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func canonicalPath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func canonicalQuery(u *url.URL) string {
	vals := u.Query()
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	first := true
	for _, k := range keys {
		vs := append([]string(nil), vals[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			if !first {
				b.WriteByte('&')
			}
			first = false
			b.WriteString(uriEsc(k))
			b.WriteByte('=')
			b.WriteString(uriEsc(v))
		}
	}
	return b.String()
}

func uriEsc(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'),
			c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		default:
			const hexd = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&0xF])
		}
	}
	return b.String()
}

func canonicalHeaderBlock(r *http.Request, signed []string) string {
	var b strings.Builder
	for _, h := range signed {
		b.WriteString(h)
		b.WriteByte(':')
		var v string
		if h == "host" {
			v = r.Host
		} else {
			v = r.Header.Get(h)
		}
		b.WriteString(strings.TrimSpace(v))
		b.WriteByte('\n')
	}
	return b.String()
}

// iamCallSigned posts a SigV4-signed ?Action= form to the signed harness using
// the supplied credential.
func iamCallSigned(t *testing.T, h *signedHarness, ak, sk, action string, kv ...string) *http.Response {
	t.Helper()
	v := url.Values{}
	v.Set("Action", action)
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	body := v.Encode()
	req, err := http.NewRequest("POST", h.ts.URL+"/", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// SigV4 over an unsigned form body is fine because we sign the body hash
	// of an empty body — the gateway accepts ?Action= via either query or
	// PostForm. Use the query path here so signing stays simple.
	req.URL.RawQuery = body
	req.Body = io.NopCloser(strings.NewReader(""))
	req.ContentLength = 0
	signRequest(t, req, ak, sk, "us-east-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// signedGet sends a GET / signed with the supplied credential.
func signedGet(t *testing.T, h *signedHarness, ak, sk string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", h.ts.URL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	signRequest(t, req, ak, sk, "us-east-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// TestIAMAccessKey_UseThenDelete covers the AC: CreateAccessKey → use →
// DeleteAccessKey → 403 InvalidAccessKeyId on next request.
func TestIAMAccessKey_UseThenDelete(t *testing.T) {
	h := newSignedHarness(t)

	// Provision a user under iam-root credentials.
	resp := iamCallSigned(t, h, iamRootAK, iamRootSK, "CreateUser", "UserName", "alice")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("CreateUser: status=%d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Mint an access key for alice.
	resp = iamCallSigned(t, h, iamRootAK, iamRootSK, "CreateAccessKey", "UserName", "alice")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("CreateAccessKey: status=%d body=%s", resp.StatusCode, body)
	}
	var created createAccessKeyResp
	if err := xml.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	aliceAK := created.Result.AccessKey.AccessKeyID
	aliceSK := created.Result.AccessKey.SecretAccessKey
	if aliceAK == "" || aliceSK == "" {
		t.Fatalf("missing creds: %+v", created.Result.AccessKey)
	}

	// Use the new credential against a signed endpoint (GET / lists buckets,
	// but we only need to verify the request authenticates — non-403 status).
	resp = signedGet(t, h, aliceAK, aliceSK)
	if resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("alice key should authenticate; got 403 body=%s", body)
	}
	resp.Body.Close()

	// Delete the access key (under root) and re-issue the same signed GET.
	resp = iamCallSigned(t, h, iamRootAK, iamRootSK, "DeleteAccessKey", "AccessKeyId", aliceAK)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("DeleteAccessKey: status=%d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()

	resp = signedGet(t, h, aliceAK, aliceSK)
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("post-delete request should be 403; got status=%d body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "InvalidAccessKeyId") {
		t.Fatalf("expected InvalidAccessKeyId, got: %s", body)
	}
}
