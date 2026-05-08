package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/metrics"
)

// DefaultFanOutBackoff is the per-shard restart schedule used by FanOut on
// panic recovery. Mirrors the supervisor's DefaultBackoff.
var DefaultFanOutBackoff = []time.Duration{
	1 * time.Second,
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
}

// DefaultFanOutStableAfter is the run duration required for a shard goroutine
// to be considered "healthy" — the per-shard attempt counter resets after
// this much uptime.
const DefaultFanOutStableAfter = 5 * time.Minute

// FanOut is a Runner that distributes the GC queue across `ShardCount`
// goroutines, each acquiring its own leader.Session keyed on
// `gc-leader-<shardID>`. A replica may hold zero, one, or multiple shards
// depending on contention with sibling replicas.
//
// FanOut owns per-shard panic recovery + restart-with-backoff so a panic in
// one shard's drain loop only releases that shard's lease — sibling shards
// keep draining. Per-shard panics increment
// `strata_worker_panic_total{worker="gc",shard="<i>"}`.
//
// HeldShards() exposes the currently-leased shard IDs to lifecycle (US-005)
// so the gateway can compute its replica ID for per-bucket lease distribution.
type FanOut struct {
	Locker     leader.Locker
	ShardCount int
	// Build constructs the inner per-shard Worker for the given shardID. The
	// caller is responsible for stamping `ShardID` and `ShardCount` on the
	// returned Worker so its drainCount targets the correct slice.
	Build func(shardID int) *Worker

	// OnLeader is invoked on the 0→1 and N→0 transitions of the held-shard
	// counter so the gateway can flip the heartbeat `leader_for=gc` chip
	// once per replica regardless of how many shards a replica owns.
	OnLeader func(acquired bool)

	Logger *slog.Logger

	// Backoff overrides DefaultFanOutBackoff. Empty falls back to default.
	Backoff []time.Duration
	// Sleep replaces the ctx-aware delay between restart attempts; tests
	// inject an instant no-op to flatten backoff.
	Sleep func(ctx context.Context, d time.Duration)
	// StableAfter overrides DefaultFanOutStableAfter so tests can reset the
	// per-shard attempt counter quickly.
	StableAfter time.Duration

	// LeaderTTL / LeaderRenew / AcquireRetry mirror the supervisor's per-
	// session timings; zero values fall back to leader.Session defaults.
	LeaderTTL    time.Duration
	LeaderRenew  time.Duration
	AcquireRetry time.Duration

	mu      sync.RWMutex
	holders map[int]bool
}

// Run spawns ShardCount goroutines and blocks until ctx is cancelled. Each
// goroutine runs the leader-elect → build → run loop for its own shard.
// Returns ctx.Err() once every shard goroutine has shut down.
func (f *FanOut) Run(ctx context.Context) error {
	if f.Logger == nil {
		f.Logger = slog.Default()
	}
	if f.Locker == nil {
		return errors.New("gc.FanOut: Locker required")
	}
	if f.Build == nil {
		return errors.New("gc.FanOut: Build required")
	}
	count := min(max(f.ShardCount, 1), 1024)

	backoff := f.Backoff
	if len(backoff) == 0 {
		backoff = DefaultFanOutBackoff
	}
	sleep := f.Sleep
	if sleep == nil {
		sleep = func(c context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-c.Done():
			case <-t.C:
			}
		}
	}
	stable := f.StableAfter
	if stable == 0 {
		stable = DefaultFanOutStableAfter
	}

	var wg sync.WaitGroup
	for i := range count {
		shardID := i
		wg.Go(func() {
			f.superviseShard(ctx, shardID, count, backoff, sleep, stable)
		})
	}
	wg.Wait()
	return ctx.Err()
}

// HeldShards returns the currently-held shard IDs sorted ascending. Used by
// lifecycle (US-005) to derive `myReplicaID = min(HeldShards())`. Returns an
// empty slice while the replica holds no shards.
func (f *FanOut) HeldShards() []int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]int, 0, len(f.holders))
	for id, held := range f.holders {
		if held {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

func (f *FanOut) superviseShard(
	parent context.Context,
	shardID, shardCount int,
	backoff []time.Duration,
	sleep func(context.Context, time.Duration),
	stable time.Duration,
) {
	logger := f.Logger.With("worker", "gc", "shard", shardID)
	attempt := 0
	for parent.Err() == nil {
		startedAt := time.Now()
		failed := f.runShardOnce(parent, shardID, shardCount, logger)
		if parent.Err() != nil {
			return
		}
		if time.Since(startedAt) >= stable {
			attempt = 0
		}
		if !failed {
			continue
		}
		idx := attempt
		if idx >= len(backoff) {
			idx = len(backoff) - 1
		}
		delay := backoff[idx]
		logger.WarnContext(parent, "gc shard restart scheduled",
			"delay", delay.String(), "attempt", attempt)
		attempt++
		sleep(parent, delay)
	}
}

func (f *FanOut) runShardOnce(parent context.Context, shardID, shardCount int, logger *slog.Logger) (failed bool) {
	defer func() {
		if r := recover(); r != nil {
			metrics.WorkerPanicTotal.WithLabelValues("gc", strconv.Itoa(shardID)).Inc()
			logger.ErrorContext(parent, "gc shard panic recovered",
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
			failed = true
		}
	}()

	holder := leader.DefaultHolder()
	session := &leader.Session{
		Locker:       f.Locker,
		Name:         leaseName(shardID),
		Holder:       holder,
		TTL:          f.LeaderTTL,
		RenewPeriod:  f.LeaderRenew,
		AcquireRetry: f.AcquireRetry,
		Logger:       logger,
	}
	if err := session.AwaitAcquire(parent); err != nil {
		return false
	}
	f.markHeld(shardID, true)
	defer f.markHeld(shardID, false)
	workCtx := session.Supervise(parent)
	defer session.Release(context.Background())

	w := f.Build(shardID)
	if w == nil {
		logger.ErrorContext(parent, "gc Build returned nil worker")
		return true
	}
	w.ShardID = shardID
	w.ShardCount = shardCount
	if w.Logger == nil {
		w.Logger = logger
	}
	if err := w.Run(workCtx); err != nil && !errors.Is(err, context.Canceled) {
		if parent.Err() != nil {
			return false
		}
		logger.WarnContext(parent, "gc shard run returned error", "error", err.Error())
		return true
	}
	return false
}

// leaseName returns the per-shard lease key. Public via FanOutLeaseName for
// tests + the supervisor's lifecycle distribution gate (US-005).
func leaseName(shardID int) string {
	return fmt.Sprintf("gc-leader-%d", shardID)
}

// FanOutLeaseName returns the lease name for shardID under the Phase 2
// per-shard layout (`gc-leader-<shardID>`). Exported so callers (lifecycle,
// tests) don't have to hard-code the format string.
func FanOutLeaseName(shardID int) string { return leaseName(shardID) }

func (f *FanOut) markHeld(shardID int, held bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holders == nil {
		f.holders = map[int]bool{}
	}
	prev := 0
	for _, ok := range f.holders {
		if ok {
			prev++
		}
	}
	if held {
		f.holders[shardID] = true
	} else {
		delete(f.holders, shardID)
	}
	cur := 0
	for _, ok := range f.holders {
		if ok {
			cur++
		}
	}
	if f.OnLeader != nil {
		if prev == 0 && cur > 0 {
			f.OnLeader(true)
		}
		if prev > 0 && cur == 0 {
			f.OnLeader(false)
		}
	}
}
