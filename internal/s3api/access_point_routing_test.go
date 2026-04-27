package s3api_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// apHostFor builds the canonical access-point host shape this gateway
// recognises. The TCP destination keeps coming from the harness URL — only
// the Host header is rewritten here.
func apHostFor(alias string) string {
	return alias + ".s3-accesspoint.us-east-1.example.com"
}

// doAP issues a request against the harness with the given Host header and
// optional principal injected via the test-principal header.
func (h *testHarness) doAP(method, path, body, host, principal string, headers ...string) *http.Response {
	h.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, r)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	if host != "" {
		req.Host = host
	}
	if principal != "" {
		req.Header.Set(testPrincipalHeader, principal)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("request %s %s: %v", method, path, err)
	}
	return resp
}

// createAccessPointForRouting seeds a bucket + access point and returns the
// alias. owner is the principal that PUTs the bucket and the object so the
// bucket-owner ACL short-circuit applies on later GETs.
func createAccessPointForRouting(t *testing.T, h *testHarness, owner, name, bucket string, vpcID string) string {
	t.Helper()
	root := s3api.IAMRootPrincipal
	h.mustStatus(h.doString(http.MethodPut, "/"+bucket, "", testPrincipalHeader, owner), http.StatusOK)
	args := []string{"Name", name, "Bucket", bucket}
	if vpcID != "" {
		args = append(args, "VpcConfiguration.VpcId", vpcID)
	}
	resp := iamCall(t, h, "CreateAccessPoint", root, args...)
	h.mustStatus(resp, http.StatusOK)
	var created apCreateResp
	decodeXML(t, resp.Body, &created)
	resp.Body.Close()
	if !strings.HasPrefix(created.Result.Alias, "ap-") {
		t.Fatalf("alias shape: %q", created.Result.Alias)
	}
	return created.Result.Alias
}

func TestAccessPointRouting_GETReachesUnderlyingBucket(t *testing.T) {
	h := newHarness(t)
	owner := s3api.IAMRootPrincipal
	alias := createAccessPointForRouting(t, h, owner, "ap-route", "bkt", "")
	h.mustStatus(h.doString(http.MethodPut, "/bkt/key.txt", "hello", testPrincipalHeader, owner), http.StatusOK)

	resp := h.doAP(http.MethodGet, "/key.txt", "", apHostFor(alias), owner)
	h.mustStatus(resp, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "hello" {
		t.Fatalf("body: got %q want %q", string(body), "hello")
	}
}

func TestAccessPointRouting_AliasNotFound(t *testing.T) {
	h := newHarness(t)
	resp := h.doAP(http.MethodGet, "/k", "", apHostFor("ap-missing12"), s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusNotFound)
	body := h.readBody(resp)
	if !strings.Contains(body, "NoSuchAccessPoint") {
		t.Fatalf("body: %s", body)
	}
}

// setAccessPointPolicy writes a JSON policy document directly onto the AP
// row via the meta store. The wire-side PUT-policy endpoint for access points
// is not part of this story; future work can add ?Action=PutAccessPointPolicy.
func setAccessPointPolicy(t *testing.T, h *testHarness, name, doc string) {
	t.Helper()
	ctx := context.Background()
	ap, err := h.meta.GetAccessPoint(ctx, name)
	if err != nil {
		t.Fatalf("get ap: %v", err)
	}
	ap.Policy = []byte(doc)
	if err := h.meta.DeleteAccessPoint(ctx, name); err != nil {
		t.Fatalf("delete ap: %v", err)
	}
	if err := h.meta.CreateAccessPoint(ctx, ap); err != nil {
		t.Fatalf("recreate ap with policy: %v", err)
	}
}

const apPolicyAllowAlice = `{
	"Statement":[
		{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}
	]
}`

const apPolicyDenyAlice = `{
	"Statement":[
		{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"},
		{"Effect":"Deny","Principal":{"AWS":"alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/secret"}
	]
}`

const bucketPolicyAllowAlice = `{
	"Statement":[
		{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}
	]
}`

const bucketPolicyDenyAlice = `{
	"Statement":[
		{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"},
		{"Effect":"Deny","Principal":{"AWS":"alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/secret"}
	]
}`

// seedACLPassthrough sets bucket owner to the iam root and grants alice ACL
// READ via a persisted bucket grant so the policy gate is the only deciding
// authority on later GETs (otherwise the ACL gate would 403 alice).
func seedACLPassthrough(t *testing.T, h *testHarness) {
	t.Helper()
	ctx := context.Background()
	b, err := h.meta.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	if err := h.meta.SetBucketGrants(ctx, b.ID, []meta.Grant{{
		GranteeType: "Group", URI: "http://acs.amazonaws.com/groups/global/AllUsers", Permission: "FULL_CONTROL",
	}}); err != nil {
		t.Fatalf("seed grants: %v", err)
	}
}

func TestAccessPointRouting_APDenyOverridesBucketAllow(t *testing.T) {
	h := newHarness(t)
	owner := s3api.IAMRootPrincipal
	alias := createAccessPointForRouting(t, h, owner, "ap-deny", "bkt", "")
	h.mustStatus(h.doString(http.MethodPut, "/bkt/secret", "x", testPrincipalHeader, owner), http.StatusOK)
	seedACLPassthrough(t, h)
	h.mustStatus(h.doString(http.MethodPut, "/bkt?policy", bucketPolicyAllowAlice, testPrincipalHeader, owner), http.StatusNoContent)
	setAccessPointPolicy(t, h, "ap-deny", apPolicyDenyAlice)

	// alice GET via AP — bucket Allow but AP explicit Deny on /secret → 403.
	resp := h.doAP(http.MethodGet, "/secret", "", apHostFor(alias), "alice")
	h.mustStatus(resp, http.StatusForbidden)
}

func TestAccessPointRouting_BucketDenyOverridesAPAllow(t *testing.T) {
	h := newHarness(t)
	owner := s3api.IAMRootPrincipal
	alias := createAccessPointForRouting(t, h, owner, "ap-bdeny", "bkt", "")
	h.mustStatus(h.doString(http.MethodPut, "/bkt/secret", "x", testPrincipalHeader, owner), http.StatusOK)
	seedACLPassthrough(t, h)
	h.mustStatus(h.doString(http.MethodPut, "/bkt?policy", bucketPolicyDenyAlice, testPrincipalHeader, owner), http.StatusNoContent)
	setAccessPointPolicy(t, h, "ap-bdeny", apPolicyAllowAlice)

	resp := h.doAP(http.MethodGet, "/secret", "", apHostFor(alias), "alice")
	h.mustStatus(resp, http.StatusForbidden)
}

func TestAccessPointRouting_BothAllow(t *testing.T) {
	h := newHarness(t)
	owner := s3api.IAMRootPrincipal
	alias := createAccessPointForRouting(t, h, owner, "ap-both", "bkt", "")
	h.mustStatus(h.doString(http.MethodPut, "/bkt/k", "ok", testPrincipalHeader, owner), http.StatusOK)
	seedACLPassthrough(t, h)
	h.mustStatus(h.doString(http.MethodPut, "/bkt?policy", bucketPolicyAllowAlice, testPrincipalHeader, owner), http.StatusNoContent)
	setAccessPointPolicy(t, h, "ap-both", apPolicyAllowAlice)

	resp := h.doAP(http.MethodGet, "/k", "", apHostFor(alias), "alice")
	h.mustStatus(resp, http.StatusOK)
	_ = resp.Body.Close()
}

func TestAccessPointRouting_VPCOriginMismatch(t *testing.T) {
	h := newHarness(t)
	owner := s3api.IAMRootPrincipal
	alias := createAccessPointForRouting(t, h, owner, "ap-vpc", "bkt", "vpc-abc123")
	h.mustStatus(h.doString(http.MethodPut, "/bkt/k", "v", testPrincipalHeader, owner), http.StatusOK)

	// No VPC header → 403.
	resp := h.doAP(http.MethodGet, "/k", "", apHostFor(alias), owner)
	h.mustStatus(resp, http.StatusForbidden)

	// Wrong VPC header → 403.
	resp = h.doAP(http.MethodGet, "/k", "", apHostFor(alias), owner, "X-Strata-VPC-ID", "vpc-other")
	h.mustStatus(resp, http.StatusForbidden)

	// Matching VPC header → 200.
	resp = h.doAP(http.MethodGet, "/k", "", apHostFor(alias), owner, "X-Strata-VPC-ID", "vpc-abc123")
	h.mustStatus(resp, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "v" {
		t.Fatalf("vpc-allowed body: %q", string(body))
	}
}

func TestExtractAccessPointAlias(t *testing.T) {
	cases := []struct {
		host  string
		want  string
		ok    bool
	}{
		{"ap-abc.s3-accesspoint.us-east-1.example.com", "ap-abc", true},
		{"ap-abc.s3-accesspoint.us-east-1.example.com:9000", "ap-abc", true},
		{"AP-ABC.S3-AccessPoint.US-EAST-1.Example.Com", "ap-abc", true},
		{"bkt.s3.local", "", false},
		{"foo.s3-accesspoint.region", "", false}, // too few labels
		{"", "", false},
	}
	for _, c := range cases {
		alias, ok := s3api.ExtractAccessPointAliasForTest(c.host)
		if alias != c.want || ok != c.ok {
			t.Errorf("host=%q: got (%q,%v) want (%q,%v)", c.host, alias, ok, c.want, c.ok)
		}
	}
}

