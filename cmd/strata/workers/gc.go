package workers

import (
	"os"
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
	cfg := workerCfg(deps)
	gcCfg := cfg.Workers.GC
	interval := orDuration(gcCfg.Interval, 30*time.Second)
	grace := orDuration(gcCfg.Grace, 5*time.Minute)
	batch := gcCfg.BatchSize
	concurrency := clampConcurrency(orInt(gcCfg.Concurrency, 1))
	shards := clampShards(orInt(gcCfg.Shards, 1))
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

// ResolvedGCConfig is the env-resolved GC worker tunable snapshot surfaced
// on GET /admin/v1/gc-config (US-001 drain-rebalance-transparency). Values
// match what buildGC reads at boot — the read-only endpoint is the env-
// authoritative snapshot, so a gateway restart picks up env changes.
type ResolvedGCConfig struct {
	GraceSeconds    int
	IntervalSeconds int
	BatchSize       int
	Concurrency     int
	Shards          int
}

// ResolveGCConfig re-reads workers.gc.* tunables (env > TOML precedence
// already applied at config.Load() time) with the same defaults +
// clamping as buildGC. Read-only; no side effects. Kept in this file
// (rather than the adminapi layer) so the snapshot stays lock-step with
// the worker constructor.
func ResolveGCConfig() ResolvedGCConfig {
	cfg := workerCfg(Dependencies{})
	gcCfg := cfg.Workers.GC
	interval := orDuration(gcCfg.Interval, 30*time.Second)
	grace := orDuration(gcCfg.Grace, 5*time.Minute)
	batch := gcCfg.BatchSize
	concurrency := clampConcurrency(orInt(gcCfg.Concurrency, 1))
	shards := clampShards(orInt(gcCfg.Shards, 1))
	return ResolvedGCConfig{
		GraceSeconds:    int(grace.Seconds()),
		IntervalSeconds: int(interval.Seconds()),
		BatchSize:       batch,
		Concurrency:     concurrency,
		Shards:          shards,
	}
}

// orDuration returns v when non-zero, else fallback. Defaults flow through
// config.defaults() in production; tests that bypass Cfg via workerCfg
// fallback get a zero-valued config, so the explicit fallback keeps the
// historical default contract.
func orDuration(v, fallback time.Duration) time.Duration {
	if v == 0 {
		return fallback
	}
	return v
}

func orInt(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

func orInt64(v, fallback int64) int64 {
	if v == 0 {
		return fallback
	}
	return v
}

func orString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// durationFromEnv is retained for the small set of env knobs that are owned
// by config sections wired in later cycles (e.g. STRATA_AUDIT_RETENTION,
// owned by US-004 audit_log section).
func durationFromEnv(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
