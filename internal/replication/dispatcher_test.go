package replication

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/danchupin/strata/internal/meta"
)

type recordedReq struct {
	method  string
	path    string
	body    []byte
	headers http.Header
}

type fakePeer struct {
	status atomic.Int32
	mu     atomic.Pointer[recordedReq]
}

func (p *fakePeer) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("peer read body: %v", err)
		}
		hdr := r.Header.Clone()
		p.mu.Store(&recordedReq{method: r.Method, path: r.URL.Path, body: body, headers: hdr})
		s := int(p.status.Load())
		if s == 0 {
			s = 200
		}
		w.WriteHeader(s)
	})
}

func (p *fakePeer) last() *recordedReq { return p.mu.Load() }

type closingReader struct {
	*strings.Reader
	closed atomic.Bool
}

func (c *closingReader) Close() error {
	c.closed.Store(true)
	return nil
}

func newSrc(body string) (*Source, *closingReader) {
	r := &closingReader{Reader: strings.NewReader(body)}
	return &Source{
		Body:        r,
		Size:        int64(len(body)),
		ContentType: "application/octet-stream",
	}, r
}

func TestHTTPDispatcherPUTsToPeerAndReadsBody(t *testing.T) {
	peer := &fakePeer{}
	ts := httptest.NewServer(peer.handler(t))
	defer ts.Close()

	endpoint := strings.TrimPrefix(ts.URL, "http://")
	d := &HTTPDispatcher{Client: ts.Client(), Scheme: "http"}

	src, closer := newSrc("hello world")
	evt := meta.ReplicationEvent{
		Key:                 "logs/2026/04.txt",
		VersionID:           "v123",
		RuleID:              "logs",
		DestinationBucket:   "arn:aws:s3:::dest",
		DestinationEndpoint: endpoint,
	}
	if err := d.Send(context.Background(), evt, src); err != nil {
		t.Fatalf("dispatcher send: %v", err)
	}
	if !closer.closed.Load() {
		t.Fatalf("dispatcher must close source body")
	}
	got := peer.last()
	if got == nil {
		t.Fatalf("peer never saw a request")
	}
	if got.method != http.MethodPut {
		t.Fatalf("method=%q want PUT", got.method)
	}
	if got.path != "/dest/logs/2026/04.txt" {
		t.Fatalf("path=%q want /dest/logs/2026/04.txt", got.path)
	}
	if string(got.body) != "hello world" {
		t.Fatalf("body=%q", string(got.body))
	}
	if v := got.headers.Get("x-amz-replication-source-version-id"); v != "v123" {
		t.Fatalf("version header=%q", v)
	}
	if v := got.headers.Get("x-amz-replication-rule-id"); v != "logs" {
		t.Fatalf("rule header=%q", v)
	}
}

func TestHTTPDispatcherNon2xxReturnsError(t *testing.T) {
	peer := &fakePeer{}
	peer.status.Store(503)
	ts := httptest.NewServer(peer.handler(t))
	defer ts.Close()

	d := &HTTPDispatcher{Client: ts.Client(), Scheme: "http"}
	src, _ := newSrc("hi")
	evt := meta.ReplicationEvent{
		Key:                 "k",
		DestinationBucket:   "dest",
		DestinationEndpoint: strings.TrimPrefix(ts.URL, "http://"),
	}
	err := d.Send(context.Background(), evt, src)
	if err == nil {
		t.Fatalf("expected error for 503, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("error should mention 503: %v", err)
	}
}

func TestHTTPDispatcherMissingEndpointFails(t *testing.T) {
	d := &HTTPDispatcher{}
	src, closer := newSrc("x")
	evt := meta.ReplicationEvent{Key: "k", DestinationBucket: "dest"}
	err := d.Send(context.Background(), evt, src)
	if err == nil {
		t.Fatalf("expected endpoint-missing error")
	}
	if !errors.Is(err, err) {
		t.Fatalf("error type")
	}
	if !closer.closed.Load() {
		t.Fatalf("body must be closed even on early-return")
	}
}

func TestHTTPDispatcherMissingBucketFails(t *testing.T) {
	d := &HTTPDispatcher{}
	src, _ := newSrc("x")
	evt := meta.ReplicationEvent{Key: "k", DestinationEndpoint: "peer:443"}
	if err := d.Send(context.Background(), evt, src); err == nil {
		t.Fatalf("expected bucket-missing error")
	}
}

func TestStripBucketARN(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"arn:aws:s3:::dest", "dest"},
		{"plain-name", "plain-name"},
		{"arn:aws:s3:::with/slash", "with/slash"},
	} {
		if got := stripBucketARN(tc.in); got != tc.want {
			t.Fatalf("stripBucketARN(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}
