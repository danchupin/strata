package s3api_test

import (
	"strings"
	"testing"
)

func TestListBuckets_FiltersByAuthenticatedOwner(t *testing.T) {
	h := newHarness(t)

	h.mustStatus(h.doString("PUT", "/alice-one", "", "X-Test-Principal", "alice"), 200)
	h.mustStatus(h.doString("PUT", "/alice-two", "", "X-Test-Principal", "alice"), 200)
	h.mustStatus(h.doString("PUT", "/bob-one", "", "X-Test-Principal", "bob"), 200)

	resp := h.doString("GET", "/", "", "X-Test-Principal", "alice")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<Name>alice-one</Name>") || !strings.Contains(body, "<Name>alice-two</Name>") {
		t.Errorf("alice missing her buckets: %s", body)
	}
	if strings.Contains(body, "<Name>bob-one</Name>") {
		t.Errorf("alice saw bob's bucket: %s", body)
	}
	if !strings.Contains(body, "<ID>alice</ID>") || !strings.Contains(body, "<DisplayName>alice</DisplayName>") {
		t.Errorf("response Owner not alice: %s", body)
	}

	resp = h.doString("GET", "/", "", "X-Test-Principal", "bob")
	h.mustStatus(resp, 200)
	body = h.readBody(resp)
	if !strings.Contains(body, "<Name>bob-one</Name>") {
		t.Errorf("bob missing his bucket: %s", body)
	}
	if strings.Contains(body, "<Name>alice-one</Name>") || strings.Contains(body, "<Name>alice-two</Name>") {
		t.Errorf("bob saw alice's buckets: %s", body)
	}
	if !strings.Contains(body, "<ID>bob</ID>") {
		t.Errorf("response Owner not bob: %s", body)
	}
}

func TestListBuckets_AnonymousReturnsEmpty(t *testing.T) {
	h := newHarness(t)

	h.mustStatus(h.doString("PUT", "/alice-one", "", "X-Test-Principal", "alice"), 200)

	resp := h.doString("GET", "/", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if strings.Contains(body, "<Name>alice-one</Name>") {
		t.Errorf("anonymous saw alice's bucket: %s", body)
	}
	if strings.Contains(body, "<Bucket>") {
		t.Errorf("anonymous list non-empty: %s", body)
	}
}

func TestListBuckets_NoBucketsForPrincipal(t *testing.T) {
	h := newHarness(t)

	h.mustStatus(h.doString("PUT", "/alice-one", "", "X-Test-Principal", "alice"), 200)

	resp := h.doString("GET", "/", "", "X-Test-Principal", "carol")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if strings.Contains(body, "<Name>alice-one</Name>") {
		t.Errorf("carol saw alice's bucket: %s", body)
	}
	if strings.Contains(body, "<Bucket>") {
		t.Errorf("carol list non-empty: %s", body)
	}
	if !strings.Contains(body, "<ID>carol</ID>") {
		t.Errorf("response Owner not carol: %s", body)
	}
}
