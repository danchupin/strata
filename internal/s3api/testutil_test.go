package s3api_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

const testPrincipalHeader = "X-Test-Principal"

type testHarness struct {
	t  *testing.T
	ts *httptest.Server
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	api := s3api.New(datamem.New(), metamem.New())
	api.Region = "default"
	api.Master = harnessMasterProvider{}
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

// harnessMasterProvider is a fixed-key master.Provider used by the default
// test harness so SSE-S3 PUT/GET round-trips work without needing each test
// to wire its own provider.
type harnessMasterProvider struct{}

func (harnessMasterProvider) Resolve(_ context.Context) ([]byte, string, error) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0x10 + i)
	}
	return key, "harness-1", nil
}

func (h *testHarness) do(method, path string, body io.Reader, headers ...string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.ts.URL+path, body)
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

func (h *testHarness) doString(method, path, body string, headers ...string) *http.Response {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	return h.do(method, path, r, headers...)
}

func (h *testHarness) readBody(resp *http.Response) string {
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return string(b)
}

func (h *testHarness) mustStatus(resp *http.Response, want int) {
	h.t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		h.t.Fatalf("status: got %d want %d; body=%s", resp.StatusCode, want, string(body))
	}
}

func mustWriteFull(t *testing.T, w io.Writer, p []byte) {
	t.Helper()
	n, err := w.Write(p)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(p) {
		t.Fatalf("short write %d/%d", n, len(p))
	}
}

var _ = bytes.NewReader
