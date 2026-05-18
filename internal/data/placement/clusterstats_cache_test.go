package placement

import (
	"testing"
	"time"
)

func TestClusterStatsCacheNilSafe(t *testing.T) {
	var c *ClusterStatsCache
	if _, _, ok := c.Get("c1"); ok {
		t.Fatalf("nil cache must report ok=false")
	}
	c.Set("c1", 1, 2) // must not panic
}

func TestClusterStatsCacheSetGetWithinTTL(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewClusterStatsCache(10 * time.Second)
	c.SetClockForTest(func() time.Time { return now })

	c.Set("c1", 1024, 42)
	bytes, objects, ok := c.Get("c1")
	if !ok {
		t.Fatalf("ok: got false want true")
	}
	if bytes != 1024 || objects != 42 {
		t.Fatalf("bytes/objects: got %d/%d want 1024/42", bytes, objects)
	}
	now = now.Add(9 * time.Second)
	if _, _, ok := c.Get("c1"); !ok {
		t.Fatalf("within-TTL get reported expired")
	}
}

func TestClusterStatsCacheExpiresAfterTTL(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewClusterStatsCache(10 * time.Second)
	c.SetClockForTest(func() time.Time { return now })
	c.Set("c1", 1024, 42)
	now = now.Add(11 * time.Second)
	if _, _, ok := c.Get("c1"); ok {
		t.Fatalf("expired entry must report ok=false")
	}
}

func TestClusterStatsCacheMissingKey(t *testing.T) {
	c := NewClusterStatsCache(10 * time.Second)
	if _, _, ok := c.Get("c1"); ok {
		t.Fatalf("missing key must report ok=false")
	}
}

func TestClusterStatsCacheDefaultTTL(t *testing.T) {
	c := NewClusterStatsCache(0)
	if c.ttl != DefaultClusterStatsCacheTTL {
		t.Fatalf("ttl: got %v want %v", c.ttl, DefaultClusterStatsCacheTTL)
	}
}

func TestClusterStatsCachePerCluster(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewClusterStatsCache(10 * time.Second)
	c.SetClockForTest(func() time.Time { return now })
	c.Set("c1", 1, 10)
	c.Set("c2", 2, 20)
	if b, o, ok := c.Get("c1"); !ok || b != 1 || o != 10 {
		t.Fatalf("c1: got %d/%d ok=%v", b, o, ok)
	}
	if b, o, ok := c.Get("c2"); !ok || b != 2 || o != 20 {
		t.Fatalf("c2: got %d/%d ok=%v", b, o, ok)
	}
}
