package s3api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// putAndGetETag uploads "payload" to /bkt/src and returns the unquoted ETag.
func putAndGetETag(t *testing.T, h *testHarness) string {
	t.Helper()
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("PUT", "/bkt/src", "payload",
		"Content-Type", "text/plain",
		"x-amz-meta-foo", "bar",
		"x-amz-tagging", "k1=v1&k2=v2")
	h.mustStatus(resp, 200)
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if etag == "" {
		t.Fatal("ETag missing on src PUT")
	}
	return etag
}

func TestCopyObjectMetadataDirectiveCopy(t *testing.T) {
	h := newHarness(t)
	putAndGetETag(t, h)

	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src")
	h.mustStatus(resp, 200)

	resp = h.doString("HEAD", "/bkt/dst", "")
	h.mustStatus(resp, 200)
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("Content-Type: got %q want text/plain", ct)
	}
}

func TestCopyObjectMetadataDirectiveReplace(t *testing.T) {
	h := newHarness(t)
	putAndGetETag(t, h)

	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-metadata-directive", "REPLACE",
		"Content-Type", "application/octet-stream",
		"x-amz-meta-baz", "qux")
	h.mustStatus(resp, 200)

	resp = h.doString("HEAD", "/bkt/dst", "")
	h.mustStatus(resp, 200)
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type: got %q want application/octet-stream", ct)
	}
}

func TestCopyObjectInvalidMetadataDirective(t *testing.T) {
	h := newHarness(t)
	putAndGetETag(t, h)

	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-metadata-directive", "BOGUS")
	h.mustStatus(resp, http.StatusBadRequest)
}

func TestCopyObjectTaggingDirectiveCopy(t *testing.T) {
	h := newHarness(t)
	putAndGetETag(t, h)

	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src")
	h.mustStatus(resp, 200)

	resp = h.doString("GET", "/bkt/dst?tagging", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<Key>k1</Key>") || !strings.Contains(body, "<Value>v1</Value>") {
		t.Fatalf("tag k1=v1 not preserved: %s", body)
	}
}

func TestCopyObjectTaggingDirectiveReplace(t *testing.T) {
	h := newHarness(t)
	putAndGetETag(t, h)

	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-tagging-directive", "REPLACE",
		"x-amz-tagging", "newK=newV")
	h.mustStatus(resp, 200)

	resp = h.doString("GET", "/bkt/dst?tagging", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<Key>newK</Key>") {
		t.Fatalf("expected newK tag: %s", body)
	}
	if strings.Contains(body, "<Key>k1</Key>") {
		t.Fatalf("source tags should be replaced: %s", body)
	}
}

func TestCopyObjectInvalidTaggingDirective(t *testing.T) {
	h := newHarness(t)
	putAndGetETag(t, h)

	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-tagging-directive", "INVALID")
	h.mustStatus(resp, http.StatusBadRequest)
}

func TestCopyObjectIfMatchHit(t *testing.T) {
	h := newHarness(t)
	etag := putAndGetETag(t, h)

	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-copy-source-if-match", `"`+etag+`"`)
	h.mustStatus(resp, 200)
}

func TestCopyObjectIfMatchMiss(t *testing.T) {
	h := newHarness(t)
	putAndGetETag(t, h)

	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-copy-source-if-match", `"deadbeef"`)
	h.mustStatus(resp, http.StatusPreconditionFailed)
}

func TestCopyObjectIfNoneMatchHit(t *testing.T) {
	h := newHarness(t)
	putAndGetETag(t, h)

	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-copy-source-if-none-match", `"deadbeef"`)
	h.mustStatus(resp, 200)
}

func TestCopyObjectIfNoneMatchMiss(t *testing.T) {
	h := newHarness(t)
	etag := putAndGetETag(t, h)

	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-copy-source-if-none-match", `"`+etag+`"`)
	h.mustStatus(resp, http.StatusPreconditionFailed)
}

func TestCopyObjectIfModifiedSinceHit(t *testing.T) {
	h := newHarness(t)
	putAndGetETag(t, h)

	past := time.Now().UTC().Add(-1 * time.Hour).Format(http.TimeFormat)
	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-copy-source-if-modified-since", past)
	h.mustStatus(resp, 200)
}

func TestCopyObjectIfModifiedSinceMiss(t *testing.T) {
	h := newHarness(t)
	putAndGetETag(t, h)

	future := time.Now().UTC().Add(1 * time.Hour).Format(http.TimeFormat)
	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-copy-source-if-modified-since", future)
	h.mustStatus(resp, http.StatusPreconditionFailed)
}

func TestCopyObjectIfUnmodifiedSinceHit(t *testing.T) {
	h := newHarness(t)
	putAndGetETag(t, h)

	future := time.Now().UTC().Add(1 * time.Hour).Format(http.TimeFormat)
	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-copy-source-if-unmodified-since", future)
	h.mustStatus(resp, 200)
}

func TestCopyObjectIfUnmodifiedSinceMiss(t *testing.T) {
	h := newHarness(t)
	putAndGetETag(t, h)

	past := time.Now().UTC().Add(-1 * time.Hour).Format(http.TimeFormat)
	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-copy-source-if-unmodified-since", past)
	h.mustStatus(resp, http.StatusPreconditionFailed)
}
