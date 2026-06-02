package cassandra

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunBoundedFanOut_CapsConcurrency is the US-012 discriminator: the
// pre-US-012 ListObjects fan-out spawned one goroutine per shard with NO
// semaphore, so peak in-flight == shardCount. runBoundedFanOut must hold peak
// in-flight at or below the configured concurrency regardless of shard count,
// while still visiting every shard exactly once.
func TestRunBoundedFanOut_CapsConcurrency(t *testing.T) {
	const (
		shardCount  = 128
		concurrency = 8
	)

	var inFlight, maxInFlight int64
	visits := make([]int64, shardCount)

	err := runBoundedFanOut(context.Background(), shardCount, concurrency, func(shard int) error {
		cur := atomic.AddInt64(&inFlight, 1)
		for {
			prev := atomic.LoadInt64(&maxInFlight)
			if cur <= prev || atomic.CompareAndSwapInt64(&maxInFlight, prev, cur) {
				break
			}
		}
		// Hold the slot briefly so concurrency actually builds up — otherwise a
		// fast callback could finish before the next is dispatched and the test
		// would pass vacuously with maxInFlight==1.
		time.Sleep(time.Millisecond)
		atomic.AddInt64(&visits[shard], 1)
		atomic.AddInt64(&inFlight, -1)
		return nil
	})
	if err != nil {
		t.Fatalf("runBoundedFanOut returned error: %v", err)
	}

	if got := atomic.LoadInt64(&maxInFlight); got > concurrency {
		t.Fatalf("peak in-flight = %d, want <= concurrency %d (semaphore not bounding fan-out)", got, concurrency)
	}
	// Non-vacuous: with 128 shards and a 1ms hold the pool must have actually
	// parallelised, else the bound is meaningless.
	if got := atomic.LoadInt64(&maxInFlight); got < 2 {
		t.Fatalf("peak in-flight = %d, expected real parallelism (>=2)", got)
	}
	for shard, n := range visits {
		if n != 1 {
			t.Fatalf("shard %d visited %d times, want exactly 1", shard, n)
		}
	}
}

// TestRunBoundedFanOut_PropagatesError proves a single shard failure surfaces
// to the caller (ListObjects must not silently drop a shard's read error).
func TestRunBoundedFanOut_PropagatesError(t *testing.T) {
	sentinel := errors.New("shard read failed")
	err := runBoundedFanOut(context.Background(), 32, 4, func(shard int) error {
		if shard == 17 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want %v", err, sentinel)
	}
}

// TestRunBoundedFanOut_RespectsCancellation proves a cancelled context stops
// dispatch and returns ctx.Err() rather than draining every shard.
func TestRunBoundedFanOut_RespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var started int64
	var once sync.Once
	err := runBoundedFanOut(ctx, 1024, 2, func(shard int) error {
		atomic.AddInt64(&started, 1)
		once.Do(cancel)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt64(&started); got >= 1024 {
		t.Fatalf("dispatched all %d shards despite cancellation", got)
	}
}

// TestRunBoundedFanOut_DegenerateConcurrency clamps a non-positive or
// oversized concurrency to a sane range.
func TestRunBoundedFanOut_DegenerateConcurrency(t *testing.T) {
	cases := []struct {
		name        string
		shardCount  int
		concurrency int
	}{
		{name: "zero concurrency clamps to 1", shardCount: 8, concurrency: 0},
		{name: "negative concurrency clamps to 1", shardCount: 8, concurrency: -4},
		{name: "concurrency above shardCount is fine", shardCount: 4, concurrency: 64},
		{name: "zero shards is a no-op", shardCount: 0, concurrency: 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var visited int64
			err := runBoundedFanOut(context.Background(), tc.shardCount, tc.concurrency, func(shard int) error {
				atomic.AddInt64(&visited, 1)
				return nil
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if int(visited) != tc.shardCount {
				t.Fatalf("visited %d shards, want %d", visited, tc.shardCount)
			}
		})
	}
}

func TestListConcurrencyFromEnv(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		expectedConc int
	}{
		{name: "empty uses default", raw: "", expectedConc: DefaultListConcurrency},
		{name: "valid value honored", raw: "32", expectedConc: 32},
		{name: "one is allowed", raw: "1", expectedConc: 1},
		{name: "zero falls back to default", raw: "0", expectedConc: DefaultListConcurrency},
		{name: "negative falls back to default", raw: "-5", expectedConc: DefaultListConcurrency},
		{name: "garbage falls back to default", raw: "abc", expectedConc: DefaultListConcurrency},
		{name: "above max clamps down", raw: "100000", expectedConc: maxListConcurrency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvListConcurrency, tc.raw)
			if got := ListConcurrencyFromEnv(); got != tc.expectedConc {
				t.Fatalf("ListConcurrencyFromEnv()=%d, want %d", got, tc.expectedConc)
			}
		})
	}
}
