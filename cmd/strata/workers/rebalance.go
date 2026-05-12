package workers

import (
	"time"

	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/rebalance"
)

func init() {
	Register(Worker{
		Name:  "rebalance",
		Build: buildRebalance,
	})
}

// buildRebalance reads STRATA_REBALANCE_INTERVAL / _RATE_MB_S / _INFLIGHT
// at constructor time, clamps out-of-range values with a WARN, and wires
// the prometheus observer. Single leader cluster-wide via the outer
// `rebalance-leader` lease (SkipLease=false). The PlanEmitter is a
// MoverChain seeded with whichever movers the build tag enables (RADOS
// under `ceph`, S3 under US-005); chains with no movers fall back to
// the plan-logging only behaviour shipped in US-003.
func buildRebalance(deps Dependencies) (Runner, error) {
	interval := clampDuration(deps,
		"STRATA_REBALANCE_INTERVAL", time.Hour,
		1*time.Minute, 24*time.Hour,
	)
	rateMBPerSec := clampInt(deps, "STRATA_REBALANCE_RATE_MB_S", 100, 1, 10000)
	inflight := clampInt(deps, "STRATA_REBALANCE_INFLIGHT", 4, 1, 64)
	throttle := rebalance.NewThrottle(int64(rateMBPerSec)*1024*1024, int64(rateMBPerSec)*1024*1024)
	chain := &rebalance.MoverChain{
		Movers: rebalanceMovers(deps, throttle, inflight),
		Logger: deps.Logger,
	}
	return rebalance.New(rebalance.Config{
		Meta:         deps.Meta,
		Data:         deps.Data,
		Logger:       deps.Logger,
		Metrics:      metrics.RebalanceObserver{},
		Emitter:      chain,
		Interval:     interval,
		RateMBPerSec: rateMBPerSec,
		Inflight:     inflight,
		Tracer:       deps.Tracer.Tracer("strata.worker.rebalance"),
	})
}

func clampDuration(deps Dependencies, key string, fallback, lo, hi time.Duration) time.Duration {
	v := durationFromEnv(key, fallback)
	if v < lo {
		deps.Logger.Warn("rebalance: clamping env",
			"env", key, "value", v.String(), "min", lo.String())
		return lo
	}
	if v > hi {
		deps.Logger.Warn("rebalance: clamping env",
			"env", key, "value", v.String(), "max", hi.String())
		return hi
	}
	return v
}

func clampInt(deps Dependencies, key string, fallback, lo, hi int) int {
	v := intFromEnv(key, fallback)
	if v < lo {
		deps.Logger.Warn("rebalance: clamping env",
			"env", key, "value", v, "min", lo)
		return lo
	}
	if v > hi {
		deps.Logger.Warn("rebalance: clamping env",
			"env", key, "value", v, "max", hi)
		return hi
	}
	return v
}
