package adminapi

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/auditstream"
	"github.com/danchupin/strata/internal/meta"
)

func newAuditStreamServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.AuditStream = auditstream.New(nil, nil)
	s.AuditStreamKeepAliveInterval = 50 * time.Millisecond
	return s
}

// readSSEUntil scans the SSE response body and returns the first frame whose
// "data:" payload contains needle. Returns the raw frame on match.
func readSSEUntil(t *testing.T, body io.Reader, needle string, deadline time.Duration) string {
	t.Helper()
	r := bufio.NewReader(body)
	type result struct {
		frame string
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		var buf strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				ch <- result{err: err}
				return
			}
			buf.WriteString(line)
			if line == "\n" {
				frame := buf.String()
				buf.Reset()
				if strings.Contains(frame, needle) {
					ch <- result{frame: frame}
					return
				}
			}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read sse: %v", r.err)
		}
		return r.frame
	case <-time.After(deadline):
		t.Fatalf("timeout waiting for SSE frame containing %q", needle)
		return ""
	}
}

func TestAuditStreamServiceUnavailableWhenBroadcasterMissing(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/audit/stream", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAuditStreamEmitsDataFrameForPublishedRow(t *testing.T) {
	s := newAuditStreamServer(t)
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/admin/v1/audit/stream", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("content-type=%q want text/event-stream", got)
	}

	// Wait until the subscriber has registered before publishing — otherwise
	// the publish racing the subscribe could be missed and the test deadlocks.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if s.AuditStream.SubscriberCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.AuditStream.SubscriberCount() != 1 {
		t.Fatalf("subscriber not registered")
	}

	s.AuditStream.Publish(&meta.AuditEvent{
		BucketID:  uuid.Nil,
		Bucket:    "b1",
		Action:    "PutObject",
		Principal: "alice",
		Resource:  "/b1/key",
		Result:    "200",
		RequestID: "req-1",
		Time:      time.Unix(1_700_000_000, 0).UTC(),
	})

	frame := readSSEUntil(t, resp.Body, "PutObject", 2*time.Second)
	if !strings.HasPrefix(frame, "data: ") {
		t.Errorf("frame missing data: prefix; got %q", frame)
	}
	if !strings.HasSuffix(frame, "\n\n") {
		t.Errorf("frame missing terminator; got %q", frame)
	}
	if !strings.Contains(frame, `"action":"PutObject"`) {
		t.Errorf("frame body missing action: %q", frame)
	}
	if !strings.Contains(frame, `"principal":"alice"`) {
		t.Errorf("frame body missing principal: %q", frame)
	}
}

func TestAuditStreamEmitsKeepAlivePings(t *testing.T) {
	s := newAuditStreamServer(t)
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/admin/v1/audit/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	r := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("read: %v", err)
		}
		if strings.HasPrefix(line, ":keep-alive") {
			return
		}
	}
	t.Fatalf("no keep-alive received")
}

func TestAuditStreamFilterServerSide(t *testing.T) {
	s := newAuditStreamServer(t)
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/admin/v1/audit/stream?action=DeleteObject", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if s.AuditStream.SubscriberCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	s.AuditStream.Publish(&meta.AuditEvent{Action: "PutObject", Principal: "alice"})
	s.AuditStream.Publish(&meta.AuditEvent{Action: "DeleteObject", Principal: "alice"})

	frame := readSSEUntil(t, resp.Body, "DeleteObject", 2*time.Second)
	if strings.Contains(frame, "PutObject") {
		t.Errorf("filter leaked PutObject into DeleteObject stream: %q", frame)
	}
}
