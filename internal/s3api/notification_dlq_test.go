package s3api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

func enqueueDLQ(t *testing.T, h *notifyHarness, bucket, key, eventID string, attempts int, reason string, payload []byte) {
	t.Helper()
	b, err := h.store.GetBucket(context.Background(), bucket)
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	entry := &meta.NotificationDLQEntry{
		NotificationEvent: meta.NotificationEvent{
			BucketID:   b.ID,
			Bucket:     bucket,
			Key:        key,
			EventID:    eventID,
			EventName:  "s3:ObjectCreated:Put",
			EventTime:  time.Now().UTC(),
			ConfigID:   "OnPut",
			TargetType: "topic",
			TargetARN:  "arn:aws:sns:us-east-1:123:t",
			Payload:    payload,
		},
		Attempts:   attempts,
		Reason:     reason,
		EnqueuedAt: time.Now().UTC(),
	}
	if err := h.store.EnqueueNotificationDLQ(context.Background(), entry); err != nil {
		t.Fatalf("enqueue dlq: %v", err)
	}
}

func TestNotificationDLQAnonymousDenied(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("GET", "/?notify-dlq&bucket=bkt", "")
	h.mustStatus(resp, http.StatusForbidden)
}

func TestNotificationDLQNonRootPrincipalDenied(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("GET", "/?notify-dlq&bucket=bkt", "", testPrincipalHeader, "alice")
	h.mustStatus(resp, http.StatusForbidden)
}

func TestNotificationDLQMissingBucketParam(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("GET", "/?notify-dlq", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusBadRequest)
}

func TestNotificationDLQUnknownBucket(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("GET", "/?notify-dlq&bucket=nope", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusNotFound)
}

func TestNotificationDLQEnumeratesRows(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enqueueDLQ(t, h, "bkt", "img/a.png", "ev-1", 6, "503 from webhook", []byte(`{"Records":[{"eventName":"s3:ObjectCreated:Put"}]}`))
	enqueueDLQ(t, h, "bkt", "img/b.png", "ev-2", 6, "connection refused", []byte(`{"Records":[{"eventName":"s3:ObjectCreated:Put"}]}`))
	enqueueDLQ(t, h, "bkt", "img/c.png", "ev-3", 1, "no sink registered for target", []byte(`{"Records":[{"eventName":"s3:ObjectCreated:Put"}]}`))

	resp := h.doString("GET", "/?notify-dlq&bucket=bkt", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type: %q", got)
	}
	var body struct {
		Records []struct {
			BucketID   string          `json:"bucket_id"`
			Bucket     string          `json:"bucket"`
			Key        string          `json:"key"`
			EventID    string          `json:"event_id"`
			EventName  string          `json:"event_name"`
			TargetARN  string          `json:"target_arn"`
			Payload    json.RawMessage `json:"payload"`
			Attempts   int             `json:"attempts"`
			Reason     string          `json:"reason"`
			EnqueuedAt time.Time       `json:"enqueued_at"`
		} `json:"records"`
		NextContinuationToken string `json:"next_continuation_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if len(body.Records) != 3 {
		t.Fatalf("records=%d want 3", len(body.Records))
	}
	if body.NextContinuationToken != "" {
		t.Fatalf("unexpected continuation: %q", body.NextContinuationToken)
	}
	first := body.Records[0]
	if first.Bucket != "bkt" || first.Key != "img/a.png" || first.EventID != "ev-1" {
		t.Fatalf("first record: %+v", first)
	}
	if first.Attempts != 6 || first.Reason != "503 from webhook" {
		t.Fatalf("attempts/reason: %+v", first)
	}
	if first.TargetARN != "arn:aws:sns:us-east-1:123:t" {
		t.Fatalf("target arn: %q", first.TargetARN)
	}
	var inner struct {
		Records []struct {
			EventName string `json:"eventName"`
		}
	}
	if err := json.Unmarshal(first.Payload, &inner); err != nil {
		t.Fatalf("payload not preserved as JSON: %v", err)
	}
	if len(inner.Records) != 1 || inner.Records[0].EventName != "s3:ObjectCreated:Put" {
		t.Fatalf("payload contents: %+v", inner)
	}
}

func TestNotificationDLQPaginationContinuation(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	for i := range 5 {
		enqueueDLQ(t, h, "bkt", fmt.Sprintf("k-%d", i), fmt.Sprintf("ev-%d", i), 6, "boom", []byte(`{}`))
	}
	type page struct {
		Records []struct {
			EventID string `json:"event_id"`
		} `json:"records"`
		NextContinuationToken string `json:"next_continuation_token"`
	}

	collected := []string{}
	cont := ""
	for round := range 5 {
		path := "/?notify-dlq&bucket=bkt&limit=2"
		if cont != "" {
			path += "&continuation=" + cont
		}
		resp := h.doString("GET", path, "", testPrincipalHeader, s3api.IAMRootPrincipal)
		h.mustStatus(resp, http.StatusOK)
		var p page
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatalf("decode round %d: %v", round, err)
		}
		resp.Body.Close()
		for _, r := range p.Records {
			collected = append(collected, r.EventID)
		}
		if p.NextContinuationToken == "" {
			break
		}
		cont = p.NextContinuationToken
	}
	if len(collected) != 5 {
		t.Fatalf("collected=%v (len=%d) want 5 ids", collected, len(collected))
	}
	want := []string{"ev-0", "ev-1", "ev-2", "ev-3", "ev-4"}
	for i, id := range want {
		if collected[i] != id {
			t.Fatalf("page order at %d: got %q want %q (full=%v)", i, collected[i], id, collected)
		}
	}
}

func TestNotificationDLQLimitClampedAndInvalid(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enqueueDLQ(t, h, "bkt", "k", "ev-1", 6, "boom", []byte(`{}`))

	resp := h.doString("GET", "/?notify-dlq&bucket=bkt&limit=abc", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()

	resp = h.doString("GET", "/?notify-dlq&bucket=bkt&limit=0", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()
}
