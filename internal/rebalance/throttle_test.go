package rebalance

import (
	"context"
	"testing"
	"time"
)

func TestThrottleNilIsNoop(t *testing.T) {
	var th *Throttle
	if err := th.Wait(context.Background(), 1<<30); err != nil {
		t.Fatalf("nil throttle should be no-op; got %v", err)
	}
}

func TestThrottleZeroRateDisables(t *testing.T) {
	th := NewThrottle(0, 0)
	if th != nil {
		t.Fatalf("zero rate should return nil throttle")
	}
}

func TestThrottleBlocksUntilTokensAvailable(t *testing.T) {
	// 1024 B/s with 1024 B burst. Spending 2048 B should take roughly
	// one second of refill on top of the burst.
	th := NewThrottle(1024, 1024)
	if th == nil {
		t.Fatal("expected non-nil throttle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := th.Wait(ctx, 1024); err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	if err := th.Wait(ctx, 1024); err != nil {
		t.Fatalf("second Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("expected ~1s of throttling; got %v", elapsed)
	}
}

func TestThrottleHonoursContextCancellation(t *testing.T) {
	th := NewThrottle(1, 1) // 1 B/s — anything above 1 B blocks forever
	if th == nil {
		t.Fatal("expected non-nil throttle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := th.Wait(ctx, 1<<20); err == nil {
		t.Fatal("expected context error when throttle starves")
	}
}

func TestThrottleClampsTokensToBurst(t *testing.T) {
	// 1 MB/s with 4 KiB burst — requesting 8 KiB must not deadlock.
	th := NewThrottle(1<<20, 4096)
	if th == nil {
		t.Fatal("expected non-nil throttle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := th.Wait(ctx, 8192); err != nil {
		t.Fatalf("Wait(8KiB) under 4KiB burst should not deadlock; got %v", err)
	}
}
