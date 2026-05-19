package adminapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/heartbeat"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/s3api"
)

// GCConfig is the env-resolved GC worker tunable snapshot returned by
// GET /admin/v1/gc-config (US-001 drain-rebalance-transparency). int
// seconds (not Go duration strings) so the UI can do `seconds / 60`
// for minutes without parsing.
type GCConfig struct {
	GraceSeconds    int `json:"grace_seconds"`
	IntervalSeconds int `json:"interval_seconds"`
	BatchSize       int `json:"batch_size"`
	Concurrency     int `json:"concurrency"`
	Shards          int `json:"shards"`
}

// RebalanceConfig is the env-resolved rebalance worker tunable snapshot
// returned by GET /admin/v1/rebalance-config (US-001 drain-rebalance-
// transparency). ReplicasCount is derived per request from the heartbeat
// store (live nodes within `2 × HeartbeatInterval` of now) and is NOT
// part of the boot-time env snapshot.
type RebalanceConfig struct {
	IntervalSeconds int `json:"interval_seconds"`
	RateMBPerSec    int `json:"rate_mb_s"`
	Inflight        int `json:"inflight"`
	Shards          int `json:"shards"`
	ReplicasCount   int `json:"replicas_count"`
}

// handleGetGCConfig serves GET /admin/v1/gc-config. Always 200 — the
// snapshot is captured at boot from env, so there is no I/O path that
// can fail. Audit-stamped admin:GetGCConfig.
func (s *Server) handleGetGCConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:GetGCConfig", "gc-config", "-", owner)
	writeJSON(w, http.StatusOK, s.GCConfig)
}

// handleGetRebalanceConfig serves GET /admin/v1/rebalance-config. The
// env-resolved knobs are returned verbatim; replicas_count is derived
// from the heartbeat store filtered to live nodes (within
// `2 × HeartbeatInterval` of now). A ListNodes failure bumps the
// strata_admin_config_endpoint_errors_total{endpoint="rebalance-config"}
// counter and falls through to replicas_count=0 — the UI treats that as
// the `~24h+ ETA` fallback (US-002). Audit-stamped admin:GetRebalanceConfig.
func (s *Server) handleGetRebalanceConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:GetRebalanceConfig", "rebalance-config", "-", owner)

	out := s.RebalanceConfig
	out.ReplicasCount = s.countLiveReplicas(ctx)
	writeJSON(w, http.StatusOK, out)
}

// countLiveReplicas counts heartbeat rows whose last_heartbeat is within
// `2 × HeartbeatInterval` of now. nil Heartbeat store / list errors bump
// the per-endpoint error counter and return 0 — callers (UI) interpret
// that as the fallback path.
func (s *Server) countLiveReplicas(ctx context.Context) int {
	if s.Heartbeat == nil {
		return 0
	}
	nodes, err := s.Heartbeat.ListNodes(ctx)
	if err != nil {
		metrics.AdminConfigEndpointErrorsTotal.WithLabelValues("rebalance-config").Inc()
		s.Logger.Printf("adminapi: rebalance-config list nodes: %v", err)
		return 0
	}
	ttl := s.HeartbeatInterval * 2
	if ttl <= 0 {
		ttl = heartbeat.DefaultInterval * 2
	}
	now := time.Now().UTC()
	count := 0
	for _, n := range nodes {
		if n.LastHeartbeat.IsZero() {
			continue
		}
		if now.Sub(n.LastHeartbeat) <= ttl {
			count++
		}
	}
	return count
}
