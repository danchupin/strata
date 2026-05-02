package s3api_test

import (
	"net/http"
	"strings"
	"testing"
)

const (
	ownerPrincipal = "owner"
	altPrincipal   = "alt"
)

// TestACLGate_BucketCannedMatrix walks every (canned, method, principal) cell
// the AC enumerates and asserts the resulting status code.
func TestACLGate_BucketCannedMatrix(t *testing.T) {
	type cell struct {
		method string
		body   string
		ok     int
	}
	get := cell{method: "GET", ok: http.StatusOK}
	put := cell{method: "PUT", body: "x", ok: http.StatusOK}
	del := cell{method: "DELETE", ok: http.StatusNoContent}

	cases := []struct {
		canned    string
		op        cell
		principal string // "" = anonymous
		want      int
	}{
		// private — only owner.
		{"private", get, ownerPrincipal, http.StatusOK},
		{"private", put, ownerPrincipal, http.StatusOK},
		{"private", del, ownerPrincipal, http.StatusNoContent},
		{"private", get, altPrincipal, http.StatusForbidden},
		{"private", put, altPrincipal, http.StatusForbidden},
		{"private", del, altPrincipal, http.StatusForbidden},
		{"private", get, "", http.StatusForbidden},
		{"private", put, "", http.StatusForbidden},
		{"private", del, "", http.StatusForbidden},

		// public-read — anyone GET, only owner mutates.
		{"public-read", get, ownerPrincipal, http.StatusOK},
		{"public-read", get, altPrincipal, http.StatusOK},
		{"public-read", get, "", http.StatusOK},
		{"public-read", put, altPrincipal, http.StatusForbidden},
		{"public-read", put, "", http.StatusForbidden},
		{"public-read", del, altPrincipal, http.StatusForbidden},
		{"public-read", del, "", http.StatusForbidden},

		// public-read-write — anyone, anything.
		{"public-read-write", get, altPrincipal, http.StatusOK},
		{"public-read-write", put, altPrincipal, http.StatusOK},
		{"public-read-write", del, altPrincipal, http.StatusNoContent},
		{"public-read-write", get, "", http.StatusOK},
		{"public-read-write", put, "", http.StatusOK},
		{"public-read-write", del, "", http.StatusNoContent},

		// authenticated-read — non-anon GET, anon nothing, alt cannot mutate.
		{"authenticated-read", get, altPrincipal, http.StatusOK},
		{"authenticated-read", put, altPrincipal, http.StatusForbidden},
		{"authenticated-read", del, altPrincipal, http.StatusForbidden},
		{"authenticated-read", get, "", http.StatusForbidden},
		{"authenticated-read", put, "", http.StatusForbidden},
		{"authenticated-read", del, "", http.StatusForbidden},
	}

	for _, tc := range cases {
		name := tc.canned + "_" + tc.op.method + "_" + principalLabel(tc.principal)
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ownerHdr := []string{testPrincipalHeader, ownerPrincipal}

			h.mustStatus(h.doString("PUT", "/bkt", "", ownerHdr...), http.StatusOK)
			h.mustStatus(h.doString("PUT", "/bkt?acl=", "", append(ownerHdr, "x-amz-acl", tc.canned)...), http.StatusOK)
			// Seed an object owner-side so DELETE/GET have a target.
			h.mustStatus(h.doString("PUT", "/bkt/k", "seed", ownerHdr...), http.StatusOK)

			var headers []string
			if tc.principal != "" {
				headers = []string{testPrincipalHeader, tc.principal}
			}
			resp := h.doString(tc.op.method, "/bkt/k", tc.op.body, headers...)
			h.mustStatus(resp, tc.want)
		})
	}
}

func principalLabel(p string) string {
	if p == "" {
		return "anon"
	}
	return p
}

// TestACLGate_ObjectGrantsOverrideBucketReads — per-object grants take
// priority over bucket ACL on GET (per AC).
func TestACLGate_ObjectGrantsOverrideBucketReads(t *testing.T) {
	h := newHarness(t)
	ownerHdr := []string{testPrincipalHeader, ownerPrincipal}

	// Bucket private. Object grants AllUsers READ. Anon GET succeeds.
	h.mustStatus(h.doString("PUT", "/bkt", "", ownerHdr...), http.StatusOK)
	h.mustStatus(h.doString("PUT", "/bkt/pub", "hi", ownerHdr...), http.StatusOK)
	h.mustStatus(h.doString("PUT", "/bkt/pub?acl=", aclBodyAllUsersRead, ownerHdr...), http.StatusOK)
	h.mustStatus(h.doString("GET", "/bkt/pub", ""), http.StatusOK)

	// Bucket public-read. Object grants exclude AllUsers. Anon GET denied
	// because object grants override the canned bucket ACL.
	h.mustStatus(h.doString("PUT", "/bkt2", "", append(ownerHdr, "x-amz-acl", "public-read")...), http.StatusOK)
	h.mustStatus(h.doString("PUT", "/bkt2/k", "hi", ownerHdr...), http.StatusOK)
	h.mustStatus(h.doString("PUT", "/bkt2/k?acl=", aclBodyOwnerOnly, ownerHdr...), http.StatusOK)
	h.mustStatus(h.doString("GET", "/bkt2/k", ""), http.StatusForbidden)
}

// TestACLGate_PolicyDenyOverridesACL — explicit policy Deny still wins even
// when ACL would allow.
func TestACLGate_PolicyDenyOverridesACL(t *testing.T) {
	h := newHarness(t)
	ownerHdr := []string{testPrincipalHeader, ownerPrincipal}

	h.mustStatus(h.doString("PUT", "/bkt", "", append(ownerHdr, "x-amz-acl", "public-read-write")...), http.StatusOK)
	h.mustStatus(h.doString("PUT", "/bkt/k", "secret", ownerHdr...), http.StatusOK)
	denyAnon := `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/k"}]}`
	h.mustStatus(h.doString("PUT", "/bkt?policy", denyAnon, ownerHdr...), http.StatusNoContent)

	// Anon GET: ACL would allow (public-read-write) but policy Deny wins.
	h.mustStatus(h.doString("GET", "/bkt/k", ""), http.StatusForbidden)
	// Owner is not exempt from explicit Deny in the policy gate (anonymous-style
	// principal "*" matches owner too only if Principal=='*'). For Owner here:
	// principal == "owner", policy denies "*" → matches → 403.
	h.mustStatus(h.doString("GET", "/bkt/k", "", ownerHdr...), http.StatusForbidden)
}

// TestACLGate_AnonNoLeakOnDeny — denied 403 must not leak object body.
func TestACLGate_AnonNoLeakOnDeny(t *testing.T) {
	h := newHarness(t)
	ownerHdr := []string{testPrincipalHeader, ownerPrincipal}
	h.mustStatus(h.doString("PUT", "/bkt", "", ownerHdr...), http.StatusOK)
	h.mustStatus(h.doString("PUT", "/bkt/k", "top-secret", ownerHdr...), http.StatusOK)
	resp := h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, http.StatusForbidden)
	if body := h.readBody(resp); strings.Contains(body, "top-secret") {
		t.Fatalf("denied response leaked body: %s", body)
	}
}

const aclBodyAllUsersRead = `<AccessControlPolicy xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<Owner><ID>owner</ID><DisplayName>owner</DisplayName></Owner>
	<AccessControlList>
		<Grant>
			<Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser">
				<ID>owner</ID><DisplayName>owner</DisplayName>
			</Grantee>
			<Permission>FULL_CONTROL</Permission>
		</Grant>
		<Grant>
			<Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Group">
				<URI>http://acs.amazonaws.com/groups/global/AllUsers</URI>
			</Grantee>
			<Permission>READ</Permission>
		</Grant>
	</AccessControlList>
</AccessControlPolicy>`

const aclBodyOwnerOnly = `<AccessControlPolicy xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<Owner><ID>owner</ID><DisplayName>owner</DisplayName></Owner>
	<AccessControlList>
		<Grant>
			<Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser">
				<ID>owner</ID><DisplayName>owner</DisplayName>
			</Grantee>
			<Permission>FULL_CONTROL</Permission>
		</Grant>
	</AccessControlList>
</AccessControlPolicy>`
