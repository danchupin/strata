package workers

import (
	"log/slog"
	"testing"
	"time"

	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/rebalance"
)

func TestRebalanceWorkerRegistered(t *testing.T) {
	w, ok := Lookup("rebalance")
	if !ok {
		t.Fatal("rebalance worker not registered (init() did not fire)")
	}
	if !w.SkipLease {
		t.Fatal("rebalance Phase 2 must own its own leader election (SkipLease=true)")
	}
}

func TestResolveRebalanceConfigEnv(t *testing.T) {
	t.Setenv("STRATA_REBALANCE_INTERVAL", "30s")
	t.Setenv("STRATA_REBALANCE_RATE_MB_S", "250")
	t.Setenv("STRATA_REBALANCE_INFLIGHT", "8")
	t.Setenv("STRATA_REBALANCE_SHARDS", "2")
	got := ResolveRebalanceConfig()
	// 30s clamps up to 60s (min interval).
	want := ResolvedRebalanceConfig{
		IntervalSeconds: 60,
		RateMBPerSec:    250,
		Inflight:        8,
		Shards:          2,
	}
	if got != want {
		t.Fatalf("ResolveRebalanceConfig=%+v want %+v", got, want)
	}
}

func TestResolveRebalanceConfigDefaults(t *testing.T) {
	t.Setenv("STRATA_REBALANCE_INTERVAL", "")
	t.Setenv("STRATA_REBALANCE_RATE_MB_S", "")
	t.Setenv("STRATA_REBALANCE_INFLIGHT", "")
	t.Setenv("STRATA_REBALANCE_SHARDS", "")
	got := ResolveRebalanceConfig()
	want := ResolvedRebalanceConfig{
		IntervalSeconds: 300,
		RateMBPerSec:    100,
		Inflight:        4,
		Shards:          1,
	}
	if got != want {
		t.Fatalf("ResolveRebalanceConfig defaults=%+v want %+v", got, want)
	}
}

func TestResolveRebalanceConfigClampsOutOfRange(t *testing.T) {
	t.Setenv("STRATA_REBALANCE_INTERVAL", "48h")
	t.Setenv("STRATA_REBALANCE_RATE_MB_S", "0")
	t.Setenv("STRATA_REBALANCE_INFLIGHT", "1000")
	t.Setenv("STRATA_REBALANCE_SHARDS", "9999")
	got := ResolveRebalanceConfig()
	want := ResolvedRebalanceConfig{
		IntervalSeconds: 86400,
		RateMBPerSec:    1,
		Inflight:        64,
		Shards:          1024,
	}
	if got != want {
		t.Fatalf("ResolveRebalanceConfig clamps=%+v want %+v", got, want)
	}
}

func TestBuildRebalanceReadsEnv(t *testing.T) {
	t.Setenv("STRATA_REBALANCE_INTERVAL", "5m")
	t.Setenv("STRATA_REBALANCE_RATE_MB_S", "250")
	t.Setenv("STRATA_REBALANCE_INFLIGHT", "8")

	deps := Dependencies{
		Logger: slog.Default(),
		Meta:   metamem.New(),
		Data:   datamem.New(),
		Locker: metamem.NewLocker(),
	}
	r, err := buildRebalance(deps)
	if err != nil {
		t.Fatalf("buildRebalance: %v", err)
	}
	fan, ok := r.(*rebalance.ShardedFanOut)
	if !ok {
		t.Fatalf("buildRebalance returned %T, want *rebalance.ShardedFanOut", r)
	}
	if fan.ShardCount != 1 {
		t.Fatalf("default ShardCount: got %d want 1", fan.ShardCount)
	}
}

func TestBuildRebalanceShardsEnv(t *testing.T) {
	t.Setenv("STRATA_REBALANCE_SHARDS", "4")
	deps := Dependencies{
		Logger: slog.Default(),
		Meta:   metamem.New(),
		Data:   datamem.New(),
		Locker: metamem.NewLocker(),
	}
	r, err := buildRebalance(deps)
	if err != nil {
		t.Fatalf("buildRebalance: %v", err)
	}
	fan := r.(*rebalance.ShardedFanOut)
	if fan.ShardCount != 4 {
		t.Fatalf("ShardCount: got %d want 4", fan.ShardCount)
	}
}

func TestBuildRebalanceClampsOutOfRange(t *testing.T) {
	// Below min — should clamp up.
	t.Setenv("STRATA_REBALANCE_INTERVAL", "10s")
	// Above max — should clamp down.
	t.Setenv("STRATA_REBALANCE_RATE_MB_S", "999999")
	t.Setenv("STRATA_REBALANCE_INFLIGHT", "0")
	t.Setenv("STRATA_REBALANCE_SHARDS", "99999")

	deps := Dependencies{
		Logger: slog.Default(),
		Meta:   metamem.New(),
		Data:   datamem.New(),
		Locker: metamem.NewLocker(),
	}
	r, err := buildRebalance(deps)
	if err != nil {
		t.Fatalf("buildRebalance: %v", err)
	}
	fan, ok := r.(*rebalance.ShardedFanOut)
	if !ok {
		t.Fatalf("buildRebalance returned %T, want *rebalance.ShardedFanOut", r)
	}
	if fan.ShardCount != 1024 {
		t.Fatalf("ShardCount clamp: got %d want 1024", fan.ShardCount)
	}
}

func TestRebalanceResolveAcceptsName(t *testing.T) {
	got, err := Resolve([]string{"rebalance"})
	if err != nil {
		t.Fatalf("Resolve(rebalance): %v", err)
	}
	if len(got) != 1 || got[0].Name != "rebalance" {
		t.Fatalf("Resolve(rebalance) = %#v", got)
	}
}

func TestClampDurationHelper(t *testing.T) {
	deps := Dependencies{Logger: slog.Default()}
	got := clampDuration(deps, "STRATA_REBALANCE_INTERVAL_TEST_ABSENT", 30*time.Minute, time.Minute, time.Hour)
	if got != 30*time.Minute {
		t.Errorf("default returned: %v", got)
	}
	t.Setenv("STRATA_REBALANCE_INTERVAL_TEST_LO", "10s")
	if got := clampDuration(deps, "STRATA_REBALANCE_INTERVAL_TEST_LO", 30*time.Minute, time.Minute, time.Hour); got != time.Minute {
		t.Errorf("clamp-lo: got %v want 1m", got)
	}
	t.Setenv("STRATA_REBALANCE_INTERVAL_TEST_HI", "100h")
	if got := clampDuration(deps, "STRATA_REBALANCE_INTERVAL_TEST_HI", 30*time.Minute, time.Minute, time.Hour); got != time.Hour {
		t.Errorf("clamp-hi: got %v want 1h", got)
	}
}

func TestClampIntHelper(t *testing.T) {
	deps := Dependencies{Logger: slog.Default()}
	if got := clampInt(deps, "STRATA_REBALANCE_RATE_MB_S_TEST_ABSENT", 100, 1, 1000); got != 100 {
		t.Errorf("default: got %d", got)
	}
	t.Setenv("STRATA_REBALANCE_RATE_MB_S_TEST_LO", "-5")
	if got := clampInt(deps, "STRATA_REBALANCE_RATE_MB_S_TEST_LO", 100, 1, 1000); got != 1 {
		t.Errorf("clamp-lo: got %d want 1", got)
	}
	t.Setenv("STRATA_REBALANCE_RATE_MB_S_TEST_HI", "99999")
	if got := clampInt(deps, "STRATA_REBALANCE_RATE_MB_S_TEST_HI", 100, 1, 1000); got != 1000 {
		t.Errorf("clamp-hi: got %d want 1000", got)
	}
}
