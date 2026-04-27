package s3api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// auditHarness wires the s3api server through AuditMiddleware so the US-022
// row hook fires per state-changing request. Exposes meta so tests can inspect
// audit_log rows directly.
type auditHarness struct {
	t    *testing.T
	ts   *httptest.Server
	meta *metamem.Store
	ttl  time.Duration
}

func newAuditHarness(t *testing.T) *auditHarness {
	t.Helper()
	store := metamem.New()
	api := s3api.New(datamem.New(), store)
	api.Region = "default"
	api.Master = harnessMasterProvider{}
	ttl := time.Hour
	handler := s3api.NewAuditMiddleware(store, ttl, api)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &auditHarness{t: t, ts: ts, meta: store, ttl: ttl}
}

func (h *auditHarness) do(method, path, body string, headers ...string) *http.Response {
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

func (h *auditHarness) status(resp *http.Response, want int) {
	h.t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		h.t.Fatalf("status: got %d want %d; body=%s", resp.StatusCode, want, string(body))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func TestAuditEmitsRowForStateChangingObjectWrites(t *testing.T) {
	h := newAuditHarness(t)
	h.status(h.do("PUT", "/bkt", "", "X-Request-Id", "req-create-bucket"), 200)
	h.status(h.do("PUT", "/bkt/key.txt", "hello", "X-Request-Id", "req-put-obj"), 200)
	h.status(h.do("DELETE", "/bkt/key.txt", "", "X-Request-Id", "req-del-obj"), 204)

	b, err := h.meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	rows, err := h.meta.ListAudit(context.Background(), b.ID, 100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d audit rows, want 3 (CreateBucket+PutObject+DeleteObject) — %+v", len(rows), rows)
	}
	want := map[string]struct {
		action, resource, requestID string
	}{
		"req-create-bucket": {"CreateBucket", "/bkt", "req-create-bucket"},
		"req-put-obj":       {"PutObject", "/bkt/key.txt", "req-put-obj"},
		"req-del-obj":       {"DeleteObject", "/bkt/key.txt", "req-del-obj"},
	}
	for _, r := range rows {
		w, ok := want[r.RequestID]
		if !ok {
			t.Fatalf("unexpected row: %+v", r)
		}
		if r.Action != w.action {
			t.Errorf("%s action: got %q want %q", r.RequestID, r.Action, w.action)
		}
		if r.Resource != w.resource {
			t.Errorf("%s resource: got %q want %q", r.RequestID, r.Resource, w.resource)
		}
		if r.Result == "" {
			t.Errorf("%s result empty", r.RequestID)
		}
		if r.Time.IsZero() {
			t.Errorf("%s time zero", r.RequestID)
		}
	}
}

func TestAuditSkipsReadPaths(t *testing.T) {
	h := newAuditHarness(t)
	h.status(h.do("PUT", "/bkt", ""), 200)
	h.status(h.do("PUT", "/bkt/k", "hi"), 200)

	// Reset the per-bucket buffer; we only care about the GET/HEAD/list reads
	// below.
	b, err := h.meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	rows, _ := h.meta.ListAudit(context.Background(), b.ID, 100)
	beforeReads := len(rows)

	h.status(h.do("GET", "/bkt/k", ""), 200)
	h.status(h.do("HEAD", "/bkt/k", ""), 200)
	h.status(h.do("GET", "/bkt", ""), 200)
	// Root-bucket-list is anonymous + no IAM action → returns 501; skip
	// asserting on it. The relevant invariant: zero new rows for read paths.

	rowsAfter, _ := h.meta.ListAudit(context.Background(), b.ID, 100)
	if len(rowsAfter) != beforeReads {
		t.Fatalf("read paths emitted audit rows: before=%d after=%d (%+v)", beforeReads, len(rowsAfter), rowsAfter)
	}
}

func TestAuditEmitsRowForIAMActions(t *testing.T) {
	h := newAuditHarness(t)
	form := url.Values{}
	form.Set("Action", "CreateUser")
	form.Set("UserName", "alice")
	resp := h.do("POST", "/?Action=CreateUser&UserName=alice", "",
		"X-Request-Id", "req-iam-create",
		"Content-Type", "application/x-www-form-urlencoded")
	// Anonymous → 403 AccessDenied (no IAM root). The audit row must still be
	// written because the request is state-changing and reached the gateway.
	h.status(resp, http.StatusForbidden)

	rows, err := h.meta.ListAudit(context.Background(), uuid.Nil, 100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Action != "CreateUser" {
		t.Fatalf("action: got %q want CreateUser", r.Action)
	}
	if r.Resource != "iam:CreateUser" {
		t.Fatalf("resource: got %q want iam:CreateUser", r.Resource)
	}
	if r.RequestID != "req-iam-create" {
		t.Fatalf("request id: %q", r.RequestID)
	}
	if r.Result != "403" {
		t.Fatalf("result: %q want 403", r.Result)
	}
	if r.Bucket != "-" {
		t.Fatalf("bucket placeholder: %q want -", r.Bucket)
	}
}

func TestAuditTTLPurgesExpiredRows(t *testing.T) {
	store := metamem.New()
	api := s3api.New(datamem.New(), store)
	api.Region = "default"
	api.Master = harnessMasterProvider{}
	mw := &s3api.AuditMiddleware{Meta: store, Next: api, TTL: 50 * time.Millisecond}
	ts := httptest.NewServer(mw)
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/bkt", "", nil)
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	req, _ := http.NewRequest("PUT", ts.URL+"/bkt", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put bucket: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	b, err := store.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	rows, _ := store.ListAudit(context.Background(), b.ID, 100)
	if len(rows) == 0 {
		t.Fatalf("expected at least one row pre-expiry")
	}

	time.Sleep(120 * time.Millisecond)
	rows, _ = store.ListAudit(context.Background(), b.ID, 100)
	if len(rows) != 0 {
		t.Fatalf("expected zero rows after TTL, got %d: %+v", len(rows), rows)
	}
}

func TestParseAuditRetention(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", s3api.DefaultAuditRetention},
		{"30d", 30 * 24 * time.Hour},
		{"720h", 720 * time.Hour},
		{"15m", 15 * time.Minute},
	}
	for _, c := range cases {
		got, err := s3api.ParseAuditRetention(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q: got %s want %s", c.in, got, c.want)
		}
	}
	if _, err := s3api.ParseAuditRetention("garbage"); err == nil {
		t.Fatal("garbage input should error")
	}
}
