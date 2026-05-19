package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
)

// transientErr satisfies the Temporary() bool interface — emulates a
// net.OpError or similar wrapped transient.
type transientErr struct{ msg string }

func (e *transientErr) Error() string   { return e.msg }
func (e *transientErr) Temporary() bool { return true }

func TestIsTransientClassifier(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ctx deadline exceeded", context.DeadlineExceeded, true},
		{"syscall ECONNRESET", syscall.ECONNRESET, true},
		{"syscall ETIMEDOUT", syscall.ETIMEDOUT, true},
		{"wrapped transient", fmt.Errorf("wrap: %w", &transientErr{msg: "net blip"}), true},
		{"net.OpError style", &net.OpError{Op: "dial", Err: &transientErr{msg: "x"}}, true},
		{"meta object not found", meta.ErrObjectNotFound, false},
		{"meta bucket not found", meta.ErrBucketNotFound, false},
		{"data chunk not found", data.ErrChunkNotFound, false},
		{"data not found", data.ErrNotFound, false},
		{"generic error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransient(tc.err); got != tc.want {
				t.Fatalf("isTransient(%v) = %v want %v", tc.err, got, tc.want)
			}
		})
	}
}

// recordingSleep counts sleeps + records durations so tests can assert
// (a) the right number of sleeps fired, (b) the documented 1s/3s backoff
// ladder is consumed in order.
type recordingSleep struct {
	durs   []time.Duration
	cancel context.CancelFunc // when non-nil, fired on Nth sleep to test ctx-cancel mid-backoff
	cancelOn int
}

func (s *recordingSleep) sleep(ctx context.Context, d time.Duration) error {
	s.durs = append(s.durs, d)
	if s.cancel != nil && len(s.durs) == s.cancelOn {
		s.cancel()
		return ctx.Err()
	}
	return nil
}

func counterValue(t *testing.T, outcome string) float64 {
	t.Helper()
	return testutil.ToFloat64(metrics.LifecycleRetryTotal.WithLabelValues(outcome))
}

func TestRetryActionSuccessFirstAttempt(t *testing.T) {
	before := struct{ ok, terminal, exhausted float64 }{
		counterValue(t, "ok"), counterValue(t, "terminal"), counterValue(t, "exhausted"),
	}
	sleeper := &recordingSleep{}
	calls := 0
	err := retryActionWith(context.Background(), func() error {
		calls++
		return nil
	}, sleeper.sleep)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d want 1", calls)
	}
	if len(sleeper.durs) != 0 {
		t.Fatalf("sleeps = %v want none", sleeper.durs)
	}
	// no counter bumps on first-attempt success
	if v := counterValue(t, "ok"); v != before.ok {
		t.Fatalf("ok counter drifted: was %v now %v", before.ok, v)
	}
	if v := counterValue(t, "terminal"); v != before.terminal {
		t.Fatalf("terminal counter drifted: was %v now %v", before.terminal, v)
	}
	if v := counterValue(t, "exhausted"); v != before.exhausted {
		t.Fatalf("exhausted counter drifted: was %v now %v", before.exhausted, v)
	}
}

func TestRetryActionTransientThenSuccess(t *testing.T) {
	beforeOk := counterValue(t, "ok")
	sleeper := &recordingSleep{}
	calls := 0
	err := retryActionWith(context.Background(), func() error {
		calls++
		if calls <= 2 {
			return context.DeadlineExceeded
		}
		return nil
	}, sleeper.sleep)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d want 3", calls)
	}
	wantDurs := []time.Duration{1 * time.Second, 3 * time.Second}
	if len(sleeper.durs) != len(wantDurs) {
		t.Fatalf("sleeps = %v want %v", sleeper.durs, wantDurs)
	}
	for i, d := range wantDurs {
		if sleeper.durs[i] != d {
			t.Fatalf("sleep[%d] = %v want %v", i, sleeper.durs[i], d)
		}
	}
	if got := counterValue(t, "ok"); got != beforeOk+1 {
		t.Fatalf("ok counter = %v want %v", got, beforeOk+1)
	}
}

func TestRetryActionExhausted(t *testing.T) {
	beforeExhausted := counterValue(t, "exhausted")
	sleeper := &recordingSleep{}
	sentinel := &transientErr{msg: "always transient"}
	calls := 0
	err := retryActionWith(context.Background(), func() error {
		calls++
		return sentinel
	}, sleeper.sleep)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v want sentinel %v", err, sentinel)
	}
	if calls != 3 {
		t.Fatalf("calls = %d want 3", calls)
	}
	if got := counterValue(t, "exhausted"); got != beforeExhausted+1 {
		t.Fatalf("exhausted counter = %v want %v", got, beforeExhausted+1)
	}
}

func TestRetryActionTerminalFirstAttempt(t *testing.T) {
	beforeTerminal := counterValue(t, "terminal")
	sleeper := &recordingSleep{}
	calls := 0
	err := retryActionWith(context.Background(), func() error {
		calls++
		return meta.ErrObjectNotFound
	}, sleeper.sleep)
	if !errors.Is(err, meta.ErrObjectNotFound) {
		t.Fatalf("err = %v want ErrObjectNotFound", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d want 1 (terminal short-circuits)", calls)
	}
	if len(sleeper.durs) != 0 {
		t.Fatalf("sleeps = %v want none on terminal", sleeper.durs)
	}
	if got := counterValue(t, "terminal"); got != beforeTerminal+1 {
		t.Fatalf("terminal counter = %v want %v", got, beforeTerminal+1)
	}
}

func TestRetryActionCtxCancelMidBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sleeper := &recordingSleep{cancel: cancel, cancelOn: 1}
	calls := 0
	err := retryActionWith(ctx, func() error {
		calls++
		return context.DeadlineExceeded
	}, sleeper.sleep)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d want 1 (cancelled before 2nd attempt)", calls)
	}
}

// TestCtxSleepCancelImmediate exercises the production sleeper helper
// (not the test seam): if ctx is already done, ctxSleep returns ctx.Err()
// immediately without blocking the full backoff duration.
func TestCtxSleepCancelImmediate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := ctxSleep(ctx, 1*time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("ctxSleep blocked %v despite cancelled ctx", elapsed)
	}
}
