package s3api_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

func TestPutObjectUnderQuotaSucceeds(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, "alice"), 200)
	b, err := h.meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	if err := h.meta.SetBucketQuota(context.Background(), b.ID, meta.BucketQuota{MaxBytes: 100}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	h.mustStatus(h.doString("PUT", "/bkt/a", strings.Repeat("x", 50), testPrincipalHeader, "alice"), 200)
	h.mustStatus(h.doString("PUT", "/bkt/b", strings.Repeat("x", 50), testPrincipalHeader, "alice"), 200)
}

func TestPutObjectAtQuotaSucceeds(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, "alice"), 200)
	b, _ := h.meta.GetBucket(context.Background(), "bkt")
	if err := h.meta.SetBucketQuota(context.Background(), b.ID, meta.BucketQuota{MaxBytes: 100}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	h.mustStatus(h.doString("PUT", "/bkt/exact", strings.Repeat("x", 100), testPrincipalHeader, "alice"), 200)
}

func TestPutObjectPastBucketBytesQuotaRejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, "alice"), 200)
	b, _ := h.meta.GetBucket(context.Background(), "bkt")
	if err := h.meta.SetBucketQuota(context.Background(), b.ID, meta.BucketQuota{MaxBytes: 100}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	resp := h.doString("PUT", "/bkt/over", strings.Repeat("x", 101), testPrincipalHeader, "alice")
	h.mustStatus(resp, 403)
	if body := h.readBody(resp); !strings.Contains(body, "QuotaExceeded") {
		t.Fatalf("expected QuotaExceeded error code, body=%s", body)
	}
}

func TestPutObjectPastBucketObjectsQuotaRejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, "alice"), 200)
	b, _ := h.meta.GetBucket(context.Background(), "bkt")
	if err := h.meta.SetBucketQuota(context.Background(), b.ID, meta.BucketQuota{MaxObjects: 1}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	h.mustStatus(h.doString("PUT", "/bkt/first", "ok", testPrincipalHeader, "alice"), 200)
	resp := h.doString("PUT", "/bkt/second", "ok", testPrincipalHeader, "alice")
	h.mustStatus(resp, 403)
	if body := h.readBody(resp); !strings.Contains(body, "QuotaExceeded") {
		t.Fatalf("expected QuotaExceeded, body=%s", body)
	}
}

func TestPutObjectMaxBytesPerObjectRejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, "alice"), 200)
	b, _ := h.meta.GetBucket(context.Background(), "bkt")
	if err := h.meta.SetBucketQuota(context.Background(), b.ID, meta.BucketQuota{MaxBytesPerObject: 5}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	h.mustStatus(h.doString("PUT", "/bkt/small", "12345", testPrincipalHeader, "alice"), 200)
	resp := h.doString("PUT", "/bkt/big", "123456", testPrincipalHeader, "alice")
	h.mustStatus(resp, 403)
	if body := h.readBody(resp); !strings.Contains(body, "QuotaExceeded") {
		t.Fatalf("expected QuotaExceeded, body=%s", body)
	}
}

func TestPutObjectOverwriteUnversionedDoesNotDoubleCount(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, "alice"), 200)
	b, _ := h.meta.GetBucket(context.Background(), "bkt")
	if err := h.meta.SetBucketQuota(context.Background(), b.ID, meta.BucketQuota{MaxBytes: 100}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	h.mustStatus(h.doString("PUT", "/bkt/a", strings.Repeat("x", 90), testPrincipalHeader, "alice"), 200)
	h.mustStatus(h.doString("PUT", "/bkt/a", strings.Repeat("x", 95), testPrincipalHeader, "alice"), 200)
	resp := h.doString("PUT", "/bkt/a", strings.Repeat("x", 101), testPrincipalHeader, "alice")
	h.mustStatus(resp, 403)
}

func TestCreateBucketUserMaxBucketsRejected(t *testing.T) {
	h := newHarness(t)
	if err := h.meta.SetUserQuota(context.Background(), "alice", meta.UserQuota{MaxBuckets: 1}); err != nil {
		t.Fatalf("set user quota: %v", err)
	}

	h.mustStatus(h.doString("PUT", "/bkt1", "", testPrincipalHeader, "alice"), 200)
	resp := h.doString("PUT", "/bkt2", "", testPrincipalHeader, "alice")
	h.mustStatus(resp, 403)
	if body := h.readBody(resp); !strings.Contains(body, "QuotaExceeded") {
		t.Fatalf("expected QuotaExceeded, body=%s", body)
	}
}

func TestPutObjectUserTotalBytesQuotaRejected(t *testing.T) {
	h := newHarness(t)
	if err := h.meta.SetUserQuota(context.Background(), "alice", meta.UserQuota{TotalMaxBytes: 10}); err != nil {
		t.Fatalf("set user quota: %v", err)
	}
	h.mustStatus(h.doString("PUT", "/bkta", "", testPrincipalHeader, "alice"), 200)
	h.mustStatus(h.doString("PUT", "/bktb", "", testPrincipalHeader, "alice"), 200)

	h.mustStatus(h.doString("PUT", "/bkta/x", "12345", testPrincipalHeader, "alice"), 200)
	h.mustStatus(h.doString("PUT", "/bktb/x", "12345", testPrincipalHeader, "alice"), 200)
	resp := h.doString("PUT", "/bkta/y", "1", testPrincipalHeader, "alice")
	h.mustStatus(resp, 403)
}

func TestMultipartCompletePastQuotaRejected(t *testing.T) {
	restore := s3api.SetMultipartMinPartSizeForTest(1)
	defer restore()

	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, "alice"), 200)
	b, _ := h.meta.GetBucket(context.Background(), "bkt")
	if err := h.meta.SetBucketQuota(context.Background(), b.ID, meta.BucketQuota{MaxBytesPerObject: 5}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	resp := h.doString("POST", "/bkt/mp?uploads", "", testPrincipalHeader, "alice")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	var complete strings.Builder
	complete.WriteString("<CompleteMultipartUpload>")
	for i := 1; i <= 2; i++ {
		url := fmt.Sprintf("/bkt/mp?uploadId=%s&partNumber=%d", uploadID, i)
		r := h.do("PUT", url, bytes.NewReader([]byte("XXX")), testPrincipalHeader, "alice")
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		complete.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, i, etag))
	}
	complete.WriteString("</CompleteMultipartUpload>")

	resp = h.doString("POST", "/bkt/mp?uploadId="+uploadID, complete.String(), testPrincipalHeader, "alice")
	h.mustStatus(resp, 403)
	if body := h.readBody(resp); !strings.Contains(body, "QuotaExceeded") {
		t.Fatalf("expected QuotaExceeded, body=%s", body)
	}
}

func TestUploadPartPastQuotaRejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, "alice"), 200)
	b, _ := h.meta.GetBucket(context.Background(), "bkt")
	if err := h.meta.SetBucketQuota(context.Background(), b.ID, meta.BucketQuota{MaxBytes: 4}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	resp := h.doString("POST", "/bkt/mp?uploads", "", testPrincipalHeader, "alice")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	url := fmt.Sprintf("/bkt/mp?uploadId=%s&partNumber=1", uploadID)
	r := h.do("PUT", url, bytes.NewReader([]byte("HELLO")), testPrincipalHeader, "alice")
	h.mustStatus(r, 403)
}

