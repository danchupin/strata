package s3api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// accessLogHarness wires the s3api server through AccessLogMiddleware so the
// US-013 buffer hook fires per request. Exposes meta so tests can inspect
// access_log_buffer rows directly.
type accessLogHarness struct {
	t    *testing.T
	ts   *httptest.Server
	meta *metamem.Store
}

func newAccessLogHarness(t *testing.T) *accessLogHarness {
	t.Helper()
	store := metamem.New()
	api := s3api.New(datamem.New(), store)
	api.Region = "default"
	api.Master = harnessMasterProvider{}
	handler := s3api.NewAccessLogMiddleware(store, api)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &accessLogHarness{t: t, ts: ts, meta: store}
}

func (h *accessLogHarness) do(method, path, body string, headers ...string) *http.Response {
	h.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, r)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("request %s %s: %v", method, path, err)
	}
	return resp
}

func (h *accessLogHarness) status(resp *http.Response, want int) {
	h.t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		h.t.Fatalf("status: got %d want %d; body=%s", resp.StatusCode, want, string(body))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func TestAccessLogPutWithLoggingEnabledWritesRow(t *testing.T) {
	h := newAccessLogHarness(t)
	h.status(h.do("PUT", "/bkt", ""), 200)
	h.status(h.do("PUT", "/bkt?logging=", loggingEnabledXML), 200)

	// Write an object — should be logged.
	h.status(h.do("PUT", "/bkt/key.txt", "hello",
		"X-Request-Id", "req-abc",
		"User-Agent", "aws-cli/2.15",
		"Referer", "https://example.test/"), 200)

	b, err := h.meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	rows, err := h.meta.ListPendingAccessLog(context.Background(), b.ID, 100)
	if err != nil {
		t.Fatalf("list access log: %v", err)
	}
	// PUT /bkt happens before logging is configured (no row).
	// PUT ?logging= and PUT /bkt/key.txt both happen with logging set → 2 rows.
	if len(rows) != 2 {
		t.Fatalf("got %d access log rows, want 2", len(rows))
	}
	// Find the object PUT row.
	var got *struct {
		Op, Key, RequestID, UserAgent, Referrer string
		Status                                  int
		ObjectSize                              int64
	}
	for _, r := range rows {
		if r.Key == "key.txt" {
			got = &struct {
				Op, Key, RequestID, UserAgent, Referrer string
				Status                                  int
				ObjectSize                              int64
			}{r.Op, r.Key, r.RequestID, r.UserAgent, r.Referrer, r.Status, r.ObjectSize}
		}
	}
	if got == nil {
		t.Fatalf("no row for key.txt: %+v", rows)
	}
	if got.Op != "REST.PUT.OBJECT" {
		t.Fatalf("op: got %q want REST.PUT.OBJECT", got.Op)
	}
	if got.Status != http.StatusOK {
		t.Fatalf("status: %d", got.Status)
	}
	if got.RequestID != "req-abc" {
		t.Fatalf("request id: %q", got.RequestID)
	}
	if got.UserAgent != "aws-cli/2.15" {
		t.Fatalf("user agent: %q", got.UserAgent)
	}
	if got.Referrer != "https://example.test/" {
		t.Fatalf("referrer: %q", got.Referrer)
	}
	if got.ObjectSize != int64(len("hello")) {
		t.Fatalf("object size: got %d want %d", got.ObjectSize, len("hello"))
	}
}

func TestAccessLogPutWithoutLoggingEnabledNoRow(t *testing.T) {
	h := newAccessLogHarness(t)
	h.status(h.do("PUT", "/bkt", ""), 200)
	h.status(h.do("PUT", "/bkt/key.txt", "hello"), 200)

	b, err := h.meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	rows, err := h.meta.ListPendingAccessLog(context.Background(), b.ID, 100)
	if err != nil {
		t.Fatalf("list access log: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no rows without logging configured, got %d: %+v", len(rows), rows)
	}
}

func TestAccessLogGetEchoesObjectSizeFromContentLength(t *testing.T) {
	h := newAccessLogHarness(t)
	h.status(h.do("PUT", "/bkt", ""), 200)
	h.status(h.do("PUT", "/bkt?logging=", loggingEnabledXML), 200)
	h.status(h.do("PUT", "/bkt/blob", "0123456789"), 200)

	resp := h.do("GET", "/bkt/blob", "")
	h.status(resp, 200)

	b, err := h.meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	rows, err := h.meta.ListPendingAccessLog(context.Background(), b.ID, 100)
	if err != nil {
		t.Fatalf("list access log: %v", err)
	}
	var get *struct {
		Op         string
		BytesSent  int64
		ObjectSize int64
	}
	for _, r := range rows {
		if r.Op == "REST.GET.OBJECT" {
			get = &struct {
				Op         string
				BytesSent  int64
				ObjectSize int64
			}{r.Op, r.BytesSent, r.ObjectSize}
		}
	}
	if get == nil {
		t.Fatalf("no GET object row: %+v", rows)
	}
	if get.BytesSent != 10 {
		t.Fatalf("bytes_sent: got %d want 10", get.BytesSent)
	}
	if get.ObjectSize != 10 {
		t.Fatalf("object_size: got %d want 10", get.ObjectSize)
	}
}

func TestAccessLogSubresourceOpDerivation(t *testing.T) {
	h := newAccessLogHarness(t)
	h.status(h.do("PUT", "/bkt", ""), 200)
	h.status(h.do("PUT", "/bkt?logging=", loggingEnabledXML), 200)

	// GET ?policy on a bucket without policy yields 404; the row is still logged.
	h.status(h.do("GET", "/bkt?policy", ""), 404)

	b, err := h.meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	rows, err := h.meta.ListPendingAccessLog(context.Background(), b.ID, 100)
	if err != nil {
		t.Fatalf("list access log: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Op == "REST.GET.POLICY" && r.Status == 404 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected REST.GET.POLICY row with status 404, got: %+v", rows)
	}
}
