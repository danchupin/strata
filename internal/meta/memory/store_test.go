package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/meta/storetest"
)

func TestMemoryStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) meta.Store { return memory.New() })
}

func TestMemoryClusterRegistry(t *testing.T) {
	storetest.CaseClusterRegistry(t, memory.New())
}

// TestMemoryBucketStatsHotPath verifies the PutObject / DeleteObject /
// CompleteMultipartUpload paths bump the live counter on the memory backend
// (US-004). Cassandra + TiKV hot-path wiring lands in US-005.
func TestMemoryBucketStatsHotPath(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	b, err := s.CreateBucket(ctx, "hp", "owner-hp", "STANDARD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// PUT — counter incremented by Size + 1.
	o := &meta.Object{
		BucketID:     b.ID,
		Key:          "k1",
		Size:         100,
		StorageClass: "STANDARD",
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{Class: "STANDARD", Size: 100},
	}
	if err := s.PutObject(ctx, o, false); err != nil {
		t.Fatalf("put: %v", err)
	}
	stats, err := s.GetBucketStats(ctx, b.ID)
	if err != nil {
		t.Fatalf("get after put: %v", err)
	}
	if stats.UsedBytes != 100 || stats.UsedObjects != 1 {
		t.Fatalf("after put: %+v", stats)
	}

	// PUT overwrite (unversioned) — count stays 1; bytes track new size.
	o2 := &meta.Object{
		BucketID:     b.ID,
		Key:          "k1",
		Size:         250,
		StorageClass: "STANDARD",
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{Class: "STANDARD", Size: 250},
	}
	if err := s.PutObject(ctx, o2, false); err != nil {
		t.Fatalf("put overwrite: %v", err)
	}
	stats, _ = s.GetBucketStats(ctx, b.ID)
	if stats.UsedBytes != 250 || stats.UsedObjects != 1 {
		t.Fatalf("after overwrite: %+v", stats)
	}

	// PUT a second key — count goes to 2.
	o3 := &meta.Object{
		BucketID:     b.ID,
		Key:          "k2",
		Size:         50,
		StorageClass: "STANDARD",
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{Class: "STANDARD", Size: 50},
	}
	if err := s.PutObject(ctx, o3, false); err != nil {
		t.Fatalf("put k2: %v", err)
	}
	stats, _ = s.GetBucketStats(ctx, b.ID)
	if stats.UsedBytes != 300 || stats.UsedObjects != 2 {
		t.Fatalf("after k2 put: %+v", stats)
	}

	// DELETE k1 — counter decrements.
	if _, err := s.DeleteObject(ctx, b.ID, "k1", "", false); err != nil {
		t.Fatalf("delete: %v", err)
	}
	stats, _ = s.GetBucketStats(ctx, b.ID)
	if stats.UsedBytes != 50 || stats.UsedObjects != 1 {
		t.Fatalf("after delete: %+v", stats)
	}
}

// TestMemoryStoreImplementsRangeScanStore confirms the memory backend
// advertises the optional meta.RangeScanStore capability surface (US-012).
// The compile-time assertion in store.go enforces the same; this is a
// runtime smoke test so a future refactor that breaks the interface
// contract surfaces here too.
func TestMemoryStoreImplementsRangeScanStore(t *testing.T) {
	var s meta.Store = memory.New()
	rs, ok := s.(meta.RangeScanStore)
	if !ok {
		t.Fatal("memory.Store must implement meta.RangeScanStore — see US-012")
	}
	if rs == nil {
		t.Fatal("type assertion returned nil RangeScanStore")
	}
}
