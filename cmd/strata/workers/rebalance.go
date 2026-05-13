package workers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/notify"
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
	var probe data.ClusterStatsProbe
	if p, ok := deps.Data.(data.ClusterStatsProbe); ok {
		probe = p
	}
	notifier := buildDrainCompleteNotifier(deps.Logger)
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
		StatsProbe:   probe,
		Progress:     deps.RebalanceProgress,
		Notifier:     notifier,
		AuditTTL:     auditRetentionFromEnv(deps.Logger),
	})
}

// buildDrainCompleteNotifier resolves STRATA_NOTIFY_TARGETS into a
// best-effort drain-complete fan-out (US-005 drain-lifecycle).
// `ErrNoTargets` (env unset) returns nil — the worker still logs +
// audits + bumps the metric on every transition; the notifier is purely
// additive. Any other parse error logs a WARN and returns nil so a
// malformed env never blocks the rebalance worker from starting.
func buildDrainCompleteNotifier(logger *slog.Logger) rebalance.DrainNotifier {
	router, err := notify.RouterFromEnv(notify.WithSQSClientFactory(sqsClientFactory))
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

// auditRetentionFromEnv re-reads STRATA_AUDIT_RETENTION inside the
// rebalance worker so the drain.complete audit rows share the gateway's
// TTL contract. Parse failure logs WARN and falls back to the rebalance
// default (30d), matching s3api.DefaultAuditRetention.
func auditRetentionFromEnv(logger *slog.Logger) time.Duration {
	raw := os.Getenv("STRATA_AUDIT_RETENTION")
	if raw == "" {
		return rebalance.DefaultDrainAuditRetention
	}
	d, err := parseAuditRetention(raw)
	if err != nil {
		logger.Warn("rebalance: audit retention parse failed; using default",
			"value", raw, "default", rebalance.DefaultDrainAuditRetention.String(), "error", err.Error())
		return rebalance.DefaultDrainAuditRetention
	}
	if d <= 0 {
		return rebalance.DefaultDrainAuditRetention
	}
	return d
}

// parseAuditRetention mirrors s3api.ParseAuditRetention without taking
// the s3api dep — keeps the worker package free of HTTP imports.
// Accepts plain Go durations and a bare "<N>d" days suffix.
func parseAuditRetention(s string) (time.Duration, error) {
	if len(s) > 0 && s[len(s)-1] == 'd' {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err != nil {
			return 0, err
		}
		if n < 0 {
			return 0, fmt.Errorf("negative retention: %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
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
