package s3api_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

// drainToggleBackend wraps a memory data backend with an atomic refuse flag.
// While the flag is set PutChunks returns data.NewDrainRefusedError (the
// always-strict drain stop-write); GetChunks / Delete always pass through to
// the inner backend. Objects seeded before the flag flips remain fully
// readable/deletable — that's the invariant US-009 proves under load.
type drainToggleBackend struct {
	inner  *datamem.Backend
	refuse atomic.Bool
}

func (b *drainToggleBackend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	if b.refuse.Load() {
		_, _ = io.Copy(io.Discard, r)
		return nil, data.NewDrainRefusedError("default")
	}
	return b.inner.PutChunks(ctx, r, class)
}

func (b *drainToggleBackend) GetChunks(ctx context.Context, m *data.Manifest, offset, length int64) (io.ReadCloser, error) {
	return b.inner.GetChunks(ctx, m, offset, length)
}

func (b *drainToggleBackend) Delete(ctx context.Context, m *data.Manifest) error {
	return b.inner.Delete(ctx, m)
}

func (b *drainToggleBackend) Close() error { return b.inner.Close() }

// TestDrainStopWriteUnderConcurrentLoad pins the always-strict drain
// invariant under live traffic (US-009 AC1): once a cluster is draining,
// every concurrent PUT is refused with 503 DrainRefused + Retry-After: 300,
// while GET / HEAD / DELETE on objects already stored keep working. PUT-only
// stop-write — reads/deletes/HEAD never see the refusal.
func TestDrainStopWriteUnderConcurrentLoad(t *testing.T) {
	const (
		putters  = 24
		readers  = 12
		heads    = 12
		deleters = 8
	)

	backend := &drainToggleBackend{inner: datamem.New()}
	api := s3api.New(backend, metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)

	client := ts.Client()
	do := func(method, path, body string) *http.Response {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, ts.URL+path, rdr)
		if err != nil {
			t.Fatalf("new req %s %s: %v", method, path, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do %s %s: %v", method, path, err)
		}
		return resp
	}

	// Bucket + seed objects BEFORE the cluster starts draining.
	if resp := do(http.MethodPut, "/bkt", ""); resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("create bucket: status %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
	const getBody = "payload-readable"
	if resp := do(http.MethodPut, "/bkt/seed-get", getBody); resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("seed get-object: status %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
	if resp := do(http.MethodPut, "/bkt/seed-head", "head-body"); resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("seed head-object: status %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
	delKeys := make([]string, deleters)
	for i := range delKeys {
		delKeys[i] = fmt.Sprintf("seed-del-%d", i)
		if resp := do(http.MethodPut, "/bkt/"+delKeys[i], "del-body"); resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			t.Fatalf("seed del-object %d: status %d", i, resp.StatusCode)
		} else {
			_ = resp.Body.Close()
		}
	}

	// Cluster now drains: PUT stop-write, reads/deletes/HEAD keep working.
	backend.refuse.Store(true)

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		failMu  sync.Mutex
		failure string
	)
	fail := func(format string, args ...any) {
		failMu.Lock()
		if failure == "" {
			failure = fmt.Sprintf(format, args...)
		}
		failMu.Unlock()
	}

	// Concurrent PUTs — every one must be refused 503 + Retry-After: 300.
	for i := 0; i < putters; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			resp := do(http.MethodPut, fmt.Sprintf("/bkt/put-%d", id), "new-data")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				fail("PUT put-%d: status %d, want 503", id, resp.StatusCode)
				return
			}
			if got := resp.Header.Get("Retry-After"); got != "300" {
				fail("PUT put-%d: Retry-After %q, want 300", id, got)
				return
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), "<Code>DrainRefused</Code>") {
				fail("PUT put-%d: body missing DrainRefused: %s", id, body)
			}
		}(i)
	}

	// Concurrent GETs — reads keep working against the draining cluster.
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp := do(http.MethodGet, "/bkt/seed-get", "")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				fail("GET seed-get: status %d, want 200", resp.StatusCode)
				return
			}
			body, _ := io.ReadAll(resp.Body)
			if string(body) != getBody {
				fail("GET seed-get: body %q, want %q", body, getBody)
			}
		}()
	}

	// Concurrent HEADs — metadata reads keep working.
	for i := 0; i < heads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp := do(http.MethodHead, "/bkt/seed-head", "")
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != http.StatusOK {
				fail("HEAD seed-head: status %d, want 200", resp.StatusCode)
			}
		}()
	}

	// Concurrent DELETEs — deletes keep working (one per seeded key).
	for i := range delKeys {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			<-start
			resp := do(http.MethodDelete, "/bkt/"+key, "")
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != http.StatusNoContent {
				fail("DELETE %s: status %d, want 204", key, resp.StatusCode)
			}
		}(delKeys[i])
	}

	close(start)
	wg.Wait()

	if failure != "" {
		t.Fatal(failure)
	}
}
