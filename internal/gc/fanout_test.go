package gc

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

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/metrics"
)

// stubLocker mirrors memory.Locker but with a smaller struct surface for
// tests that need a process-local leader.Locker without pulling in the
// meta-memory dependency.
//
// Reuses memory.NewLocker() — left here as a comment to mark the design
// choice: gc.FanOut takes leader.Locker, the in-memory Locker satisfies it.

// TestFanOutThreeReplicasDisjointShards spawns three concurrent FanOut
// runners against a shared in-memory locker (the Phase-2 production shape:
// N gateway replicas, one Cassandra/TiKV LWT-backed locker). Asserts:
//   - every shard is eventually leased by some replica (`gc-leader-0..2`
//     all held)
//   - no shard is held by two replicas at the same moment
//
// Fair distribution across replicas is a property of the underlying
// locker's lease scheduler (Cassandra LWT, TiKV pessimistic-txn) which the
// in-process locker does not model — once a replica wins a lease its
// renew loop holds it indefinitely. The Phase-2 contract only requires
// disjointness; fairness is delivered by the production locker.
func TestFanOutThreeReplicasDisjointShards(t *testing.T) {
	const replicas = 3
	const shards = 3

	locker := memory.NewLocker()

	type rep struct {
		fan *FanOut
		ctx context.Context
		cxl context.CancelFunc
	}
	reps := make([]*rep, replicas)
	for i := range replicas {
		ctx, cxl := context.WithCancel(context.Background())
		fan := &FanOut{
			Locker:       locker,
			ShardCount:   shards,
			Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			LeaderTTL:    50 * time.Millisecond,
			LeaderRenew:  10 * time.Millisecond,
			AcquireRetry: 5 * time.Millisecond,
			Build: func(shardID int) *Worker {
				return &Worker{
					Meta:     memory.New(),
					Region:   "default",
					Interval: 50 * time.Millisecond,
				}
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

	// Wait until every shard is leased somewhere.
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

	// Disjointness invariant: no shard is held by two replicas at once.
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

// TestFanOutSingleReplicaSingleShard pins the Phase-1-equivalent shape:
// `STRATA_GC_SHARDS=1` with one replica. The replica owns shard 0 and
// drains entries via ListGCEntriesShard(shardID=0, shardCount=1) — same
// rows the legacy ListGCEntries returns.
func TestFanOutSingleReplicaSingleShard(t *testing.T) {
	store := memory.New()
	chunks := []data.ChunkRef{
		{Cluster: "default", Pool: "hot", OID: "a", Size: 1},
		{Cluster: "default", Pool: "hot", OID: "b", Size: 2},
		{Cluster: "default", Pool: "hot", OID: "c", Size: 3},
	}
	if err := store.EnqueueChunkDeletion(context.Background(), "default", chunks); err != nil {
		t.Fatalf("EnqueueChunkDeletion: %v", err)
	}

	be := &recordingBackend{}
	w := &Worker{Meta: store, Data: be, Region: "default", ShardID: 0, ShardCount: 1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	processed := w.RunOnce(context.Background())
	if processed != 3 {
		t.Fatalf("processed=%d want 3", processed)
	}
}

// TestFanOutPanicIsolation pins the AC: a panic in shard-N's drain loop
// releases shard-N only; sibling shards keep draining.
//
// Drives one FanOut runner with shardCount=2; shard 0's Build returns a
// Worker that panics on first Run; shard 1's Build returns a Worker that
// blocks until ctx cancellation. We assert the panic counter on shard 0
// increments WITHOUT touching shard 1's lease (still held), and the
// strata_worker_panic_total{worker="gc",shard="0"} counter ticks.
func TestFanOutPanicIsolation(t *testing.T) {
	locker := memory.NewLocker()

	var shardZeroPanics atomic.Int64
	beforeShardZero := readPanicCounter(t, "gc", "0")
	beforeShardOne := readPanicCounter(t, "gc", "1")

	fan := &FanOut{
		Locker:       locker,
		ShardCount:   2,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Backoff:      []time.Duration{0, 0, 0, 0},
		Sleep:        func(context.Context, time.Duration) {},
		LeaderTTL:    50 * time.Millisecond,
		LeaderRenew:  15 * time.Millisecond,
		AcquireRetry: 5 * time.Millisecond,
		StableAfter:  time.Hour,
		Build: func(shardID int) *Worker {
			if shardID == 0 {
				return &Worker{
					Meta:     &panicOnListMeta{Store: memory.New(), counter: &shardZeroPanics},
					Region:   "default",
					Interval: 1 * time.Millisecond,
					Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
				}
			}
			return &Worker{
				Meta:     memory.New(),
				Region:   "default",
				Interval: 50 * time.Millisecond,
				Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = fan.Run(ctx)
	}()

	// Wait until shard 0's panic counter fires at least 3× — proving the
	// per-shard restart loop is alive.
	deadline := time.Now().Add(3 * time.Second)
	for shardZeroPanics.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("shard 0 only panicked %d times in 3s; expected >= 3", shardZeroPanics.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Shard 1 must be held throughout; HeldShards() reflects that.
	heldShards := fan.HeldShards()
	if !slices.Contains(heldShards, 1) {
		t.Fatalf("shard 1 lease lost while shard 0 was panicking; HeldShards=%v", heldShards)
	}

	cancel()
	<-done

	gotShardZero := readPanicCounter(t, "gc", "0") - beforeShardZero
	if gotShardZero < 3 {
		t.Fatalf("strata_worker_panic_total{shard=0} delta = %v, want >= 3", gotShardZero)
	}
	gotShardOne := readPanicCounter(t, "gc", "1") - beforeShardOne
	if gotShardOne != 0 {
		t.Fatalf("strata_worker_panic_total{shard=1} delta = %v, want 0 — sibling shard must not panic", gotShardOne)
	}
}

// TestFanOutOnLeaderTransitions verifies OnLeader fires exactly twice per
// run: once when the first shard is acquired (true) and once when the
// last is released (false). Multi-shard acquire/release inside a single
// replica is folded into one acquired/released pair.
func TestFanOutOnLeaderTransitions(t *testing.T) {
	locker := memory.NewLocker()

	var transitions []bool
	var mu sync.Mutex
	fan := &FanOut{
		Locker:       locker,
		ShardCount:   3,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		LeaderTTL:    50 * time.Millisecond,
		LeaderRenew:  15 * time.Millisecond,
		AcquireRetry: 5 * time.Millisecond,
		OnLeader: func(acquired bool) {
			mu.Lock()
			transitions = append(transitions, acquired)
			mu.Unlock()
		},
		Build: func(shardID int) *Worker {
			return &Worker{
				Meta:     memory.New(),
				Region:   "default",
				Interval: 50 * time.Millisecond,
				Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = fan.Run(ctx)
	}()

	// Wait for first acquire transition.
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
}

// TestFanOutLeaseNamePin verifies the lease key shape is the documented
// `gc-leader-<shardID>` — operator dashboards / runbooks rely on this
// naming.
func TestFanOutLeaseNamePin(t *testing.T) {
	if got := FanOutLeaseName(0); got != "gc-leader-0" {
		t.Fatalf("FanOutLeaseName(0)=%q want gc-leader-0", got)
	}
	if got := FanOutLeaseName(7); got != "gc-leader-7" {
		t.Fatalf("FanOutLeaseName(7)=%q want gc-leader-7", got)
	}
}

// TestFanOutHeldShardsSorted: HeldShards always returns ascending IDs so
// callers can use HeldShards()[0] as a stable myReplicaID.
func TestFanOutHeldShardsSorted(t *testing.T) {
	f := &FanOut{}
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

// TestFanOutRequiresLockerAndBuild covers the construction guard rails:
// missing Locker or Build returns a startup error rather than panicking.
func TestFanOutRequiresLockerAndBuild(t *testing.T) {
	cases := map[string]*FanOut{
		"missing locker": {Build: func(int) *Worker { return &Worker{} }},
		"missing build":  {Locker: memory.NewLocker()},
	}
	for name, fan := range cases {
		t.Run(name, func(t *testing.T) {
			fan.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			err := fan.Run(context.Background())
			if err == nil {
				t.Fatal("expected startup error, got nil")
			}
		})
	}
}

func readPanicCounter(t *testing.T, worker, shard string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := metrics.WorkerPanicTotal.WithLabelValues(worker, shard).Write(m); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// panicOnListMeta is a meta.Store stub whose ListGCEntriesShard panics on
// every call, used to drive the per-shard panic isolation test. All other
// methods of meta.Store delegate to a fresh in-memory Store so the type
// satisfies the interface; only the listing path is rigged to misbehave.
type panicOnListMeta struct {
	*memory.Store
	counter *atomic.Int64
}

func (p *panicOnListMeta) ListGCEntriesShard(_ context.Context, _ string, _, _ int, _ time.Time, _ int) ([]meta.GCEntry, error) {
	if p.counter != nil {
		p.counter.Add(1)
	}
	panic("rigged panic for shard isolation test")
}
