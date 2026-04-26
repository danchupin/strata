package s3api_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func TestCompleteMultipartIdempotentRetry(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/idem?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	resp = h.do("PUT", fmt.Sprintf("/bkt/idem?uploadId=%s&partNumber=1", uploadID), strings.NewReader("hello world"))
	h.mustStatus(resp, 200)
	etag := strings.Trim(resp.Header.Get("Etag"), `"`)
	body := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`, etag)

	first := h.doString("POST", "/bkt/idem?uploadId="+uploadID, body)
	h.mustStatus(first, 200)
	firstBody := h.readBody(first)

	if !regexp.MustCompile(`-1&#34;</ETag>`).MatchString(firstBody) {
		t.Fatalf("expected composite etag with -1 suffix: %s", firstBody)
	}

	second := h.doString("POST", "/bkt/idem?uploadId="+uploadID, body)
	h.mustStatus(second, 200)
	secondBody := h.readBody(second)

	if firstBody != secondBody {
		t.Fatalf("retry body mismatch:\nfirst:  %s\nsecond: %s", firstBody, secondBody)
	}
}

func TestCompleteMultipartIdempotencyExpiry(t *testing.T) {
	now := time.Now()
	metamem.SetClockForTest(func() time.Time { return now })
	t.Cleanup(metamem.ResetClockForTest)

	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/exp?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	resp = h.do("PUT", fmt.Sprintf("/bkt/exp?uploadId=%s&partNumber=1", uploadID), strings.NewReader("expiring data"))
	h.mustStatus(resp, 200)
	etag := strings.Trim(resp.Header.Get("Etag"), `"`)
	body := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`, etag)

	h.mustStatus(h.doString("POST", "/bkt/exp?uploadId="+uploadID, body), 200)

	// Advance past the 10-minute idempotency window.
	now = now.Add(10*time.Minute + time.Second)

	retry := h.doString("POST", "/bkt/exp?uploadId="+uploadID, body)
	h.mustStatus(retry, 404)
	if !strings.Contains(h.readBody(retry), "NoSuchUpload") {
		t.Fatalf("expected NoSuchUpload after expiry")
	}
}
