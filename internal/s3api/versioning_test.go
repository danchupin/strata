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
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enableVersioning(h, "bkt")

	v1 := putObjectReturnVersion(t, h, "/bkt/doc", "v1")
	v2 := putObjectReturnVersion(t, h, "/bkt/doc", "v2")
	v3 := putObjectReturnVersion(t, h, "/bkt/doc", "v3")
	if v1 == "" || v2 == "" || v3 == "" || v1 == v2 || v2 == v3 {
		t.Fatalf("version ids not distinct: %q %q %q", v1, v2, v3)
	}

	resp := h.doString("GET", "/bkt/doc", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "v3" {
		t.Errorf("latest: got %q want v3", body)
	}

	resp = h.doString("GET", "/bkt/doc?versionId="+v1, "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "v1" {
		t.Errorf("versionId=v1: got %q want v1", body)
	}
}

func TestVersioningDeleteMarker(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enableVersioning(h, "bkt")
	_ = putObjectReturnVersion(t, h, "/bkt/doc", "v1")
	v2 := putObjectReturnVersion(t, h, "/bkt/doc", "v2")

	resp := h.doString("DELETE", "/bkt/doc", "")
	h.mustStatus(resp, 204)
	if resp.Header.Get("X-Amz-Delete-Marker") != "true" {
		t.Errorf("expected X-Amz-Delete-Marker: true")
	}

	h.mustStatus(h.doString("GET", "/bkt/doc", ""), 404)

	resp = h.doString("GET", "/bkt/doc?versionId="+v2, "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "v2" {
		t.Errorf("old version body: got %q want v2", body)
	}
}

// TestNullVersionPlainPut mirrors s3-tests
// test_versioning_obj_plain_null_version_overwrite: a PUT to an
// unversioned bucket lands as the literal-"null" version. Once the
// bucket flips to Enabled, the prior null is preserved alongside the
// new UUID-versioned write and remains addressable via ?versionId=null.
func TestNullVersionPlainPut(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	// Unversioned PUT — no x-amz-version-id header per S3.
	resp := h.doString("PUT", "/bkt/doc", "v1-disabled")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("X-Amz-Version-Id"); got != "" {
		t.Errorf("disabled PUT: should not emit X-Amz-Version-Id, got %q", got)
	}

	enableVersioning(h, "bkt")

	resp = h.doString("GET", "/bkt/doc?versionId=null", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "v1-disabled" {
		t.Errorf("?versionId=null after Enable: got %q want v1-disabled", body)
	}
	if got := resp.Header.Get("X-Amz-Version-Id"); got != "null" {
		t.Errorf("X-Amz-Version-Id: got %q want null", got)
	}

	v2 := putObjectReturnVersion(t, h, "/bkt/doc", "v2-uuid")
	if v2 == "" || v2 == "null" {
		t.Fatalf("Enabled overwrite VersionId: %q (want UUID)", v2)
	}

	resp = h.doString("GET", "/bkt/doc", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "v2-uuid" {
		t.Errorf("latest after overwrite: got %q want v2-uuid", body)
	}

	resp = h.doString("GET", "/bkt/doc?versionId=null", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "v1-disabled" {
		t.Errorf("?versionId=null preserved: got %q", body)
	}
}

// TestNullVersionSuspendOverwrite mirrors s3-tests
// test_versioning_obj_suspend_versions: PUT under Suspended versioning
// replaces the prior null version atomically while preserving any
// UUID-versioned rows from the bucket's earlier Enabled lifetime.
func TestNullVersionSuspendOverwrite(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enableVersioning(h, "bkt")
	v1 := putObjectReturnVersion(t, h, "/bkt/doc", "v1-uuid")
	if v1 == "" {
		t.Fatal("missing v1 UUID")
	}

	h.mustStatus(h.doString("PUT", "/bkt?versioning",
		"<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>"), 200)

	resp := h.doString("PUT", "/bkt/doc", "v2-suspended")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("X-Amz-Version-Id"); got != "null" {
		t.Errorf("Suspended PUT VersionId: got %q want null", got)
	}

	resp = h.doString("PUT", "/bkt/doc", "v3-suspended")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("X-Amz-Version-Id"); got != "null" {
		t.Errorf("Suspended re-PUT VersionId: got %q want null", got)
	}

	// Latest is the most recent null write.
	resp = h.doString("GET", "/bkt/doc", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "v3-suspended" {
		t.Errorf("latest: got %q want v3-suspended", body)
	}
	resp = h.doString("GET", "/bkt/doc?versionId=null", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "v3-suspended" {
		t.Errorf("?versionId=null: got %q want v3-suspended", body)
	}

	// UUID version from the bucket's Enabled lifetime is preserved.
	resp = h.doString("GET", "/bkt/doc?versionId="+v1, "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "v1-uuid" {
		t.Errorf("UUID version preserved: got %q want v1-uuid", body)
	}
}

// TestNullVersionDelete pins ?versionId=null DELETE targeting only the
// null row, leaving UUID-versioned rows intact.
func TestNullVersionDelete(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/doc", "v1-disabled"), 200)
	enableVersioning(h, "bkt")
	v2 := putObjectReturnVersion(t, h, "/bkt/doc", "v2-uuid")

	resp := h.doString("DELETE", "/bkt/doc?versionId=null", "")
	h.mustStatus(resp, 204)

	resp = h.doString("GET", "/bkt/doc?versionId=null", "")
	h.mustStatus(resp, 404)

	resp = h.doString("GET", "/bkt/doc?versionId="+v2, "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "v2-uuid" {
		t.Errorf("UUID survives null delete: got %q", body)
	}
}

func TestListObjectVersions(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enableVersioning(h, "bkt")
	putObjectReturnVersion(t, h, "/bkt/x", "v1")
	putObjectReturnVersion(t, h, "/bkt/x", "v2")

	resp := h.doString("GET", "/bkt?versions", "")
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
