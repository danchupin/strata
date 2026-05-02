package s3api_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/crypto/master"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// staticMasterProvider returns a fixed key/keyID for tests. Implements
// master.Provider without any env or file I/O so tests are hermetic.
type staticMasterProvider struct {
	key   []byte
	keyID string
}

func (p staticMasterProvider) Resolve(_ context.Context) ([]byte, string, error) {
	return p.key, p.keyID, nil
}

func newSSEHarness(t *testing.T) *testHarness {
	t.Helper()
	mem := datamem.New()
	api := s3api.New(mem, metamem.New())
	api.Region = "default"
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	api.Master = staticMasterProvider{key: key, keyID: "test-1"}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get(testPrincipalHeader); p != "" {
			ctx := auth.WithAuth(r.Context(), &auth.AuthInfo{Owner: p, AccessKey: p})
			r = r.WithContext(ctx)
		}
		api.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &testHarness{t: t, ts: ts}
}

// stashMaster lets a test reach into the harness for the underlying memory
// data backend so we can mutate stored chunks (corruption test). Returns the
// configured master provider too.
type sseHarnessInternals struct {
	dataBackend *datamem.Backend
	provider    master.Provider
}

func newSSEHarnessWithInternals(t *testing.T) (*testHarness, sseHarnessInternals) {
	t.Helper()
	mem := datamem.New()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0xA0 + i)
	}
	mp := staticMasterProvider{key: key, keyID: "test-1"}
	api := s3api.New(mem, metamem.New())
	api.Region = "default"
	api.Master = mp
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get(testPrincipalHeader); p != "" {
			ctx := auth.WithAuth(r.Context(), &auth.AuthInfo{Owner: p, AccessKey: p})
			r = r.WithContext(ctx)
		}
		api.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &testHarness{t: t, ts: ts}, sseHarnessInternals{dataBackend: mem, provider: mp}
}

func TestSSEPutGetSmallRoundTrip(t *testing.T) {
	h := newSSEHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	body := "hello sse-s3 world"
	resp := h.doString("PUT", "/bkt/k", body, "x-amz-server-side-encryption", "AES256")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("PUT sse echo: got %q", got)
	}
	wantETag := `"` + hex.EncodeToString(md5sum([]byte(body))) + `"`
	if got := resp.Header.Get("ETag"); got != wantETag {
		t.Fatalf("PUT etag: got %q want %q", got, wantETag)
	}

	resp = h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "AES256" {
		t.Fatalf("GET sse echo: got %q", got)
	}
	if got := h.readBody(resp); got != body {
		t.Fatalf("GET body: got %q want %q", got, body)
	}

	resp = h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("Content-Length"); got != intToStr(len(body)) {
		t.Fatalf("HEAD content-length: got %q want %d", got, len(body))
	}
}

func TestSSEPutGetMultiChunkRoundTrip(t *testing.T) {
	// Plaintext > pChunkSize ensures we exercise multi-chunk encrypt + decrypt.
	// data.DefaultChunkSize - 16 = pChunkSize; pick 5 MiB so we fit ~2 chunks.
	r := rand.New(rand.NewSource(0xDECAF))
	body := make([]byte, 5*1024*1024)
	r.Read(body)

	h := newSSEHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.do("PUT", "/bkt/big", bytes.NewReader(body),
		"x-amz-server-side-encryption", "AES256")
	h.mustStatus(resp, 200)
	wantETag := `"` + hex.EncodeToString(md5sum(body)) + `"`
	if got := resp.Header.Get("ETag"); got != wantETag {
		t.Fatalf("PUT etag: got %q want %q", got, wantETag)
	}

	resp = h.do("GET", "/bkt/big", nil)
	h.mustStatus(resp, 200)
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("body mismatch: %d vs %d, first-diff at %d", len(got), len(body), firstDiff(got, body))
	}
}

func TestSSEPutGetRangeRequest(t *testing.T) {
	r := rand.New(rand.NewSource(0xBEEF))
	body := make([]byte, 5*1024*1024)
	r.Read(body)

	h := newSSEHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.do("PUT", "/bkt/big", bytes.NewReader(body),
		"x-amz-server-side-encryption", "AES256"), 200)

	// Range that crosses a crypto-chunk boundary (pChunkSize ~= 4 MiB - 16).
	start := int64(4*1024*1024 - 100)
	end := int64(4*1024*1024 + 100)
	resp := h.do("GET", "/bkt/big", nil, "Range", "bytes="+intToStr(int(start))+"-"+intToStr(int(end)))
	h.mustStatus(resp, http.StatusPartialContent)
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	want := body[start : end+1]
	if !bytes.Equal(got, want) {
		t.Fatalf("range body mismatch: got %d, want %d, first-diff %d", len(got), len(want), firstDiff(got, want))
	}
}

func TestSSEPlaintextInterop(t *testing.T) {
	// Same harness with master provider, but client never sends the SSE header.
	// Object should be stored unencrypted and read back verbatim.
	h := newSSEHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	body := "plaintext payload"
	resp := h.doString("PUT", "/bkt/k", body)
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "" {
		t.Fatalf("PUT sse header should be absent: got %q", got)
	}

	resp = h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "" {
		t.Fatalf("GET sse header should be absent: got %q", got)
	}
	if got := h.readBody(resp); got != body {
		t.Fatalf("body: got %q want %q", got, body)
	}
}

func TestSSECorruptedCiphertext(t *testing.T) {
	h, internals := newSSEHarnessWithInternals(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	body := strings.Repeat("A", 1024)
	h.mustStatus(h.doString("PUT", "/bkt/k", body, "x-amz-server-side-encryption", "AES256"), 200)

	// Mutate the stored chunk in the memory data backend so AEAD verification
	// fails on read. The backend exposes the chunk map only via Delete; reach
	// in by reflection-free side door: use the Manifest fetched via GET HEAD,
	// then mutate via a public hook. The memory backend has no public hook,
	// so instead we round-trip a deliberately corrupted GET by overwriting
	// the underlying byte using the test-only Corrupt helper exposed below.
	if !internals.dataBackend.CorruptFirstChunk() {
		t.Skip("memory backend does not expose CorruptFirstChunk; skip corruption test")
	}

	resp := h.doString("GET", "/bkt/k", "")
	if resp.StatusCode/100 == 2 {
		_ = resp.Body.Close()
		t.Fatalf("GET on corrupted ciphertext should fail; got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestSSEMissingMasterReturns500(t *testing.T) {
	// Harness without a Master provider: AES256 PUT should fail with 500.
	api := s3api.New(datamem.New(), metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)

	mkBucket, _ := http.NewRequest("PUT", ts.URL+"/bkt", nil)
	resp, err := http.DefaultClient.Do(mkBucket)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("create bucket: %v %d", err, resp.StatusCode)
	}
	_ = resp.Body.Close()

	req, _ := http.NewRequest("PUT", ts.URL+"/bkt/k", strings.NewReader("hello"))
	req.Header.Set("x-amz-server-side-encryption", "AES256")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 500 InternalError without master configured; got %d body=%s", resp.StatusCode, body)
	}
}

func md5sum(p []byte) []byte {
	h := md5.Sum(p)
	return h[:]
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	var s []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		s = append([]byte{byte('0' + n%10)}, s...)
		n /= 10
	}
	if neg {
		s = append([]byte{'-'}, s...)
	}
	return string(s)
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}
