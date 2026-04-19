package s3api_test

import (
	"regexp"
	"strings"
	"testing"
)

var versionIDRE = regexp.MustCompile(`<VersionId>([^<]+)</VersionId>`)

func enableVersioning(h *testHarness, bucket string) {
	h.mustStatus(h.doString("PUT", "/"+bucket+"?versioning",
		"<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>"), 200)
}

func putObjectReturnVersion(t *testing.T, h *testHarness, path, body string) string {
	t.Helper()
	resp := h.doString("PUT", path, body)
	h.mustStatus(resp, 200)
	return resp.Header.Get("X-Amz-Version-Id")
}

func TestVersioningEnableAndReadVersion(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/b", ""), 200)
	enableVersioning(h, "b")

	v1 := putObjectReturnVersion(t, h, "/b/doc", "v1")
	v2 := putObjectReturnVersion(t, h, "/b/doc", "v2")
	v3 := putObjectReturnVersion(t, h, "/b/doc", "v3")
	if v1 == "" || v2 == "" || v3 == "" || v1 == v2 || v2 == v3 {
		t.Fatalf("version ids not distinct: %q %q %q", v1, v2, v3)
	}

	resp := h.doString("GET", "/b/doc", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "v3" {
		t.Errorf("latest: got %q want v3", body)
	}

	resp = h.doString("GET", "/b/doc?versionId="+v1, "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "v1" {
		t.Errorf("versionId=v1: got %q want v1", body)
	}
}

func TestVersioningDeleteMarker(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/b", ""), 200)
	enableVersioning(h, "b")
	_ = putObjectReturnVersion(t, h, "/b/doc", "v1")
	v2 := putObjectReturnVersion(t, h, "/b/doc", "v2")

	resp := h.doString("DELETE", "/b/doc", "")
	h.mustStatus(resp, 204)
	if resp.Header.Get("X-Amz-Delete-Marker") != "true" {
		t.Errorf("expected X-Amz-Delete-Marker: true")
	}

	h.mustStatus(h.doString("GET", "/b/doc", ""), 404)

	resp = h.doString("GET", "/b/doc?versionId="+v2, "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "v2" {
		t.Errorf("old version body: got %q want v2", body)
	}
}

func TestListObjectVersions(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/b", ""), 200)
	enableVersioning(h, "b")
	putObjectReturnVersion(t, h, "/b/x", "v1")
	putObjectReturnVersion(t, h, "/b/x", "v2")

	resp := h.doString("GET", "/b?versions", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	matches := versionIDRE.FindAllStringSubmatch(body, -1)
	if len(matches) != 2 {
		t.Fatalf("expected 2 versions, got %d: %s", len(matches), body)
	}
	if !strings.Contains(body, "<IsLatest>true</IsLatest>") {
		t.Errorf("no IsLatest=true marker: %s", body)
	}
}
