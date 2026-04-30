package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/danchupin/strata/internal/data"
)

// TestStubReturnsErrUnsupported pins the US-001 acceptance: a New() stub
// must satisfy data.Backend and surface errors.ErrUnsupported on every
// mutating method until the real implementation lands.
func TestStubReturnsErrUnsupported(t *testing.T) {
	b := New()

	var _ data.Backend = b

	ctx := context.Background()

	if _, err := b.PutChunks(ctx, bytes.NewReader(nil), "STANDARD"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("PutChunks: want errors.ErrUnsupported, got %v", err)
	}
	if _, err := b.GetChunks(ctx, &data.Manifest{}, 0, 0); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("GetChunks: want errors.ErrUnsupported, got %v", err)
	}
	if err := b.Delete(ctx, &data.Manifest{}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Delete: want errors.ErrUnsupported, got %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: want nil, got %v", err)
	}
}

// TestStubPutReturnsErrUnsupported guards the US-002 streaming Put: a
// stub Backend (no Open) must not silently succeed — callers without a
// live S3 client get errors.ErrUnsupported.
func TestStubPutReturnsErrUnsupported(t *testing.T) {
	b := New()

	_, err := b.Put(context.Background(), "k", bytes.NewReader(nil), 0)
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Put: want errors.ErrUnsupported, got %v", err)
	}
}

// TestStubGetReturnsErrUnsupported guards US-003: a stub Backend (no
// Open) must surface errors.ErrUnsupported on Get / GetRange — never
// silently succeed.
func TestStubGetReturnsErrUnsupported(t *testing.T) {
	b := New()
	ctx := context.Background()

	if _, err := b.Get(ctx, "k"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Get: want errors.ErrUnsupported, got %v", err)
	}
	if _, err := b.GetRange(ctx, "k", 0, 1); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("GetRange: want errors.ErrUnsupported, got %v", err)
	}
}

// TestGetRangeValidatesArguments pins US-003 input validation: negative
// offset or non-positive length is a programmer error and must fail
// before any network call.
func TestGetRangeValidatesArguments(t *testing.T) {
	b := &Backend{bucket: "b", client: &awss3.Client{}}
	ctx := context.Background()

	if _, err := b.GetRange(ctx, "k", -1, 1); err == nil {
		t.Fatal("GetRange with negative offset: want error, got nil")
	}
	if _, err := b.GetRange(ctx, "k", 0, 0); err == nil {
		t.Fatal("GetRange with zero length: want error, got nil")
	}
	if _, err := b.GetRange(ctx, "k", 0, -5); err == nil {
		t.Fatal("GetRange with negative length: want error, got nil")
	}
}

// TestStubDeleteObjectReturnsErrUnsupported guards US-004: the stub
// Backend (no Open) must surface errors.ErrUnsupported on DeleteObject /
// DeleteBatch — never silently succeed against an absent client.
func TestStubDeleteObjectReturnsErrUnsupported(t *testing.T) {
	b := New()
	ctx := context.Background()

	if err := b.DeleteObject(ctx, "k", ""); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("DeleteObject: want errors.ErrUnsupported, got %v", err)
	}
	if err := b.DeleteObject(ctx, "k", "v"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("DeleteObject(versioned): want errors.ErrUnsupported, got %v", err)
	}
	if _, err := b.DeleteBatch(ctx, []ObjectRef{{Key: "k"}}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("DeleteBatch: want errors.ErrUnsupported, got %v", err)
	}
}

// TestDeleteBatchEmpty is a no-op: empty refs returns (nil, nil) without
// touching the network — works on stub and live Backend alike.
func TestDeleteBatchEmpty(t *testing.T) {
	b := &Backend{bucket: "b", client: &awss3.Client{}}
	failures, err := b.DeleteBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("DeleteBatch(nil): want nil error, got %v", err)
	}
	if failures != nil {
		t.Fatalf("DeleteBatch(nil): want nil failures, got %v", failures)
	}
}

// TestOpenValidatesRequiredConfig pins the US-002 fail-fast contract:
// missing bucket / region must error at construction, not at first Put.
func TestOpenValidatesRequiredConfig(t *testing.T) {
	ctx := context.Background()

	if _, err := Open(ctx, Config{Region: "us-east-1"}); err == nil {
		t.Fatal("Open with empty bucket: want error, got nil")
	}
	if _, err := Open(ctx, Config{Bucket: "x"}); err == nil {
		t.Fatal("Open with empty region: want error, got nil")
	}
}

// TestOpenSkipProbeAvoidsNetwork pins the US-005 SkipProbe escape hatch:
// callers that don't want the boot-time writability probe (mostly tests)
// can flip it off and Open returns a live Backend without any HTTP
// round-trip to the configured endpoint.
func TestOpenSkipProbeAvoidsNetwork(t *testing.T) {
	ctx := context.Background()
	// Endpoint points at a port no one is listening on. With SkipProbe=true
	// Open must not connect; with SkipProbe=false (covered by integration
	// tests against MinIO) it would fail.
	cfg := Config{
		Bucket:         "strata-test",
		Region:         "us-east-1",
		Endpoint:       "http://127.0.0.1:1",
		AccessKey:      "ak",
		SecretKey:      "sk",
		ForcePathStyle: true,
		SkipProbe:      true,
	}
	b, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open with SkipProbe=true: want nil error, got %v", err)
	}
	if b == nil {
		t.Fatal("Open returned nil backend with no error")
	}
	if b.client == nil {
		t.Fatal("Open returned backend with nil client")
	}
}

// TestProbeStubReturnsErrUnsupported guards Probe on a New() stub: with
// no live client, Probe must surface errors.ErrUnsupported — never
// silently no-op.
func TestProbeStubReturnsErrUnsupported(t *testing.T) {
	b := New()
	if err := b.Probe(context.Background()); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Probe on stub: want errors.ErrUnsupported, got %v", err)
	}
}

// TestPutRetriesOn503ThenSucceeds pins US-006 retry behaviour: the SDK's
// adaptive retryer must absorb 503 SlowDown responses on the first two
// PutObject attempts and surface only the third (200) success to the
// gateway. No real network — uses an in-process http.RoundTripper that
// replays a fixed response sequence.
func TestPutRetriesOn503ThenSucceeds(t *testing.T) {
	ctx := context.Background()

	seq := &sequenceTransport{
		responses: []responseFn{
			slowDownResponse,
			slowDownResponse,
			putObjectSuccessResponse,
		},
	}
	cfg := Config{
		Bucket:         "strata-test",
		Region:         "us-east-1",
		Endpoint:       "http://example.invalid",
		AccessKey:      "ak",
		SecretKey:      "sk",
		ForcePathStyle: true,
		SkipProbe:      true,
		HTTPClient:     &http.Client{Transport: seq},
		// MaxRetries left at default (5) — 3 attempts fits.
	}

	b, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	res, err := b.Put(ctx, "k", strings.NewReader("payload"), 7)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.ETag == "" {
		t.Fatalf("etag empty in result")
	}
	if got := seq.count(); got != 3 {
		t.Fatalf("transport calls: want 3, got %d", got)
	}
}

// responseFn synthesises an http.Response for the test sequence.
type responseFn func(req *http.Request) *http.Response

// sequenceTransport returns the i-th synthetic response on the i-th
// RoundTrip. Surplus calls return an error so over-retry surfaces.
type sequenceTransport struct {
	mu        sync.Mutex
	responses []responseFn
	idx       int
}

func (s *sequenceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx >= len(s.responses) {
		return nil, errors.New("sequenceTransport: out of responses")
	}
	resp := s.responses[s.idx](req)
	s.idx++
	return resp, nil
}

func (s *sequenceTransport) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idx
}

func slowDownResponse(req *http.Request) *http.Response {
	body := `<?xml version="1.0" encoding="UTF-8"?><Error><Code>SlowDown</Code><Message>backend asks the client to slow down</Message><RequestId>test</RequestId><HostId>test</HostId></Error>`
	return &http.Response{
		Status:        "503 Slow Down",
		StatusCode:    http.StatusServiceUnavailable,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/xml"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func putObjectSuccessResponse(req *http.Request) *http.Response {
	h := http.Header{}
	h.Set("ETag", `"abc123"`)
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(strings.NewReader("")),
		ContentLength: 0,
		Request:       req,
	}
}
