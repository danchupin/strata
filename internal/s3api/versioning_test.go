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

// TestListObjectVersionsNullLiteral covers US-028: a row written under
// Versioning=Disabled stays addressable as ?versionId=null after the bucket
// is toggled to Enabled. ListObjectVersions surfaces it with VersionId="null"
// and IsLatest=false once a TimeUUID version is layered on top, while the
// new TimeUUID row is IsLatest=true.
func TestListObjectVersionsNullLiteral(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	// Disabled-mode PUT (no versioning yet) → null version row.
	h.mustStatus(h.doString("PUT", "/bkt/doc", "first"), 200)
	enableVersioning(h, "bkt")

	// Versioned PUT prepends a TimeUUID without overwriting the null row.
	v2 := putObjectReturnVersion(t, h, "/bkt/doc", "second")
	if v2 == "" || v2 == "null" {
		t.Fatalf("enabled put VersionID=%q want fresh TimeUUID", v2)
	}

	// Null version still readable via the literal.
	resp := h.doString("GET", "/bkt/doc?versionId=null", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "first" {
		t.Errorf("?versionId=null body: got %q want first", body)
	}

	// ListObjectVersions surfaces both rows.
	resp = h.doString("GET", "/bkt?versions", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	matches := versionIDRE.FindAllStringSubmatch(body, -1)
	if len(matches) != 2 {
		t.Fatalf("expected 2 versions, got %d: %s", len(matches), body)
	}
	if !strings.Contains(body, "<VersionId>null</VersionId>") {
		t.Errorf("null VersionId entry missing: %s", body)
	}
	if !strings.Contains(body, "<VersionId>"+v2+"</VersionId>") {
		t.Errorf("v2 VersionId missing: %s", body)
	}
	// New TimeUUID row should be the latest.
	latestIdx := strings.Index(body, "<VersionId>"+v2+"</VersionId>")
	nullIdx := strings.Index(body, "<VersionId>null</VersionId>")
	if latestIdx < 0 || nullIdx < 0 {
		t.Fatalf("indexes: latest=%d null=%d body=%s", latestIdx, nullIdx, body)
	}
	// The XML emits Version elements; the first one (lower index) is current.
	if latestIdx > nullIdx {
		t.Errorf("expected TimeUUID version before null version in XML; body=%s", body)
	}
}

// TestVersioningSuspendedReplaceNull covers US-029: in Suspended mode an
// unversioned PUT replaces just the prior null-versioned row (preserving any
// TimeUUID-versioned ancestors), and an unversioned DELETE replaces the prior
// null row with a delete marker addressed by VersionId="null".
func TestVersioningSuspendedReplaceNull(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enableVersioning(h, "bkt")

	v1 := putObjectReturnVersion(t, h, "/bkt/doc", "first")
	if v1 == "" || v1 == "null" {
		t.Fatalf("enabled put v1=%q want TimeUUID", v1)
	}

	// Toggle to Suspended.
	h.mustStatus(h.doString("PUT", "/bkt?versioning",
		"<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>"), 200)

	// Suspended PUT writes the null-version row.
	resp := h.doString("PUT", "/bkt/doc", "second")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("X-Amz-Version-Id"); got != "null" {
		t.Errorf("suspended PUT version-id: got %q want null", got)
	}

	// Latest is the new null row.
	resp = h.doString("GET", "/bkt/doc", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "second" {
		t.Errorf("latest after suspended put: got %q want second", body)
	}

	// v1 still reachable.
	resp = h.doString("GET", "/bkt/doc?versionId="+v1, "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "first" {
		t.Errorf("v1 lost after suspended put: got %q", body)
	}

	// Suspended PUT again replaces the null row in place.
	resp = h.doString("PUT", "/bkt/doc", "third")
	h.mustStatus(resp, 200)
	resp = h.doString("GET", "/bkt/doc?versionId=null", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "third" {
		t.Errorf("?versionId=null after replace: got %q want third", body)
	}

	// Suspended unversioned DELETE writes a null delete marker.
	resp = h.doString("DELETE", "/bkt/doc", "")
	h.mustStatus(resp, 204)
	if got := resp.Header.Get("X-Amz-Version-Id"); got != "null" {
		t.Errorf("suspended DELETE version-id: got %q want null", got)
	}
	if got := resp.Header.Get("X-Amz-Delete-Marker"); got != "true" {
		t.Errorf("suspended DELETE: missing X-Amz-Delete-Marker")
	}

	// Latest GET hits the marker → 404.
	h.mustStatus(h.doString("GET", "/bkt/doc", ""), 404)

	// v1 still reachable.
	resp = h.doString("GET", "/bkt/doc?versionId="+v1, "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "first" {
		t.Errorf("v1 lost after suspended delete: got %q", body)
	}

	// ListObjectVersions reports the null delete marker + v1.
	resp = h.doString("GET", "/bkt?versions", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<VersionId>null</VersionId>") {
		t.Errorf("null marker missing: %s", body)
	}
	if !strings.Contains(body, "<VersionId>"+v1+"</VersionId>") {
		t.Errorf("v1 missing: %s", body)
	}
	// Delete markers go in DeleteMarker elements, not Version.
	if !strings.Contains(body, "<DeleteMarker>") {
		t.Errorf("expected DeleteMarker element: %s", body)
	}
}
