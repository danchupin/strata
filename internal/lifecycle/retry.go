package lifecycle

import (
	"context"
	"errors"
	"syscall"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
)

// retryBackoffs is the lifecycle worker's transient-error retry budget.
// 3 attempts total; sleeps fire BEFORE the 2nd and 3rd attempt. A 200ms
// network blip should not delay the next 999 expirations by a full
// Interval tick.
var retryBackoffs = []time.Duration{
	1 * time.Second,
	3 * time.Second,
	10 * time.Second,
}

// isTransient reports whether err is the kind of failure (ctx-deadline,
// network reset/timeout, anything implementing Temporary() bool) that
// justifies retrying. Terminal errors — object/bucket already gone,
// chunk already swept by a sibling leader — short-circuit the retry
// loop so the worker moves on to the next action immediately.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, meta.ErrObjectNotFound),
		errors.Is(err, meta.ErrBucketNotFound),
		errors.Is(err, data.ErrChunkNotFound),
		errors.Is(err, data.ErrNotFound):
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var temp interface{ Temporary() bool }
	if errors.As(err, &temp) && temp.Temporary() {
		return true
	}
	return false
}

// retryAction runs fn with a 3-attempt bounded retry. Transient errors
// sleep then retry; terminal errors short-circuit. Counter
// strata_lifecycle_retry_total{outcome} fires once per call:
//
//   - ok       — succeeded on attempt > 1 (no bump if first attempt OK)
//   - terminal — first non-retryable error encountered (any attempt)
//   - exhausted — all 3 attempts hit transient errors
//
// A cancelled ctx mid-backoff returns ctx.Err() immediately without a
// counter bump — shutdown is not an action outcome.
func retryAction(ctx context.Context, fn func() error) error {
	return retryActionWith(ctx, fn, ctxSleep)
}

// retryActionWith is the test seam — production callers use retryAction
// which threads a real time-based sleeper.
func retryActionWith(ctx context.Context, fn func() error, sleep func(context.Context, time.Duration) error) error {
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			if err := sleep(ctx, retryBackoffs[attempt-1]); err != nil {
				return err
			}
		}
		err := fn()
		if err == nil {
			if attempt > 0 {
				metrics.LifecycleRetryTotal.WithLabelValues("ok").Inc()
			}
			return nil
		}
		if !isTransient(err) {
			metrics.LifecycleRetryTotal.WithLabelValues("terminal").Inc()
			return err
		}
		lastErr = err
	}
	metrics.LifecycleRetryTotal.WithLabelValues("exhausted").Inc()
	return lastErr
}

// ctxSleep races time.After(d) against ctx.Done() so an in-flight retry
// exits immediately on worker shutdown.
func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
