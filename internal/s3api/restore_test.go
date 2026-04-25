package s3api_test

import (
	"strings"
	"testing"
)

func TestRestoreObjectRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "payload"), 200)

	body := `<RestoreRequest><Days>3</Days><Tier>Standard</Tier></RestoreRequest>`
	resp := h.doString("POST", "/bkt/k?restore", body)
	h.mustStatus(resp, 200)
	_ = h.readBody(resp)

	head := h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(head, 200)
	_ = h.readBody(head)

	got := head.Header.Get("x-amz-restore")
	if got == "" {
		t.Fatalf("expected x-amz-restore header on HEAD")
	}
	if !strings.Contains(got, `ongoing-request="false"`) {
		t.Fatalf("missing ongoing-request: %s", got)
	}
	if !strings.Contains(got, `expiry-date=`) {
		t.Fatalf("missing expiry-date: %s", got)
	}
}

func TestRestoreObjectMissingKey(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/missing?restore", `<RestoreRequest><Days>1</Days></RestoreRequest>`)
	h.mustStatus(resp, 404)
	body := h.readBody(resp)
	if !strings.Contains(body, "NoSuchKey") {
		t.Fatalf("expected NoSuchKey in body: %s", body)
	}
}

func TestRestoreObjectMalformedXML(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "x"), 200)

	resp := h.doString("POST", "/bkt/k?restore", `<RestoreRequest><Days>not a number`)
	h.mustStatus(resp, 400)
	body := h.readBody(resp)
	if !strings.Contains(body, "MalformedXML") {
		t.Fatalf("expected MalformedXML: %s", body)
	}
}

func TestRestoreObjectEmptyBodyDefaultsDays(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "x"), 200)

	resp := h.doString("POST", "/bkt/k?restore", "")
	h.mustStatus(resp, 200)
	_ = h.readBody(resp)

	head := h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(head, 200)
	_ = h.readBody(head)
	if got := head.Header.Get("x-amz-restore"); got == "" {
		t.Fatalf("expected x-amz-restore header on HEAD with default days")
	}
}
