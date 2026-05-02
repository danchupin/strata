// Package notify ships the notification fan-out worker that drains
// meta.NotificationEvent rows from notify_queue and dispatches them to
// configured sinks (webhook in this package; SQS in US-010). Failed
// deliveries retry with exponential backoff and land in notify_dlq after
// the retry budget is exhausted.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

// SignatureHeader is the request header carrying the per-target HMAC-SHA256
// signature of the JSON body. Hex-lowercased so the consumer can compare with
// constant-time crypto/subtle.ConstantTimeCompare after hex-decode.
const SignatureHeader = "X-Strata-Signature"

// Sink is the per-target delivery interface. Implementations must be safe
// for concurrent calls — the worker may dispatch multiple events to the
// same sink in parallel in future iterations.
type Sink interface {
	Type() string
	Name() string
	Send(ctx context.Context, evt meta.NotificationEvent) error
}

// Router resolves an event to a sink. The default StaticRouter looks up by
// (TargetType + ":" + TargetARN) and falls back to TargetARN alone so
// configuration can omit the type prefix when ARNs are globally unique.
type Router interface {
	Resolve(evt meta.NotificationEvent) (Sink, bool)
}

// StaticRouter is a map-backed Router; key is "<type>:<arn>" or just "<arn>".
type StaticRouter map[string]Sink

func (r StaticRouter) Resolve(evt meta.NotificationEvent) (Sink, bool) {
	if s, ok := r[evt.TargetType+":"+evt.TargetARN]; ok {
		return s, true
	}
	if s, ok := r[evt.TargetARN]; ok {
		return s, true
	}
	return nil, false
}

// ComputeSignature returns the lowercase-hex HMAC-SHA256 of body under secret.
// Used by the webhook sink to populate SignatureHeader and exposed for tests +
// downstream consumers that want to verify with the same algorithm.
func ComputeSignature(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature constant-time-compares received signature header against
// the recomputed HMAC. False means the signature did not match (or the header
// was malformed).
func VerifySignature(secret, body []byte, header string) bool {
	expected, err := hex.DecodeString(strings.TrimSpace(header))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(expected, mac.Sum(nil))
}

// WebhookSink delivers events as JSON-bodied HTTPS POSTs to URL with
// SignatureHeader populated by ComputeSignature(Secret, body).
type WebhookSink struct {
	SinkName string
	URL      string
	Secret   []byte
	Client   *http.Client
	Timeout  time.Duration
}

func (s *WebhookSink) Type() string { return "webhook" }
func (s *WebhookSink) Name() string { return s.SinkName }

func (s *WebhookSink) Send(ctx context.Context, evt meta.NotificationEvent) error {
	if s.URL == "" {
		return errors.New("webhook: URL not configured")
	}
	client := s.Client
	if client == nil {
		timeout := s.Timeout
		if timeout == 0 {
			timeout = 10 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	body := evt.Payload
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, ComputeSignature(s.Secret, body))
	req.Header.Set("X-Strata-Event-Id", evt.EventID)
	req.Header.Set("X-Strata-Event-Name", evt.EventName)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("webhook %s returned %d: %s", s.URL, resp.StatusCode, strings.TrimSpace(string(preview)))
}
