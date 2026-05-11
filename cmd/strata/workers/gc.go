package workers

import (
	"os"
	"strconv"
	"time"

	"github.com/danchupin/strata/internal/gc"
	"github.com/danchupin/strata/internal/metrics"
)

// GCFanOut is the *gc.FanOut instance built by the most recent buildGC call,
// captured so the lifecycle worker (US-005) can read the currently-held
// shard set off the same supervisor process. nil until buildGC has run.
var GCFanOut *gc.FanOut

func init() {
	Register(Worker{
		Name:      "gc",
		Build:     buildGC,
		SkipLease: true,
	})
}

func buildGC(deps Dependencies) (Runner, error) {
	interval := durationFromEnv("STRATA_GC_INTERVAL", 30*time.Second)
	grace := durationFromEnv("STRATA_GC_GRACE", 5*time.Minute)
	batch := intFromEnv("STRATA_GC_BATCH_SIZE", 0)
	concurrency := clampConcurrency(intFromEnv("STRATA_GC_CONCURRENCY", 1))
	shards := clampShards(intFromEnv("STRATA_GC_SHARDS", 1))
	tracer := deps.Tracer.Tracer("strata.worker.gc")

	fan := &gc.FanOut{
		Locker:     deps.Locker,
		ShardCount: shards,
		Logger:     deps.Logger,
		Build: func(shardID int) *gc.Worker {
			return &gc.Worker{
				Meta:        deps.Meta,
				Data:        deps.Data,
				Region:      deps.Region,
				Interval:    interval,
				Grace:       grace,
				Batch:       batch,
				Concurrency: concurrency,
				ShardID:     shardID,
				ShardCount:  shards,
				Logger:      deps.Logger,
				Metrics:     metrics.GCObserver{},
				Tracer:      tracer,
			}
		},
	}
	if deps.EmitLeader != nil {
		fan.OnLeader = func(acquired bool) { deps.EmitLeader("gc", acquired) }
	}
	GCFanOut = fan
	return fan, nil
}

// clampConcurrency clamps to [1, 256]; zero/negative -> 1.
func clampConcurrency(n int) int {
	if n < 1 {
		return 1
	}
	if n > 256 {
		return 256
	}
	return n
}

// clampShards clamps to [1, 1024]; zero/negative -> 1. Mirrors the
// 1024-wide logical-shard ceiling baked into meta.GCShardCount so an
// operator can never request a runtime shard count that the queue layout
// cannot satisfy.
func clampShards(n int) int {
	if n < 1 {
		return 1
	}
	if n > 1024 {
		return 1024
	}
	return n
}

func durationFromEnv(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func intFromEnv(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
