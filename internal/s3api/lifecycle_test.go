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

func TestBucketLifecycleCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	h.mustStatus(h.doString("GET", "/bkt?lifecycle", ""), 404)

	rules := `<LifecycleConfiguration><Rule><ID>r1</ID><Status>Enabled</Status><Filter><Prefix>logs/</Prefix></Filter><Transition><Days>30</Days><StorageClass>STANDARD_IA</StorageClass></Transition></Rule></LifecycleConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?lifecycle", rules), 200)

	resp := h.doString("GET", "/bkt?lifecycle", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "<ID>r1</ID>") {
		t.Errorf("lifecycle GET missing rule: %s", body)
	}

	h.mustStatus(h.doString("DELETE", "/bkt?lifecycle", ""), 204)
	h.mustStatus(h.doString("GET", "/bkt?lifecycle", ""), 404)
}

// TestPutBucketLifecycleTranslatesToBackend pins US-014 wiring: when the
// data backend implements data.LifecycleBackend, PutBucketLifecycle on the
// Strata gateway also pushes the translated rules to the backend (filter
// scoped to the Strata bucket UUID prefix). Strata's own lifecycle store
// remains the source of truth — translation is fire-and-forget on errors.
func TestPutBucketLifecycleTranslatesToBackend(t *testing.T) {
	fake := newFakeLifecycleBackend()
	api := s3api.New(fake, metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)

	h := &testHarness{t: t, ts: ts}

	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	rules := `<LifecycleConfiguration>
<Rule><ID>r-native</ID><Status>Enabled</Status><Filter><Prefix>logs/</Prefix></Filter><Transition><Days>30</Days><StorageClass>STANDARD_IA</StorageClass></Transition></Rule>
<Rule><ID>r-expire</ID><Status>Enabled</Status><Filter><Prefix>tmp/</Prefix></Filter><Expiration><Days>7</Days></Expiration></Rule>
<Rule><ID>r-cold</ID><Status>Enabled</Status><Filter><Prefix>cold/</Prefix></Filter><Transition><Days>1</Days><StorageClass>STRATA_COLD</StorageClass></Transition></Rule>
<Rule><ID>r-disabled</ID><Status>Disabled</Status><Filter><Prefix>off/</Prefix></Filter><Expiration><Days>1</Days></Expiration></Rule>
</LifecycleConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?lifecycle", rules), 200)

	got := fake.lastRules()
	if len(got) != 3 {
		t.Fatalf("backend rules: want 3 (disabled rule dropped), got %d: %+v", len(got), got)
	}
	if got[0].ID != "r-native" || got[0].TransitionStorageClass != "STANDARD_IA" || got[0].TransitionDays != 30 {
		t.Errorf("rule[0] mistranslated: %+v", got[0])
	}
	if got[1].ID != "r-expire" || got[1].ExpirationDays != 7 {
		t.Errorf("rule[1] mistranslated: %+v", got[1])
	}
	if got[2].ID != "r-cold" || got[2].TransitionStorageClass != "STRATA_COLD" {
		t.Errorf("rule[2] non-native transition must reach backend so it can report it skipped: %+v", got[2])
	}

	if prefix := fake.lastPrefix(); !strings.HasSuffix(prefix, "/") {
		t.Errorf("backend prefix must end with /, got %q", prefix)
	}

	// DELETE on Strata also clears backend lifecycle.
	h.mustStatus(h.doString("DELETE", "/bkt?lifecycle", ""), 204)
	if got := fake.deleteCalls(); got != 1 {
		t.Errorf("backend DELETE not invoked: got %d calls, want 1", got)
	}
}

// TestPutBucketLifecycleBackendFailureNonFatal pins the contract: a backend
// translation failure surfaces in logs but the user request still succeeds —
// Strata's stored config is the source of truth.
func TestPutBucketLifecycleBackendFailureNonFatal(t *testing.T) {
	fake := newFakeLifecycleBackend()
	fake.putErr = errors.New("backend lifecycle service down")
	api := s3api.New(fake, metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)

	h := &testHarness{t: t, ts: ts}
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	rules := `<LifecycleConfiguration><Rule><ID>r1</ID><Status>Enabled</Status><Filter><Prefix>x/</Prefix></Filter><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`
	h.mustStatus(h.doString("PUT", "/bkt?lifecycle", rules), 200)

	resp := h.doString("GET", "/bkt?lifecycle", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); !strings.Contains(body, "<ID>r1</ID>") {
		t.Errorf("Strata-stored lifecycle should remain authoritative even when backend translation fails: %s", body)
	}
}

func TestObjectTagging(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "x"), 200)

	tags := `<Tagging><TagSet><Tag><Key>env</Key><Value>prod</Value></Tag><Tag><Key>owner</Key><Value>team-x</Value></Tag></TagSet></Tagging>`
	h.mustStatus(h.doString("PUT", "/bkt/k?tagging", tags), 200)

	resp := h.doString("GET", "/bkt/k?tagging", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<Key>env</Key>") || !strings.Contains(body, "<Value>prod</Value>") {
		t.Errorf("tags not returned: %s", body)
	}

	resp = h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if cnt := resp.Header.Get("X-Amz-Tagging-Count"); cnt != "2" {
		t.Errorf("tagging-count: got %q want 2", cnt)
	}

	h.mustStatus(h.doString("DELETE", "/bkt/k?tagging", ""), 204)
	resp = h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if cnt := resp.Header.Get("X-Amz-Tagging-Count"); cnt != "" {
		t.Errorf("tagging-count after delete: got %q want empty", cnt)
	}
}

func TestObjectLockBlocksDelete(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "x"), 200)

	future := "2099-12-31T00:00:00Z"
	retention := `<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>` + future + `</RetainUntilDate></Retention>`
	h.mustStatus(h.doString("PUT", "/bkt/k?retention", retention), 200)

	resp := h.doString("DELETE", "/bkt/k", "")
	h.mustStatus(resp, 403)
	if body := h.readBody(resp); !strings.Contains(body, "AccessDenied") {
		t.Errorf("expected AccessDenied, got: %s", body)
	}

	h.mustStatus(h.doString("PUT", "/bkt/k?legal-hold", `<LegalHold><Status>ON</Status></LegalHold>`), 200)
	past := `<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>2000-01-01T00:00:00Z</RetainUntilDate></Retention>`
	h.mustStatus(h.doString("PUT", "/bkt/k?retention", past), 200)

	resp = h.doString("DELETE", "/bkt/k", "", "x-amz-bypass-governance-retention", "true")
	h.mustStatus(resp, 403)

	h.mustStatus(h.doString("PUT", "/bkt/k?legal-hold", `<LegalHold><Status>OFF</Status></LegalHold>`), 200)
	h.mustStatus(h.doString("DELETE", "/bkt/k", "", "x-amz-bypass-governance-retention", "true"), 204)
}

// fakeLifecycleBackend is a chunk-passthrough data backend (delegates to
// memory) plus an inspectable LifecycleBackend impl. Tests assert on
// captured rules + prefix to verify gateway translation wiring.
type fakeLifecycleBackend struct {
	mu      sync.Mutex
	inner   data.Backend
	rules   []data.LifecycleRule
	prefix  string
	deletes int
	putErr  error
}

func newFakeLifecycleBackend() *fakeLifecycleBackend {
	return &fakeLifecycleBackend{inner: datamem.New()}
}

func (f *fakeLifecycleBackend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	return f.inner.PutChunks(ctx, r, class)
}

func (f *fakeLifecycleBackend) GetChunks(ctx context.Context, m *data.Manifest, off, length int64) (io.ReadCloser, error) {
	return f.inner.GetChunks(ctx, m, off, length)
}

func (f *fakeLifecycleBackend) Delete(ctx context.Context, m *data.Manifest) error {
	return f.inner.Delete(ctx, m)
}

func (f *fakeLifecycleBackend) Close() error { return f.inner.Close() }

func (f *fakeLifecycleBackend) PutBackendLifecycle(ctx context.Context, bucketPrefix string, rules []data.LifecycleRule) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return nil, f.putErr
	}
	f.prefix = bucketPrefix
	f.rules = append([]data.LifecycleRule(nil), rules...)
	return nil, nil
}

func (f *fakeLifecycleBackend) DeleteBackendLifecycle(ctx context.Context, bucketPrefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	return nil
}

func (f *fakeLifecycleBackend) lastRules() []data.LifecycleRule {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rules
}

func (f *fakeLifecycleBackend) lastPrefix() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prefix
}

func (f *fakeLifecycleBackend) deleteCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deletes
}

var _ data.LifecycleBackend = (*fakeLifecycleBackend)(nil)
