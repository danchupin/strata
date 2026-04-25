package s3api_test

import (
	"strings"
	"testing"
)

func TestBucketLifecycleCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	h.mustStatus(h.doString("GET", "/bkt?lifecycle", ""), 404)

	rules := `<LifecycleConfiguration><Rule><ID>r1</ID><Status>Enabled</Status><Filter><Prefix>logs/</Prefix></Filter><Transition><Days>30</Days><StorageClass>STANDARD_IA</StorageClass></Transition></Rule></LifecycleConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?lifecycle", rules), 200)

	resp := h.doString("GET", "/bkt?lifecycle", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "<ID>r1</ID>") {
		t.Errorf("lifecycle GET missing rule: %s", body)
	}

	h.mustStatus(h.doString("DELETE", "/bkt?lifecycle", ""), 204)
	h.mustStatus(h.doString("GET", "/bkt?lifecycle", ""), 404)
}

func TestObjectTagging(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "x"), 200)

	tags := `<Tagging><TagSet><Tag><Key>env</Key><Value>prod</Value></Tag><Tag><Key>owner</Key><Value>team-x</Value></Tag></TagSet></Tagging>`
	h.mustStatus(h.doString("PUT", "/bkt/k?tagging", tags), 200)

	resp := h.doString("GET", "/bkt/k?tagging", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<Key>env</Key>") || !strings.Contains(body, "<Value>prod</Value>") {
		t.Errorf("tags not returned: %s", body)
	}

	resp = h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if cnt := resp.Header.Get("X-Amz-Tagging-Count"); cnt != "2" {
		t.Errorf("tagging-count: got %q want 2", cnt)
	}

	h.mustStatus(h.doString("DELETE", "/bkt/k?tagging", ""), 204)
	resp = h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if cnt := resp.Header.Get("X-Amz-Tagging-Count"); cnt != "" {
		t.Errorf("tagging-count after delete: got %q want empty", cnt)
	}
}

func TestObjectLockBlocksDelete(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "x"), 200)

	future := "2099-12-31T00:00:00Z"
	retention := `<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>` + future + `</RetainUntilDate></Retention>`
	h.mustStatus(h.doString("PUT", "/bkt/k?retention", retention), 200)

	resp := h.doString("DELETE", "/bkt/k", "")
	h.mustStatus(resp, 403)
	if body := h.readBody(resp); !strings.Contains(body, "AccessDenied") {
		t.Errorf("expected AccessDenied, got: %s", body)
	}

	h.mustStatus(h.doString("PUT", "/bkt/k?legal-hold", `<LegalHold><Status>ON</Status></LegalHold>`), 200)
	past := `<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>2000-01-01T00:00:00Z</RetainUntilDate></Retention>`
	h.mustStatus(h.doString("PUT", "/bkt/k?retention", past), 200)

	resp = h.doString("DELETE", "/bkt/k", "", "x-amz-bypass-governance-retention", "true")
	h.mustStatus(resp, 403)

	h.mustStatus(h.doString("PUT", "/bkt/k?legal-hold", `<LegalHold><Status>OFF</Status></LegalHold>`), 200)
	h.mustStatus(h.doString("DELETE", "/bkt/k", "", "x-amz-bypass-governance-retention", "true"), 204)
}
