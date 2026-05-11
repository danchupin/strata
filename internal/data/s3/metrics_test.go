package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/danchupin/strata/internal/data"
)

// TestObserveOpTotalAndRetryTotal pins the US-007 acceptance: a known
// sequence of operations produces matching counter values. Drives the
// SDK's middleware stack with synthetic HTTP responses so the assertion
// is hermetic — no MinIO dependency.
func TestObserveOpTotalAndRetryTotal(t *testing.T) {
	ctx := context.Background()
	seq := &sequenceTransport{
		responses: []responseFn{
			// 1. Put: succeeds first try → put/ok +1.
			putObjectSuccessResponse,
			// 2. Put: 503, 503, 200 → put/retried +1, put_retry +2.
			slowDownResponse,
			slowDownResponse,
			putObjectSuccessResponse,
			// 3. Get: NoSuchKey → get/error +1, no body.
			noSuchKeyResponse,
		},
	}
	b := openTestBackend(t, seq)

	putOK := snapCounter(t, opTotal.WithLabelValues("put", "ok"))
	putRetried := snapCounter(t, opTotal.WithLabelValues("put", "retried"))
	getError := snapCounter(t, opTotal.WithLabelValues("get", "error"))
	putRetries := snapCounter(t, retryTotal.WithLabelValues("put"))

	if _, err := b.Put(ctx, "k1", strings.NewReader("payload"), 7); err != nil {
		t.Fatalf("Put #1: %v", err)
	}
	if _, err := b.Put(ctx, "k2", strings.NewReader("payload"), 7); err != nil {
		t.Fatalf("Put #2 (retry): %v", err)
	}
	_, err := b.Get(ctx, "missing")
	if err == nil {
		t.Fatal("Get: expected error for NoSuchKey")
	}
	if !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("Get: want data.ErrNotFound, got %v", err)
	}

	if got := snapCounter(t, opTotal.WithLabelValues("put", "ok")) - putOK; got != 1 {
		t.Errorf("put/ok delta: want 1, got %v", got)
	}
	if got := snapCounter(t, opTotal.WithLabelValues("put", "retried")) - putRetried; got != 1 {
		t.Errorf("put/retried delta: want 1, got %v", got)
	}
	if got := snapCounter(t, opTotal.WithLabelValues("get", "error")) - getError; got != 1 {
		t.Errorf("get/error delta: want 1, got %v", got)
	}
	if got := snapCounter(t, retryTotal.WithLabelValues("put")) - putRetries; got != 2 {
		t.Errorf("retry_total{put} delta: want 2 (one retried Put with two retries), got %v", got)
	}
}

// TestRegisterMetricsIdempotent guards the package init contract: the
// public RegisterMetrics is called from Open() on every backend
// construction; double-registration must not panic the binary even
// when multiple s3.Open calls happen in the same process (e.g.
// test-suite re-entry, future multi-backend dispatch).
func TestRegisterMetricsIdempotent(t *testing.T) {
	RegisterMetrics()
	RegisterMetrics()
	RegisterMetrics()
}

// snapCounter reads a single CounterVec leaf via testutil so the test
// can compute deltas across operations without resetting the
// package-level collectors (which would corrupt other tests sharing
// the same registry state).
func snapCounter(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	return testutil.ToFloat64(c)
}

func noSuchKeyResponse(req *http.Request) *http.Response {
	body := `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message><Key>missing</Key><RequestId>test</RequestId><HostId>test</HostId></Error>`
	return &http.Response{
		Status:        "404 Not Found",
		StatusCode:    http.StatusNotFound,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/xml"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}
