# PRD: Web UI — Debug Tooling (Phase 3 of 3)

> **Status:** Detailed AC. Phase 1 (`tasks/archive/prd-web-ui-foundation.md`)
> shipped 2026-05-03 (`e27cf21`). Phase 2 (`prd-web-ui-admin.md`) is
> queued. Phase 3 ships after Phase 2.
>
> **Backend audit (2026-05-03, post-merge `main` = modern-complete +
> binary-consolidation + tikv-meta-backend + web-ui-foundation +
> s3-over-s3-backend):**
>
> - Every backing surface Phase 3 reads from already exists on main:
>   OTel tracing through Cassandra + RADOS (`modern-complete` US-033),
>   audit log + per-request `request_id` correlation (US-022), per-
>   bucket shard layout via `meta.Bucket.ShardCount`, Prometheus
>   request metrics, `internal/heartbeat` (memory + cassandra) for
>   per-replica node lists.
> - **TiKV heartbeat gap:** `internal/heartbeat` ships memory +
>   cassandra only. TiKV-backed deployments need a TiKV heartbeat
>   store before the Per-node Drilldown story (US-011 below) populates.
>   Tracked under ROADMAP "Web UI — TiKV heartbeat backend" (P3) — a
>   prerequisite for US-011 on tikv clusters.
> - **Hot Shards on s3-over-s3 backend:** the s3-backend stores one
>   Strata object as one backend object via backend multipart upload —
>   no chunking, no shards. Hot-Shards heatmap is meaningful only for
>   RADOS-backed clusters; on s3-backend the page renders an
>   explanatory empty-state.
> - **Object-store SSE shape:** s3-over-s3 records SSE disposition per
>   object on `Manifest.SSE`. Slow-Queries / OTel trace browser surfaces
>   this when relevant.
>
> The new surfaces this PRD adds: SSE audit-stream endpoint, slow-
> queries table from audit-log filter, OTel trace ring buffer + browser,
> hot-bucket / hot-shard heatmap aggregations (PromQL), per-node
> drilldown, bucket-shard distribution view, replication-lag chart.

## Introduction

Phase 1 ships read-only dashboards. Phase 2 ships admin write actions.
Phase 3 ships **performance debug tooling** so an operator can answer:

- "Which bucket / key / IAM user is driving the current load?"
- "Why is p99 GET latency spiking right now?"
- "Where is the hot shard? Should we reshard?"
- "Show me the OTel trace for request `<request-id>`"
- "What is the replication lag for bucket X?"

Modelled on CockroachDB DB Console's "Statements" + "Sessions" + "Hot
Ranges" tabs.

## Goals

- **Live audit tail** — SSE stream of audit events; filter by action,
  bucket, principal. Pause / resume / clear
- **Slow queries** — table of last N requests above a configurable
  latency threshold, with request-id click-through to OTel trace
- **OTel trace ring buffer + browser** — in-process ring buffer holds
  the last ~10 minutes of traces; UI renders the span waterfall
  (gateway → meta.Store → Cassandra/TiKV → RADOS / S3-backend) when
  the operator pastes / clicks a request-id. Forwards to Jaeger /
  Tempo when `OTEL_EXPORTER_OTLP_ENDPOINT` is set (existing OTel
  pipeline)
- **Hot Buckets heatmap** — bucket × time-bucket grid, cell intensity
  = request count via PromQL aggregation
- **Hot Shards heatmap** — per-bucket shard heatmap (RADOS-backed
  clusters only; s3-over-s3 renders empty-state with explainer)
- **Per-node drilldown** — click a node on cluster overview → CPU /
  RAM / FD-count / goroutine-count / GC-pause sparklines
- **Bucket-Shard Distribution** — single-bucket page tab: bytes per
  shard, object-count per shard
- **Replication Lag** — per-bucket lag chart for replicator-configured
  buckets
- Playwright e2e at `web/e2e/debug.spec.ts` covers critical debug
  paths
- ROADMAP P3 entries flip to Done close-flip per CLAUDE.md

## User Stories

### US-001: SSE audit-tail backend
**Description:** As a debug-tool author, I want a Server-Sent Events
endpoint that streams new audit-log rows so the live-tail UI can
subscribe without polling.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/audit/stream?action=<str>&principal=<str>&bucket=<str>`
      sets `Content-Type: text/event-stream` and emits one
      `data: <audit-row-json>\n\n` per matching row
- [ ] Backend implementation: `internal/auditstream` package adds
      `Broadcaster` — an in-process pub-sub. Hooks into existing
      `s3api.AuditMiddleware`'s ack path: after the row is persisted,
      a non-blocking send onto a fan-out channel. Slow subscribers
      drop frames (logged once per minute via WARN, never blocks the
      audit write hot path)
- [ ] `auditstream.Broadcaster.Subscribe(ctx, filter) <-chan AuditRow`
      returns a per-subscriber channel sized 256. Cancel via ctx
- [ ] `adminapi.handleAuditStream` reads the channel, applies the
      filter (action / principal / bucket — server-side so filtered
      subs do not receive irrelevant frames), writes SSE frames.
      Closes on ctx done, emits a `:keep-alive\n\n` ping every 25 s
      to defeat proxies that close idle connections
- [ ] One Prometheus gauge `strata_audit_stream_subscribers` exposed
      via `metrics.AuditStreamSubscribers`
- [ ] Typecheck passes
- [ ] Tests pass (Broadcaster test + handler test that asserts the
      framing shape)

### US-002: SSE consumer + live-tail UI page
**Description:** As an operator, I want a live-tailing page that
streams audit events with filter / pause / resume.

**Acceptance Criteria:**
- [ ] New `web/src/pages/AuditTail.tsx`. Subscribes to
      `/admin/v1/audit/stream` via the EventSource API. Re-subscribes
      with the new query string when filters change
- [ ] Top bar: filters (Action multi-select, Principal autocomplete,
      Bucket autocomplete), buttons Pause | Resume | Clear, counter
      "<n> events streamed"
- [ ] Event list: virtualised (`@tanstack/react-virtual`) so 10k+
      events do not balloon DOM. Each row: relative time | Action |
      Principal | Resource | Result. Click → opens the same side
      panel as the Phase 2 audit viewer (US-017 of Phase 2)
- [ ] "Pause" stops appending; the EventSource stays subscribed.
      "Resume" re-attaches the renderer to the live stream — gap
      banner: "<n> events skipped while paused"
- [ ] "Clear" empties the ring; subscription stays alive
- [ ] When the EventSource errors / closes, banner "Connection lost
      — reconnecting in N s" + auto-reconnect (exponential backoff
      starting at 1 s, capped at 30 s)
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-003: Slow-queries page backend
**Description:** As a debug-tool author, I want an endpoint that
returns the last N requests above a latency threshold so the slow-
queries UI table has data to render.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/diagnostics/slow-queries?since=<dur>&min_ms=<int>&page_token=<base64>`
      returns rows + next page-token. Default `since=15m`,
      `min_ms=100`
- [ ] Source: existing `audit_log` table — `total_time_ms` column is
      populated by `s3api.AccessLogMiddleware`. Wrap the existing
      `meta.Store.ListAudit` with a filter `WHERE total_time_ms >=
      ?`. For Cassandra this is server-side filtering (allowed under
      partitioning by `(bucket_id, day)`); for tikv use the existing
      sweeper-friendly range scan
- [ ] Returns: time | bucket | op | latency_ms | status | request_id
      | principal | source_ip | object_key (truncated to 100 chars).
      Sortable by latency_ms (server-side; default DESC)
- [ ] New `meta.Store.ListSlowQueries(ctx, since, minMs, pageToken)`
      method (one-line wrapper over `ListAudit` + filter; landed in
      lockstep across cassandra + memory + tikv with storetest
      contract entries)
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: Slow-queries page UI
**Description:** As an operator, I want a slow-queries page with
filters and request-id click-through to the trace browser.

**Acceptance Criteria:**
- [ ] New `web/src/pages/SlowQueries.tsx`. Filter bar: Time window
      (15 m | 1 h | 6 h | 24 h | 7 d), Min latency (input ms),
      Bucket (autocomplete), Op (multi-select GET / PUT / DELETE /
      HEAD / Multipart*)
- [ ] Polls every 30 s via TanStack Query. Pagination via "Load more"
      with the page-token
- [ ] Latency column: rendered with a colored badge — red ≥1000 ms,
      orange ≥500 ms, yellow ≥100 ms, gray <100 ms
- [ ] RequestID column: link → `/diagnostics/trace/<request-id>`
      (US-006 page). Tooltip "Trace" on hover
- [ ] Tabbed alternate view "Latency histogram": logarithmic
      histogram of the current filtered set (recharts BarChart, 8
      buckets — 0–10ms, 10–100ms, …, ≥10s)
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-005: OTel trace ring buffer + endpoint
**Description:** As a debug-tool author, I want an in-process ring
buffer that retains the last ~10 minutes of OTel spans keyed by
request-id so the UI can render trace waterfalls without an external
Jaeger / Tempo deployment.

**Acceptance Criteria:**
- [ ] New `internal/otel/ringbuf` package. `RingBuffer{}` holds spans
      keyed by `traceID` with secondary index `requestID → traceID`.
      Two indices because the SDK keys by traceID; the operator pastes
      the X-Request-Id header value
- [ ] `OnEnd(span)` (implements `sdktrace.SpanProcessor`) records
      every span to the buffer. Capacity: 4 MiB total (configurable
      via `STRATA_OTEL_RINGBUF_BYTES`, default `4*1024*1024`); LRU-
      evict oldest traces when over budget
- [ ] Wire into existing `internal/otel.Init`: when
      `OTEL_EXPORTER_OTLP_ENDPOINT` is empty AND
      `STRATA_OTEL_RINGBUF` is `"on"` (default `"on"`), install the
      ring-buffer span-processor. When the endpoint is set, install
      both: forward to OTLP **AND** retain locally
- [ ] `GET /admin/v1/diagnostics/trace/{requestID}` returns the span
      tree as JSON: `{trace_id, request_id, root, spans: [{span_id,
      parent, name, start_ns, end_ns, attributes, status}]}`. 404 when
      not present
- [ ] One Prometheus gauge `strata_otel_ringbuf_traces` exposes
      retained-trace count; counter `strata_otel_ringbuf_evicted_total`
      exposes evictions
- [ ] Typecheck passes
- [ ] Tests pass (ringbuf bytes-budget test, eviction order test,
      handler test)

### US-006: OTel trace waterfall renderer
**Description:** As an operator, I want a waterfall view for a single
trace so I can see where time was spent.

**Acceptance Criteria:**
- [ ] New `web/src/pages/TraceBrowser.tsx` + route
      `/diagnostics/trace/:requestID`
- [ ] Top bar: search input "Paste a request-id", recent-traces list
      (from sessionStorage)
- [ ] Waterfall canvas: each span rendered as a horizontal bar,
      indented by parent → child, with ms-scale axis. Hover →
      tooltip with span name, duration, status, attributes (subset:
      `http.method`, `http.target`, `http.status_code`,
      `db.statement` truncated to 200 chars). Span color coded by
      service: gateway = blue, meta = green, data = orange. Errored
      spans get a red border
- [ ] Span details panel (right column) on click: full attribute
      table, events, status. Copy-to-clipboard for the trace JSON
- [ ] "Open in Jaeger" link visible only when
      `cluster.otel_endpoint` is non-empty (read from
      `/admin/v1/cluster/status`); URL pattern is the standard
      `<jaeger-ui>/trace/<traceID>`
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-007: Hot Buckets heatmap backend
**Description:** As a debug-tool author, I want an endpoint that
aggregates request rate per bucket per time bucket via PromQL so the
heatmap UI has data.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/diagnostics/hot-buckets?range=<dur>&step=<dur>`
      issues a PromQL query against the configured Prometheus URL
      (existing `internal/promclient.Client`). Default
      `range=1h&step=1m`
- [ ] PromQL: `sum by (bucket) (rate(strata_http_requests_total[1m]))`
      over the requested range. Returns `{matrix: [{bucket: string,
      values: [{ts, value}]}]}`. Handles `metrics_available=false`
      when Prom is unset → 503 with code `MetricsUnavailable`
- [ ] Top-N filter: only buckets in the top-50 by total request count
      over the range (server-side trim so the wire payload stays small)
- [ ] Cache: 30 s in-process LRU keyed on `(range, step)`; reduces
      Prom load when multiple operators view the page
- [ ] Typecheck passes
- [ ] Tests pass (against a fake `promclient.Client` matrix response)

### US-008: Hot Buckets heatmap UI
**Description:** As an operator, I want a heatmap of bucket request
rate so I can spot the busiest buckets at a glance.

**Acceptance Criteria:**
- [ ] New `web/src/pages/HotBuckets.tsx` + route `/diagnostics/hot-buckets`
- [ ] Range selector: 15 m | 1 h | 6 h | 24 h. Step auto-derived
      (1 m / 5 m / 30 m / 1 h)
- [ ] Heatmap rendered via custom canvas (recharts has no native
      heatmap; building on top of `@nivo/heatmap` adds 100 KiB —
      use a 200-line raw-canvas component instead). X-axis = time
      buckets, Y-axis = bucket name, cell color = log-scale of
      request rate
- [ ] Click a cell → drill into the bucket detail page filtered by
      the timespan (`?since=<RFC3339>&until=<RFC3339>`)
- [ ] When the metrics backend is unavailable, render an empty-state
      with `cluster.metrics_available=false` explainer + link to
      `docs/ui.md#prometheus-setup`
- [ ] Polls every 30 s
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-009: Hot Shards heatmap backend (RADOS-only, PromQL)
**Description:** As a debug-tool author, I want an endpoint that
aggregates LWT-conflict rate per shard per time bucket so the hot-
shards UI can identify skewed key distributions.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/diagnostics/hot-shards/{bucket}?range=<dur>&step=<dur>`
      issues a PromQL query
- [ ] PromQL:
      `sum by (shard) (rate(strata_cassandra_lwt_conflicts_total{bucket="<bucket>"}[1m]))`.
      Requires `bucket` + `shard` labels on
      `strata_cassandra_lwt_conflicts_total` — **prerequisite story**:
      add the labels in `internal/meta/cassandra/observer.go` (current
      observer emits the metric without those labels). Cardinality
      check: 1000 buckets × 64 shards = 64k series — within Prom's
      typical limits for a metrics gateway. Document in the story's
      AC notes
- [ ] When `cfg.DataBackend == "s3"`: returns 200 with
      `{empty: true, reason: "s3-over-s3 stores objects 1:1, no
      shards"}`. UI handles this case (US-010)
- [ ] Returns the same matrix shape as US-007
- [ ] Typecheck passes
- [ ] Tests pass

### US-010: Hot Shards heatmap UI
**Description:** As an operator, I want a per-bucket shard heatmap
that highlights LWT-contention skew on RADOS-backed deployments.

**Acceptance Criteria:**
- [ ] Bucket detail page gains a "Hot Shards" tab. Heatmap component
      reused from US-008
- [ ] When the backend returns `empty: true` (s3-over-s3 backend),
      render an empty-state card: "Shard heatmap is meaningful only
      for RADOS-backed clusters. The s3-over-s3 backend stores each
      Strata object as one backend object — no shards."
- [ ] Click a cell → drill into a panel listing the top contended
      keys for that shard in the timespan (read from the audit log
      via existing `meta.Store.ListAudit` filtered by `bucket` +
      reconstructing shard from `hash(key) % shard_count`)
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-011: Per-node drilldown panel
**Description:** As an operator, I want a per-node panel with CPU /
RAM / FD-count / goroutine / GC sparklines when I click a node in
the cluster overview.

**Acceptance Criteria:**
- [ ] **Prerequisite:** strata exposes `process_*` Go runtime metrics
      via `metrics.Handler()` (existing — `prometheus/client_golang`
      ships them by default). Verify the standard set is present:
      `process_cpu_seconds_total`, `process_resident_memory_bytes`,
      `process_open_fds`, `go_goroutines`, `go_gc_duration_seconds`
- [ ] `GET /admin/v1/diagnostics/node/{nodeID}?range=<dur>` issues
      five PromQL queries (one per sparkline) with the standard label
      filter `instance="<node-address>"`. Returns a single response
      `{cpu, mem, fds, goroutines, gc_pause}` each = matrix shape
- [ ] Cluster overview node row click → opens the side panel
      (`<NodeDetailDrawer>`). Panel header: NodeID, Address, Version,
      Uptime, Workers (chips), Leader-For (chips). Body: 5 small
      recharts LineChart sparklines stacked
- [ ] Range selector inside the drawer (15 m | 1 h | 6 h)
- [ ] On TiKV-backed clusters, the cluster overview only shows the
      local replica until US-NN of "TiKV heartbeat backend" lands
      (ROADMAP P3) — drawer empty-state explains this
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-012: Bucket-Shard Distribution tab
**Description:** As an operator, I want to see bytes per shard +
object-count per shard for a bucket so I can spot hot-spot risk
before it bites.

**Acceptance Criteria:**
- [ ] Bucket detail page gains a "Distribution" tab. Backend:
      `GET /admin/v1/buckets/{bucket}/distribution` reads
      `strata_bucket_shard_bytes` + `strata_bucket_shard_objects`
      gauges via the existing `bucketstats` sampler. **Prerequisite:**
      add `bucketstats.Sampler` per-shard sampling — currently it
      samples `(bucket, total_bytes, total_objects)`; extend to
      `(bucket, shard, bytes, objects)`. Cassandra: read
      `objects` table aggregates per `(bucket_id, shard)`. Memory:
      compute on demand. TiKV: scan with the existing range-scan
      shape and group by hash(key) % ShardCount
- [ ] Tab renders two recharts BarCharts side-by-side: bytes per
      shard, objects per shard. X-axis = shard ID (0..N-1), Y-axis
      = log-scale value
- [ ] Skew banner: when max(shard) / median(shard) > 5, render a
      yellow warning "Shard skew detected — consider resharding"
      with link to the existing `/admin/bucket/reshard` endpoint
      docs
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-013: Replication Lag chart
**Description:** As an operator, I want a per-bucket replication-lag
chart so I can see whether the replicator worker is keeping up.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/buckets/{bucket}/replication-lag?range=<dur>`
      returns a recharts-friendly time-series. Source: PromQL on
      `strata_replication_queue_age_seconds{bucket="<bucket>"}`
      (existing gauge from `internal/replication/worker.go`). Returns
      empty matrix when the bucket has no replication configured
      (`meta.Store.GetBucketReplication` returns `ErrNoSuchReplication`)
- [ ] Bucket detail "Replication" tab — visible only when the bucket
      has a replication configuration. Renders the recharts LineChart
      with the lag gauge over the selected range
- [ ] Threshold annotations: dashed lines at 1 s, 60 s, 600 s. Color
      band when lag exceeds 600 s (red)
- [ ] Polls every 30 s
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-014: Playwright e2e for debug flows
**Description:** As a maintainer, I want Playwright e2e for the
critical debug paths so regressions surface in CI.

**Acceptance Criteria:**
- [ ] New `web/e2e/debug.spec.ts` with these flows:
  - `audit-tail`: login → AuditTail → trigger a PutObject via
    fetch → assert event appears in the live tail
  - `slow-queries`: SlowQueries → set `min_ms=0` → assert recent
    rows render
  - `trace-browser`: trigger a request → grab the request-id from
    response header → paste into trace browser → assert the
    waterfall renders ≥1 span
  - `hot-buckets-empty`: HotBuckets with no Prom configured →
    asserts the empty-state card renders
  - `hot-shards-s3`: HotShards on a bucket with the s3 backend →
    asserts the s3-explainer empty-state renders
- [ ] CI workflow `.github/workflows/ci.yml` `e2e-ui` job adds
      `pnpm exec playwright test debug.spec.ts` after the existing
      `critical-path.spec.ts` and `admin.spec.ts` invocations
- [ ] Tests run against `make run-memory` — the OTel ring buffer +
      audit log + bucketstats sampler all run on memory backends
- [ ] Typecheck passes
- [ ] Tests pass

### US-015: docs/ui.md Phase 3 update + ROADMAP close-flip
**Description:** As a developer, I want `docs/ui.md` updated with
Phase 3 capability matrix and the ROADMAP P3 web-ui-debug entry
flipped to Done.

**Acceptance Criteria:**
- [ ] `docs/ui.md` "Capability Matrix" gains a Phase 3 column with
      row entries: AuditTail | SlowQueries | OTelTraceBrowser |
      HotBuckets | HotShards | NodeDrilldown | ShardDistribution |
      ReplicationLag
- [ ] ROADMAP "Web UI — Phase 3 (debug)" P3 entry flips to:
      `~~**P3 — Web UI — Phase 3 (debug).**~~ — **Done.** <one-line
      summary>. (commit `<sha>`)`
- [ ] Add a new ROADMAP entry under Web UI: any P3 hardening items
      the cycle exposed (e.g. "OTel ring-buffer eviction policy
      under burst load needs benchmark-driven cap-tuning")
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- FR-1: Every Phase 3 endpoint sits under `/admin/v1/*` with the same
  JWT session-cookie + SigV4 auth chain as Phase 1 / Phase 2
- FR-2: SSE audit-tail (US-001) is push-based — `s3api.AuditMiddleware`
  fans out to the broadcaster after the row persists; never blocks
  the audit-write hot path
- FR-3: OTel ring buffer (US-005) is in-process and capped by bytes
  (configurable via `STRATA_OTEL_RINGBUF_BYTES`); LRU-evicts oldest
  traces. When `OTEL_EXPORTER_OTLP_ENDPOINT` is set, both the OTLP
  forward AND the ring buffer run in parallel
- FR-4: Hot Buckets / Hot Shards heatmaps (US-007..US-010) source
  data from PromQL via the existing `internal/promclient.Client`.
  When Prom is unset, every page renders an explainer empty-state
  with a link to `docs/ui.md#prometheus-setup`
- FR-5: Per-node drilldown (US-011) reads standard `process_*` /
  `go_*` runtime metrics — no new metrics emitted from strata
- FR-6: Hot Shards (US-009/US-010) renders an explainer empty-state
  on s3-over-s3 backends (no shards there)
- FR-7: Bucket-Shard Distribution (US-012) requires extending the
  existing `bucketstats.Sampler` to per-shard sampling. Cardinality:
  10000 buckets × 64 shards = 640k series — bounded by the operator's
  `STRATA_BUCKETSTATS_TOPN` env var (default 100, applied per-shard
  too)
- FR-8: Slow Queries (US-003) reads the existing `audit_log` table
  filtered by `total_time_ms >= ?`; one new `meta.Store.ListSlowQueries`
  method ships in lockstep across cassandra + memory + tikv
- FR-9: Replication Lag (US-013) reads the existing
  `strata_replication_queue_age_seconds` gauge; no new metrics needed
- FR-10: All Phase 3 pages are read-only — debug tooling never
  mutates state. Aborting a multipart from the watchdog (Phase 2
  US-016) is the only "destructive debug" action and it lives there

## Non-Goals

- No statement-level diagnostics (akin to CockroachDB statement
  diagnostics). Strata is request-shaped (S3 verbs), not SQL — slow-
  queries page covers this without a separate statement page
- No query-plan visualizer — same reason as above
- No third-party APM integration (Datadog, New Relic). Operators
  with those have OTel forward via collector
- No alerts / paging from the UI — Grafana / Alertmanager is the
  right tool. Our UI is point-in-time inspection only
- No long-term trace retention — ring buffer is ~10 minutes; for
  longer retention configure OTLP forward to Jaeger / Tempo
- No log file viewer — `docker logs` / `kubectl logs` is the right
  tool. The audit-tail viewer covers what is queryable

## Design Considerations

- **Bundle budget**: home / metrics / buckets-list bundles MUST stay
  ≤500 KiB gzipped. Heatmap canvas (US-008/US-010) is a 200-line
  raw-canvas component — no `@nivo/heatmap` (~100 KiB)
- **Component reuse**: extend Phase 1/2 shadcn/ui components. Ring-
  buffer trace renderer is the only genuinely new chart shape;
  everything else is recharts LineChart / BarChart already in the
  bundle
- **SSE handling**: `EventSource` (browser native) → no
  third-party lib. Reconnect with exponential backoff handled in a
  small wrapper hook `useEventSource(url, opts)`
- **Empty states are first-class**: every page that depends on Prom
  / OTel / replication-config renders a polished empty-state when
  the dependency is missing, never a broken chart
- **No optimistic UI**: same rule as Phase 2 — debug pages do not
  mutate state, but should never lie about the state of an
  in-flight read

## Technical Considerations

- **`internal/auditstream` (NEW)**: per-process pub-sub, fan-out
  bounded by 256 per subscriber, slow-subscriber-drop policy with
  one WARN log per minute
- **`internal/otel/ringbuf` (NEW)**: span-level ring buffer with
  bytes-budget; secondary index on request-id → trace-id
- **`internal/meta/cassandra/observer.go`**: extend
  `SlowQueryObserver` to emit `bucket` + `shard` labels on
  `strata_cassandra_lwt_conflicts_total`. Cardinality bounded by
  `STRATA_BUCKETSTATS_TOPN`
- **`bucketstats.Sampler`**: extend to per-shard aggregates. Cassandra
  reads `objects` rows grouped by `(bucket_id, shard)`. Memory
  computes from the in-process map. TiKV scans with the existing
  range-scan shape and groups in-process
- **PromQL query budget**: cap at 10 concurrent queries from the
  admin process to a single Prometheus host; queue beyond that.
  `internal/promclient.Client` gains a `Semaphore(10)` chan
- **TiKV heartbeat**: prerequisite for US-011 on tikv clusters.
  Tracked under ROADMAP "Web UI — TiKV heartbeat backend" (P3) —
  not blocking Phase 3 itself; cassandra clusters get the full
  drilldown experience day-one
- **OTel ring buffer eviction**: bytes-budget + LRU — simpler than
  rate-based + sliding-window. Risk: a single huge trace evicts
  many small traces. Mitigation: cap per-trace span count at 256
  (drops further spans with a warning; documented in the OTel
  pipeline operator guide)

## Success Metrics

- Phase 3 closes the "operator visibility gap" identified at the
  start of the cycle (a checklist of 5 outage scenarios from the
  past quarter is documented and each can be diagnosed in the UI in
  <2 minutes)
- `bin/strata` binary size grows by <1 MiB after Phase 3 (the new
  packages are pure Go; no large dependencies)
- All 15 stories close with the new pages exercised in the cycle's
  smoke pass + Playwright suite
- `debug.spec.ts` runs in <60 s on CI (chromium-only); no flaky test
  exemptions

## Open Questions

- OTel ring-buffer cardinality limit on a busy gateway —
  STRATA_OTEL_RINGBUF_BYTES default of 4 MiB holds ~2000 traces with
  ~50 spans each. Operators with denser workloads bump the env var.
  Decision: keep the default; document the trade-off in
  `docs/ui.md#otel-ring-buffer-tuning`
- Hot-Shards drill-into-keys story (US-010) reads from `audit_log`
  filtered by hash(key)%shard. This requires the audit-log to record
  `object_key` for the request — already true on PUT / GET / DELETE,
  but the Cassandra schema's `object_key` column truncates at 200
  chars. For very long keys the displayed key is the truncation.
  Decision at story-start in US-010
- Replication-lag chart range — should the UI default to the bucket's
  `ReplicationConfiguration.MaxLag` if set? AWS does not expose
  MaxLag; Strata's gauge is just the queue age. Decision: render
  threshold annotations at 1 s / 60 s / 600 s regardless of bucket
  config; operators can read the chart against their internal SLO
