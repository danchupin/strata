package s3api_test

import (
	"strings"
	"testing"
)

func TestBucketObjectLockConfigRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", "x-amz-bucket-object-lock-enabled", "true"), 200)

	h.mustStatus(h.doString("GET", "/bkt?object-lock", ""), 404)

	cfg := `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?object-lock", cfg), 200)

	resp := h.doString("GET", "/bkt?object-lock", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<Mode>COMPLIANCE</Mode>") || !strings.Contains(body, "<Days>1</Days>") {
		t.Fatalf("object-lock config GET missing fields: %s", body)
	}
}

func TestBucketObjectLockRequiresEnabled(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	cfg := `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`
	resp := h.doString("PUT", "/bkt?object-lock", cfg)
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidRequest") {
		t.Fatalf("expected InvalidRequest, got: %s", body)
	}
}

func TestBucketObjectLockMalformedXML(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", "x-amz-bucket-object-lock-enabled", "true"), 200)

	resp := h.doString("PUT", "/bkt?object-lock", "<ObjectLockConfiguration><Rule><DefaultRetention><Mode>BOGUS</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>")
	h.mustStatus(resp, 400)
}

func TestPutObjectInheritsDefaultRetention(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", "x-amz-bucket-object-lock-enabled", "true"), 200)

	cfg := `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>30</Days></DefaultRetention></Rule></ObjectLockConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?object-lock", cfg), 200)

	h.mustStatus(h.doString("PUT", "/bkt/k", "x"), 200)

	resp := h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if mode := resp.Header.Get("X-Amz-Object-Lock-Mode"); mode != "COMPLIANCE" {
		t.Fatalf("inherited mode: got %q want COMPLIANCE", mode)
	}
	if until := resp.Header.Get("X-Amz-Object-Lock-Retain-Until-Date"); until == "" {
		t.Fatalf("inherited retain-until-date missing")
	}
}

func TestPutObjectInheritedComplianceBlocksDelete(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", "x-amz-bucket-object-lock-enabled", "true"), 200)

	cfg := `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>30</Days></DefaultRetention></Rule></ObjectLockConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?object-lock", cfg), 200)

	h.mustStatus(h.doString("PUT", "/bkt/k", "x"), 200)

	resp := h.doString("DELETE", "/bkt/k", "")
	h.mustStatus(resp, 403)
	if body := h.readBody(resp); !strings.Contains(body, "AccessDenied") {
		t.Fatalf("expected AccessDenied, got: %s", body)
	}
}

func TestPutObjectExplicitHeaderOverridesDefault(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", "x-amz-bucket-object-lock-enabled", "true"), 200)

	cfg := `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>30</Days></DefaultRetention></Rule></ObjectLockConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?object-lock", cfg), 200)

	until := "2099-01-01T00:00:00Z"
	h.mustStatus(h.doString("PUT", "/bkt/k", "x",
		"x-amz-object-lock-mode", "GOVERNANCE",
		"x-amz-object-lock-retain-until-date", until,
	), 200)

	resp := h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if mode := resp.Header.Get("X-Amz-Object-Lock-Mode"); mode != "GOVERNANCE" {
		t.Fatalf("explicit mode: got %q want GOVERNANCE", mode)
	}
}
