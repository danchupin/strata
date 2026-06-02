package s3api_test

import (
	"net/http"
	"testing"
)

// US-008 (R6): object access is policy-UNION-ACL, matching AWS — an explicit
// bucket-policy Allow grants regardless of the ACL gate, an explicit Deny wins,
// and a neutral policy falls back to the ACL gate.
//
// The bucket is owned by a distinct principal (policyOwner) and every GET is
// issued by a GENUINE non-owner: anonymous (no X-Test-Principal) or a different
// signed principal. That stops the requireACL owner-match short-circuit from
// masking the union — under the old intersection gate an anon GET allowed by
// policy but denied by the private ACL returned 403; under the union it is 200.

const policyOwner = "policy-owner"

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

// ownerHeader is the X-Test-Principal pair that owns the policy-gate bucket.
func ownerHeader() []string { return []string{testPrincipalHeader, policyOwner} }

// seedPolicyBucket creates a private bucket + object owned by policyOwner.
func seedPolicyBucket(t *testing.T, h *testHarness, key string) {
	t.Helper()
	h.mustStatus(h.doString("PUT", "/bkt", "", ownerHeader()...), 200)
	h.mustStatus(h.doString("PUT", "/bkt/"+key, "hi", ownerHeader()...), 200)
}

// Owner full control: the bucket owner reads its own private object with no
// policy configured — the ACL owner-match grants.
func TestPolicyGate_OwnerFullControlNoPolicy(t *testing.T) {
	h := newHarness(t)
	seedPolicyBucket(t, h, "k")
	h.mustStatus(h.doString("GET", "/bkt/k", "", ownerHeader()...), 200)
}

// No policy + private ACL + genuine non-owner → 403. (Was masked before by the
// anon-owns-anon-bucket coincidence; now the caller is a real non-owner.)
func TestPolicyGate_NonOwnerNoPolicyDenied(t *testing.T) {
	h := newHarness(t)
	seedPolicyBucket(t, h, "k")
	h.mustStatus(h.doString("GET", "/bkt/k", ""), 403)
}

// Headline R6 union case: an anonymous non-owner GET allowed by the bucket
// policy but denied by the private ACL now returns 200 (was 403 under the
// intersection gate). Explicit policy Allow grants regardless of the ACL.
func TestPolicyGate_AnonGetAllowedByPolicyDespitePrivateACL(t *testing.T) {
	h := newHarness(t)
	seedPolicyBucket(t, h, "k")
	h.mustStatus(h.doString("PUT", "/bkt?policy", policyAllowAnonGet, ownerHeader()...), http.StatusNoContent)
	h.mustStatus(h.doString("GET", "/bkt/k", ""), 200)
}

// Neutral policy (grants only DeleteObject) + private ACL + non-owner → 403.
// A no-match policy falls back to the ACL gate, which denies the non-owner.
func TestPolicyGate_AnonGetDeniedWithoutMatchingAllow(t *testing.T) {
	h := newHarness(t)
	seedPolicyBucket(t, h, "k")
	h.mustStatus(h.doString("PUT", "/bkt?policy", policyDeleteOnly, ownerHeader()...), http.StatusNoContent)
	h.mustStatus(h.doString("GET", "/bkt/k", ""), 403)
}

// Explicit Deny wins over both the policy Allow and the owner full-control ACL.
// Even the bucket owner is refused on the denied key (AWS precedence:
// explicit deny > allow).
func TestPolicyGate_DenyOverridesAllowAndOwner(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", ownerHeader()...), 200)
	h.mustStatus(h.doString("PUT", "/bkt/secret", "hi", ownerHeader()...), 200)
	h.mustStatus(h.doString("PUT", "/bkt/public", "hi", ownerHeader()...), 200)
	h.mustStatus(h.doString("PUT", "/bkt?policy", policyAllowAnonGetWithDeny, ownerHeader()...), http.StatusNoContent)

	// /public: anon allowed by policy → 200.
	h.mustStatus(h.doString("GET", "/bkt/public", ""), 200)
	// /secret: explicit Deny → anon refused.
	h.mustStatus(h.doString("GET", "/bkt/secret", ""), 403)
	// /secret: explicit Deny refuses the owner too — deny beats owner ACL.
	h.mustStatus(h.doString("GET", "/bkt/secret", "", ownerHeader()...), 403)
}

// Policy scoped to alice does not grant an anonymous "*" principal; the private
// ACL then denies the non-owner anon → 403.
func TestPolicyGate_WrongPrincipalDenied(t *testing.T) {
	h := newHarness(t)
	seedPolicyBucket(t, h, "k")
	h.mustStatus(h.doString("PUT", "/bkt?policy", policyAllowAliceOnly, ownerHeader()...), http.StatusNoContent)
	h.mustStatus(h.doString("GET", "/bkt/k", ""), 403)
}

func TestPolicyGate_DeniedDoesNotLeakBody(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", ownerHeader()...), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "secret-body", ownerHeader()...), 200)
	h.mustStatus(h.doString("PUT", "/bkt?policy", policyAllowAliceOnly, ownerHeader()...), http.StatusNoContent)
	resp := h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 403)
	body := h.readBody(resp)
	if body == "secret-body" {
		t.Fatalf("denied response leaked object body")
	}
}
