package s3api_test

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// completeMultipart3Parts uploads three random 4MiB parts under uploadID and
// returns the assembled <CompleteMultipartUpload> body (no surrounding
// whitespace).
func uploadThreeParts(t *testing.T, h *testHarness, bucketKey, uploadID string) string {
	t.Helper()
	var body strings.Builder
	body.WriteString("<CompleteMultipartUpload>")
	for i := 1; i <= 3; i++ {
		part := make([]byte, 4<<20)
		if _, err := rand.Read(part); err != nil {
			t.Fatal(err)
		}
		url := fmt.Sprintf("/%s?uploadId=%s&partNumber=%d", bucketKey, uploadID, i)
		resp := h.do("PUT", url, byteReader(part))
		h.mustStatus(resp, 200)
		etag := strings.Trim(resp.Header.Get("Etag"), `"`)
		fmt.Fprintf(&body, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, i, etag)
	}
	body.WriteString("</CompleteMultipartUpload>")
	return body.String()
}

func initiateMultipart(t *testing.T, h *testHarness, bucketKey string) string {
	t.Helper()
	resp := h.doString("POST", "/"+bucketKey+"?uploads", "")
	h.mustStatus(resp, 200)
	m := uploadIDRE.FindStringSubmatch(h.readBody(resp))
	if len(m) != 2 {
		t.Fatalf("no UploadId in init response")
	}
	return m[1]
}

// TestMultipartCompleteIfMatchOverwriteExisting mirrors s3-tests
// test_multipart_put_object_if_match_overwrite_existing_good: PUT a baseline
// object, MPU on the same key with If-Match=<existing-etag>, expect Complete
// to succeed and overwrite the body.
func TestMultipartCompleteIfMatchOverwriteExisting(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt/k", "seed")
	h.mustStatus(resp, 200)
	baseETag := strings.Trim(resp.Header.Get("Etag"), `"`)
	if baseETag == "" {
		t.Fatalf("missing baseline etag")
	}

	uploadID := initiateMultipart(t, h, "bkt/k")
	completeBody := uploadThreeParts(t, h, "bkt/k", uploadID)

	resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody, "If-Match", `"`+baseETag+`"`)
	h.mustStatus(resp, http.StatusOK)
}

// TestMultipartCompleteIfMatchNonExistingReturnsNoSuchKey mirrors s3-tests
// test_multipart_put_object_if_match (object does NOT exist branch): AWS
// returns 404 NoSuchKey rather than RFC-7232's 412 PreconditionFailed when
// the destination object is absent on Complete with If-Match.
func TestMultipartCompleteIfMatchNonExistingReturnsNoSuchKey(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	uploadID := initiateMultipart(t, h, "bkt/k")
	completeBody := uploadThreeParts(t, h, "bkt/k", uploadID)

	resp := h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody, "If-Match", `"deadbeef"`)
	h.mustStatus(resp, http.StatusNotFound)
	if !strings.Contains(h.readBody(resp), "NoSuchKey") {
		t.Fatalf("expected NoSuchKey in body")
	}
}

// TestMultipartCompleteIfMatchExistingMismatchFails mirrors s3-tests
// test_multipart_put_object_if_match_existing_failed: If-Match=<wrong-etag>
// returns 412 even when the object exists.
func TestMultipartCompleteIfMatchExistingMismatchFails(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "seed"), 200)

	uploadID := initiateMultipart(t, h, "bkt/k")
	completeBody := uploadThreeParts(t, h, "bkt/k", uploadID)

	resp := h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody, "If-Match", `"deadbeef"`)
	h.mustStatus(resp, http.StatusPreconditionFailed)
}

// TestMultipartCompleteIfNoneMatchStarNoObject mirrors s3-tests
// test_multipart_put_object_if_none_match: If-None-Match=* succeeds when no
// object exists yet on the key.
func TestMultipartCompleteIfNoneMatchStarNoObject(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	uploadID := initiateMultipart(t, h, "bkt/k")
	completeBody := uploadThreeParts(t, h, "bkt/k", uploadID)

	resp := h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody, "If-None-Match", "*")
	h.mustStatus(resp, http.StatusOK)
}

// TestMultipartCompleteIfNoneMatchStarExistingFails mirrors s3-tests
// test_multipart_put_object_if_none_match_failed: If-None-Match=* with an
// existing object returns 412.
func TestMultipartCompleteIfNoneMatchStarExistingFails(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "seed"), 200)

	uploadID := initiateMultipart(t, h, "bkt/k")
	completeBody := uploadThreeParts(t, h, "bkt/k", uploadID)

	resp := h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody, "If-None-Match", "*")
	h.mustStatus(resp, http.StatusPreconditionFailed)
}

// TestMultipartCompleteIfNoneMatchEtagFails: If-None-Match=<existing-etag>
// returns 412.
func TestMultipartCompleteIfNoneMatchEtagFails(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("PUT", "/bkt/k", "seed")
	h.mustStatus(resp, 200)
	baseETag := strings.Trim(resp.Header.Get("Etag"), `"`)

	uploadID := initiateMultipart(t, h, "bkt/k")
	completeBody := uploadThreeParts(t, h, "bkt/k", uploadID)

	resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody, "If-None-Match", `"`+baseETag+`"`)
	h.mustStatus(resp, http.StatusPreconditionFailed)
}

// TestMultipartCompleteIfMatchPreconditionDoesNotLeakCompletingState verifies
// the precondition check runs BEFORE the LWT flip — a 412 (existing-object,
// mismatched ETag) must leave the upload in 'uploading' state so a subsequent
// retry without preconditions completes successfully.
func TestMultipartCompleteIfMatchPreconditionDoesNotLeakCompletingState(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "seed"), 200)

	uploadID := initiateMultipart(t, h, "bkt/k")
	completeBody := uploadThreeParts(t, h, "bkt/k", uploadID)

	resp := h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody, "If-Match", `"deadbeef"`)
	h.mustStatus(resp, http.StatusPreconditionFailed)

	resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody)
	h.mustStatus(resp, http.StatusOK)
}

// TestMultipartCompleteIfMatchMissingObjectDoesNotLeakCompletingState mirrors
// the same invariant for the 404 NoSuchKey branch (object absent + If-Match):
// a retry without preconditions must still succeed because the upload row was
// not flipped to 'completing'.
func TestMultipartCompleteIfMatchMissingObjectDoesNotLeakCompletingState(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	uploadID := initiateMultipart(t, h, "bkt/k")
	completeBody := uploadThreeParts(t, h, "bkt/k", uploadID)

	resp := h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody, "If-Match", `"deadbeef"`)
	h.mustStatus(resp, http.StatusNotFound)

	resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody)
	h.mustStatus(resp, http.StatusOK)
}

// TestMultipartCompleteSetsVersionIDOnVersionedBucket mirrors s3-tests
// test_multipart_put_current_object_if_match: a versioned bucket Complete
// returns x-amz-version-id on the response.
func TestMultipartCompleteSetsVersionIDOnVersionedBucket(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enableVersioning(h, "bkt")

	uploadID := initiateMultipart(t, h, "bkt/k")
	completeBody := uploadThreeParts(t, h, "bkt/k", uploadID)

	resp := h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody)
	h.mustStatus(resp, http.StatusOK)
	if vid := resp.Header.Get("X-Amz-Version-Id"); vid == "" {
		t.Fatalf("expected x-amz-version-id on Complete response in versioned bucket")
	}
}
