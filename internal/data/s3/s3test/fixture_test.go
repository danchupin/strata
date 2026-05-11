package s3test_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	s3 "github.com/danchupin/strata/internal/data/s3"
	"github.com/danchupin/strata/internal/data/s3/s3test"
)

func newCountingServer(t *testing.T, c *countingRT) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c.bump()
		w.Header().Set("ETag", `"counting-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestNewFixtureDefaults pins the zero-option contract: NewFixture
// returns a wired Backend, an httptest.Server, and the default
// (clusterID, className, bucket) triple.
func TestNewFixtureDefaults(t *testing.T) {
	f := s3test.NewFixture(t)
	if f.Backend == nil {
		t.Fatal("Backend must be non-nil")
	}
	if f.Server == nil {
		t.Fatal("Server must be non-nil for the default fixture")
	}
	if f.ClusterID != "primary" {
		t.Errorf("ClusterID: want primary, got %q", f.ClusterID)
	}
	if f.ClassName != "STANDARD" {
		t.Errorf("ClassName: want STANDARD, got %q", f.ClassName)
	}
	if !strings.HasPrefix(f.Bucket, "strata-test-") {
		t.Errorf("Bucket should default to strata-test-<random>, got %q", f.Bucket)
	}
}

// TestNewFixtureDefaultEndpointAcceptsPut drives a PutChunks against
// the auto-spawned httptest.Server. The default handler returns 200 OK
// + ETag, so the SDK happy path runs without per-test transport wiring.
func TestNewFixtureDefaultEndpointAcceptsPut(t *testing.T) {
	f := s3test.NewFixture(t)
	m, err := f.Backend.PutChunks(context.Background(), strings.NewReader("payload"), f.ClassName)
	if err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	if m.BackendRef == nil || m.BackendRef.ETag == "" {
		t.Fatalf("PutChunks must round-trip a BackendRef with non-empty ETag, got %+v", m.BackendRef)
	}
	if m.Class != f.ClassName {
		t.Errorf("Manifest.Class: want %q, got %q", f.ClassName, m.Class)
	}
}

// TestNewFixtureWithRoundTripper proves WithRoundTripper takes over the
// transport — no httptest.Server is spawned, and the Backend funnels SDK
// traffic through the supplied http.RoundTripper.
func TestNewFixtureWithRoundTripper(t *testing.T) {
	rt := &countingRT{}
	f := s3test.NewFixture(t, s3test.WithRoundTripper(rt))
	if f.Server != nil {
		t.Fatal("Server must be nil when WithRoundTripper takes over")
	}
	if _, err := f.Backend.PutChunks(context.Background(), strings.NewReader("x"), f.ClassName); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	if rt.count() == 0 {
		t.Fatal("custom transport never saw a request")
	}
}

// TestNewFixtureWithExtraClass extends the default fixture with a second
// class on the same cluster. Both classes must resolve to PutChunks-
// addressable buckets.
func TestNewFixtureWithExtraClass(t *testing.T) {
	f := s3test.NewFixture(t, s3test.WithClass("COLD", "primary", "cold-tier"))

	for _, class := range []string{f.ClassName, "COLD"} {
		m, err := f.Backend.PutChunks(context.Background(), strings.NewReader("x"), class)
		if err != nil {
			t.Fatalf("PutChunks(%s): %v", class, err)
		}
		if m.Class != class {
			t.Errorf("Manifest.Class: want %q, got %q", class, m.Class)
		}
	}
}

// TestNewFixtureWithCluster wires a second cluster and proves traffic
// routes to the right endpoint per class. Each cluster gets its own
// counting handler so leakage is visible.
func TestNewFixtureWithCluster(t *testing.T) {
	primaryHits := &countingRT{}
	euHits := &countingRT{}

	primaryHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.bump()
		w.Header().Set("ETag", `"primary-etag"`)
		w.WriteHeader(http.StatusOK)
	})
	// Default fixture handler instrumented to count.
	euServer := newCountingServer(t, euHits)

	f := s3test.NewFixture(t,
		s3test.WithHandler(primaryHandler),
		s3test.WithCluster("eu", s3.S3ClusterSpec{
			Endpoint:       euServer.URL,
			Region:         "eu-west-1",
			ForcePathStyle: true,
			Credentials:    s3.CredentialsRef{Type: s3.CredentialsChain},
		}),
		s3test.WithClass("HOT", "eu", "hot-bucket"),
	)

	if _, err := f.Backend.PutChunks(context.Background(), strings.NewReader("x"), "HOT"); err != nil {
		t.Fatalf("PutChunks(HOT): %v", err)
	}
	if primaryHits.count() != 0 {
		t.Errorf("HOT class leaked to primary handler (%d hits)", primaryHits.count())
	}
	if euHits.count() == 0 {
		t.Error("HOT class never reached the eu cluster")
	}

	primaryHits.reset()
	euHits.reset()
	if _, err := f.Backend.PutChunks(context.Background(), strings.NewReader("x"), f.ClassName); err != nil {
		t.Fatalf("PutChunks(STANDARD): %v", err)
	}
	if primaryHits.count() == 0 {
		t.Error("STANDARD class never reached the primary handler")
	}
	if euHits.count() != 0 {
		t.Errorf("STANDARD class leaked to eu (%d hits)", euHits.count())
	}
}

type countingRT struct {
	mu sync.Mutex
	n  int
}

func (c *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	c.bump()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Etag": []string{`"rt-etag"`}},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

func (c *countingRT) bump()         { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *countingRT) reset()        { c.mu.Lock(); c.n = 0; c.mu.Unlock() }
func (c *countingRT) count() int    { c.mu.Lock(); defer c.mu.Unlock(); return c.n }
