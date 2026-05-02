package workers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/metrics"
)

// silentLogger discards output so test runs stay quiet.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// flatBackoff turns the supervisor's backoff schedule into a no-op so tests
// drive panic-restart cycles deterministically without sleeping.
var flatBackoff = []time.Duration{0, 0, 0, 0}

func instantSleep(ctx context.Context, d time.Duration) {}

// fastTimings shortens the leader.Session schedule so tests acquire/renew
// in milliseconds rather than seconds.
func fastTimings(s *Supervisor) {
	s.LeaderTTL = 50 * time.Millisecond
	s.LeaderRenew = 15 * time.Millisecond
	s.AcquireRetry = 5 * time.Millisecond
}

func newSupervisor(t *testing.T, deps Dependencies) *Supervisor {
	t.Helper()
	s := &Supervisor{
		Deps:        deps,
		Backoff:     flatBackoff,
		Sleep:       instantSleep,
		StableAfter: time.Hour, // never reset attempt counter inside a test
	}
	fastTimings(s)
	return s
}

func panicCounterValue(t *testing.T, name string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := metrics.WorkerPanicTotal.WithLabelValues(name).Write(m); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// TestSupervisor_PanicRestartCycle: a worker that panics on every run still
// restarts (counter increments per panic) without taking down the
// supervisor. The supervisor exits cleanly on ctx cancel.
func TestSupervisor_PanicRestartCycle(t *testing.T) {
	t.Cleanup(restoreInitial)
	Reset()

	const name = "panic-test"
	before := panicCounterValue(t, name)

	var attempts atomic.Int32
	Register(Worker{
		Name: name,
		Build: func(Dependencies) (Runner, error) {
			return RunnerFunc(func(ctx context.Context) error {
				attempts.Add(1)
				panic("boom")
			}), nil
		},
	})

	deps := Dependencies{Logger: silentLogger(), Locker: memory.NewLocker()}
	sup := newSupervisor(t, deps)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- sup.Run(ctx, []Worker{mustLookup(t, name)})
	}()

	// Wait until the worker has panicked at least 3 times.
	deadline := time.Now().Add(2 * time.Second)
	for attempts.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("worker panicked only %d times in 2s; expected >= 3", attempts.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Supervisor.Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Supervisor.Run did not return after ctx cancel")
	}

	got := panicCounterValue(t, name) - before
	if got < 3 {
		t.Fatalf("strata_worker_panic_total{worker=%q} delta = %v, want >= 3", name, got)
	}
}

// TestSupervisor_LeaseLossIsolation: dropping worker A's lease must NOT
// affect worker B's run. Uses a fakeLocker so we can deterministically
// expire one name's lease without touching the others.
func TestSupervisor_LeaseLossIsolation(t *testing.T) {
	t.Cleanup(restoreInitial)
	Reset()

	var aRuns, bRuns atomic.Int32
	startedB := make(chan struct{})
	var startedBOnce sync.Once

	Register(Worker{
		Name: "a",
		Build: func(Dependencies) (Runner, error) {
			return RunnerFunc(func(ctx context.Context) error {
				aRuns.Add(1)
				<-ctx.Done()
				return ctx.Err()
			}), nil
		},
	})
	Register(Worker{
		Name: "b",
		Build: func(Dependencies) (Runner, error) {
			return RunnerFunc(func(ctx context.Context) error {
				bRuns.Add(1)
				startedBOnce.Do(func() { close(startedB) })
				<-ctx.Done()
				return ctx.Err()
			}), nil
		},
	})

	locker := newFakeLocker()
	deps := Dependencies{Logger: silentLogger(), Locker: locker}
	sup := newSupervisor(t, deps)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- sup.Run(ctx, []Worker{mustLookup(t, "a"), mustLookup(t, "b")})
	}()

	// Wait until both workers have started at least once.
	select {
	case <-startedB:
	case <-time.After(2 * time.Second):
		t.Fatal("worker b never started")
	}

	priorA := aRuns.Load()
	priorB := bRuns.Load()

	// Force the next Renew of a-leader to fail so the supervised ctx for
	// worker "a" cancels, while b-leader keeps renewing successfully.
	locker.expire("a-leader")

	// "a" should re-acquire and re-run.
	deadline := time.Now().Add(2 * time.Second)
	for aRuns.Load() <= priorA {
		if time.Now().After(deadline) {
			t.Fatalf("worker a did not restart after lease drop; aRuns=%d prior=%d", aRuns.Load(), priorA)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// b's run count must not have advanced — its lease was untouched, its
	// Run loop kept ticking the same goroutine, Build never re-invoked.
	if bRuns.Load() != priorB {
		t.Fatalf("worker b restarted unexpectedly: bRuns=%d prior=%d", bRuns.Load(), priorB)
	}

	cancel()
	<-done
}

// TestSupervisor_DependencyInjection: Build receives the Dependencies struct
// the supervisor was constructed with — verify a custom field round-trips.
func TestSupervisor_DependencyInjection(t *testing.T) {
	t.Cleanup(restoreInitial)
	Reset()

	var got Dependencies
	gotCh := make(chan struct{}, 1)
	Register(Worker{
		Name: "inject",
		Build: func(d Dependencies) (Runner, error) {
			got = d
			select {
			case gotCh <- struct{}{}:
			default:
			}
			return RunnerFunc(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }), nil
		},
	})

	wantRegion := "us-east-1"
	deps := Dependencies{Logger: silentLogger(), Locker: memory.NewLocker(), Region: wantRegion}
	sup := newSupervisor(t, deps)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- sup.Run(ctx, []Worker{mustLookup(t, "inject")})
	}()

	select {
	case <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Build never invoked")
	}
	if got.Region != wantRegion {
		t.Fatalf("Region not propagated: got %q want %q", got.Region, wantRegion)
	}
	cancel()
	<-done
}

// TestSupervisor_RejectsMissingDeps: nil Logger or nil Locker is a startup
// error, not a panic.
func TestSupervisor_RejectsMissingDeps(t *testing.T) {
	cases := map[string]Dependencies{
		"missing logger": {Locker: memory.NewLocker()},
		"missing locker": {Logger: silentLogger()},
	}
	for name, deps := range cases {
		t.Run(name, func(t *testing.T) {
			sup := &Supervisor{Deps: deps}
			err := sup.Run(context.Background(), nil)
			if err == nil {
				t.Fatal("expected error from missing dependency")
			}
		})
	}
}

// TestSupervisor_LeaseKeyedOnWorkerName: the lease name must be
// "<worker>-leader" so US-005..US-012 follow the documented contract.
func TestSupervisor_LeaseKeyedOnWorkerName(t *testing.T) {
	t.Cleanup(restoreInitial)
	Reset()

	Register(Worker{
		Name:  "gc",
		Build: func(Dependencies) (Runner, error) {
			return RunnerFunc(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }), nil
		},
	})

	locker := memory.NewLocker()
	deps := Dependencies{Logger: silentLogger(), Locker: locker}
	sup := newSupervisor(t, deps)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx, []Worker{mustLookup(t, "gc")}) }()

	// Spin until the lease is held; an external acquire under the same name
	// should fail because the supervisor owns it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		ok, err := locker.Acquire(context.Background(), "gc-leader", "tester", 100*time.Millisecond)
		if err != nil {
			t.Fatalf("locker.Acquire: %v", err)
		}
		if !ok {
			// Supervisor holds the lease — that's what we want.
			break
		}
		// We accidentally won; release and retry.
		_ = locker.Release(context.Background(), "gc-leader", "tester")
		if time.Now().After(deadline) {
			t.Fatal("supervisor never acquired gc-leader within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func mustLookup(t *testing.T, name string) Worker {
	t.Helper()
	w, ok := Lookup(name)
	if !ok {
		t.Fatalf("worker %q not registered", name)
	}
	return w
}

// fakeLocker is a controllable leader.Locker for tests. expire(name)
// forces the next Renew(name, ...) to return false, simulating a lease
// loss event without affecting other names.
type fakeLocker struct {
	mu      sync.Mutex
	held    map[string]string // name -> holder
	expired map[string]bool   // names whose next Renew should fail
}

func newFakeLocker() *fakeLocker {
	return &fakeLocker{held: map[string]string{}, expired: map[string]bool{}}
}

func (f *fakeLocker) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.held[name]; ok && cur != "" {
		return false, nil
	}
	f.held[name] = holder
	delete(f.expired, name)
	return true, nil
}

func (f *fakeLocker) Renew(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.expired[name] {
		delete(f.held, name)
		delete(f.expired, name)
		return false, nil
	}
	cur, ok := f.held[name]
	if !ok || cur != holder {
		return false, nil
	}
	return true, nil
}

func (f *fakeLocker) Release(ctx context.Context, name, holder string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.held[name]; ok && cur == holder {
		delete(f.held, name)
	}
	return nil
}

// expire flags name so the next Renew returns false; the held holder is
// then dropped so a fresh Acquire can succeed.
func (f *fakeLocker) expire(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired[name] = true
}
