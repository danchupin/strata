package s3api_test

import (
	"net/http"
	"testing"
)

const (
	policyAllowAnonGet = `{
		"Statement":[
			{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}
		]
	}`

	policyAllowAliceOnly = `{
		"Statement":[
			{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::1:user/alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}
		]
	}`

	policyAllowAnonGetWithDeny = `{
		"Statement":[
			{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"},
			{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/secret"}
		]
	}`

	policyDeleteOnly = `{
		"Statement":[
			{"Effect":"Allow","Principal":"*","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::bkt/*"}
		]
	}`
)

func TestPolicyGate_NoPolicyPasses(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "hi"), 200)
	h.mustStatus(h.doString("GET", "/bkt/k", ""), 200)
}

func TestPolicyGate_AnonGetAllowed(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "hi"), 200)
	h.mustStatus(h.doString("PUT", "/bkt?policy", policyAllowAnonGet), http.StatusNoContent)
	h.mustStatus(h.doString("GET", "/bkt/k", ""), 200)
}

func TestPolicyGate_AnonGetDeniedWithoutMatchingAllow(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "hi"), 200)
	// Policy only grants s3:DeleteObject — GetObject has no matching Allow.
	h.mustStatus(h.doString("PUT", "/bkt?policy", policyDeleteOnly), http.StatusNoContent)
	h.mustStatus(h.doString("GET", "/bkt/k", ""), 403)
}

func TestPolicyGate_DenyOverridesAllow(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/secret", "hi"), 200)
	h.mustStatus(h.doString("PUT", "/bkt/public", "hi"), 200)
	h.mustStatus(h.doString("PUT", "/bkt?policy", policyAllowAnonGetWithDeny), http.StatusNoContent)
	h.mustStatus(h.doString("GET", "/bkt/public", ""), 200)
	h.mustStatus(h.doString("GET", "/bkt/secret", ""), 403)
}

func TestPolicyGate_WrongPrincipalDenied(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "hi"), 200)
	h.mustStatus(h.doString("PUT", "/bkt?policy", policyAllowAliceOnly), http.StatusNoContent)
	// Anonymous principal "*" must not match an allow scoped to alice.
	h.mustStatus(h.doString("GET", "/bkt/k", ""), 403)
}

func TestPolicyGate_DeniedDoesNotLeakBody(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "secret-body"), 200)
	h.mustStatus(h.doString("PUT", "/bkt?policy", policyAllowAliceOnly), http.StatusNoContent)
	resp := h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 403)
	body := h.readBody(resp)
	if body == "secret-body" {
		t.Fatalf("denied response leaked object body")
	}
}
