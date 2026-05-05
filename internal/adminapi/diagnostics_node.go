package adminapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/danchupin/strata/internal/heartbeat"
	"github.com/danchupin/strata/internal/promclient"
)

// Per-node drilldown PromQL fragments (US-011). Each query is filtered by the
// resolved heartbeat address via `instance="<addr>"`. The metric names are the
// stock prometheus/client_golang process collector + go collector, exposed by
// metrics.Handler() without extra wiring.
const (
	nodeCPUExprFmt        = `rate(process_cpu_seconds_total{instance="%s"}[1m])`
	nodeMemExprFmt        = `process_resident_memory_bytes{instance="%s"}`
	nodeFDsExprFmt        = `process_open_fds{instance="%s"}`
	nodeGoroutinesExprFmt = `go_goroutines{instance="%s"}`
	nodeGCPauseExprFmt    = `go_gc_duration_seconds{instance="%s",quantile="0.99"}`
)

const (
	defaultNodeRange = 15 * time.Minute
	maxNodeRange     = 24 * time.Hour
)

// NodeDrilldownResponse mirrors the wire shape consumed by NodeDetailDrawer.tsx.
// Each sparkline is an independent series so the UI can render them in
// individual recharts LineCharts without a join step.
type NodeDrilldownResponse struct {
	Node       NodeDrilldownNode  `json:"node"`
	CPU        []NodeMetricPoint  `json:"cpu"`
	Mem        []NodeMetricPoint  `json:"mem"`
	FDs        []NodeMetricPoint  `json:"fds"`
	Goroutines []NodeMetricPoint  `json:"goroutines"`
	GCPause    []NodeMetricPoint  `json:"gc_pause"`
}

// NodeDrilldownNode echoes the heartbeat row for the requested nodeID so the
// drawer header can render NodeID/Address/Version/Uptime/Workers/LeaderFor in
// a single round-trip.
type NodeDrilldownNode struct {
	ID            string   `json:"id"`
	Address       string   `json:"address"`
	Version       string   `json:"version"`
	StartedAt     int64    `json:"started_at"`
	UptimeSec     int64    `json:"uptime_sec"`
	Status        string   `json:"status"`
	Workers       []string `json:"workers"`
	LeaderFor     []string `json:"leader_for"`
	LastHeartbeat int64    `json:"last_heartbeat"`
}

type NodeMetricPoint struct {
	TS    time.Time `json:"ts"`
	Value float64   `json:"value"`
}

// handleDiagnosticsNode serves GET /admin/v1/diagnostics/node/{nodeID} (US-011).
// Resolves the heartbeat row by ID, then issues 5 PromQL range queries scoped
// by `instance=<addr>` for the CPU / mem / fds / goroutines / gc-pause
// sparklines. 503 MetricsUnavailable when Prom is unset; 404 NodeNotFound
// when the heartbeat table has no matching row.
func (s *Server) handleDiagnosticsNode(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeID")
	if nodeID == "" {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "nodeID path segment is required")
		return
	}
	stampAuditOverride(r, "admin:GetNodeDrilldown", "diagnostics:node:"+nodeID, "")

	q := r.URL.Query()
	rangeDur, err := parsePositiveDuration(q.Get("range"), defaultNodeRange)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "range must be a positive Go duration")
		return
	}
	if rangeDur > maxNodeRange {
		rangeDur = maxNodeRange
	}
	stepDur := defaultStep(rangeDur)

	if s.Heartbeat == nil {
		writeJSONError(w, http.StatusNotFound, "NodeNotFound", "heartbeat store unavailable")
		return
	}
	nodes, err := s.Heartbeat.ListNodes(r.Context())
	if err != nil {
		s.Logger.Printf("adminapi: diagnostics/node list: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Internal", "list nodes failed")
		return
	}
	var node *heartbeat.Node
	for i := range nodes {
		if nodes[i].ID == nodeID {
			node = &nodes[i]
			break
		}
	}
	if node == nil {
		writeJSONError(w, http.StatusNotFound, "NodeNotFound", fmt.Sprintf("node %q not found in heartbeat table", nodeID))
		return
	}

	if !s.Prom.Available() {
		writeJSONError(w, http.StatusServiceUnavailable, "MetricsUnavailable",
			"Prometheus is not configured (STRATA_PROMETHEUS_URL is empty)")
		return
	}

	resp, err := buildNodeDrilldown(r.Context(), s.Prom, *node, time.Now(), rangeDur, stepDur)
	if err != nil {
		if errors.Is(err, promclient.ErrUnavailable) {
			writeJSONError(w, http.StatusServiceUnavailable, "MetricsUnavailable", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildNodeDrilldown runs the 5 PromQL queries in sequence (the promclient
// semaphore caps concurrency cluster-wide; serial here keeps the per-call
// fan-out predictable) and shapes the response. now is injected so tests can
// pin the result window.
func buildNodeDrilldown(ctx context.Context, prom *promclient.Client, node heartbeat.Node, now time.Time, rangeDur, step time.Duration) (NodeDrilldownResponse, error) {
	end := now
	start := end.Add(-rangeDur)

	type queryRef struct {
		expr string
		dest *[]NodeMetricPoint
	}
	resp := NodeDrilldownResponse{
		Node:       toDrilldownNode(node, now),
		CPU:        []NodeMetricPoint{},
		Mem:        []NodeMetricPoint{},
		FDs:        []NodeMetricPoint{},
		Goroutines: []NodeMetricPoint{},
		GCPause:    []NodeMetricPoint{},
	}
	queries := []queryRef{
		{fmt.Sprintf(nodeCPUExprFmt, node.Address), &resp.CPU},
		{fmt.Sprintf(nodeMemExprFmt, node.Address), &resp.Mem},
		{fmt.Sprintf(nodeFDsExprFmt, node.Address), &resp.FDs},
		{fmt.Sprintf(nodeGoroutinesExprFmt, node.Address), &resp.Goroutines},
		{fmt.Sprintf(nodeGCPauseExprFmt, node.Address), &resp.GCPause},
	}
	for _, qr := range queries {
		series, err := prom.QueryRange(ctx, qr.expr, start, end, step)
		if err != nil {
			return NodeDrilldownResponse{}, err
		}
		*qr.dest = flattenSeries(series)
	}
	return resp, nil
}

// flattenSeries collapses a Series list (one per matched label set) into a
// flat ascending-time []NodeMetricPoint. The per-node queries are scoped by
// instance so we expect zero or one series; collapse handles both.
func flattenSeries(series []promclient.Series) []NodeMetricPoint {
	out := []NodeMetricPoint{}
	for _, s := range series {
		for _, p := range s.Points {
			out = append(out, NodeMetricPoint{TS: p.Timestamp, Value: p.Value})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out
}

// toDrilldownNode maps a heartbeat.Node to the wire shape carried in the
// drawer header. Mirrors the per-row mapping in handleClusterNodes.
func toDrilldownNode(n heartbeat.Node, now time.Time) NodeDrilldownNode {
	uptime := max(int64(now.Sub(n.StartedAt).Seconds()), 0)
	return NodeDrilldownNode{
		ID:            n.ID,
		Address:       n.Address,
		Version:       n.Version,
		StartedAt:     n.StartedAt.Unix(),
		UptimeSec:     uptime,
		Status:        nodeStatus(n, now),
		Workers:       nilSliceToEmpty(n.Workers),
		LeaderFor:     nilSliceToEmpty(n.LeaderFor),
		LastHeartbeat: n.LastHeartbeat.Unix(),
	}
}
