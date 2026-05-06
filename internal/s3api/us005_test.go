package s3api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// US-005: DELETE on a non-existent bucket via the bucket-aware deny handler
// returns 404 NoSuchBucket BEFORE the auth gate's 403, matching AWS S3
// (s3-test test_object_delete_key_bucket_gone).
func TestAuthDeny_DeleteOnMissingBucket_Returns404NoSuchBucket(t *testing.T) {
	ms := metamem.New()
	api := s3api.New(datamem.New(), ms)
	api.Region = "us-east-1"
	store := auth.NewStaticStore(map[string]*auth.Credential{})
	multi := auth.NewMultiStore(time.Minute, store)
	mw := &auth.Middleware{Store: multi, Mode: auth.ModeRequired}
	ts := httptest.NewServer(mw.Wrap(api, s3api.NewAuthDenyHandler(ms)))
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/no-such-bucket/foo", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", resp.StatusCode)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "NoSuchBucket") {
		t.Fatalf("body: missing NoSuchBucket; got %s", body)
	}
}

// US-005: bucket EXISTS but caller is unauthenticated → still 403 AccessDenied
// (the bucket-existence shortcut only fires on missing bucket).
func TestAuthDeny_DeleteOnExistingBucket_Returns403(t *testing.T) {
	ms := metamem.New()
	api := s3api.New(datamem.New(), ms)
	api.Region = "us-east-1"

	const ak = "AKIATESTOWNER0000000"
	const sk = "ownersecretownersecretownerse00"
	store := auth.NewStaticStore(map[string]*auth.Credential{
		ak: {AccessKey: ak, Secret: sk, Owner: "owner"},
	})
	multi := auth.NewMultiStore(time.Minute, store)
	mw := &auth.Middleware{Store: multi, Mode: auth.ModeRequired}
	ts := httptest.NewServer(mw.Wrap(api, s3api.NewAuthDenyHandler(ms)))
	t.Cleanup(ts.Close)

	createReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/existing", nil)
	signRequest(t, createReq, ak, sk, "us-east-1")
	createResp, err := http.DefaultClient.Do(createReq)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusOK {
		t.Fatalf("create status: got %d", createResp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/existing/k", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", resp.StatusCode)
	}
}

// US-005: ListObjectVersions <Owner><DisplayName> is non-empty (matches the
// bucket owner). s3-test test_bucket_list_return_data_versioning asserts
// `obj['Owner']['DisplayName']` is set to the principal name.
func TestListObjectVersions_OwnerDisplayName(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.do("PUT", "/bkt", nil, testPrincipalHeader, "alice"), 200)
	h.mustStatus(h.do("PUT", "/bkt?versioning",
		strings.NewReader(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`),
		testPrincipalHeader, "alice"), 200)
	h.mustStatus(h.do("PUT", "/bkt/k", strings.NewReader("v1"), testPrincipalHeader, "alice"), 200)

	resp := h.do("GET", "/bkt?versions", nil, testPrincipalHeader, "alice")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<Owner><ID>alice</ID><DisplayName>alice</DisplayName></Owner>") {
		t.Fatalf("Owner element missing or empty DisplayName: %s", body)
	}
}

// US-005: a multipart Complete with a stale per-part ETag (resend
// finishes-last race) returns ErrInvalidPart but leaves the upload re-
// completable — the status flip to "completing" must NOT happen until ALL
// per-part ETags validate. s3-test test_multipart_resend_first_finishes_last.
func TestMultipartResendFinishesLast_StaleETagDoesNotWedgeCompleting(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	initiate := h.doString("POST", "/bkt/key?uploads", "")
	h.mustStatus(initiate, 200)
	body := h.readBody(initiate)
	uploadID := extractTag(body, "UploadId")
	if uploadID == "" {
		t.Fatalf("no UploadId in: %s", body)
	}

	// First UploadPart returns ETag for "AAA..." content.
	resp := h.doString("PUT",
		"/bkt/key?partNumber=1&uploadId="+uploadID,
		strings.Repeat("A", 5*1024*1024))
	h.mustStatus(resp, 200)
	staleETag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if staleETag == "" {
		t.Fatalf("missing ETag on first upload-part")
	}

	// Resend: second UploadPart for same partNumber wins → updates stored ETag.
	resp = h.doString("PUT",
		"/bkt/key?partNumber=1&uploadId="+uploadID,
		strings.Repeat("B", 5*1024*1024))
	h.mustStatus(resp, 200)
	freshETag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if freshETag == "" || freshETag == staleETag {
		t.Fatalf("second upload-part etag: got %q stale=%q", freshETag, staleETag)
	}

	// Complete with stale ETag → InvalidPart, but status stays "uploading"
	// so a retry with the fresh ETag succeeds. Without the fix, the status
	// would have flipped to "completing" before validation, wedging retries.
	completeStale := completeXML(staleETag)
	resp = h.doString("POST",
		"/bkt/key?uploadId="+uploadID, completeStale)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("stale Complete: got status %d, want 400; body=%s",
			resp.StatusCode, h.readBody(resp))
	}

	completeFresh := completeXML(freshETag)
	resp = h.doString("POST",
		"/bkt/key?uploadId="+uploadID, completeFresh)
	h.mustStatus(resp, 200)
}

func completeXML(etag string) string {
	return `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"` +
		etag + `"</ETag></Part></CompleteMultipartUpload>`
}

func extractTag(body, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i+len(open):], close)
	if j < 0 {
		return ""
	}
	return body[i+len(open) : i+len(open)+j]
}

// US-005: copy from a suspended-bucket null-row source resolves to the null
// row (latest write wins) and the response carries the source's wire
// "x-amz-copy-source-version-id: null" header. s3-test
// test_versioning_obj_suspended_copy.
func TestCopyObject_SuspendedBucketNullSource(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enableVersioning(h, "bkt")
	// Versioned PUT — establishes a real-timeuuid version (the latest before
	// the suspend). Body = "content-0".
	v1 := putObjectReturnVersion(t, h, "/bkt/src", "content-0")
	if v1 == "" || v1 == "null" {
		t.Fatalf("v1=%q want timeuuid", v1)
	}
	// Suspend, then PUT replaces with the null-version row. Body = "null content".
	h.mustStatus(h.doString("PUT", "/bkt?versioning",
		"<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>"), 200)
	h.mustStatus(h.doString("PUT", "/bkt/src", "null content"), 200)

	// Sanity: GET without versionId returns the null row body.
	resp := h.doString("GET", "/bkt/src", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "null content" {
		t.Fatalf("latest body: got %q want 'null content'", body)
	}

	// Copy src → dst (no CopySourceVersionId) must read the null row, and the
	// dst-side response must carry x-amz-copy-source-version-id: null.
	resp = h.doString("PUT", "/bkt/dst", "", "x-amz-copy-source", "/bkt/src")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-copy-source-version-id"); got != "null" {
		t.Fatalf("x-amz-copy-source-version-id: got %q want null", got)
	}
	resp = h.doString("GET", "/bkt/dst", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "null content" {
		t.Fatalf("dst body: got %q want 'null content'", body)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			b = append(b, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(b)
}
