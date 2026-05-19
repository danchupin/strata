package rados

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestNextRoundRobinWraps(t *testing.T) {
	var counter atomic.Uint64
	got := make([]int, 9)
	for i := range got {
		got[i] = nextRoundRobin(&counter, 4)
	}
	want := []int{0, 1, 2, 3, 0, 1, 2, 3, 0}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("idx[%d]: want %d got %d", i, want[i], got[i])
		}
	}
}

func TestNextRoundRobinSizeOne(t *testing.T) {
	var counter atomic.Uint64
	for i := range 5 {
		if got := nextRoundRobin(&counter, 1); got != 0 {
			t.Fatalf("size=1 idx[%d]: want 0 got %d", i, got)
		}
	}
}

func TestNextRoundRobinEmptyPool(t *testing.T) {
	var counter atomic.Uint64
	if got := nextRoundRobin(&counter, 0); got != 0 {
		t.Fatalf("size=0: want 0 got %d", got)
	}
}

func TestNextRoundRobinConcurrentFairness(t *testing.T) {
	var counter atomic.Uint64
	const size = 8
	const iters = 1000
	counts := make([]atomic.Int64, size)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range iters {
				idx := nextRoundRobin(&counter, size)
				counts[idx].Add(1)
			}
		})
	}
	wg.Wait()
	total := int64(0)
	for i := range counts {
		total += counts[i].Load()
	}
	if total != int64(16*iters) {
		t.Fatalf("total observations: want %d got %d", 16*iters, total)
	}
	// Each slot should get within 5% of perfect-share. With 16 goroutines
	// × 1000 calls = 16000 observations on 8 slots → perfect-share = 2000.
	const perfect = int64(16 * iters / size)
	for i := range counts {
		got := counts[i].Load()
		if got < perfect*95/100 || got > perfect*105/100 {
			t.Fatalf("slot %d: count %d outside 5%% of perfect %d", i, got, perfect)
		}
	}
}
