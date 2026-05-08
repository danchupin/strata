package workers

import (
	"os"
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
	replicaCount := clampShards(intFromEnv("STRATA_GC_SHARDS", 1))
	return &lifecycle.Worker{
		Meta:        deps.Meta,
		Data:        deps.Data,
		Region:      deps.Region,
		Interval:    durationFromEnv("STRATA_LIFECYCLE_INTERVAL", 60*time.Second),
		AgeUnit:     ageUnitFromEnv("STRATA_LIFECYCLE_UNIT", 24*time.Hour),
		Concurrency: clampConcurrency(intFromEnv("STRATA_LIFECYCLE_CONCURRENCY", 1)),
		Logger:      deps.Logger,
		Locker:      deps.Locker,
		ReplicaInfo: lifecycleReplicaInfo(replicaCount),
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

// ageUnitFromEnv mirrors the legacy cmd/strata-lifecycle string -> duration
// switch so existing STRATA_LIFECYCLE_UNIT values (second/minute/hour/day)
// keep working byte-for-byte.
func ageUnitFromEnv(key string, fallback time.Duration) time.Duration {
	switch os.Getenv(key) {
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
