package s3api_test

import (
	"strings"
	"testing"
)

const aclBodyFullCanonical = `<AccessControlPolicy xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<Owner>
		<ID>strata</ID>
		<DisplayName>strata</DisplayName>
	</Owner>
	<AccessControlList>
		<Grant>
			<Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser">
				<ID>alice</ID>
				<DisplayName>alice</DisplayName>
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

const aclBodyMalformed = `<AccessControlPolicy>not-xml`

const aclBodyUnknownType = `<AccessControlPolicy xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<Owner><ID>o</ID><DisplayName>o</DisplayName></Owner>
	<AccessControlList>
		<Grant>
			<Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Bogus">
				<ID>x</ID>
			</Grantee>
			<Permission>READ</Permission>
		</Grant>
	</AccessControlList>
</AccessControlPolicy>`

const aclBodyUnknownPerm = `<AccessControlPolicy xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<Owner><ID>o</ID><DisplayName>o</DisplayName></Owner>
	<AccessControlList>
		<Grant>
			<Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser">
				<ID>x</ID>
			</Grantee>
			<Permission>SUDO</Permission>
		</Grant>
	</AccessControlList>
</AccessControlPolicy>`

func TestPutBucketACLBodyRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?acl=", aclBodyFullCanonical), 200)

	resp := h.doString("GET", "/bkt?acl=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<ID>alice</ID>") {
		t.Fatalf("alice grant missing: %s", body)
	}
	if !strings.Contains(body, "AllUsers") {
		t.Fatalf("group grant missing: %s", body)
	}
	if !strings.Contains(body, "<Permission>FULL_CONTROL</Permission>") {
		t.Fatalf("FULL_CONTROL missing: %s", body)
	}
}

func TestPutBucketACLMalformedXML(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("PUT", "/bkt?acl=", aclBodyMalformed)
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "MalformedACLError") {
		t.Fatalf("expected MalformedACLError: %s", body)
	}
}

func TestPutBucketACLUnknownGranteeType(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("PUT", "/bkt?acl=", aclBodyUnknownType)
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "MalformedACLError") {
		t.Fatalf("expected MalformedACLError: %s", body)
	}
}

func TestPutBucketACLUnknownPermission(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("PUT", "/bkt?acl=", aclBodyUnknownPerm)
	h.mustStatus(resp, 400)
}

func TestPutBucketACLMixedHeaderAndBody(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?acl=", aclBodyFullCanonical, "x-amz-acl", "public-read"), 200)

	// Persisted grants from the body win on Get.
	resp := h.doString("GET", "/bkt?acl=", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "<ID>alice</ID>") {
		t.Fatalf("body grants should be returned: %s", body)
	}
}

func TestGetBucketACLFallsBackToCanned(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", "x-amz-acl", "public-read"), 200)

	resp := h.doString("GET", "/bkt?acl=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "AllUsers") {
		t.Fatalf("public-read should expand to AllUsers READ: %s", body)
	}
}

func TestPutObjectACLBodyRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/key.txt", "hello"), 200)

	h.mustStatus(h.doString("PUT", "/bkt/key.txt?acl=", aclBodyFullCanonical), 200)

	resp := h.doString("GET", "/bkt/key.txt?acl=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<ID>alice</ID>") {
		t.Fatalf("object grants missing: %s", body)
	}
}

func TestPutObjectACLMalformed(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/key.txt", "hello"), 200)
	resp := h.doString("PUT", "/bkt/key.txt?acl=", aclBodyMalformed)
	h.mustStatus(resp, 400)
}
