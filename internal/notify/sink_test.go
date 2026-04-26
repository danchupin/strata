package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

func TestComputeAndVerifySignature(t *testing.T) {
	secret := []byte("topsecret")
	body := []byte(`{"hello":"world"}`)
	sig := ComputeSignature(secret, body)
	if !VerifySignature(secret, body, sig) {
		t.Fatal("signature did not verify")
	}
	if VerifySignature(secret, body, sig+"00") {
		t.Fatal("malformed sig should not verify")
	}
	if VerifySignature(secret, body, "not-hex") {
		t.Fatal("non-hex header should not verify")
	}
	if VerifySignature([]byte("wrong"), body, sig) {
		t.Fatal("wrong secret should not verify")
	}
}

func TestWebhookSinkSendsAndSignsBody(t *testing.T) {
	secret := []byte("shh")
	body := []byte(`{"Records":[{"eventName":"s3:ObjectCreated:Put"}]}`)
	var receivedSig, receivedCT, receivedEventID atomic.Value
	var receivedBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		receivedBody.Store(string(got))
		receivedSig.Store(r.Header.Get(SignatureHeader))
		receivedCT.Store(r.Header.Get("Content-Type"))
		receivedEventID.Store(r.Header.Get("X-Strata-Event-Id"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := &WebhookSink{SinkName: "wh1", URL: srv.URL, Secret: secret}
	evt := meta.NotificationEvent{EventID: "evt-123", EventName: "s3:ObjectCreated:Put", Payload: body}
	if err := sink.Send(context.Background(), evt); err != nil {
		t.Fatalf("send: %v", err)
	}
	if receivedBody.Load().(string) != string(body) {
		t.Fatalf("body mismatch: %q", receivedBody.Load())
	}
	if got := receivedSig.Load().(string); !VerifySignature(secret, body, got) {
		t.Fatalf("signature header %q does not verify under secret", got)
	}
	if receivedCT.Load().(string) != "application/json" {
		t.Fatalf("content-type: %q", receivedCT.Load())
	}
	if receivedEventID.Load().(string) != "evt-123" {
		t.Fatalf("event id header: %q", receivedEventID.Load())
	}
}

func TestWebhookSinkNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream timeout"))
	}))
	defer srv.Close()
	sink := &WebhookSink{SinkName: "wh", URL: srv.URL, Secret: []byte("s")}
	err := sink.Send(context.Background(), meta.NotificationEvent{Payload: []byte(`{}`)})
	if err == nil {
		t.Fatal("expected error on 502")
	}
	if !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "upstream timeout") {
		t.Fatalf("error %q missing status/body", err.Error())
	}
}

func TestWebhookSinkContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	sink := &WebhookSink{SinkName: "wh", URL: srv.URL, Secret: []byte("s")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := sink.Send(ctx, meta.NotificationEvent{Payload: []byte(`{}`)}); err == nil {
		t.Fatal("expected ctx error")
	}
}

func TestStaticRouterResolves(t *testing.T) {
	wh := &WebhookSink{SinkName: "wh", URL: "http://x", Secret: []byte("s")}
	r := StaticRouter{
		"topic:arn:aws:sns:us-east-1:0:t":   wh,
		"arn:aws:sns:us-east-1:0:fallback":  wh,
	}
	if _, ok := r.Resolve(meta.NotificationEvent{TargetType: "topic", TargetARN: "arn:aws:sns:us-east-1:0:t"}); !ok {
		t.Fatal("type+arn lookup should hit")
	}
	if _, ok := r.Resolve(meta.NotificationEvent{TargetType: "queue", TargetARN: "arn:aws:sns:us-east-1:0:fallback"}); !ok {
		t.Fatal("arn-only fallback should hit")
	}
	if _, ok := r.Resolve(meta.NotificationEvent{TargetType: "topic", TargetARN: "missing"}); ok {
		t.Fatal("unknown should miss")
	}
}
