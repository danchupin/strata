package s3api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// drainRefusingBackend wraps a memory data backend but returns
// data.NewDrainRefusedError on PutChunks. Lets us drive the gateway
// 503/Retry-After mapping without a real RADOS/S3 backend.
type drainRefusingBackend struct {
	inner *datamem.Backend
}

func (b *drainRefusingBackend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	_, _ = io.Copy(io.Discard, r)
	return nil, data.NewDrainRefusedError("default")
}

func (b *drainRefusingBackend) GetChunks(ctx context.Context, m *data.Manifest, offset, length int64) (io.ReadCloser, error) {
	return b.inner.GetChunks(ctx, m, offset, length)
}

func (b *drainRefusingBackend) Delete(ctx context.Context, m *data.Manifest) error {
	return b.inner.Delete(ctx, m)
}

func (b *drainRefusingBackend) Close() error { return b.inner.Close() }

// TestPutObjectDrainRefusedMapsTo503 pins the US-002 gateway mapping:
// PutChunks returning data.ErrDrainRefused yields HTTP 503 +
// Retry-After: 300 + a DrainRefused-coded XML body.
func TestPutObjectDrainRefusedMapsTo503(t *testing.T) {
	metaStore := metamem.New()
	api := s3api.New(&drainRefusingBackend{inner: datamem.New()}, metaStore)
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)

	// Create bucket via the API so meta tracks it.
	req, err := http.NewRequest(http.MethodPut, ts.URL+"/bkt", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create bucket: status %d", resp.StatusCode)
	}

	// PUT object — backend refuses.
	req, err = http.NewRequest(http.MethodPut, ts.URL+"/bkt/obj", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put object: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: want 503, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "300" {
		t.Fatalf("Retry-After: want %q, got %q", "300", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "<Code>DrainRefused</Code>") {
		t.Fatalf("body must contain DrainRefused code; got %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "cluster default") {
		t.Fatalf("body must contain cluster id; got %s", bodyStr)
	}
}
