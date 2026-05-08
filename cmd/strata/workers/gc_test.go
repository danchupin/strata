package workers

import (
	"testing"
	"time"

	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/gc"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func TestGCWorkerRegistered(t *testing.T) {
	w, ok := Lookup("gc")
	if !ok {
		t.Fatal("gc worker not registered (init() did not fire)")
	}
	if w.Name != "gc" {
		t.Fatalf("name=%q want gc", w.Name)
	}
	if !w.SkipLease {
		t.Fatal("gc worker must run with SkipLease=true (FanOut owns its leases)")
	}
}

func TestBuildGCReadsEnv(t *testing.T) {
	t.Setenv("STRATA_GC_INTERVAL", "7s")
	t.Setenv("STRATA_GC_GRACE", "11m")
	t.Setenv("STRATA_GC_BATCH_SIZE", "250")
	t.Setenv("STRATA_GC_SHARDS", "4")

	deps := Dependencies{
		Meta:   metamem.New(),
		Data:   datamem.New(),
		Locker: metamem.NewLocker(),
		Region: "test-region",
	}
	r, err := buildGC(deps)
	if err != nil {
		t.Fatalf("buildGC: %v", err)
	}
	fan, ok := r.(*gc.FanOut)
	if !ok {
		t.Fatalf("buildGC returned %T, want *gc.FanOut", r)
	}
	if fan.ShardCount != 4 {
		t.Fatalf("ShardCount=%d want 4", fan.ShardCount)
	}
	if fan.Locker == nil {
		t.Fatal("Locker not propagated")
	}
	w := fan.Build(0)
	if w.Interval != 7*time.Second {
		t.Errorf("Interval=%v want 7s", w.Interval)
	}
	if w.Grace != 11*time.Minute {
		t.Errorf("Grace=%v want 11m", w.Grace)
	}
	if w.Batch != 250 {
		t.Errorf("Batch=%d want 250", w.Batch)
	}
	if w.Region != "test-region" {
		t.Errorf("Region=%q", w.Region)
	}
	if w.ShardCount != 4 {
		t.Errorf("inner ShardCount=%d want 4", w.ShardCount)
	}
	if w.ShardID != 0 {
		t.Errorf("inner ShardID=%d want 0", w.ShardID)
	}
	if w.Meta == nil || w.Data == nil {
		t.Error("Meta/Data not propagated")
	}
}

func TestBuildGCDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv("STRATA_GC_INTERVAL", "")
	t.Setenv("STRATA_GC_GRACE", "")
	t.Setenv("STRATA_GC_BATCH_SIZE", "")
	t.Setenv("STRATA_GC_SHARDS", "")

	r, err := buildGC(Dependencies{
		Meta:   metamem.New(),
		Data:   datamem.New(),
		Locker: metamem.NewLocker(),
	})
	if err != nil {
		t.Fatalf("buildGC: %v", err)
	}
	fan := r.(*gc.FanOut)
	if fan.ShardCount != 1 {
		t.Errorf("ShardCount=%d want 1 default", fan.ShardCount)
	}
	w := fan.Build(0)
	if w.Interval != 30*time.Second {
		t.Errorf("Interval=%v want 30s default", w.Interval)
	}
	if w.Grace != 5*time.Minute {
		t.Errorf("Grace=%v want 5m default", w.Grace)
	}
	if w.Batch != 0 {
		t.Errorf("Batch=%d want 0 (gc.Worker default kicks in)", w.Batch)
	}
}

func TestBuildGCClampShardsAboveCeiling(t *testing.T) {
	t.Setenv("STRATA_GC_SHARDS", "9999")
	r, err := buildGC(Dependencies{
		Meta:   metamem.New(),
		Data:   datamem.New(),
		Locker: metamem.NewLocker(),
	})
	if err != nil {
		t.Fatalf("buildGC: %v", err)
	}
	fan := r.(*gc.FanOut)
	if fan.ShardCount != 1024 {
		t.Fatalf("ShardCount=%d want 1024 (clamped to logical-shard ceiling)", fan.ShardCount)
	}
}

func TestBuildGCClampShardsBelowFloor(t *testing.T) {
	t.Setenv("STRATA_GC_SHARDS", "-7")
	r, err := buildGC(Dependencies{
		Meta:   metamem.New(),
		Data:   datamem.New(),
		Locker: metamem.NewLocker(),
	})
	if err != nil {
		t.Fatalf("buildGC: %v", err)
	}
	fan := r.(*gc.FanOut)
	if fan.ShardCount != 1 {
		t.Fatalf("ShardCount=%d want 1 (clamped to floor)", fan.ShardCount)
	}
}
