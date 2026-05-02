# PRD: Web UI — Debug Tooling (Phase 3 of 3)

> **Status:** Outline. Detailed AC will be authored when
> `prd-web-ui-foundation.md` and `prd-web-ui-admin.md` ship.

## Introduction

Phase 1 ships the read-only console. Phase 2 ships admin write-actions.
Phase 3 ships **performance debug tooling**: hotspot heatmaps, slow-query
tracer, OTel trace browser, bucket-shard distribution, per-node disk /
RAM / CPU drilldowns, live audit-log tail. Modelled on the CockroachDB
DB Console's "Statements" + "Sessions" + "Hot Ranges" tabs.

After Phase 3 an operator can answer:
- "Which bucket / key / IAM user is driving the current load?"
- "Why is p99 GET latency spiking right now?"
- "Where is the hot shard? Should we resharder?"
- "Show me the OTel trace for request `<request-id>`"

## Goals (sketch)

- **Hot Buckets / Keys** — heatmap: bucket × time-bucket grid, cell
  intensity = request count. Click a cell → drill into that bucket's
  slow queries
- **Hot Shards** — per-bucket shard heatmap (Strata shards each bucket
  64-way for the `objects` table). Cell = shard, intensity = LWT
  conflict rate. Identifies skewed key distributions; click → see
  top contended keys (when audit log allows)
- **Slow Queries** — table of last N requests above latency threshold
  (configurable; default p99). Columns: time, bucket, op, latency,
  status, request-id (link → trace). Sortable + filterable
- **OTel Trace Browser** — paste/click a request-id → render the
  span waterfall (gateway → meta.Store → Cassandra/TiKV → RADOS / S3-
  backend), with timing breakdown per span. Source: in-process OTel
  exporter buffering recent traces (~10 min) OR live Jaeger/Tempo
  forward when configured
- **Live Audit Tail** — SSE stream of audit events; filter by action,
  bucket, principal. Pause / resume / clear
- **Per-node Drilldown** — click a node on the cluster overview →
  panel with CPU / RAM / FD-count / goroutine-count / GC pause sparklines
- **Bucket-Shard Distribution** — single-bucket page tab: bytes per
  shard, object-count per shard. Identifies hot-spot risk before it
  bites
- **Replication Lag** — per-bucket lag chart for replicator-configured
  buckets

## User Stories (titles only; AC TBD)

- US-001: SSE audit-tail backend (`/admin/v1/audit/stream`)
- US-002: SSE consumer + live-tail UI page (filter / pause / resume)
- US-003: Slow-queries page backend
  (`/admin/v1/diagnostics/slow-queries?since=<dur>&min_ms=<ms>`)
- US-004: Slow-queries page UI (table + filters)
- US-005: OTel trace buffer + endpoint
  (`/admin/v1/diagnostics/trace/<request-id>`)
- US-006: OTel trace waterfall renderer
- US-007: Hot Buckets heatmap backend
- US-008: Hot Buckets heatmap UI
- US-009: Hot Shards heatmap backend (per-bucket)
- US-010: Hot Shards heatmap UI
- US-011: Per-node drilldown panel (CPU / RAM / FD / goroutines / GC)
- US-012: Bucket-Shard Distribution tab on bucket detail page
- US-013: Replication Lag chart
- US-014: Playwright e2e for debug flows
- US-015: docs/ui.md Phase 3 capability matrix update + ROADMAP
  close-flip

## Non-Goals

- No statement-level statement-history (akin to CockroachDB statement
  diagnostics). Strata is request-shaped (S3 verbs), not SQL — slow-
  queries page covers this without a separate statement page
- No query plan visualizer — same reason as above
- No third-party APM integration (Datadog, New Relic). Operators with
  those already have OTel forward via collector
- No alerts / paging from the UI — Grafana / Alertmanager is the
  right tool. Our UI is point-in-time inspection

## Open Questions

- OTel trace buffer storage: in-memory ring buffer (~10 min) vs forward
  to Jaeger/Tempo? Memory option works without external deps; forward
  is needed for >10-min-old traces. Probably both, with auto-fallback
- Hot-shard detection: poll-based (PromQL aggregation by `shard` label)
  vs push-based (gateway sends per-shard telemetry to the console
  backend)? Poll is simpler; push is faster. Decide at story-start
