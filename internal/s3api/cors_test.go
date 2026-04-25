package s3api_test

import (
	"strings"
	"testing"
)

const corsXML = `<CORSConfiguration>
	<CORSRule>
		<AllowedOrigin>https://example.com</AllowedOrigin>
		<AllowedMethod>GET</AllowedMethod>
		<AllowedMethod>PUT</AllowedMethod>
		<AllowedHeader>x-amz-*</AllowedHeader>
		<ExposeHeader>ETag</ExposeHeader>
		<MaxAgeSeconds>3000</MaxAgeSeconds>
	</CORSRule>
</CORSConfiguration>`

func TestCORSConfigCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	// Not configured → 404 NoSuchCORSConfiguration.
	resp := h.doString("GET", "/bkt?cors=", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "NoSuchCORSConfiguration") {
		t.Fatalf("expected NoSuchCORSConfiguration, got: %s", body)
	}

	h.mustStatus(h.doString("PUT", "/bkt?cors=", corsXML), 200)

	resp = h.doString("GET", "/bkt?cors=", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "https://example.com") {
		t.Fatalf("GET cors body missing origin: %s", body)
	}

	h.mustStatus(h.doString("DELETE", "/bkt?cors=", ""), 204)
	h.mustStatus(h.doString("GET", "/bkt?cors=", ""), 404)
}

func TestCORSPreflightMatch(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?cors=", corsXML), 200)

	resp := h.doString("OPTIONS", "/bkt/key", "",
		"Origin", "https://example.com",
		"Access-Control-Request-Method", "GET",
		"Access-Control-Request-Headers", "x-amz-content-sha256",
	)
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Fatalf("Allow-Origin: %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "GET") {
		t.Fatalf("Allow-Methods: %q", got)
	}
	if got := resp.Header.Get("Access-Control-Max-Age"); got != "3000" {
		t.Fatalf("Max-Age: %q", got)
	}
}

func TestCORSPreflightNoMatch(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?cors=", corsXML), 200)

	// Origin mismatch.
	resp := h.doString("OPTIONS", "/bkt/key", "",
		"Origin", "https://evil.com",
		"Access-Control-Request-Method", "GET",
	)
	h.mustStatus(resp, 403)
}

func TestCORSPreflightWithoutConfig(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("OPTIONS", "/bkt/key", "",
		"Origin", "https://example.com",
		"Access-Control-Request-Method", "GET",
	)
	h.mustStatus(resp, 403)
}

func TestBucketPolicyCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("GET", "/bkt?policy=", "")
	h.mustStatus(resp, 404)

	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::b/*"}]}`
	h.mustStatus(h.doString("PUT", "/bkt?policy=", policy), 204)

	resp = h.doString("GET", "/bkt?policy=", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "2012-10-17") {
		t.Fatalf("GET policy body: %s", body)
	}

	h.mustStatus(h.doString("DELETE", "/bkt?policy=", ""), 204)
	h.mustStatus(h.doString("GET", "/bkt?policy=", ""), 404)
}

func TestBucketPolicyMalformed(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("PUT", "/bkt?policy=", "not json")
	h.mustStatus(resp, 400)
}

const pabXML = `<PublicAccessBlockConfiguration>
	<BlockPublicAcls>true</BlockPublicAcls>
	<IgnorePublicAcls>true</IgnorePublicAcls>
	<BlockPublicPolicy>false</BlockPublicPolicy>
	<RestrictPublicBuckets>false</RestrictPublicBuckets>
</PublicAccessBlockConfiguration>`

func TestPublicAccessBlockCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("GET", "/bkt?publicAccessBlock=", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "NoSuchPublicAccessBlockConfiguration") {
		t.Fatalf("body: %s", body)
	}

	h.mustStatus(h.doString("PUT", "/bkt?publicAccessBlock=", pabXML), 200)

	resp = h.doString("GET", "/bkt?publicAccessBlock=", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "BlockPublicAcls") {
		t.Fatalf("GET pab body: %s", body)
	}

	h.mustStatus(h.doString("DELETE", "/bkt?publicAccessBlock=", ""), 204)
	h.mustStatus(h.doString("GET", "/bkt?publicAccessBlock=", ""), 404)
}

const ownershipXML = `<OwnershipControls>
	<Rule>
		<ObjectOwnership>BucketOwnerEnforced</ObjectOwnership>
	</Rule>
</OwnershipControls>`

func TestOwnershipControlsCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("GET", "/bkt?ownershipControls=", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "OwnershipControlsNotFoundError") {
		t.Fatalf("body: %s", body)
	}

	h.mustStatus(h.doString("PUT", "/bkt?ownershipControls=", ownershipXML), 200)

	resp = h.doString("GET", "/bkt?ownershipControls=", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "BucketOwnerEnforced") {
		t.Fatalf("GET body: %s", body)
	}

	h.mustStatus(h.doString("DELETE", "/bkt?ownershipControls=", ""), 204)
	h.mustStatus(h.doString("GET", "/bkt?ownershipControls=", ""), 404)
}

func TestOwnershipControlsRejectsInvalid(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	bad := `<OwnershipControls><Rule><ObjectOwnership>Bogus</ObjectOwnership></Rule></OwnershipControls>`
	resp := h.doString("PUT", "/bkt?ownershipControls=", bad)
	h.mustStatus(resp, 400)
}
