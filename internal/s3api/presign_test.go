package s3api_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// TestBucketBackendPresignAdminEndpoint pins the round-trip on the
// PUT/GET /<bucket>?backendPresign admin endpoint that toggles the
// per-bucket passthrough flag.
func TestBucketBackendPresignAdminEndpoint(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("GET", "/bkt?backendPresign", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "<Enabled>false</Enabled>") {
		t.Errorf("default state: got %s want Enabled=false", body)
	}

	enable := `<BackendPresignConfiguration><Enabled>true</Enabled></BackendPresignConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?backendPresign", enable), 200)

	resp = h.doString("GET", "/bkt?backendPresign", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "<Enabled>true</Enabled>") {
		t.Errorf("after enable: got %s", body)
	}

	disable := `<BackendPresignConfiguration><Enabled>false</Enabled></BackendPresignConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?backendPresign", disable), 200)
	resp = h.doString("GET", "/bkt?backendPresign", "")
	if body := h.readBody(resp); !strings.Contains(body, "<Enabled>false</Enabled>") {
		t.Errorf("after disable: got %s", body)
	}
}

func TestBackendPresignDisabledByDefaultStrataServes(t *testing.T) {
	fake := newFakePresignBackend("https://backend.example.com/test-bucket/abc?presigned=1")
	api := s3api.New(fake, metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	h := &testHarness{t: t, ts: ts}

	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "hello"), 200)

	resp := h.doString("GET", "/bkt/k?X-Amz-Signature=fake", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "hello" {
		t.Errorf("body: got %q want hello", body)
	}
	if fake.calls() != 0 {
		t.Errorf("backend presign should not be called when bucket flag is off, got %d", fake.calls())
	}
}

// TestBackendPresignPassthroughRedirectsToBackendURL pins US-016 AC: when
// the bucket flag is on, the data backend supports presign, and the request
// is presigned, the gateway 307-redirects the client to the backend URL.
// X-Amz-Expires is forwarded verbatim.
func TestBackendPresignPassthroughRedirectsToBackendURL(t *testing.T) {
	expectedURL := "https://backend.example.com/test-bucket/abc?signature=fake"
	fake := newFakePresignBackend(expectedURL)
	api := s3api.New(fake, metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	h := &testHarness{t: t, ts: ts}

	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/key", "hello"), 200)
	h.mustStatus(h.doString("PUT", "/bkt?backendPresign",
		`<BackendPresignConfiguration><Enabled>true</Enabled></BackendPresignConfiguration>`), 200)

	// Build a presigned-shaped GET; the gateway only checks for the
	// presence of X-Amz-Signature, so the test doesn't need a real
	// SigV4 query (auth middleware is off in this harness).
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequest("GET", h.ts.URL+"/bkt/key?X-Amz-Signature=fake&X-Amz-Expires=600", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("presigned GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status: got %d want 307", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != expectedURL {
		t.Errorf("Location: got %q want %q", loc, expectedURL)
	}
	if fake.calls() != 1 {
		t.Errorf("PresignGetObject calls: got %d want 1", fake.calls())
	}
	if got := fake.lastExpires(); got != 600*time.Second {
		t.Errorf("forwarded X-Amz-Expires: got %v want 600s", got)
	}
}

// TestBackendPresignNonPresignedGETStillServesInline asserts that a normal
// (non-presigned) authenticated GET keeps serving from Strata even when
// the bucket flag is on — passthrough is opt-in per request via the
// presign mechanism.
func TestBackendPresignNonPresignedGETStillServesInline(t *testing.T) {
	fake := newFakePresignBackend("https://backend.example.com/x")
	api := s3api.New(fake, metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	h := &testHarness{t: t, ts: ts}

	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "hello"), 200)
	h.mustStatus(h.doString("PUT", "/bkt?backendPresign",
		`<BackendPresignConfiguration><Enabled>true</Enabled></BackendPresignConfiguration>`), 200)

	resp := h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "hello" {
		t.Errorf("non-presigned GET body: got %q want hello", body)
	}
	if fake.calls() != 0 {
		t.Errorf("non-presigned GET must not invoke backend presign, got %d calls", fake.calls())
	}
}

// TestBackendPresignFallsBackOnMintError pins the failure-mode AC: when
// the backend errors on PresignGetObject, the gateway falls back to its
// in-process serve path so the client still gets a response.
func TestBackendPresignFallsBackOnMintError(t *testing.T) {
	fake := newFakePresignBackend("")
	fake.setErr(errors.New("backend down"))
	api := s3api.New(fake, metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	h := &testHarness{t: t, ts: ts}

	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "hello"), 200)
	h.mustStatus(h.doString("PUT", "/bkt?backendPresign",
		`<BackendPresignConfiguration><Enabled>true</Enabled></BackendPresignConfiguration>`), 200)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequest("GET", h.ts.URL+"/bkt/k?X-Amz-Signature=fake&X-Amz-Expires=600", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("presigned GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200 (fallback), body=%s", resp.StatusCode, h.readBody(resp))
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" {
		t.Errorf("fallback body: got %q want hello", body)
	}
}

// fakePresignBackend wraps an in-memory data backend with an inspectable
// PresignBackend so tests can assert call topology + forwarded args
// without a real S3 endpoint. Returns a fixed canned URL on each call.
type fakePresignBackend struct {
	mu          sync.Mutex
	inner       data.Backend
	url         string
	err         error
	callCount   int
	lastExpire  time.Duration
	lastBackend string
}

func newFakePresignBackend(url string) *fakePresignBackend {
	return &fakePresignBackend{inner: datamem.New(), url: url}
}

func (f *fakePresignBackend) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakePresignBackend) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCount
}

func (f *fakePresignBackend) lastExpires() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastExpire
}

func (f *fakePresignBackend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	m, err := f.inner.PutChunks(ctx, r, class)
	if err != nil {
		return nil, err
	}
	// Fake a BackendRef so the gateway's passthrough check passes — the
	// inner memory backend produces chunks-shape manifests; the gateway
	// requires BackendRef to mint a backend URL.
	if m.BackendRef == nil {
		m.BackendRef = &data.BackendRef{
			Backend: "s3",
			Key:     "fake/" + m.ETag,
			Size:    m.Size,
			ETag:    m.ETag,
		}
	}
	return m, nil
}

func (f *fakePresignBackend) GetChunks(ctx context.Context, m *data.Manifest, off, length int64) (io.ReadCloser, error) {
	// Inner memory backend serves from the chunks slice — the BackendRef
	// we tacked on in PutChunks doesn't change the inner storage shape.
	mInner := *m
	mInner.BackendRef = nil
	return f.inner.GetChunks(ctx, &mInner, off, length)
}

func (f *fakePresignBackend) Delete(ctx context.Context, m *data.Manifest) error {
	mInner := *m
	mInner.BackendRef = nil
	return f.inner.Delete(ctx, &mInner)
}

func (f *fakePresignBackend) Close() error { return f.inner.Close() }

func (f *fakePresignBackend) PresignGetObject(ctx context.Context, m *data.Manifest, expires time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	f.lastExpire = expires
	if m != nil && m.BackendRef != nil {
		f.lastBackend = m.BackendRef.Key
	}
	if f.err != nil {
		return "", f.err
	}
	return f.url, nil
}
