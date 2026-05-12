package rebalance

import (
	"context"

	"golang.org/x/time/rate"
)

// Throttle is a thin wrapper over golang.org/x/time/rate.Limiter that
// gates byte-rate across the rebalance mover. One Throttle is shared
// across the RADOS mover (US-004) and the S3 mover (US-005) so the
// operator-supplied STRATA_REBALANCE_RATE_MB_S is honoured globally.
//
// Both read and write consume from the same bucket: each chunk move
// costs roughly `chunkSize × 2` tokens so the effective on-the-wire
// throughput approximates the operator's setting on the busier leg.
//
// A nil Throttle is a valid value — Wait is a no-op so unit tests don't
// need to wire one. The cmd binary always provides one at production
// rate.
type Throttle struct {
	lim *rate.Limiter
}

// NewThrottle returns a Throttle that enforces a steady ratePerSec
// (bytes/sec). Burst is set to one chunk size to absorb the read+write
// bursts of a single move without delaying the first chunk in the
// stream. A zero ratePerSec disables throttling.
func NewThrottle(ratePerSec int64, burst int64) *Throttle {
	if ratePerSec <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = ratePerSec
	}
	return &Throttle{lim: rate.NewLimiter(rate.Limit(ratePerSec), int(burst))}
}

// Wait blocks until n tokens are available or ctx is cancelled. Tokens
// requested > burst are clamped to burst so a 4 MiB chunk move against
// a small-burst limiter does not deadlock.
func (t *Throttle) Wait(ctx context.Context, n int64) error {
	if t == nil || t.lim == nil || n <= 0 {
		return nil
	}
	burst := int64(t.lim.Burst())
	for n > 0 {
		step := min(n, burst)
		if err := t.lim.WaitN(ctx, int(step)); err != nil {
			return err
		}
		n -= step
	}
	return nil
}
