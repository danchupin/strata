package s3api_test

import (
	"net/http"
	"testing"
)

// US-005: PublicAccessBlock enforcement matrix.
//
// The auth-mode × {anonymous,signed} × {ACL,policy} cells are already proven
// in auth_mode_test.go / policy_gate_test.go / acl_gate_test.go. This file
// closes the under-exercised dimension: a PublicAccessBlock must OVERRIDE an
// otherwise-permissive bucket policy or ACL for anonymous callers — block
// wins. Before US-005 the PAB config was pure CRUD and never consulted by the
// data-plane access gates, so a block silently failed open.
//
// newHarness requests carry no Authorization header, so auth.FromContext
// resolves to the anonymous identity (Owner="anonymous", IsAnonymous=true).
// Every bucket here is owned by a DISTINCT principal so the anonymous caller is
// never the owner — the requireACL owner-match never masks the assertion. That
// matters under the US-008 policy-UNION-ACL gate: with an anon-owned bucket a
// RestrictPublicBuckets suppression would fall through to the ACL gate and
// owner-match the anon caller, hiding the block. A real non-owner anon keeps
// the suppression visible.

const (
	pabPolicyAllowAnonGet = `{
		"Statement":[
			{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}
		]
	}`

	pabRestrictPublicBuckets = `<PublicAccessBlockConfiguration>` +
		`<RestrictPublicBuckets>true</RestrictPublicBuckets>` +
		`</PublicAccessBlockConfiguration>`

	pabIgnorePublicAcls = `<PublicAccessBlockConfiguration>` +
		`<IgnorePublicAcls>true</IgnorePublicAcls>` +
		`</PublicAccessBlockConfiguration>`

	// BlockPublicAcls is a PUT-time guard only — it must NOT affect read
	// evaluation, so a config carrying only this flag is the negative control
	// proving that mere PAB presence does not deny.
	pabBlockPublicAclsOnly = `<PublicAccessBlockConfiguration>` +
		`<BlockPublicAcls>true</BlockPublicAcls>` +
		`</PublicAccessBlockConfiguration>`
)

// RestrictPublicBuckets suppresses a permissive bucket policy for anonymous
// callers — block wins over the policy Allow.
func TestPAB_RestrictPublicBuckets_OverridesAnonPolicy(t *testing.T) {
	h := newHarness(t)
	const owner = "owner-restrict"
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, owner), http.StatusOK)
	h.mustStatus(h.doString("PUT", "/bkt/k", "hi", testPrincipalHeader, owner), http.StatusOK)
	h.mustStatus(h.doString("PUT", "/bkt?policy", pabPolicyAllowAnonGet, testPrincipalHeader, owner), http.StatusNoContent)

	// Baseline: public policy grants anonymous GET.
	h.mustStatus(h.doString("GET", "/bkt/k", ""), http.StatusOK)

	// Block wins: RestrictPublicBuckets denies the anonymous policy grant.
	h.mustStatus(h.doString("PUT", "/bkt?publicAccessBlock", pabRestrictPublicBuckets), http.StatusOK)
	h.mustStatus(h.doString("GET", "/bkt/k", ""), http.StatusForbidden)

	// Deleting the block restores public access — the gate is dynamic.
	h.mustStatus(h.doString("DELETE", "/bkt?publicAccessBlock", ""), http.StatusNoContent)
	h.mustStatus(h.doString("GET", "/bkt/k", ""), http.StatusOK)
}

// IgnorePublicAcls suppresses a public-read canned ACL for anonymous callers.
// The bucket is owned by a distinct principal so the anonymous caller is not
// the owner — the ACL gate is the real decision point.
func TestPAB_IgnorePublicAcls_OverridesPublicReadACL(t *testing.T) {
	h := newHarness(t)
	const owner = "owner-pab"
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, owner), http.StatusOK)
	h.mustStatus(h.doString("PUT", "/bkt/k", "hi", testPrincipalHeader, owner), http.StatusOK)
	h.mustStatus(h.doString("PUT", "/bkt?acl", "", "x-amz-acl", "public-read"), http.StatusOK)

	// Baseline: public-read ACL grants anonymous GET (caller != owner).
	h.mustStatus(h.doString("GET", "/bkt/k", ""), http.StatusOK)

	// Block wins: IgnorePublicAcls disregards the public ACL grant.
	h.mustStatus(h.doString("PUT", "/bkt?publicAccessBlock", pabIgnorePublicAcls), http.StatusOK)
	h.mustStatus(h.doString("GET", "/bkt/k", ""), http.StatusForbidden)

	// The owner is unaffected by the block — PAB restricts only public access.
	h.mustStatus(h.doString("GET", "/bkt/k", "", testPrincipalHeader, owner), http.StatusOK)

	// Deleting the block restores anonymous read.
	h.mustStatus(h.doString("DELETE", "/bkt?publicAccessBlock", ""), http.StatusNoContent)
	h.mustStatus(h.doString("GET", "/bkt/k", ""), http.StatusOK)
}

// Negative control: a PAB carrying only PUT-time flags (BlockPublicAcls) must
// NOT alter read evaluation. Mere PAB presence does not deny — the wrong knob
// must stay inert, or the gate would over-block.
func TestPAB_NonEvalFlagDoesNotBlock(t *testing.T) {
	h := newHarness(t)
	const owner = "owner-pab2"
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, owner), http.StatusOK)
	h.mustStatus(h.doString("PUT", "/bkt/k", "hi", testPrincipalHeader, owner), http.StatusOK)
	h.mustStatus(h.doString("PUT", "/bkt?acl", "", "x-amz-acl", "public-read"), http.StatusOK)
	h.mustStatus(h.doString("PUT", "/bkt?publicAccessBlock", pabBlockPublicAclsOnly), http.StatusOK)

	// IgnorePublicAcls is false → public-read ACL still grants anonymous GET.
	h.mustStatus(h.doString("GET", "/bkt/k", ""), http.StatusOK)
}

// Table form of the headline matrix: anonymous GET against a bucket made
// public by either a policy grant or a public-read ACL, with PAB on/off,
// asserting allow/deny per cell. Owner is distinct from the anonymous caller
// in every cell so neither grant path is masked by the owner short-circuit.
func TestPAB_AnonGetEnforcementMatrix(t *testing.T) {
	const owner = "matrix-owner"
	cases := []struct {
		name     string
		grant    string // "policy" | "acl"
		pab      string // "" means no PAB configured
		wantCode int
	}{
		{"policy/no-pab", "policy", "", http.StatusOK},                 // union gate (US-008): policy Allow grants despite private ACL
		{"policy/restrict-public", "policy", pabRestrictPublicBuckets, http.StatusForbidden},
		{"acl/no-pab", "acl", "", http.StatusOK},
		{"acl/ignore-public-acls", "acl", pabIgnorePublicAcls, http.StatusForbidden},
		{"acl/restrict-public-only", "acl", pabRestrictPublicBuckets, http.StatusOK}, // wrong knob for ACL → inert
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, owner), http.StatusOK)
			h.mustStatus(h.doString("PUT", "/bkt/k", "hi", testPrincipalHeader, owner), http.StatusOK)
			switch tc.grant {
			case "policy":
				h.mustStatus(h.doString("PUT", "/bkt?policy", pabPolicyAllowAnonGet), http.StatusNoContent)
			case "acl":
				h.mustStatus(h.doString("PUT", "/bkt?acl", "", "x-amz-acl", "public-read"), http.StatusOK)
			}
			if tc.pab != "" {
				h.mustStatus(h.doString("PUT", "/bkt?publicAccessBlock", tc.pab), http.StatusOK)
			}
			h.mustStatus(h.doString("GET", "/bkt/k", ""), tc.wantCode)
		})
	}
}
