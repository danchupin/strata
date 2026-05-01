package s3api_test

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// completeMP runs Initiate → UploadPart(s) → CompleteMultipartUpload with
// optional headers on the Complete call. Returns the Complete response.
func completeMP(t *testing.T, h *testHarness, bucket, key string, partSizes []int, completeHeaders ...string) *http.Response {
	t.Helper()
	resp := h.doString("POST", "/"+bucket+"/"+key+"?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	var body strings.Builder
	body.WriteString("<CompleteMultipartUpload>")
	for i, sz := range partSizes {
		pnum := i + 1
		buf := make([]byte, sz)
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		url := fmt.Sprintf("/%s/%s?uploadId=%s&partNumber=%d", bucket, key, uploadID, pnum)
		r := h.do("PUT", url, byteReader(buf))
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		body.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pnum, etag))
	}
	body.WriteString("</CompleteMultipartUpload>")
	return h.doString("POST", "/"+bucket+"/"+key+"?uploadId="+uploadID, body.String(), completeHeaders...)
}

// TestMultipartCompleteIfNoneMatchStarRejectsExisting mirrors s3-tests
// test_multipart_put_object_if_none_match_overwrite_existing_object: a
// CompleteMultipartUpload with `If-None-Match: *` against a key that already
// exists must return 412 PreconditionFailed.
func TestMultipartCompleteIfNoneMatchStarRejectsExisting(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "first"), 200)

	const partSize = 5 << 20
	r := completeMP(t, h, "bkt", "k", []int{partSize, partSize / 2}, "If-None-Match", "*")
	h.mustStatus(r, http.StatusPreconditionFailed)

	// Original body untouched.
	g := h.doString("GET", "/bkt/k", "")
	h.mustStatus(g, http.StatusOK)
	if got := h.readBody(g); got != "first" {
		t.Fatalf("body got %q want %q", got, "first")
	}
}

// TestMultipartCompleteIfNoneMatchStarAllowsCreate pins the create case:
// `If-None-Match: *` on a key that doesn't exist completes successfully.
func TestMultipartCompleteIfNoneMatchStarAllowsCreate(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	const partSize = 5 << 20
	r := completeMP(t, h, "bkt", "k", []int{partSize, partSize / 2}, "If-None-Match", "*")
	h.mustStatus(r, http.StatusOK)
}

// TestMultipartCompleteIfMatchMismatch mirrors s3-tests
// test_multipart_put_current_object_if_match: a CompleteMultipartUpload with
// `If-Match: <wrong-etag>` must return 412 against an existing object.
func TestMultipartCompleteIfMatchMismatch(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "first"), 200)

	const partSize = 5 << 20
	r := completeMP(t, h, "bkt", "k", []int{partSize, partSize / 2}, "If-Match", `"deadbeef"`)
	h.mustStatus(r, http.StatusPreconditionFailed)

	// Original body untouched.
	g := h.doString("GET", "/bkt/k", "")
	h.mustStatus(g, http.StatusOK)
	if got := h.readBody(g); got != "first" {
		t.Fatalf("body got %q want %q", got, "first")
	}
}

// TestMultipartCompleteIfMatchMissingObject pins the case where If-Match is
// supplied but the target key doesn't exist: must 412 (no object to match).
func TestMultipartCompleteIfMatchMissingObject(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	const partSize = 5 << 20
	r := completeMP(t, h, "bkt", "k", []int{partSize, partSize / 2}, "If-Match", `"deadbeef"`)
	h.mustStatus(r, http.StatusPreconditionFailed)
}

// TestMultipartCompleteIfMatchHit pins the success case: If-Match matching the
// existing ETag completes and overwrites.
func TestMultipartCompleteIfMatchHit(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	put := h.doString("PUT", "/bkt/k", "first")
	h.mustStatus(put, 200)
	etag := put.Header.Get("Etag")
	if etag == "" {
		t.Fatal("missing ETag on first PUT")
	}

	const partSize = 5 << 20
	r := completeMP(t, h, "bkt", "k", []int{partSize, partSize / 2}, "If-Match", etag)
	h.mustStatus(r, http.StatusOK)
}

// TestMultipartCompleteVersionIDHeader pins the x-amz-version-id response
// header on a versioning-enabled bucket: Complete returns a fresh UUID; on a
// suspended bucket, Complete returns the literal "null".
func TestMultipartCompleteVersionIDHeader(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?versioning",
		`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`,
	), 200)

	const partSize = 5 << 20
	r := completeMP(t, h, "bkt", "k", []int{partSize, partSize / 2})
	h.mustStatus(r, http.StatusOK)
	v := r.Header.Get("X-Amz-Version-Id")
	if v == "" || v == "null" {
		t.Fatalf("Enabled bucket: want UUID version-id, got %q", v)
	}

	// Switch to Suspended and re-Complete.
	h.mustStatus(h.doString("PUT", "/bkt?versioning",
		`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Suspended</Status></VersioningConfiguration>`,
	), 200)
	r = completeMP(t, h, "bkt", "k", []int{partSize, partSize / 2})
	h.mustStatus(r, http.StatusOK)
	if got := r.Header.Get("X-Amz-Version-Id"); got != "null" {
		t.Fatalf("Suspended bucket: want version-id %q, got %q", "null", got)
	}
}

// TestMultipartCompleteVersionIDHeaderUnversioned pins that an unversioned
// bucket emits NO x-amz-version-id header on Complete (matches putObject).
func TestMultipartCompleteVersionIDHeaderUnversioned(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	const partSize = 5 << 20
	r := completeMP(t, h, "bkt", "k", []int{partSize, partSize / 2})
	h.mustStatus(r, http.StatusOK)
	if got := r.Header.Get("X-Amz-Version-Id"); got != "" {
		t.Fatalf("unversioned bucket: want no version-id header, got %q", got)
	}
}
