package s3api_test

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

const corsXML = `<CORSConfiguration>
	<CORSRule>
		<AllowedOrigin>https://example.com</AllowedOrigin>
		<AllowedMethod>GET</AllowedMethod>
		<AllowedMethod>PUT</AllowedMethod>
		<AllowedHeader>x-amz-*</AllowedHeader>
		<ExposeHeader>ETag</ExposeHeader>
		<MaxAgeSeconds>3000</MaxAgeSeconds>
	</CORSRule>
</CORSConfiguration>`

func TestCORSConfigCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	// Not configured → 404 NoSuchCORSConfiguration.
	resp := h.doString("GET", "/bkt?cors=", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "NoSuchCORSConfiguration") {
		t.Fatalf("expected NoSuchCORSConfiguration, got: %s", body)
	}

	h.mustStatus(h.doString("PUT", "/bkt?cors=", corsXML), 200)

	resp = h.doString("GET", "/bkt?cors=", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "https://example.com") {
		t.Fatalf("GET cors body missing origin: %s", body)
	}

	h.mustStatus(h.doString("DELETE", "/bkt?cors=", ""), 204)
	h.mustStatus(h.doString("GET", "/bkt?cors=", ""), 404)
}

func TestCORSPreflightMatch(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?cors=", corsXML), 200)

	resp := h.doString("OPTIONS", "/bkt/key", "",
		"Origin", "https://example.com",
		"Access-Control-Request-Method", "GET",
		"Access-Control-Request-Headers", "x-amz-content-sha256",
	)
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Fatalf("Allow-Origin: %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "GET") {
		t.Fatalf("Allow-Methods: %q", got)
	}
	if got := resp.Header.Get("Access-Control-Max-Age"); got != "3000" {
		t.Fatalf("Max-Age: %q", got)
	}
}

func TestCORSPreflightNoMatch(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?cors=", corsXML), 200)

	// Origin mismatch.
	resp := h.doString("OPTIONS", "/bkt/key", "",
		"Origin", "https://evil.com",
		"Access-Control-Request-Method", "GET",
	)
	h.mustStatus(resp, 403)
}

func TestCORSPreflightWithoutConfig(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("OPTIONS", "/bkt/key", "",
		"Origin", "https://example.com",
		"Access-Control-Request-Method", "GET",
	)
	h.mustStatus(resp, 403)
}

func TestBucketPolicyCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("GET", "/bkt?policy=", "")
	h.mustStatus(resp, 404)

	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::b/*"}]}`
	h.mustStatus(h.doString("PUT", "/bkt?policy=", policy), 204)

	resp = h.doString("GET", "/bkt?policy=", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "2012-10-17") {
		t.Fatalf("GET policy body: %s", body)
	}

	h.mustStatus(h.doString("DELETE", "/bkt?policy=", ""), 204)
	h.mustStatus(h.doString("GET", "/bkt?policy=", ""), 404)
}

func TestBucketPolicyMalformed(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("PUT", "/bkt?policy=", "not json")
	h.mustStatus(resp, 400)
}

const pabXML = `<PublicAccessBlockConfiguration>
	<BlockPublicAcls>true</BlockPublicAcls>
	<IgnorePublicAcls>true</IgnorePublicAcls>
	<BlockPublicPolicy>false</BlockPublicPolicy>
	<RestrictPublicBuckets>false</RestrictPublicBuckets>
</PublicAccessBlockConfiguration>`

func TestPublicAccessBlockCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("GET", "/bkt?publicAccessBlock=", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "NoSuchPublicAccessBlockConfiguration") {
		t.Fatalf("body: %s", body)
	}

	h.mustStatus(h.doString("PUT", "/bkt?publicAccessBlock=", pabXML), 200)

	resp = h.doString("GET", "/bkt?publicAccessBlock=", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "BlockPublicAcls") {
		t.Fatalf("GET pab body: %s", body)
	}

	h.mustStatus(h.doString("DELETE", "/bkt?publicAccessBlock=", ""), 204)
	h.mustStatus(h.doString("GET", "/bkt?publicAccessBlock=", ""), 404)
}

const ownershipXML = `<OwnershipControls>
	<Rule>
		<ObjectOwnership>BucketOwnerEnforced</ObjectOwnership>
	</Rule>
</OwnershipControls>`

func TestOwnershipControlsCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("GET", "/bkt?ownershipControls=", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "OwnershipControlsNotFoundError") {
		t.Fatalf("body: %s", body)
	}

	h.mustStatus(h.doString("PUT", "/bkt?ownershipControls=", ownershipXML), 200)

	resp = h.doString("GET", "/bkt?ownershipControls=", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "BucketOwnerEnforced") {
		t.Fatalf("GET body: %s", body)
	}

	h.mustStatus(h.doString("DELETE", "/bkt?ownershipControls=", ""), 204)
	h.mustStatus(h.doString("GET", "/bkt?ownershipControls=", ""), 404)
}

func TestOwnershipControlsRejectsInvalid(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	bad := `<OwnershipControls><Rule><ObjectOwnership>Bogus</ObjectOwnership></Rule></OwnershipControls>`
	resp := h.doString("PUT", "/bkt?ownershipControls=", bad)
	h.mustStatus(resp, 400)
}

// TestPutBucketCORSTranslatesToBackend pins US-015 wiring: when the data
// backend implements data.CORSBackend, PutBucketCors on the Strata gateway
// also pushes the parsed rules to the backend bucket. Strata's stored
// config remains the source of truth — translation is fire-and-forget on
// errors per AC.
func TestPutBucketCORSTranslatesToBackend(t *testing.T) {
	fake := newFakeCORSBackend()
	api := s3api.New(fake, metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	h := &testHarness{t: t, ts: ts}

	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?cors=", corsXML), 200)

	got := fake.lastRules()
	if len(got) != 1 {
		t.Fatalf("backend rules: want 1, got %d: %+v", len(got), got)
	}
	r := got[0]
	if len(r.AllowedOrigins) != 1 || r.AllowedOrigins[0] != "https://example.com" {
		t.Errorf("origins mistranslated: %v", r.AllowedOrigins)
	}
	if r.MaxAgeSeconds != 3000 {
		t.Errorf("max-age mistranslated: %d", r.MaxAgeSeconds)
	}

	// DELETE on Strata also clears backend CORS.
	h.mustStatus(h.doString("DELETE", "/bkt?cors=", ""), 204)
	if got := fake.deleteCalls(); got != 1 {
		t.Errorf("backend DELETE not invoked: got %d calls, want 1", got)
	}
}

// TestPutBucketCORSBackendFailureNonFatal pins the contract: a backend
// translation failure surfaces in logs but the user request still
// succeeds — Strata's stored config is the source of truth.
func TestPutBucketCORSBackendFailureNonFatal(t *testing.T) {
	fake := newFakeCORSBackend()
	fake.putErr = errors.New("backend cors service down")
	api := s3api.New(fake, metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	h := &testHarness{t: t, ts: ts}

	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?cors=", corsXML), 200)

	resp := h.doString("GET", "/bkt?cors=", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "https://example.com") {
		t.Errorf("Strata-stored cors should remain authoritative even when backend translation fails: %s", body)
	}
}

// TestGetBucketCORSUnionsBackendOnlyRules pins the AC: GET unions Strata-
// stored config with backend-stored rules. When the backend has a rule
// the Strata side doesn't know about (manually side-channelled), GET
// surfaces it alongside Strata's rules. ID conflicts are won by Strata.
func TestGetBucketCORSUnionsBackendOnlyRules(t *testing.T) {
	fake := newFakeCORSBackend()
	// Pre-seed a backend-only rule with no ID so it won't conflict on
	// merge with Strata's r1.
	fake.getRules = []data.CORSRule{{
		AllowedMethods: []string{"HEAD"},
		AllowedOrigins: []string{"https://backend-only.example.com"},
	}}
	api := s3api.New(fake, metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	h := &testHarness{t: t, ts: ts}

	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?cors=", corsXML), 200)

	resp := h.doString("GET", "/bkt?cors=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "https://example.com") {
		t.Errorf("merged GET missing strata rule origin: %s", body)
	}
	if !strings.Contains(body, "https://backend-only.example.com") {
		t.Errorf("merged GET missing backend-only rule: %s", body)
	}
}

// fakeCORSBackend is a chunk-passthrough backend (delegates to memory) plus
// an inspectable CORSBackend impl. Used to assert gateway wiring without
// MinIO.
type fakeCORSBackend struct {
	mu       sync.Mutex
	inner    data.Backend
	rules    []data.CORSRule
	getRules []data.CORSRule
	deletes  int
	putErr   error
}

func newFakeCORSBackend() *fakeCORSBackend {
	return &fakeCORSBackend{inner: datamem.New()}
}

func (f *fakeCORSBackend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	return f.inner.PutChunks(ctx, r, class)
}

func (f *fakeCORSBackend) GetChunks(ctx context.Context, m *data.Manifest, off, length int64) (io.ReadCloser, error) {
	return f.inner.GetChunks(ctx, m, off, length)
}

func (f *fakeCORSBackend) Delete(ctx context.Context, m *data.Manifest) error {
	return f.inner.Delete(ctx, m)
}

func (f *fakeCORSBackend) Close(ctx context.Context) error { return f.inner.Close(ctx) }

func (f *fakeCORSBackend) PutBackendCORS(ctx context.Context, rules []data.CORSRule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.rules = append([]data.CORSRule(nil), rules...)
	return nil
}

func (f *fakeCORSBackend) GetBackendCORS(ctx context.Context) ([]data.CORSRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]data.CORSRule(nil), f.getRules...), nil
}

func (f *fakeCORSBackend) DeleteBackendCORS(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	return nil
}

func (f *fakeCORSBackend) lastRules() []data.CORSRule {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rules
}

func (f *fakeCORSBackend) deleteCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deletes
}

var _ data.CORSBackend = (*fakeCORSBackend)(nil)
