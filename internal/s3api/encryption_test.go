package s3api_test

import (
	"context"
	"strings"
	"testing"
)

const sseConfigXML = `<ServerSideEncryptionConfiguration>
	<Rule>
		<ApplyServerSideEncryptionByDefault>
			<SSEAlgorithm>AES256</SSEAlgorithm>
		</ApplyServerSideEncryptionByDefault>
	</Rule>
</ServerSideEncryptionConfiguration>`

func TestObjectSSEHeaderRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt/k", "payload",
		"x-amz-server-side-encryption", "AES256")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "" {
		// PutObject response echo is optional; not required here.
		_ = got
	}

	resp = h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("GET sse header: got %q want AES256", got)
	}

	resp = h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("HEAD sse header: got %q want AES256", got)
	}
}

func TestObjectSSEKMSDSSERejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt/k", "x",
		"x-amz-server-side-encryption", "aws:kms:dsse")
	h.mustStatus(resp, 501)
	if body := h.readBody(resp); !strings.Contains(body, "NotImplemented") {
		t.Fatalf("expected NotImplemented, got: %s", body)
	}
}

func TestObjectSSEKMSWithoutKeyIDReturns400(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt/k", "x",
		"x-amz-server-side-encryption", "aws:kms")
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidArgument") {
		t.Fatalf("expected InvalidArgument, got: %s", body)
	}
}

func TestObjectSSEUnknownAlgorithm(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt/k", "x",
		"x-amz-server-side-encryption", "DES")
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidArgument") {
		t.Fatalf("expected InvalidArgument, got: %s", body)
	}
}

func TestBucketEncryptionConfigCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("GET", "/bkt?encryption=", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "ServerSideEncryptionConfigurationNotFoundError") {
		t.Fatalf("expected NotFound, got: %s", body)
	}

	h.mustStatus(h.doString("PUT", "/bkt?encryption=", sseConfigXML), 200)

	resp = h.doString("GET", "/bkt?encryption=", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "AES256") {
		t.Fatalf("GET encryption body missing algo: %s", body)
	}

	h.mustStatus(h.doString("DELETE", "/bkt?encryption=", ""), 204)
	h.mustStatus(h.doString("GET", "/bkt?encryption=", ""), 404)
}

func TestBucketEncryptionDefaultAppliedOnPut(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?encryption=", sseConfigXML), 200)

	resp := h.doString("PUT", "/bkt/k", "payload")
	h.mustStatus(resp, 200)

	resp = h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("default SSE not applied: got %q", got)
	}
}

func TestBucketEncryptionDefaultClearedAfterDelete(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?encryption=", sseConfigXML), 200)

	resp := h.doString("PUT", "/bkt/k1", "before")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("default SSE not applied before delete: got %q", got)
	}

	h.mustStatus(h.doString("DELETE", "/bkt?encryption=", ""), 204)

	resp = h.doString("PUT", "/bkt/k2", "after")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "" {
		t.Fatalf("default still applied after delete: got %q", got)
	}
	resp = h.doString("HEAD", "/bkt/k2", "")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "" {
		t.Fatalf("HEAD after delete: got %q want unset", got)
	}
}

func TestBucketEncryptionMultipartDefaultApplied(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?encryption=", sseConfigXML), 200)

	resp := h.doString("POST", "/bkt/k?uploads=", "")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("multipart Initiate did not inherit default: got %q", got)
	}
	uploadID := extractUploadID(h.readBody(resp))
	if uploadID == "" {
		t.Fatalf("could not parse uploadId")
	}

	resp = h.doString("PUT", "/bkt/k?partNumber=1&uploadId="+uploadID, "abc")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("UploadPart sse header (default-inherited): got %q", got)
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	completeXML := "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>\"" + etag + "\"</ETag></Part></CompleteMultipartUpload>"
	resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, completeXML)
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("Complete sse header (default-inherited): got %q", got)
	}

	resp = h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("HEAD after multipart default: got %q", got)
	}
}

func TestBucketEncryptionPutKMSDSSERejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	const kmsCfg = `<ServerSideEncryptionConfiguration>
		<Rule>
			<ApplyServerSideEncryptionByDefault>
				<SSEAlgorithm>aws:kms:dsse</SSEAlgorithm>
			</ApplyServerSideEncryptionByDefault>
		</Rule>
	</ServerSideEncryptionConfiguration>`
	resp := h.doString("PUT", "/bkt?encryption=", kmsCfg)
	h.mustStatus(resp, 501)
}

func TestBucketEncryptionPutKMSAccepted(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	const kmsCfg = `<ServerSideEncryptionConfiguration>
		<Rule>
			<ApplyServerSideEncryptionByDefault>
				<SSEAlgorithm>aws:kms</SSEAlgorithm>
				<KMSMasterKeyID>alias/strata-test</KMSMasterKeyID>
			</ApplyServerSideEncryptionByDefault>
		</Rule>
	</ServerSideEncryptionConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?encryption=", kmsCfg), 200)
}

func TestObjectSSEKMSRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt/k", "payload",
		"x-amz-server-side-encryption", "aws:kms",
		"x-amz-server-side-encryption-aws-kms-key-id", "alias/strata-test")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "aws:kms" {
		t.Fatalf("PUT sse header: got %q want aws:kms", got)
	}
	if got := resp.Header.Get("x-amz-server-side-encryption-aws-kms-key-id"); got != "alias/strata-test" {
		t.Fatalf("PUT key id header: got %q", got)
	}

	resp = h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if got := h.readBody(resp); got != "payload" {
		t.Fatalf("GET body: got %q want payload", got)
	}
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "aws:kms" {
		t.Fatalf("GET sse header: got %q want aws:kms", got)
	}
	if got := resp.Header.Get("x-amz-server-side-encryption-aws-kms-key-id"); got != "alias/strata-test" {
		t.Fatalf("GET key id header: got %q", got)
	}
}

func TestObjectSSEKMSDefaultApplied(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	const kmsCfg = `<ServerSideEncryptionConfiguration>
		<Rule>
			<ApplyServerSideEncryptionByDefault>
				<SSEAlgorithm>aws:kms</SSEAlgorithm>
				<KMSMasterKeyID>alias/strata-default</KMSMasterKeyID>
			</ApplyServerSideEncryptionByDefault>
		</Rule>
	</ServerSideEncryptionConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?encryption=", kmsCfg), 200)

	resp := h.doString("PUT", "/bkt/k", "payload")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "aws:kms" {
		t.Fatalf("PUT inherited SSE: got %q", got)
	}
	if got := resp.Header.Get("x-amz-server-side-encryption-aws-kms-key-id"); got != "alias/strata-default" {
		t.Fatalf("PUT inherited key id: got %q", got)
	}

	resp = h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if got := h.readBody(resp); got != "payload" {
		t.Fatalf("GET body: got %q", got)
	}
}

func TestObjectSSEKMSDefaultRequiresKeyID(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	// bucket default selects aws:kms but omits KMSMasterKeyID — header-less PUT
	// must fail 400 rather than fall back to a missing-key-id wrap.
	const kmsCfgNoID = `<ServerSideEncryptionConfiguration>
		<Rule>
			<ApplyServerSideEncryptionByDefault>
				<SSEAlgorithm>aws:kms</SSEAlgorithm>
			</ApplyServerSideEncryptionByDefault>
		</Rule>
	</ServerSideEncryptionConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?encryption=", kmsCfgNoID), 200)

	resp := h.doString("PUT", "/bkt/k", "x")
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidArgument") {
		t.Fatalf("expected InvalidArgument, got: %s", body)
	}
}

func TestObjectSSEKMSMismatchedKeyIDForbidden(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt/k", "payload",
		"x-amz-server-side-encryption", "aws:kms",
		"x-amz-server-side-encryption-aws-kms-key-id", "alias/orig")
	h.mustStatus(resp, 200)

	ctx := context.Background()
	b, err := h.meta.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	o, err := h.meta.GetObject(ctx, b.ID, "k", "")
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	// Tamper: leave wrapped DEK but switch persisted key id so UnwrapDEK
	// recomputes a mac that does NOT match the stored wrap → ErrKeyIDMismatch.
	if err := h.meta.UpdateObjectSSEWrap(ctx, b.ID, "k", o.VersionID, o.SSEKey, "alias/wrong"); err != nil {
		t.Fatalf("tamper key id: %v", err)
	}

	resp = h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 403)
	if body := h.readBody(resp); !strings.Contains(body, "AccessDenied") {
		t.Fatalf("expected AccessDenied, got: %s", body)
	}
}

func TestObjectSSEKMSMultipartRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads=", "",
		"x-amz-server-side-encryption", "aws:kms",
		"x-amz-server-side-encryption-aws-kms-key-id", "alias/multi")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "aws:kms" {
		t.Fatalf("Initiate sse header: got %q", got)
	}
	if got := resp.Header.Get("x-amz-server-side-encryption-aws-kms-key-id"); got != "alias/multi" {
		t.Fatalf("Initiate key id header: got %q", got)
	}
	uploadID := extractUploadID(h.readBody(resp))
	if uploadID == "" {
		t.Fatalf("could not parse uploadId")
	}

	resp = h.doString("PUT", "/bkt/k?partNumber=1&uploadId="+uploadID, "abc")
	h.mustStatus(resp, 200)
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)

	completeXML := "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>\"" + etag + "\"</ETag></Part></CompleteMultipartUpload>"
	resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, completeXML)
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "aws:kms" {
		t.Fatalf("Complete sse header: got %q", got)
	}
	if got := resp.Header.Get("x-amz-server-side-encryption-aws-kms-key-id"); got != "alias/multi" {
		t.Fatalf("Complete key id header: got %q", got)
	}

	resp = h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if got := h.readBody(resp); got != "abc" {
		t.Fatalf("GET body: got %q", got)
	}
}

func TestBucketEncryptionMalformedRejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt?encryption=", "<not-xml")
	h.mustStatus(resp, 400)
}

func TestMultipartSSEHeaderRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads=", "",
		"x-amz-server-side-encryption", "AES256")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("Initiate sse header: got %q", got)
	}
	body := h.readBody(resp)
	uploadID := extractUploadID(body)
	if uploadID == "" {
		t.Fatalf("could not parse uploadId from %s", body)
	}

	resp = h.doString("PUT", "/bkt/k?partNumber=1&uploadId="+uploadID, "abc")
	h.mustStatus(resp, 200)
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("UploadPart sse header: got %q", got)
	}

	completeXML := "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>\"" + etag + "\"</ETag></Part></CompleteMultipartUpload>"
	resp = h.doString("POST", "/bkt/k?uploadId="+uploadID, completeXML)
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("Complete sse header: got %q", got)
	}

	resp = h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("HEAD after multipart sse: got %q", got)
	}
}

func extractUploadID(body string) string {
	const open = "<UploadId>"
	const close_ = "</UploadId>"
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, close_)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
