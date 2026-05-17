package rebalance

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/metrics"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestShardedFanOutThreeReplicasDisjointShards spawns three concurrent
// ShardedFanOut runners against a shared in-memory locker (the Phase-2
// production shape: N gateway replicas, one Cassandra/TiKV LWT-backed
// locker). Asserts every shard is eventually leased by some replica and
// no shard is held by two replicas concurrently.
func TestShardedFanOutThreeReplicasDisjointShards(t *testing.T) {
	const replicas = 3
	const shards = 3

	locker := metamem.NewLocker()

	type rep struct {
		fan *ShardedFanOut
		ctx context.Context
		cxl context.CancelFunc
	}
	reps := make([]*rep, replicas)
	for i := range replicas {
		ctx, cxl := context.WithCancel(context.Background())
		fan := &ShardedFanOut{
			Locker:       locker,
			ShardCount:   shards,
			Logger:       discardLogger(),
			LeaderTTL:    50 * time.Millisecond,
			LeaderRenew:  10 * time.Millisecond,
			AcquireRetry: 5 * time.Millisecond,
			Build: func(shardID int) *Worker {
				w, _ := New(Config{
					Meta:     metamem.New(),
					Data:     datamem.New(),
					Logger:   discardLogger(),
					Interval: time.Hour,
				})
				return w
			},
		}
		reps[i] = &rep{fan: fan, ctx: ctx, cxl: cxl}
	}

	var wg sync.WaitGroup
	for _, r := range reps {
		wg.Go(func() {
			_ = r.fan.Run(r.ctx)
		})
	}

	t.Cleanup(func() {
		for _, r := range reps {
			r.cxl()
		}
		wg.Wait()
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		held := map[int]int{}
		for _, r := range reps {
			for _, sh := range r.fan.HeldShards() {
				held[sh]++
			}
		}
		covered := true
		for sh := range shards {
			if held[sh] == 0 {
				covered = false
				break
			}
		}
		if covered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shards never fully covered: held=%v", held)
		}
		time.Sleep(10 * time.Millisecond)
	}

	for range 20 {
		held := map[int]int{}
		for _, r := range reps {
			for _, sh := range r.fan.HeldShards() {
				held[sh]++
			}
		}
		for sh, n := range held {
			if n > 1 {
				t.Fatalf("shard %d held by %d replicas concurrently — leader-election broken", sh, n)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestShardedFanOutSingleReplicaSingleShard pins the Phase-1-equivalent
// shape: `STRATA_REBALANCE_SHARDS=1` with one replica. The replica owns
// shard 0 and ticks once. RunOnce-style assertion would require coupling
// to the tick loop; instead we verify the lease + shard count via
// HeldShards.
func TestShardedFanOutSingleReplicaSingleShard(t *testing.T) {
	locker := metamem.NewLocker()
	var ranWith atomic.Int64
	var ranShardCount atomic.Int64
	fan := &ShardedFanOut{
		Locker:       locker,
		ShardCount:   1,
		Logger:       discardLogger(),
		LeaderTTL:    50 * time.Millisecond,
		LeaderRenew:  10 * time.Millisecond,
		AcquireRetry: 5 * time.Millisecond,
		Build: func(shardID int) *Worker {
			ranWith.Store(int64(shardID))
			ranShardCount.Add(1)
			w, _ := New(Config{
				Meta:     metamem.New(),
				Data:     datamem.New(),
				Logger:   discardLogger(),
				Interval: time.Hour,
			})
			return w
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = fan.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if slices.Equal(fan.HeldShards(), []int{0}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shard 0 never leased; HeldShards=%v", fan.HeldShards())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if ranWith.Load() != 0 {
		t.Fatalf("Build called with shardID=%d, want 0", ranWith.Load())
	}
	if ranShardCount.Load() < 1 {
		t.Fatalf("Build called %d times, want >= 1", ranShardCount.Load())
	}
}

// TestShardedFanOutPanicIsolation pins the AC: a panic in shard-N's scan
// loop releases shard-N only; sibling shards keep scanning.
func TestShardedFanOutPanicIsolation(t *testing.T) {
	locker := metamem.NewLocker()

	var shardZeroPanics atomic.Int64
	beforeShardZero := readPanicCounter(t, "rebalance", "0")
	beforeShardOne := readPanicCounter(t, "rebalance", "1")

	fan := &ShardedFanOut{
		Locker:       locker,
		ShardCount:   2,
		Logger:       discardLogger(),
		Backoff:      []time.Duration{0, 0, 0, 0},
		Sleep:        func(context.Context, time.Duration) {},
		LeaderTTL:    50 * time.Millisecond,
		LeaderRenew:  15 * time.Millisecond,
		AcquireRetry: 5 * time.Millisecond,
		StableAfter:  time.Hour,
		Build: func(shardID int) *Worker {
			if shardID == 0 {
				w, _ := New(Config{
					Meta:     &panicOnListMeta{Store: metamem.New(), counter: &shardZeroPanics},
					Data:     datamem.New(),
					Logger:   discardLogger(),
					Interval: 1 * time.Millisecond,
				})
				return w
			}
			w, _ := New(Config{
				Meta:     metamem.New(),
				Data:     datamem.New(),
				Logger:   discardLogger(),
				Interval: 50 * time.Millisecond,
			})
			return w
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = fan.Run(ctx)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for shardZeroPanics.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("shard 0 only panicked %d times in 3s; expected >= 3", shardZeroPanics.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}

	heldShards := fan.HeldShards()
	if !slices.Contains(heldShards, 1) {
		t.Fatalf("shard 1 lease lost while shard 0 was panicking; HeldShards=%v", heldShards)
	}

	cancel()
	<-done

	gotShardZero := readPanicCounter(t, "rebalance", "0") - beforeShardZero
	if gotShardZero < 3 {
		t.Fatalf("strata_worker_panic_total{worker=rebalance,shard=0} delta = %v, want >= 3", gotShardZero)
	}
	gotShardOne := readPanicCounter(t, "rebalance", "1") - beforeShardOne
	if gotShardOne != 0 {
		t.Fatalf("strata_worker_panic_total{worker=rebalance,shard=1} delta = %v, want 0 — sibling shard must not panic", gotShardOne)
	}
}

// TestShardedFanOutOnLeaderTransitions verifies OnLeader fires exactly
// twice per run: once on the 0→1 acquire and once on the N→0 release.
// Multi-shard acquire/release inside a single replica is folded into one
// acquired/released pair so the heartbeat chip flips at most twice.
func TestShardedFanOutOnLeaderTransitions(t *testing.T) {
	locker := metamem.NewLocker()

	var transitions []bool
	var mu sync.Mutex
	fan := &ShardedFanOut{
		Locker:       locker,
		ShardCount:   3,
		Logger:       discardLogger(),
		LeaderTTL:    50 * time.Millisecond,
		LeaderRenew:  15 * time.Millisecond,
		AcquireRetry: 5 * time.Millisecond,
		OnLeader: func(acquired bool) {
			mu.Lock()
			transitions = append(transitions, acquired)
			mu.Unlock()
		},
		Build: func(shardID int) *Worker {
			w, _ := New(Config{
				Meta:     metamem.New(),
				Data:     datamem.New(),
				Logger:   discardLogger(),
				Interval: 50 * time.Millisecond,
			})
			return w
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = fan.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(transitions)
		mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("OnLeader(true) never fired")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(transitions) < 2 {
		t.Fatalf("transitions=%v, want at least one acquired+released pair", transitions)
	}
	if transitions[0] != true {
		t.Fatalf("first transition = %v, want true (acquired)", transitions[0])
	}
	if transitions[len(transitions)-1] != false {
		t.Fatalf("last transition = %v, want false (released)", transitions[len(transitions)-1])
	}
	// Folding invariant: a single replica that races multiple shards
	// must never emit a tight (true,false,true,...) cascade. The first
	// half should be all-true and the trailing edge all-false at most.
	acquireCount, releaseCount := 0, 0
	for _, v := range transitions {
		if v {
			acquireCount++
		} else {
			releaseCount++
		}
	}
	if acquireCount > 1 || releaseCount > 1 {
		t.Fatalf("folded leader chip violated: transitions=%v acquires=%d releases=%d",
			transitions, acquireCount, releaseCount)
	}
}

// TestShardedFanOutLeaseNamePin verifies the lease key shape is the
// documented `rebalance-leader-<shardID>` — operator dashboards /
// runbooks rely on this naming.
func TestShardedFanOutLeaseNamePin(t *testing.T) {
	if got := FanOutLeaseName(0); got != "rebalance-leader-0" {
		t.Fatalf("FanOutLeaseName(0)=%q want rebalance-leader-0", got)
	}
	if got := FanOutLeaseName(7); got != "rebalance-leader-7" {
		t.Fatalf("FanOutLeaseName(7)=%q want rebalance-leader-7", got)
	}
}

// TestShardedFanOutHeldShardsSorted: HeldShards always returns ascending
// IDs so callers can use HeldShards()[0] as a stable myReplicaID.
func TestShardedFanOutHeldShardsSorted(t *testing.T) {
	f := &ShardedFanOut{}
	f.markHeld(7, true)
	f.markHeld(2, true)
	f.markHeld(5, true)
	got := f.HeldShards()
	want := []int{2, 5, 7}
	if len(got) != len(want) {
		t.Fatalf("HeldShards()=%v want %v", got, want)
	}
	if !sort.IntsAreSorted(got) {
		t.Fatalf("HeldShards not sorted: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("HeldShards[%d]=%d want %d", i, got[i], want[i])
		}
	}
	f.markHeld(2, false)
	got = f.HeldShards()
	want = []int{5, 7}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("after release, HeldShards[%d]=%d want %d", i, got[i], want[i])
		}
	}
}

// TestShardedFanOutRequiresLockerAndBuild covers the construction guard
// rails: missing Locker or Build returns a startup error rather than
// panicking.
func TestShardedFanOutRequiresLockerAndBuild(t *testing.T) {
	cases := map[string]*ShardedFanOut{
		"missing locker": {Build: func(int) *Worker { return nil }},
		"missing build":  {Locker: metamem.NewLocker()},
	}
	for name, fan := range cases {
		t.Run(name, func(t *testing.T) {
			fan.Logger = discardLogger()
			err := fan.Run(context.Background())
			if err == nil {
				t.Fatal("expected startup error, got nil")
			}
		})
	}
}

// TestShardedFanOutThreeShardsAllRunShardCalled verifies every shard ID
// 0..ShardCount-1 makes it through Build → RunShard at least once when
// a single replica owns the whole keyspace.
func TestShardedFanOutThreeShardsAllRunShardCalled(t *testing.T) {
	locker := metamem.NewLocker()
	var mu sync.Mutex
	seen := map[int]int{}
	fan := &ShardedFanOut{
		Locker:       locker,
		ShardCount:   3,
		Logger:       discardLogger(),
		LeaderTTL:    50 * time.Millisecond,
		LeaderRenew:  10 * time.Millisecond,
		AcquireRetry: 5 * time.Millisecond,
		Build: func(shardID int) *Worker {
			mu.Lock()
			seen[shardID]++
			mu.Unlock()
			w, _ := New(Config{
				Meta:     metamem.New(),
				Data:     datamem.New(),
				Logger:   discardLogger(),
				Interval: 50 * time.Millisecond,
			})
			return w
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = fan.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("Build never invoked for every shard; seen=%v", seen)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func readPanicCounter(t *testing.T, worker, shard string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := metrics.WorkerPanicTotal.WithLabelValues(worker, shard).Write(m); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// panicOnListMeta wraps metamem.Store with a ListBuckets that panics on
// every call so the per-shard scan loop trips the panic-recovery path.
// Used by TestShardedFanOutPanicIsolation to drive the metrics + restart
// contract.
type panicOnListMeta struct {
	*metamem.Store
	counter *atomic.Int64
}

func (p *panicOnListMeta) ListBuckets(_ context.Context, _ string) ([]*meta.Bucket, error) {
	if p.counter != nil {
		p.counter.Add(1)
	}
	panic("rigged panic for shard isolation test")
}

// ListBucketsShard overrides the bucket-scan entry point used by the
// US-003 rebalance-scale-phase-2 worker so the panic-isolation contract
// still trips even though Phase 2 routes through the sharded API.
func (p *panicOnListMeta) ListBucketsShard(_ context.Context, _, _ int) ([]*meta.Bucket, error) {
	if p.counter != nil {
		p.counter.Add(1)
	}
	panic("rigged panic for shard isolation test")
}
