package workers

import (
	"time"

	"github.com/danchupin/strata/internal/lifecycle"
)

func init() {
	Register(Worker{
		Name:      "lifecycle",
		Build:     buildLifecycle,
		SkipLease: true,
	})
}

func buildLifecycle(deps Dependencies) (Runner, error) {
	cfg := workerCfg(deps)
	lcCfg := cfg.Workers.Lifecycle
	replicaCount := clampShards(orInt(cfg.Workers.GC.Shards, 1))
	return &lifecycle.Worker{
		Meta:        deps.Meta,
		Data:        deps.Data,
		Region:      deps.Region,
		Interval:    orDuration(lcCfg.Interval, 60*time.Second),
		AgeUnit:     ageUnitOrDefault(lcCfg.Unit, 24*time.Hour),
		Concurrency: clampConcurrency(orInt(lcCfg.Concurrency, 1)),
		Logger:      deps.Logger,
		Locker:      deps.Locker,
		ReplicaInfo: lifecycleReplicaInfo(replicaCount),
		Tracer:      deps.Tracer.Tracer("strata.worker.lifecycle"),
	}, nil
}

// lifecycleReplicaInfo returns the (count, id) closure the lifecycle worker
// invokes per cycle to evaluate its distribution gate. When the gc worker is
// running in the same supervisor (`workers.GCFanOut != nil`), the closure
// returns (replicaCount, min(HeldShards())) — or (replicaCount, -1) if the
// replica is between leases and holds no shard. When gc is not registered
// (lifecycle-only deploy), the closure flattens to (1, 0): every bucket
// passes the gate and the per-bucket lease is the sole serializer.
func lifecycleReplicaInfo(replicaCount int) func() (int, int) {
	return func() (int, int) {
		if GCFanOut == nil {
			return 1, 0
		}
		held := GCFanOut.HeldShards()
		if len(held) == 0 {
			return replicaCount, -1
		}
		return replicaCount, held[0]
	}
}

// ageUnitOrDefault maps the cfg-resolved unit string ("second" | "minute" |
// "hour" | "day") to a time.Duration; unrecognised / empty values fall back
// to the supplied default.
func ageUnitOrDefault(unit string, fallback time.Duration) time.Duration {
	switch unit {
	case "second":
		return time.Second
	case "minute":
		return time.Minute
	case "hour":
		return time.Hour
	case "day":
		return 24 * time.Hour
	}
	return fallback
}
