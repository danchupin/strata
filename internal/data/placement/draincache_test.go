package placement

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDrainCacheNilSafe(t *testing.T) {
	var c *DrainCache
	if got := c.Get(context.Background()); got != nil {
		t.Fatalf("nil cache Get: got %v want nil", got)
	}
	c.Invalidate() // must not panic
}

func TestDrainCacheReloadAfterTTL(t *testing.T) {
	loads := atomic.Int32{}
	loader := func(_ context.Context) (map[string]string, error) {
		loads.Add(1)
		return map[string]string{"c1": "draining", "c2": "live", "c3": "removed"}, nil
	}
	now := time.Unix(0, 0)
	c := NewDrainCache(loader, 30*time.Second)
	c.SetClockForTest(func() time.Time { return now })

	got := c.Get(context.Background())
	if !got["c1"] || got["c2"] || got["c3"] {
		t.Fatalf("expected only c1 in draining set, got %v", got)
	}
	if loads.Load() != 1 {
		t.Fatalf("expected 1 load, got %d", loads.Load())
	}
	// Cached read — no reload.
	now = now.Add(29 * time.Second)
	_ = c.Get(context.Background())
	if loads.Load() != 1 {
		t.Fatalf("within-TTL Get reloaded: %d", loads.Load())
	}
	// Past TTL — reloads.
	now = now.Add(2 * time.Second)
	_ = c.Get(context.Background())
	if loads.Load() != 2 {
		t.Fatalf("past-TTL Get did not reload: %d", loads.Load())
	}
}

func TestDrainCacheInvalidate(t *testing.T) {
	loads := atomic.Int32{}
	loader := func(_ context.Context) (map[string]string, error) {
		loads.Add(1)
		return map[string]string{}, nil
	}
	now := time.Unix(0, 0)
	c := NewDrainCache(loader, time.Hour)
	c.SetClockForTest(func() time.Time { return now })
	_ = c.Get(context.Background())
	_ = c.Get(context.Background())
	if loads.Load() != 1 {
		t.Fatalf("expected 1 load, got %d", loads.Load())
	}
	c.Invalidate()
	_ = c.Get(context.Background())
	if loads.Load() != 2 {
		t.Fatalf("Invalidate did not trigger reload: %d", loads.Load())
	}
}

func TestDrainCacheLoaderErrorPreservesSnapshot(t *testing.T) {
	calls := atomic.Int32{}
	loader := func(_ context.Context) (map[string]string, error) {
		n := calls.Add(1)
		if n == 1 {
			return map[string]string{"c1": "draining"}, nil
		}
		return nil, errors.New("meta hiccup")
	}
	now := time.Unix(0, 0)
	c := NewDrainCache(loader, 30*time.Second)
	c.SetClockForTest(func() time.Time { return now })

	got := c.Get(context.Background())
	if !got["c1"] {
		t.Fatalf("initial: %v", got)
	}
	now = now.Add(time.Minute)
	got = c.Get(context.Background())
	if !got["c1"] {
		t.Fatalf("after-error: prior snapshot lost, got %v", got)
	}
}

func TestDrainCacheFirstLoadErrorReturnsEmpty(t *testing.T) {
	loader := func(_ context.Context) (map[string]string, error) {
		return nil, errors.New("boom")
	}
	c := NewDrainCache(loader, time.Hour)
	got := c.Get(context.Background())
	if got == nil {
		t.Fatalf("first-load error: want empty map (not nil) so subsequent reads stay cached")
	}
	if len(got) != 0 {
		t.Fatalf("first-load error: got=%v want empty", got)
	}
}
