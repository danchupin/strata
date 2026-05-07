package workers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/metrics"
)

// DefaultBackoff is the per-worker restart schedule. Indexed by attempt
// count (capped at the last entry). Resets to the first entry once the
// worker has stayed up for stableAfter without crashing.
var DefaultBackoff = []time.Duration{
	1 * time.Second,
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
}

// stableAfter is the run duration required for the supervisor to consider a
// worker "healthy" and reset its backoff counter to zero.
const stableAfter = 5 * time.Minute

const (
	defaultLeaderTTL    = 30 * time.Second
	defaultLeaderRenew  = 10 * time.Second
	defaultAcquireRetry = 5 * time.Second
)

// Sleeper is a context-aware delay used between restart attempts. The
// production implementation wraps time.NewTimer; tests inject a no-op so
// backoff flattens to zero.
type Sleeper func(ctx context.Context, d time.Duration)

func defaultSleeper(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Supervisor runs a fixed set of registered workers, one goroutine each,
// until ctx is cancelled. Lease loss or panic in one worker only affects
// that worker — siblings and the gateway carry on.
type Supervisor struct {
	Deps Dependencies

	// Backoff overrides DefaultBackoff. Empty slice falls back to default.
	Backoff []time.Duration
	// Sleep replaces the default ctx-aware timer between restart attempts.
	// Inject a no-op in tests to flatten backoff.
	Sleep Sleeper
	// StableAfter overrides stableAfter so tests can reset the backoff
	// counter quickly.
	StableAfter time.Duration

	// LeaderTTL / LeaderRenew / AcquireRetry override the per-worker
	// leader.Session timings. Zero values fall back to defaults.
	LeaderTTL    time.Duration
	LeaderRenew  time.Duration
	AcquireRetry time.Duration

	eventsOnce   sync.Once
	leaderEvents chan LeaderEvent
}

// LeaderEvent is emitted on every per-worker lease state transition:
// Acquired=true once runOnce has won the lease, Acquired=false once the
// session has been released (including lease-loss exits and panics).
type LeaderEvent struct {
	Worker   string
	Acquired bool
}

// LeaderEvents returns a buffered channel (cap 8) that receives a
// LeaderEvent on every lease acquire / release. The channel is allocated
// on first call and closed by Run() once every per-worker goroutine has
// shut down. Sends are non-blocking — a stalled consumer drops events
// rather than stalling supervision.
func (s *Supervisor) LeaderEvents() <-chan LeaderEvent {
	s.eventsOnce.Do(func() { s.leaderEvents = make(chan LeaderEvent, 8) })
	return s.leaderEvents
}

func (s *Supervisor) emitLeader(name string, acquired bool) {
	if s.leaderEvents == nil {
		return
	}
	select {
	case s.leaderEvents <- LeaderEvent{Worker: name, Acquired: acquired}:
	default:
	}
}

// Run starts each worker in its own goroutine and blocks until ctx is
// cancelled. Returns ctx.Err() once every per-worker goroutine has shut
// down. Each goroutine acquires a leader.Session keyed on
// "<worker-name>-leader" before constructing its Runner.
func (s *Supervisor) Run(ctx context.Context, workers []Worker) error {
	if s.Deps.Logger == nil {
		return errors.New("workers: Dependencies.Logger required")
	}
	if s.Deps.Locker == nil {
		return errors.New("workers: Dependencies.Locker required")
	}
	backoff := s.Backoff
	if len(backoff) == 0 {
		backoff = DefaultBackoff
	}
	sleep := s.Sleep
	if sleep == nil {
		sleep = defaultSleeper
	}
	stable := s.StableAfter
	if stable == 0 {
		stable = stableAfter
	}

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Go(func() {
			s.superviseOne(ctx, w, backoff, sleep, stable)
		})
	}
	<-ctx.Done()
	wg.Wait()
	if s.leaderEvents != nil {
		close(s.leaderEvents)
	}
	return ctx.Err()
}

// superviseOne is the per-worker loop. While parent is live: acquire lease,
// build + run, release. On failure back off; on lease loss re-acquire
// immediately.
func (s *Supervisor) superviseOne(parent context.Context, w Worker, backoff []time.Duration, sleep Sleeper, stable time.Duration) {
	logger := s.Deps.Logger.With("worker", w.Name)
	attempt := 0
	for parent.Err() == nil {
		startedAt := time.Now()
		failed := s.runOnce(parent, w, logger)
		if parent.Err() != nil {
			return
		}
		if time.Since(startedAt) >= stable {
			attempt = 0
		}
		if !failed {
			continue
		}
		delay := backoff[min(attempt, len(backoff)-1)]
		logger.WarnContext(parent, "worker restart scheduled", "delay", delay.String(), "attempt", attempt)
		attempt++
		sleep(parent, delay)
	}
}

// runOnce acquires the leader lease, builds the runner, runs it under a
// supervised ctx, releases the lease, and recovers from panics. Returns
// true on failure (Build error, Run error, or panic) so the caller backs
// off; returns false on clean exit (ctx cancellation or lease loss) so the
// caller re-acquires immediately.
func (s *Supervisor) runOnce(parent context.Context, w Worker, logger *slog.Logger) (failed bool) {
	defer func() {
		if r := recover(); r != nil {
			metrics.WorkerPanicTotal.WithLabelValues(w.Name).Inc()
			logger.ErrorContext(parent, "worker panic recovered",
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()))
			failed = true
		}
	}()

	ttl := s.LeaderTTL
	if ttl == 0 {
		ttl = defaultLeaderTTL
	}
	renew := s.LeaderRenew
	if renew == 0 {
		renew = defaultLeaderRenew
	}
	retry := s.AcquireRetry
	if retry == 0 {
		retry = defaultAcquireRetry
	}
	session := &leader.Session{
		Locker:       s.Deps.Locker,
		Name:         w.Name + "-leader",
		Holder:       leader.DefaultHolder(),
		TTL:          ttl,
		RenewPeriod:  renew,
		AcquireRetry: retry,
		Logger:       logger,
	}
	if err := session.AwaitAcquire(parent); err != nil {
		return false
	}
	s.emitLeader(w.Name, true)
	defer s.emitLeader(w.Name, false)
	workCtx := session.Supervise(parent)
	defer session.Release(context.Background())

	runner, err := w.Build(s.Deps)
	if err != nil {
		logger.ErrorContext(parent, "worker build failed", "error", err.Error())
		return true
	}
	if err := runner.Run(workCtx); err != nil && !errors.Is(err, context.Canceled) {
		if parent.Err() != nil {
			return false
		}
		logger.WarnContext(parent, "worker run returned error", "error", err.Error())
		return true
	}
	return false
}
