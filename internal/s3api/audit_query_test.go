package s3api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

type auditPage struct {
	Records []struct {
		Bucket    string    `json:"bucket"`
		EventID   string    `json:"event_id"`
		Principal string    `json:"principal"`
		Action    string    `json:"action"`
		Resource  string    `json:"resource"`
		Time      time.Time `json:"time"`
	} `json:"records"`
	NextContinuationToken string `json:"next_continuation_token"`
}

func enqueueAudit(t *testing.T, h *notifyHarness, bucketID uuid.UUID, bucket, principal, action string, ts time.Time) string {
	t.Helper()
	evt := &meta.AuditEvent{
		BucketID:  bucketID,
		Bucket:    bucket,
		Time:      ts,
		Principal: principal,
		Action:    action,
		Resource:  "/" + bucket,
		Result:    "200",
		RequestID: "req-" + action,
		SourceIP:  "10.0.0.5",
	}
	if err := h.store.EnqueueAudit(context.Background(), evt, time.Hour); err != nil {
		t.Fatalf("enqueue audit: %v", err)
	}
	return evt.EventID
}

func decodeAuditPage(t *testing.T, resp *http.Response) auditPage {
	t.Helper()
	var p auditPage
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	return p
}

func TestAuditAnonymousDenied(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("GET", "/?audit", "")
	h.mustStatus(resp, http.StatusForbidden)
	resp.Body.Close()
}

func TestAuditNonRootDenied(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("GET", "/?audit", "", testPrincipalHeader, "alice")
	h.mustStatus(resp, http.StatusForbidden)
	resp.Body.Close()
}

func TestAuditPresignedDenied(t *testing.T) {
	h := newNotifyHarness(t)
	// Even with the [iam root] principal injected, presence of the presigned
	// signature query param must reject the request — admin endpoints are
	// header-signed only.
	resp := h.doString("GET", "/?audit&X-Amz-Signature=abc", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusForbidden)
	resp.Body.Close()
}

func TestAuditFilterByBucket(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkta", "", testPrincipalHeader, s3api.IAMRootPrincipal), 200)
	h.mustStatus(h.doString("PUT", "/bktb", "", testPrincipalHeader, s3api.IAMRootPrincipal), 200)
	a, _ := h.store.GetBucket(context.Background(), "bkta")
	b, _ := h.store.GetBucket(context.Background(), "bktb")
	now := time.Now().UTC()
	enqueueAudit(t, h, a.ID, "bkta", "alice", "PutObject", now.Add(-2*time.Hour))
	enqueueAudit(t, h, b.ID, "bktb", "alice", "PutObject", now.Add(-1*time.Hour))

	resp := h.doString("GET", "/?audit&bucket=bkta", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type: %q", got)
	}
	page := decodeAuditPage(t, resp)
	if len(page.Records) != 1 || page.Records[0].Bucket != "bkta" {
		t.Fatalf("bucket filter: %+v", page.Records)
	}
}

func TestAuditFilterByPrincipal(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, s3api.IAMRootPrincipal), 200)
	b, _ := h.store.GetBucket(context.Background(), "bkt")
	now := time.Now().UTC()
	enqueueAudit(t, h, b.ID, "bkt", "alice", "PutObject", now.Add(-2*time.Hour))
	enqueueAudit(t, h, b.ID, "bkt", "bob", "DeleteObject", now.Add(-1*time.Hour))

	resp := h.doString("GET", "/?audit&principal=alice", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	page := decodeAuditPage(t, resp)
	if len(page.Records) != 1 || page.Records[0].Principal != "alice" {
		t.Fatalf("principal filter: %+v", page.Records)
	}
}

func TestAuditFilterByTimeWindow(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, s3api.IAMRootPrincipal), 200)
	b, _ := h.store.GetBucket(context.Background(), "bkt")
	now := time.Now().UTC()
	enqueueAudit(t, h, b.ID, "bkt", "alice", "OldPut", now.Add(-3*time.Hour))
	enqueueAudit(t, h, b.ID, "bkt", "alice", "MidPut", now.Add(-90*time.Minute))
	enqueueAudit(t, h, b.ID, "bkt", "alice", "NewPut", now.Add(-15*time.Minute))

	startISO := now.Add(-2 * time.Hour).Format(time.RFC3339)
	endISO := now.Add(-30 * time.Minute).Format(time.RFC3339)
	path := fmt.Sprintf("/?audit&start=%s&end=%s", startISO, endISO)
	resp := h.doString("GET", path, "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	page := decodeAuditPage(t, resp)
	if len(page.Records) != 1 || page.Records[0].Action != "MidPut" {
		t.Fatalf("time filter: %+v", page.Records)
	}
}

func TestAuditFilterCombined(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkta", "", testPrincipalHeader, s3api.IAMRootPrincipal), 200)
	h.mustStatus(h.doString("PUT", "/bktb", "", testPrincipalHeader, s3api.IAMRootPrincipal), 200)
	a, _ := h.store.GetBucket(context.Background(), "bkta")
	b, _ := h.store.GetBucket(context.Background(), "bktb")
	now := time.Now().UTC()
	enqueueAudit(t, h, a.ID, "bkta", "alice", "PutObject", now.Add(-2*time.Hour))
	enqueueAudit(t, h, a.ID, "bkta", "bob", "PutObject", now.Add(-1*time.Hour))
	enqueueAudit(t, h, b.ID, "bktb", "alice", "PutObject", now.Add(-90*time.Minute))

	resp := h.doString("GET", "/?audit&bucket=bkta&principal=alice", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	page := decodeAuditPage(t, resp)
	if len(page.Records) != 1 || page.Records[0].Principal != "alice" || page.Records[0].Bucket != "bkta" {
		t.Fatalf("combined filter: %+v", page.Records)
	}
}

func TestAuditPaginationRoundTrip(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", "", testPrincipalHeader, s3api.IAMRootPrincipal), 200)
	b, _ := h.store.GetBucket(context.Background(), "bkt")
	base := time.Now().UTC().Add(-1 * time.Hour)
	for i := range 5 {
		enqueueAudit(t, h, b.ID, "bkt", "alice", fmt.Sprintf("Put%d", i), base.Add(time.Duration(i)*time.Minute))
	}
	collected := []string{}
	cont := ""
	for round := range 5 {
		path := "/?audit&bucket=bkt&limit=2"
		if cont != "" {
			path += "&continuation=" + cont
		}
		resp := h.doString("GET", path, "", testPrincipalHeader, s3api.IAMRootPrincipal)
		h.mustStatus(resp, http.StatusOK)
		page := decodeAuditPage(t, resp)
		for _, rec := range page.Records {
			collected = append(collected, rec.Action)
		}
		if page.NextContinuationToken == "" {
			break
		}
		cont = page.NextContinuationToken
		_ = round
	}
	if len(collected) != 5 {
		t.Fatalf("collected=%v len=%d want 5", collected, len(collected))
	}
	// Newest-first: Put4..Put0.
	want := []string{"Put4", "Put3", "Put2", "Put1", "Put0"}
	for i, a := range want {
		if collected[i] != a {
			t.Fatalf("page order at %d got=%q want=%q (full=%v)", i, collected[i], a, collected)
		}
	}
}

func TestAuditUnknownBucket(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("GET", "/?audit&bucket=nope", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestAuditInvalidLimit(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("GET", "/?audit&limit=abc", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()
	resp = h.doString("GET", "/?audit&limit=0", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestAuditInvalidTime(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("GET", "/?audit&start=not-a-time", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()
}
