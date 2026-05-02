package s3api_test

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

type notifyHarness struct {
	*testHarness
	store *metamem.Store
}

func newNotifyHarness(t *testing.T) *notifyHarness {
	t.Helper()
	store := metamem.New()
	api := s3api.New(datamem.New(), store)
	api.Region = "default"
	api.Master = harnessMasterProvider{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get(testPrincipalHeader); p != "" {
			ctx := auth.WithAuth(r.Context(), &auth.AuthInfo{Owner: p, AccessKey: p})
			r = r.WithContext(ctx)
		}
		api.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &notifyHarness{
		testHarness: &testHarness{t: t, ts: ts},
		store:       store,
	}
}

func (h *notifyHarness) listEvents(bucket string) []meta.NotificationEvent {
	h.t.Helper()
	b, err := h.store.GetBucket(context.Background(), bucket)
	if err != nil {
		h.t.Fatalf("get bucket %q: %v", bucket, err)
	}
	evs, err := h.store.ListPendingNotifications(context.Background(), b.ID, 100)
	if err != nil {
		h.t.Fatalf("list notifications: %v", err)
	}
	return evs
}

const notifyTopicPrefix = `<NotificationConfiguration>
    <TopicConfiguration>
        <Id>OnPut</Id>
        <Topic>arn:aws:sns:us-east-1:123:t</Topic>
        <Event>s3:ObjectCreated:*</Event>
        <Filter>
            <S3Key>
                <FilterRule><Name>prefix</Name><Value>img/</Value></FilterRule>
            </S3Key>
        </Filter>
    </TopicConfiguration>
</NotificationConfiguration>`

const notifyTopicNoFilter = `<NotificationConfiguration>
    <TopicConfiguration>
        <Id>All</Id>
        <Topic>arn:aws:sns:us-east-1:123:t</Topic>
        <Event>s3:ObjectCreated:*</Event>
        <Event>s3:ObjectRemoved:*</Event>
    </TopicConfiguration>
</NotificationConfiguration>`

const notifyTopicSuffix = `<NotificationConfiguration>
    <TopicConfiguration>
        <Id>OnJpg</Id>
        <Topic>arn:aws:sns:us-east-1:123:t</Topic>
        <Event>s3:ObjectCreated:Put</Event>
        <Filter>
            <S3Key>
                <FilterRule><Name>suffix</Name><Value>.jpg</Value></FilterRule>
            </S3Key>
        </Filter>
    </TopicConfiguration>
</NotificationConfiguration>`

func TestNotificationPutEventEnqueued(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?notification=", notifyTopicNoFilter), 200)

	h.mustStatus(h.doString("PUT", "/bkt/file.txt", "hello"), 200)

	evs := h.listEvents("bkt")
	if len(evs) != 1 {
		t.Fatalf("got %d events want 1", len(evs))
	}
	got := evs[0]
	if got.EventName != "s3:ObjectCreated:Put" {
		t.Fatalf("eventName: %q", got.EventName)
	}
	if got.Key != "file.txt" {
		t.Fatalf("key: %q", got.Key)
	}
	if got.TargetType != "topic" || !strings.HasPrefix(got.TargetARN, "arn:aws:sns") {
		t.Fatalf("target: type=%q arn=%q", got.TargetType, got.TargetARN)
	}
	if got.ConfigID != "All" {
		t.Fatalf("configID: %q", got.ConfigID)
	}
	var payload struct {
		Records []struct {
			EventName string `json:"eventName"`
			S3        struct {
				Bucket struct {
					Name string `json:"name"`
				} `json:"bucket"`
				Object struct {
					Key  string `json:"key"`
					Size int64  `json:"size"`
				} `json:"object"`
			} `json:"s3"`
		} `json:"Records"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if len(payload.Records) != 1 {
		t.Fatalf("payload records: %d", len(payload.Records))
	}
	rec := payload.Records[0]
	if rec.EventName != "s3:ObjectCreated:Put" {
		t.Fatalf("payload eventName: %q", rec.EventName)
	}
	if rec.S3.Bucket.Name != "bkt" || rec.S3.Object.Key != "file.txt" {
		t.Fatalf("payload bucket/key: %+v", rec.S3)
	}
	if rec.S3.Object.Size != int64(len("hello")) {
		t.Fatalf("payload size: %d", rec.S3.Object.Size)
	}
}

func TestNotificationDeleteEventEnqueued(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/file.txt", "hello"), 200)
	h.mustStatus(h.doString("PUT", "/bkt?notification=", notifyTopicNoFilter), 200)

	h.mustStatus(h.doString("DELETE", "/bkt/file.txt", ""), 204)

	evs := h.listEvents("bkt")
	if len(evs) != 1 {
		t.Fatalf("got %d events want 1", len(evs))
	}
	if evs[0].EventName != "s3:ObjectRemoved:Delete" {
		t.Fatalf("eventName: %q", evs[0].EventName)
	}
	if evs[0].Key != "file.txt" {
		t.Fatalf("key: %q", evs[0].Key)
	}
}

func TestNotificationPrefixFilterMismatchSuppresses(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?notification=", notifyTopicPrefix), 200)

	h.mustStatus(h.doString("PUT", "/bkt/docs/readme.md", "x"), 200)

	if evs := h.listEvents("bkt"); len(evs) != 0 {
		t.Fatalf("want 0 events for non-matching prefix, got %d", len(evs))
	}

	h.mustStatus(h.doString("PUT", "/bkt/img/cat.png", "x"), 200)
	evs := h.listEvents("bkt")
	if len(evs) != 1 || evs[0].Key != "img/cat.png" {
		t.Fatalf("expected one event for img/cat.png, got %+v", evs)
	}
}

func TestNotificationSuffixFilterMatch(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?notification=", notifyTopicSuffix), 200)

	h.mustStatus(h.doString("PUT", "/bkt/img/cat.png", "x"), 200)
	if evs := h.listEvents("bkt"); len(evs) != 0 {
		t.Fatalf("want 0 events for non-matching suffix, got %d", len(evs))
	}

	h.mustStatus(h.doString("PUT", "/bkt/img/cat.jpg", "x"), 200)
	evs := h.listEvents("bkt")
	if len(evs) != 1 || evs[0].Key != "img/cat.jpg" {
		t.Fatalf("expected event for cat.jpg, got %+v", evs)
	}
}

func TestNotificationNoConfigNoEvents(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/file", "x"), 200)
	if evs := h.listEvents("bkt"); len(evs) != 0 {
		t.Fatalf("want 0 events without config, got %d", len(evs))
	}
}

func TestNotificationCompleteMultipartEvent(t *testing.T) {
	h := newNotifyHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?notification=", notifyTopicNoFilter), 200)

	resp := h.doString("POST", "/bkt/big?uploads=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	uploadID := mustExtractUploadID(t, body)

	resp = h.doString("PUT", "/bkt/big?partNumber=1&uploadId="+uploadID, strings.Repeat("a", 32))
	h.mustStatus(resp, 200)
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)

	complete := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`, etag)
	resp = h.doString("POST", "/bkt/big?uploadId="+uploadID, complete)
	h.mustStatus(resp, 200)

	evs := h.listEvents("bkt")
	if len(evs) != 1 {
		t.Fatalf("got %d events want 1", len(evs))
	}
	if evs[0].EventName != "s3:ObjectCreated:CompleteMultipartUpload" {
		t.Fatalf("eventName: %q", evs[0].EventName)
	}
	if evs[0].Key != "big" {
		t.Fatalf("key: %q", evs[0].Key)
	}
}

func mustExtractUploadID(t *testing.T, body string) string {
	t.Helper()
	type initResp struct {
		UploadID string `xml:"UploadId"`
	}
	var ir initResp
	if err := xml.Unmarshal([]byte(body), &ir); err != nil || ir.UploadID == "" {
		t.Fatalf("extract uploadId from %q: %v", body, err)
	}
	return ir.UploadID
}
