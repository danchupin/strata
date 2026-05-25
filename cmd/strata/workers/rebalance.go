package workers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/notify"
	"github.com/danchupin/strata/internal/rebalance"
)

// RebalanceFanOut captures the *rebalance.ShardedFanOut built by the most
// recent buildRebalance call so the supervisor + diagnostics can observe
// the held-shard set without a fresh build. nil until buildRebalance has
// run.
var RebalanceFanOut *rebalance.ShardedFanOut

func init() {
	Register(Worker{
		Name:      "rebalance",
		Build:     buildRebalance,
		SkipLease: true,
	})
}

// buildRebalance consumes deps.Cfg.Workers.Rebalance for tunables.
// Range clamping happens inside config.Config.clampWorkers so TOML +
// env loads land at the same post-clamp values; the worker reads the
// clamped value directly. Phase 2 (US-002 rebalance-scale):
// SkipLease=true — the per-shard leases `rebalance-leader-<i>` are owned
// by *rebalance.ShardedFanOut, not the outer supervisor. The PlanEmitter
// is a MoverChain seeded with whichever movers the build tag enables
// (RADOS under `ceph`, S3 under US-005); chains with no movers fall back
// to the plan-logging behaviour shipped in US-003.
func buildRebalance(deps Dependencies) (Runner, error) {
	cfg := workerCfg(deps)
	rbCfg := cfg.Workers.Rebalance
	interval := orDuration(rbCfg.Interval, 5*time.Minute)
	rateMBPerSec := orInt(rbCfg.RateMBPerS, 100)
	inflight := orInt(rbCfg.Inflight, 4)
	shards := clampShards(orInt(rbCfg.Shards, 1))
	throttle := rebalance.NewThrottle(int64(rateMBPerSec)*1024*1024, int64(rateMBPerSec)*1024*1024)
	chain := &rebalance.MoverChain{
		Movers: rebalanceMovers(deps, throttle, inflight),
		Logger: deps.Logger,
	}
	var probe data.ClusterStatsProbe
	if p, ok := deps.Data.(data.ClusterStatsProbe); ok {
		probe = p
	}
	notifier := buildDrainCompleteNotifier(deps.Logger, cfg.Workers.Notify.Targets)
	tracer := deps.Tracer.Tracer("strata.worker.rebalance")
	auditTTL := cfg.AuditLog.Retention
	if auditTTL <= 0 {
		auditTTL = rebalance.DefaultDrainAuditRetention
	}
	build := func(shardID int) *rebalance.Worker {
		w, err := rebalance.New(rebalance.Config{
			Meta:         deps.Meta,
			Data:         deps.Data,
			Logger:       deps.Logger,
			Metrics:      metrics.RebalanceObserver{},
			Emitter:      chain,
			Interval:     interval,
			RateMBPerSec: rateMBPerSec,
			Inflight:     inflight,
			Tracer:       tracer,
			StatsProbe:   probe,
			Progress:     deps.RebalanceProgress,
			Notifier:     notifier,
			AuditTTL:     auditTTL,
			ShardID:      shardID,
			ShardCount:   shards,
		})
		if err != nil {
			deps.Logger.Error("rebalance: shard worker build failed",
				"shard", shardID, "error", err.Error())
			return nil
		}
		return w
	}
	fan := &rebalance.ShardedFanOut{
		Locker:     deps.Locker,
		ShardCount: shards,
		Logger:     deps.Logger,
		Build:      build,
	}
	if deps.EmitLeader != nil {
		fan.OnLeader = func(acquired bool) { deps.EmitLeader("rebalance", acquired) }
	}
	RebalanceFanOut = fan
	return fan, nil
}

// buildDrainCompleteNotifier resolves the workers.notify.targets spec
// into a best-effort drain-complete fan-out (US-005 drain-lifecycle).
// `ErrNoTargets` (spec unset) returns nil — the worker still logs +
// audits + bumps the metric on every transition; the notifier is purely
// additive. Any other parse error logs a WARN and returns nil so a
// malformed spec never blocks the rebalance worker from starting.
func buildDrainCompleteNotifier(logger *slog.Logger, targets string) rebalance.DrainNotifier {
	router, err := notify.RouterFromSpec(targets, notify.WithSQSClientFactory(sqsClientFactory))
	if err != nil {
		if !errors.Is(err, notify.ErrNoTargets) {
			logger.Warn("rebalance: drain-complete notifier disabled",
				"error", err.Error())
		}
		return nil
	}
	sinks := make([]notify.Sink, 0, len(router))
	for _, sink := range router {
		sinks = append(sinks, sink)
	}
	if len(sinks) == 0 {
		return nil
	}
	return &drainCompleteNotifier{logger: logger, sinks: sinks}
}

// drainCompleteNotifier adapts notify.Sink fan-out for the rebalance
// worker's drain-complete signal. The event is synthesised as a
// meta.NotificationEvent with EventName=s3:Drain:Complete + BucketID
// uuid.Nil so existing sink shapes (webhook + SQS) consume it without
// changes. Every sink is invoked sequentially; failures log WARN and
// otherwise no-op so notify outages never block the rebalance worker.
type drainCompleteNotifier struct {
	logger *slog.Logger
	sinks  []notify.Sink
}

func (n *drainCompleteNotifier) NotifyDrainComplete(ctx context.Context, evt rebalance.DrainCompleteEvent) {
	payload := fmt.Appendf(nil,
		`{"event":"s3:Drain:Complete","cluster":%q,"bytes_moved":%d,"completed_at":%q}`,
		evt.Cluster, evt.BytesMoved, evt.CompletedAt.UTC().Format(time.RFC3339Nano),
	)
	row := meta.NotificationEvent{
		BucketID:  uuid.Nil,
		Bucket:    "-",
		EventID:   randomEventID(),
		EventName: "s3:Drain:Complete",
		EventTime: evt.CompletedAt.UTC(),
		Payload:   payload,
	}
	for _, sink := range n.sinks {
		if err := sink.Send(ctx, row); err != nil {
			n.logger.Warn("drain complete: notify sink failed",
				"sink", sink.Name(), "cluster", evt.Cluster, "error", err.Error())
		}
	}
}

func randomEventID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Fall back to timestamp so callers always get a non-empty ID.
		return fmt.Sprintf("drain-%d", time.Now().UnixNano())
	}
	return "drain-" + hex.EncodeToString(buf[:])
}

// ResolvedRebalanceConfig is the env-resolved rebalance worker tunable
// snapshot surfaced on GET /admin/v1/rebalance-config (US-001 drain-
// rebalance-transparency). replicas_count is filled in by the admin
// handler from the heartbeat store at request time — not part of this
// struct.
type ResolvedRebalanceConfig struct {
	IntervalSeconds int
	RateMBPerSec    int
	Inflight        int
	Shards          int
}

// ResolveRebalanceConfig re-reads workers.rebalance.* tunables with the
// same clamps as buildRebalance. Read-only; no side effects. Kept here
// (rather than the adminapi layer) so the snapshot stays lock-step with
// the worker constructor.
func ResolveRebalanceConfig() ResolvedRebalanceConfig {
	cfg := workerCfg(Dependencies{})
	rbCfg := cfg.Workers.Rebalance
	interval := orDuration(rbCfg.Interval, 5*time.Minute)
	rateMBPerSec := orInt(rbCfg.RateMBPerS, 100)
	inflight := orInt(rbCfg.Inflight, 4)
	shards := clampShards(orInt(rbCfg.Shards, 1))
	return ResolvedRebalanceConfig{
		IntervalSeconds: int(interval.Seconds()),
		RateMBPerSec:    rateMBPerSec,
		Inflight:        inflight,
		Shards:          shards,
	}
}
