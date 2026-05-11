package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/danchupin/strata/internal/data"
)

// TestStubLifecycleReturnsErrUnsupported pins the contract: a
// zero-value Backend (no clusters) must surface errors.ErrUnsupported
// on every LifecycleBackend method before Open wires a live client
// (US-014).
func TestStubLifecycleReturnsErrUnsupported(t *testing.T) {
	b := &Backend{}
	ctx := context.Background()

	if _, err := b.PutBackendLifecycle(ctx, "p/", nil); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("PutBackendLifecycle: want errors.ErrUnsupported, got %v", err)
	}
	if err := b.DeleteBackendLifecycle(ctx, "p/"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("DeleteBackendLifecycle: want errors.ErrUnsupported, got %v", err)
	}
}

// TestIsNativeTransitionClass pins the native-class whitelist used by both
// the worker's backend-skip path and the translator. Casing must be
// case-insensitive — Strata accepts the lowercase form on PUT object too.
func TestIsNativeTransitionClass(t *testing.T) {
	t.Helper()
	want := map[string]bool{
		"STANDARD_IA":         true,
		"ONEZONE_IA":          true,
		"GLACIER_IR":          true,
		"GLACIER":             true,
		"DEEP_ARCHIVE":        true,
		"INTELLIGENT_TIERING": true,
		"standard_ia":         true,
		"GLACIER_DEEP":        false,
		"COLD":                false,
		"":                    false,
		"STANDARD":            false,
	}
	for class, exp := range want {
		if got := IsNativeTransitionClass(class); got != exp {
			t.Errorf("IsNativeTransitionClass(%q) = %v, want %v", class, got, exp)
		}
	}
}

// TestPutBackendLifecycleTranslatesNativeTransition is the AC-driving test:
// PUT a Strata lifecycle config with a STANDARD_IA transition and assert
// the backend received a PutBucketLifecycleConfiguration call with the
// matching rule (Days=30, StorageClass=STANDARD_IA, Filter prefixed with
// the Strata bucket UUID + the user's prefix).
func TestPutBackendLifecycleTranslatesNativeTransition(t *testing.T) {
	ctx := context.Background()

	captured := newLifecycleCaptureTransport()
	b := openTestBackend(t, captured)

	rules := []data.LifecycleRule{
		{ID: "r1", Prefix: "logs/", TransitionDays: 30, TransitionStorageClass: "STANDARD_IA"},
	}
	skipped, err := b.PutBackendLifecycle(ctx, "bkt-uuid/", rules)
	if err != nil {
		t.Fatalf("PutBackendLifecycle: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped: want 0, got %v", skipped)
	}
	body := captured.lastBody()
	if !strings.Contains(body, "<StorageClass>STANDARD_IA</StorageClass>") {
		t.Errorf("backend lifecycle body missing STANDARD_IA transition: %s", body)
	}
	if !strings.Contains(body, "<Days>30</Days>") {
		t.Errorf("backend lifecycle body missing Days=30: %s", body)
	}
	if !strings.Contains(body, "<Prefix>bkt-uuid/logs/</Prefix>") {
		t.Errorf("backend lifecycle body missing prefixed filter: %s", body)
	}
	if !strings.Contains(body, "<ID>r1</ID>") {
		t.Errorf("backend lifecycle body missing rule id: %s", body)
	}
	if got := captured.lastQuery(); !strings.Contains(got, "lifecycle") {
		t.Errorf("backend call must hit ?lifecycle, got query %q", got)
	}
}

// TestPutBackendLifecycleSkipsNonNativeTransition pins the best-effort
// translation contract: a transition to a Strata-only class is reported in
// skippedRuleIDs and the rule is dropped from the backend config (worker
// keeps owning it).
func TestPutBackendLifecycleSkipsNonNativeTransition(t *testing.T) {
	ctx := context.Background()

	captured := newLifecycleCaptureTransport()
	b := openTestBackend(t, captured)

	rules := []data.LifecycleRule{
		{ID: "cold", Prefix: "x/", TransitionDays: 7, TransitionStorageClass: "STRATA_COLD"},
	}
	skipped, err := b.PutBackendLifecycle(ctx, "bkt/", rules)
	if err != nil {
		t.Fatalf("PutBackendLifecycle: %v", err)
	}
	if len(skipped) != 1 || skipped[0] != "cold" {
		t.Fatalf("skipped: want [cold], got %v", skipped)
	}
	// No translatable action remained — backend should have received a
	// DeleteBucketLifecycle (NOT a Put).
	if got := captured.lastMethod(); got != http.MethodDelete {
		t.Errorf("expected DELETE on full-skip, got %s", got)
	}
}

// TestPutBackendLifecycleEmitsExpiration pins the AC: expirations always
// translate to backend lifecycle so orphan backend bytes get cleaned up
// independently of Strata's GC.
func TestPutBackendLifecycleEmitsExpiration(t *testing.T) {
	ctx := context.Background()

	captured := newLifecycleCaptureTransport()
	b := openTestBackend(t, captured)

	rules := []data.LifecycleRule{
		{ID: "expire-old", Prefix: "", ExpirationDays: 365},
	}
	if _, err := b.PutBackendLifecycle(ctx, "bkt/", rules); err != nil {
		t.Fatalf("PutBackendLifecycle: %v", err)
	}
	body := captured.lastBody()
	if !strings.Contains(body, "<Expiration><Days>365</Days></Expiration>") {
		t.Errorf("expiration not emitted: %s", body)
	}
}

// TestPutBackendLifecycleEmitsAbortIncomplete pins a load-bearing safety
// net documented in docs/site/content/architecture/backends/s3.md: AbortIncompleteMultipartUpload
// translates to backend lifecycle so the operator's recommended cleanup
// rule actually applies.
func TestPutBackendLifecycleEmitsAbortIncomplete(t *testing.T) {
	ctx := context.Background()

	captured := newLifecycleCaptureTransport()
	b := openTestBackend(t, captured)

	rules := []data.LifecycleRule{
		{ID: "kill-mp", AbortIncompleteUploadDays: 7},
	}
	if _, err := b.PutBackendLifecycle(ctx, "bkt/", rules); err != nil {
		t.Fatalf("PutBackendLifecycle: %v", err)
	}
	body := captured.lastBody()
	if !strings.Contains(body, "<DaysAfterInitiation>7</DaysAfterInitiation>") {
		t.Errorf("abort-incomplete not emitted: %s", body)
	}
}

// TestPutBackendLifecycleEmptyRulesClearsBackend pins the contract: a
// fully-skipped translation deletes the backend lifecycle instead of
// pushing an empty config (which the SDK rejects).
func TestPutBackendLifecycleEmptyRulesClearsBackend(t *testing.T) {
	ctx := context.Background()

	captured := newLifecycleCaptureTransport()
	b := openTestBackend(t, captured)

	if _, err := b.PutBackendLifecycle(ctx, "bkt/", nil); err != nil {
		t.Fatalf("PutBackendLifecycle(nil): %v", err)
	}
	if got := captured.lastMethod(); got != http.MethodDelete {
		t.Errorf("expected DELETE on empty input, got %s", got)
	}
}

// TestDeleteBackendLifecycleIssuesDelete is the symmetric clear-state path.
func TestDeleteBackendLifecycleIssuesDelete(t *testing.T) {
	ctx := context.Background()

	captured := newLifecycleCaptureTransport()
	b := openTestBackend(t, captured)

	if err := b.DeleteBackendLifecycle(ctx, "bkt/"); err != nil {
		t.Fatalf("DeleteBackendLifecycle: %v", err)
	}
	if got := captured.lastMethod(); got != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", got)
	}
	if got := captured.lastQuery(); !strings.Contains(got, "lifecycle") {
		t.Errorf("expected ?lifecycle, got %q", got)
	}
}

// lifecycleCaptureTransport records the most recent request's method, raw
// query, and body so tests can assert against the SDK's serialised
// lifecycle XML. Returns 200 OK with empty body — both
// PutBucketLifecycleConfiguration and DeleteBucketLifecycle are
// 200/204-only on success and don't carry response bodies the SDK reads.
type lifecycleCaptureTransport struct {
	mu     sync.Mutex
	method string
	query  url.Values
	body   string
}

func newLifecycleCaptureTransport() *lifecycleCaptureTransport {
	return &lifecycleCaptureTransport{}
}

func (t *lifecycleCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyStr string
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		bodyStr = string(raw)
		_ = req.Body.Close()
	}
	t.mu.Lock()
	t.method = req.Method
	t.query = req.URL.Query()
	t.body = bodyStr
	t.mu.Unlock()
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

func (t *lifecycleCaptureTransport) lastBody() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.body
}

func (t *lifecycleCaptureTransport) lastMethod() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.method
}

func (t *lifecycleCaptureTransport) lastQuery() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.query == nil {
		return ""
	}
	return t.query.Encode()
}
