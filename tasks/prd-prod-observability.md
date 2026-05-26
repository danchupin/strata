# PRD: Prod Observability (Cycle B — Prod-Readiness)

## Introduction

Cycle B of the 2026-05-25 prod-readiness audit closes the operator-side observability gap. Strata today emits a rich set of Prometheus metrics + structured slog logs + OTel traces, but ships **zero alert rules**, **one Grafana dashboard**, **no pprof endpoint**, and **no SLO/SLI document** — meaning an SRE team adopting Strata gets metrics without alarms, observability without dashboards, and performance signals without budget targets.

This cycle delivers:

1. **Metric instrumentation gap-fill** — 9 new metrics referenced by alerts + dashboards but not yet instrumented (heartbeat staleness, RADOS cluster object/bytes count, bucket quota gauge, TiKV pessimistic-txn counter, S3-upstream API call + throttle counters, inventory object count, worker leader-event counter).
2. **Prometheus alert rules + recording rules** — `deploy/prometheus/alerts.yml` with ~18 SLO-anchored rules anchored to recording rules so SLO target changes ripple cleanly.
3. **Burn-rate alerts** — 4-window multi-burn-rate (Google SRE workbook ch.5) for top 3 SLOs, derived from recording rules.
4. **pprof endpoint behind admin auth** — opt-in heap/CPU/goroutine/block/mutex profiling on the admin (or dedicated) listener.
5. **SLO/SLI document + `strata admin slo-report` subcommand** — published targets + Go-native subcommand that scrapes Prometheus + emits Markdown summary (NO new top-level binary per single-binary invariant).
6. **5 new Grafana dashboards** — per-worker, per-cluster, per-tenant, per-meta-backend, per-data-backend (existing `strata-dashboard.json` becomes hero drill-down anchor).
7. **Dashboard drift-lint** — extends existing `deploy/grafana/dashboard_test.go` to cross-check every PromQL expr against `internal/metrics/`.

After this cycle the SRE adopting Strata can: (a) wire pagers from shipped rules, (b) ship a weekly SLO compliance report, (c) drill from cluster-overview into specific worker/cluster/tenant boards, (d) capture flamegraphs at incident time. Default behavior unchanged — `STRATA_PPROF_*` opt-in; alert rules and dashboards are static files operators import explicitly.

## User Journey

An SRE wiring Strata observability for prod cutover:

1. **Read `/operate/slo.md`** (new page). Get the 3 published SLO targets:
   - Availability: 99.9% non-5xx over 30-day window.
   - Latency: p99 GET/PUT <500ms; LIST <2s; multipart Complete <1s.
   - Worker durability: error rate of non-ENOENT GC terminal acks zero over 90-day window (oracled via `strata_gc_terminal_ack_total{reason!="enoent"}` — always-on metric).
   Aligns internal SLO docs to Strata's published baseline.
2. **Import `deploy/prometheus/alerts.yml`** into existing Prometheus rule loader. 18 SLO-anchored rules + 12 burn-rate rules + 3 recording rules (`strata:slo_availability:target`, `strata:slo_latency:target`, `strata:slo_durability:target`) load via `promtool check rules`.
3. **Read `/operate/alerts.md`** (new page) for per-rule context: label conventions (`severity=critical|warning|info`, `team=storage`, `slo=availability|latency|durability`), routing guidance, per-alert runbook links.
4. **Import 5 new Grafana dashboards** from `deploy/grafana/dashboards/` alongside existing `strata-dashboard.json`. Hero board cross-links via Grafana `links` array (URL pattern `/d/<uid>?var-<name>=$<name>`).
5. **Optionally enable pprof** on one canary replica: `STRATA_PPROF_LISTEN=127.0.0.1:9002 STRATA_PPROF_ENABLED=true`. Captures heap/CPU on demand. Read `/operate/profiling.md` for per-profile use cases.
6. **Run `strata admin slo-report --window 7d --prometheus-url http://...` weekly** — scrapes Prometheus, computes per-SLO compliance, error-budget consumption, top error-paths; emits Markdown summary. Ship to leadership.
7. **On incident**: alert fires → operator opens matching drill-down dashboard (cluster / worker / tenant) → if perf-related, snapshots pprof from canary replica → if SLO-burning, references burn-rate alert's window (5min / 30min / 6h / 24h) to pick severity.

## Goals

- Ship 9 new metrics filling the instrumentation gap so alerts + dashboards reference only real metrics.
- Ship 18+ SLO-anchored Prometheus alert rules + 12 burn-rate rules + 3 recording rules.
- Ship pprof endpoint behind admin auth, opt-in via `STRATA_PPROF_*` envs.
- Ship SLO/SLI document with concrete PromQL formulas + `strata admin slo-report` subcommand (single-binary preserved).
- Ship 5 new Grafana dashboards + extend drift-lint to cover them all.
- No regression on existing smoke / CI / s3-tests / e2e suites.
- Default behavior unchanged — every new knob opt-in; alerts/dashboards static files.

## User Stories

### US-001: Metric instrumentation gap-fill
**Description:** As a maintainer, I want the 9 metrics referenced by upcoming alerts and dashboards instrumented so US-002..US-011 can reference only real metric names without ad-hoc inline additions.

**Acceptance Criteria:**
- [ ] **`strata_heartbeat_last_write_timestamp`** (gauge, no labels): set on every `internal/heartbeat/heartbeat.go::Heartbeater.tick` to `float64(time.Now().Unix())`. Replaces upcoming alert's brittle assumption.
- [ ] **`strata_rados_cluster_object_count`** (gauge, labels `cluster`): set on every RADOS health probe per cluster from existing `internal/data/rados/health.go::DataHealth` walk; mirrors the existing `strata_bucket_bytes` gauge pattern.
- [ ] **`strata_rados_cluster_bytes_used`** (gauge, labels `cluster`): same write site as above, sourced from `ClusterStatsProbe.GetPoolStats` aggregate.
- [ ] **`strata_bucket_quota_bytes`** (gauge, labels `bucket`): updated by the existing `internal/bucketstats/sampler.go` tick; reads `meta.Store.GetBucketQuota` per (bucket, sample) and emits `q.MaxBytes` (or 0 if unlimited).
- [ ] **`strata_tikv_pessimistic_txn_total`** (counter, labels `op, outcome` where outcome∈{commit, rollback, conflict}): bumped inside every pessimistic-txn helper in `internal/meta/tikv/` (search via `grep -rn 'pessimistic' internal/meta/tikv/`); one bump per outcome path.
- [ ] **`strata_data_s3_api_calls_total`** (counter, labels `cluster, operation, outcome` where outcome∈{success, error, throttled}): bumped per AWS SDK call inside `internal/data/s3/observer.go` via SDK middleware (alongside existing OTel instrumentation from US-003c of ralph/harden-gateway).
- [ ] **`strata_data_s3_throttled_total`** (counter, labels `cluster, operation`): bumped when SDK retry chain observes `ThrottlingException` / `SlowDown` / `RequestLimitExceeded` AWS error codes.
- [ ] **`strata_inventory_objects_total`** (gauge, labels `bucket, configID`): set on every inventory worker tick to the number of objects walked. Sourced from `internal/inventory/`.
- [ ] **`strata_worker_leader_events_total`** (counter, labels `worker, event` where event∈{acquired, released}): bumped from the existing `cmd/strata/workers.Supervisor.LeaderEvents()` channel-publisher site (currently emits to heartbeat only).
- [ ] **All new histograms (if any added later) MUST use explicit bucket spec** — for sub-second ops use `prometheus.ExponentialBuckets(0.001, 2, 16)` (1ms..32s); for byte counts use `prometheus.ExponentialBuckets(1024, 4, 10)` (1 KiB..1 GiB). This story adds no histograms but the rule is doc'd in `internal/metrics/metrics.go` package comment for future use.
- [ ] Each new metric registered in `internal/metrics/metrics.go::init` registry list (the `prometheus.MustRegister(...)` block).
- [ ] Unit test `internal/metrics/metrics_test.go` extended: new metrics appear in the `requiredMetrics` set + `Help` strings non-empty.
- [ ] Live-probe smoke: `make run-memory` + `curl http://localhost:8080/metrics | grep -cE '^strata_(heartbeat_last_write_timestamp|rados_cluster_object_count|rados_cluster_bytes_used|bucket_quota_bytes|tikv_pessimistic_txn_total|data_s3_api_calls_total|data_s3_throttled_total|inventory_objects_total|worker_leader_events_total)'` returns 9.
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: Prometheus alert rules + SLO recording rules
**Description:** As an SRE, I want shipped alert rules covering every metric in the operator-facing shortlist so I can wire pagers without writing PromQL myself. SLO target values are recording rules so changes ripple cleanly.

**Acceptance Criteria:**
- [ ] New file `deploy/prometheus/alerts.yml` with rule groups: `strata.recording`, `strata.availability`, `strata.latency`, `strata.workers`, `strata.replication`, `strata.backends`, `strata.security`, `strata.drain`.
- [ ] **Recording rules** (`strata.recording` group) — SLO targets centralized:
  ```
  - record: strata:slo_availability:target
    expr: 0.999
  - record: strata:slo_latency_get_put_seconds:target
    expr: 0.5
  - record: strata:slo_latency_list_seconds:target
    expr: 2
  - record: strata:slo_latency_multipart_complete_seconds:target
    expr: 1
  - record: strata:slo_durability_error_rate:target
    expr: 0
  - record: strata:availability:ratio_rate5m
    expr: 1 - (sum(rate(strata_http_requests_total{code=~"5..",bucket!="_admin"}[5m])) / sum(rate(strata_http_requests_total{bucket!="_admin"}[5m])))
  - record: strata:latency_get_put:p99_rate5m
    expr: histogram_quantile(0.99, sum by (le) (rate(strata_http_request_duration_seconds_bucket{method=~"GET|PUT"}[5m])))
  - record: strata:durability:error_rate5m
    expr: sum(rate(strata_gc_terminal_ack_total{reason!="enoent",reason!="ok"}[5m]))
  ```
- [ ] Each alert rule carries labels: `severity={critical|warning|info}`, `team=storage`, `slo={availability|latency|durability|<empty>}`.
- [ ] Each rule carries `annotations.summary` (one-line) + `annotations.description` (template variables) + `annotations.runbook_url` pointing at `/operate/alerts.md#<rule-id>`.
- [ ] 18 alert rules:
  - `Strata5xxRateHigh` (`(1 - strata:availability:ratio_rate5m) > (1 - strata:slo_availability:target) * 14.4` for 5min).
  - `StrataLatencyP99Above500ms` (`strata:latency_get_put:p99_rate5m > strata:slo_latency_get_put_seconds:target` for 5min).
  - `StrataWorkerPanic` (`rate(strata_worker_panic_total[5m]) > 0` for 1min) — labels carry `worker` + `shard`.
  - `StrataReplicationQueueAge` (`max(strata_replication_queue_age_seconds) > 600` for 10min) — per-bucket.
  - `StrataReplicationQueueGrowth` (`deriv(strata_replication_queue_depth[15m]) > 0` for 15min).
  - `StrataCassandraLwtConflictSpike` (`rate(strata_cassandra_lwt_conflicts_total[5m]) > 10` for 5min — per-bucket-shard).
  - `StrataGCQueueGrowth` (`deriv(strata_gc_queue_depth[15m]) > 0` for 15min).
  - `StrataLifecycleErrorSpike` (`rate(strata_lifecycle_tick_total{status="error"}[5m]) > 0.1` for 5min).
  - `StrataNotifyDLQGrowth` (`rate(strata_notify_delivery_total{status="dlq"}[10m]) > 0` for 10min).
  - `StrataCassandraQueryP99Spike` (`histogram_quantile(0.99, sum by (le, table, op) (rate(strata_cassandra_query_duration_seconds_bucket[5m]))) > 0.1` for 5min).
  - `StrataRADOSOpP99Spike` (`histogram_quantile(0.99, sum by (le, pool, op) (rate(strata_rados_op_duration_seconds_bucket[5m]))) > 0.5` for 5min).
  - `StrataOTelRingbufHorizonLow` (`strata_otel_ringbuf_oldest_age_seconds < 300` for 5min).
  - `StrataAuditStreamSubscriberLeak` (`strata_audit_stream_subscribers > 50` for 10min).
  - `StrataBackendTLSSkipVerify` (`sum(strata_backend_tls_skip_verify) > 0` for 1min — Cycle A gauge).
  - `StrataBucketStatsShardImbalance` (`max by (bucket) (strata_bucket_stats_shard_writes_total) / sum by (bucket) (strata_bucket_stats_shard_writes_total) > 0.5` for 30min).
  - `StrataDrainProgressStalled` (`changes(strata_rebalance_chunks_moved_total[15m]) == 0 and on() strata_drain_complete_total == 0` for 15min).
  - `StrataHeartbeatStale` (`time() - max(strata_heartbeat_last_write_timestamp) > 60` for 2min — uses US-001 metric).
  - `StrataRateLimitRefusalSpike` (`rate(strata_ingress_rate_limit_refused_total[5m]) > 10` for 5min — Cycle A counter).
- [ ] `promtool check rules deploy/prometheus/alerts.yml` exits 0.
- [ ] `deploy/prometheus/prometheus.yml` extended with `rule_files: [alerts.yml]` so the lab loads them automatically.
- [ ] New `make promtool-check` Makefile target wraps `promtool check rules deploy/prometheus/alerts.yml` (degrades to exit 0 + WARN when promtool missing, mirrors `make helm-lint` pattern).
- [ ] CI `lint-build` job extended to `go install github.com/prometheus/prometheus/cmd/promtool@latest` + run `make promtool-check`.
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: Burn-rate alerts (4-window multi-burn-rate)
**Description:** As an SRE, I want 4-window multi-burn-rate alerts (Google SRE workbook ch.5) for the top 3 SLOs so I can distinguish slow-burn from fast-burn budget consumption.

**Acceptance Criteria:**
- [ ] `deploy/prometheus/alerts.yml` gains a `strata.burn_rate` rule group with 12 rules (4 windows × 3 SLOs).
- [ ] **All burn-rate expressions reference recording rules from US-002** — no hard-coded SLO numerics. Example shape:
  ```
  - alert: StrataAvailabilityBurnRateCritical_5m1h
    expr: |
      (1 - strata:availability:ratio_rate5m) > (1 - strata:slo_availability:target) * 14.4
      and
      (1 - strata:availability:ratio_rate1h) > (1 - strata:slo_availability:target) * 14.4
    for: 2m
    labels: {severity: critical, team: storage, slo: availability}
  ```
- [ ] **Availability SLO**: 5m+1h (14.4× — page critical, `for: 2m`), 30m+6h (6× — page critical, `for: 15m`), 6h+1d (3× — ticket warning, `for: 1h`), 24h+3d (1× — ticket info, `for: 3h`).
- [ ] **Latency SLO**: 4-window structure using `strata:latency_get_put:p99_rate5m` recording rule + matching `_rate1h` / `_rate6h` / `_rate1d` rules added to US-002 recording rules group.
- [ ] **Durability SLO**: 4-window structure using `strata:durability:error_rate5m` + matching rules. Per-bucket alert labels carry `bucket` for routing.
- [ ] Each alert has `annotations.error_budget_remaining_pct` template using PromQL `(1 - (...consumed.../...budget...))`.
- [ ] `promtool check rules` exits 0.
- [ ] New doc page `docs/site/content/operate/alerts.md` created in this story (NOT US-011 — own page per author). Body: label conventions section + routing guidance + per-rule one-paragraph runbook stub + "Burn-rate alert philosophy" section with table mapping window pair → severity → expected operator action.
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: pprof endpoint behind admin auth + /operate/profiling.md
**Description:** As an SRE responding to a perf incident, I want a pprof endpoint protected by admin auth so I can capture heap/CPU/goroutine profiles on demand without exposing them to S3 clients.

**Acceptance Criteria:**
- [ ] `internal/serverapp/serverapp.go` registers `/debug/pprof/*` handlers (heap, cpu, goroutine, block, mutex, profile, trace, allocs) via explicit `pprof.Index` / `pprof.Cmdline` / `pprof.Profile` / `pprof.Symbol` / `pprof.Trace` + `pprof.Handler("heap")` etc. wires (NOT side-effect import of `net/http/pprof` — Strata does not use DefaultServeMux).
- [ ] `STRATA_PPROF_ENABLED` env (bool, default `false` — opt-in even when admin listener set; defense-in-depth, pprof exposes heap which may contain sensitive data in error paths).
- [ ] `STRATA_PPROF_LISTEN` env (default empty = use admin listener if set, else fail-fast at boot if pprof enabled but neither admin nor pprof listener configured). When set (e.g. `127.0.0.1:9002`), pprof gets its OWN listener (third listener after main + admin).
- [ ] pprof handlers protected by the existing `auth.MultiStore`-backed admin auth chain when on admin listener; when on dedicated pprof listener, same admin auth chain re-used via shared middleware instance.
- [ ] Block + mutex profiling REQUIRE explicit `runtime.SetBlockProfileRate(N)` / `runtime.SetMutexProfileFraction(N)` calls — gated by `STRATA_PPROF_BLOCK_RATE` (default 0 = disabled) + `STRATA_PPROF_MUTEX_RATE` (default 0 = disabled) envs. Documented per-profile in `/operate/profiling.md`.
- [ ] All 4 envs wired through `Config.Pprof.*` substruct + `envMap` + TOML example `[pprof]` section + reference page (per ralph/toml-parity contract).
- [ ] Unit test `internal/serverapp/pprof_test.go`: (a) disabled = `/debug/pprof/heap` returns 404 on every listener, (b) enabled on admin listener = returns valid pprof bytes (decoded via `github.com/google/pprof/profile.Parse` — NOT `golang.org/x/perf/pprof` which doesn't exist), (c) enabled on dedicated listener = returns pprof bytes; admin listener 404, (d) admin auth required on dedicated pprof listener.
- [ ] **`github.com/google/pprof` added to `go.mod` direct require** (BSD-3-Clause, ~10k LOC vetted Google-owned). Already transitive via Go stdlib runtime/pprof — verify with `go mod why`; if transitive only, promote to direct require.
- [ ] New doc page `docs/site/content/operate/profiling.md`: per-profile coverage (heap=leak diag, cpu=hot-path, goroutine=leak/deadlock, block=lock contention, mutex=mutex contention, trace=scheduling), `go tool pprof` recipes, flamegraph workflow via `go tool pprof -http :7070 heap.pprof`. Recommends operator-side dev mode (`STRATA_PPROF_SKIP_TOOL_CHECK=1` documented for environments without `go` binary on PATH; gates tool-presence checks in `scripts/smoke-pprof.sh`).
- [ ] Smoke script `scripts/smoke-pprof.sh` boots Strata with pprof enabled, curls `/debug/pprof/heap`, decodes response via Go-native test binary `internal/serverapp/pprof_decode_test.go` (compiled once, no `go tool pprof` runtime dep).
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: SLO/SLI doc + strata admin slo-report subcommand + /operate/slo.md
**Description:** As leadership / SRE / customer, I want a published SLO baseline so I can align tenant SLAs and ship weekly compliance reports. As a maintainer, I want the report tool to live inside the single `strata` binary per the single-binary invariant.

**Acceptance Criteria:**
- [ ] New doc page `docs/site/content/operate/slo.md` defines:
  - **Availability SLO**: 99.9% non-5xx over 30-day window. SLI formula references `strata:availability:ratio_rate5m` recording rule from US-002.
  - **Latency SLO**: p99 GET/PUT <500ms; p99 LIST <2s; p99 multipart Complete <1s. SLI formulas reference matching recording rules.
  - **Worker durability SLO**: error rate of non-ENOENT-non-OK GC terminal acks zero over 90-day window. SLI formula uses `strata:durability:error_rate5m` recording rule (oracled via `strata_gc_terminal_ack_total{reason!="enoent",reason!="ok"}` — always-on, no inventory dependency).
  - **Error budget calculation**: per-SLO budget formula + how to consume it via burn-rate alerts from US-003.
  - **SLO targets are starting points** — operators tune per tenant SLA; document explicitly. Pointer to recording rules from US-002 so target changes ripple via one file edit.
  - Cross-links to `/operate/alerts.md` burn-rate section + `/operate/monitoring.md` metric definitions.
- [ ] **NEW `strata admin slo-report` subcommand** at `cmd/strata/admin/slo_report.go` (PER project CLAUDE.md `Single-binary invariant` — NO new top-level binary, NO bash script with curl+jq deps).
- [ ] Subcommand flags: `--prometheus-url <url>` (default `http://localhost:9090`), `--window <7d|30d|90d>` (default `7d`), `--out <file.md>` (default stdout), `--format <markdown|json>` (default `markdown`).
- [ ] Subcommand uses `github.com/prometheus/client_golang/api/prometheus/v1` (already a transitive dep via Strata's Prometheus client export — verify + promote to direct require if needed) to query: (a) recording rules from US-002 over window, (b) `topk(5, sum by (path) (rate(strata_http_requests_total{code=~"5.."}[<window>])))`, (c) `topk(5, sum by (path) (rate(strata_http_request_duration_seconds_count{path!=""}[<window>])))` joined with bucket histogram for slow paths, (d) burn-rate alert firings count over window via Alertmanager API IF `--alertmanager-url` flag provided, else 0.
- [ ] Emits Markdown table: per-SLO row (target, actual, status emoji ✅/⚠️/🔥), top-5 5xx paths, top-5 slow paths, burn-rate firings count.
- [ ] New `make slo-report` Makefile target wraps `bin/strata admin slo-report` with default flags.
- [ ] Subcommand integration test `cmd/strata/admin/slo_report_test.go`: mocks Prometheus HTTP API with httptest.NewServer, asserts the Markdown output contains the expected SLO rows.
- [ ] Smoke: `make up-all && make slo-report` produces non-empty markdown referencing all 3 SLOs.
- [ ] `/operate/_index.md` card grid extended with new entry pointing at `/operate/slo.md`.
- [ ] Typecheck passes
- [ ] Tests pass

### US-006: Per-worker Grafana dashboard
**Description:** As an SRE, I want a per-worker Grafana dashboard so I can drill into a specific worker's iteration count, error rate, panic rate, lease churn, per-tick duration, and queue depth.

**Acceptance Criteria:**
- [ ] New dashboard `deploy/grafana/dashboards/strata-workers.json` with template variable `worker` ∈ {gc, lifecycle, replicator, notify, access-log, inventory, audit-export, manifest-rewriter, quota-reconcile, usage-rollup, rebalance, reshard}.
- [ ] **`schemaVersion: 39`** (Grafana 10 baseline) — pin explicit value so drift-lint (US-010) catches accidental downgrade.
- [ ] Per-tab panels: (a) iteration count (`rate(worker.<name>.tick_total[5m])`), (b) error rate, (c) panic rate (`rate(strata_worker_panic_total{worker="$worker"}[5m])`), (d) leader-lease churn (`rate(strata_worker_leader_events_total{worker="$worker"}[5m])` — from US-001 metric), (e) per-tick duration histogram (heatmap), (f) queue depth where applicable (gc: `strata_gc_queue_depth`, replicator: `strata_replication_queue_depth`).
- [ ] Dashboard `uid: strata-workers`. Tags `["strata", "workers"]`. Time range default 1h.
- [ ] Dashboard provisioned via existing `deploy/grafana/dashboard.yaml` provider (auto-loaded by lab Grafana — provider already walks `/var/lib/grafana/dashboards/`).
- [ ] **Hero board `strata-dashboard.json` gains a drill-down link via Grafana panel `links` array**: URL pattern `/d/strata-workers?var-worker=$worker` — uses Grafana variable substitution so click on a worker name in hero opens this board with the variable pre-filled.
- [ ] All PromQL exprs reference real metric names (cross-checked in US-010 drift-lint).
- [ ] Template variable `worker` uses `refresh: "On Time Range Change"` (NOT `"On Dashboard Load"`) — bounded query cost.
- [ ] Typecheck passes
- [ ] Tests pass

### US-007: Per-cluster Grafana dashboard
**Description:** As an SRE, I want a per-cluster Grafana dashboard so I can see chunks/bytes used per RADOS or S3 cluster, drain progress, rebalance moves, refusals, TLS skip-verify gauge.

**Acceptance Criteria:**
- [ ] New dashboard `deploy/grafana/dashboards/strata-clusters.json` with template variable `cluster` ∈ (all configured cluster IDs).
- [ ] **`schemaVersion: 39`** pinned.
- [ ] Per-tab panels: (a) chunks_used (`strata_rados_cluster_object_count{cluster="$cluster"}` — from US-001), (b) bytes_used (`strata_rados_cluster_bytes_used{cluster="$cluster"}` — from US-001), (c) drain progress (3-line plot: migratable / stuck_single_policy / stuck_no_policy — verified existing metrics `strata_rebalance_migratable_chunks_total` / `_stuck_single_policy_chunks_total` / `_stuck_no_policy_chunks_total` via grep in US-001 verification scope; if missing, fold into US-001 instrumentation), (d) rebalance moves in/out (`rate(strata_rebalance_chunks_moved_total{from="$cluster"}[5m])` vs `{to="$cluster"}`), (e) refusals (`rate(strata_rebalance_refused_total{target="$cluster"}[5m])` + `rate(strata_putchunks_refused_total{cluster="$cluster"}[5m])`), (f) TLS skip-verify gauge (`strata_backend_tls_skip_verify{cluster="$cluster"}`).
- [ ] Dashboard `uid: strata-clusters`. Hero drill-down link via `links` array URL pattern `/d/strata-clusters?var-cluster=$cluster`.
- [ ] Template variable `cluster` uses `refresh: "On Time Range Change"`.
- [ ] Typecheck passes
- [ ] Tests pass

### US-008: Per-tenant Grafana dashboard
**Description:** As an SRE, I want a per-tenant (access_key + bucket) Grafana dashboard so I can see request rate, error rate, bytes throughput, quota utilization, rate-limit refusals per customer.

**Acceptance Criteria:**
- [ ] New dashboard `deploy/grafana/dashboards/strata-tenants.json` with template variables `access_key` + `bucket` (multi-select).
- [ ] **`schemaVersion: 39`** pinned.
- [ ] Per-row panels: (a) request rate (`sum by (method) (rate(strata_http_requests_total{access_key="$access_key",bucket="$bucket"}[5m]))` — verified `access_key` + `bucket` labels exist), (b) error rate by code class, (c) bytes uploaded/downloaded (gauge from `strata_bucket_bytes{bucket="$bucket"}` + per-request size histogram), (d) quota utilization (`strata_bucket_bytes{bucket="$bucket"} / strata_bucket_quota_bytes{bucket="$bucket"}` — both from existing + US-001), (e) rate-limit refusals (`rate(strata_ingress_rate_limit_refused_total{reason=~"key|ip"}[5m])` — Cycle A counter).
- [ ] Dashboard `uid: strata-tenants`. Hero drill-down link.
- [ ] **Cardinality bound**: template variables use `query_result()` with `topk(50, ...)` to avoid blowing up the picker on 10k+ buckets. `refresh: "On Time Range Change"` so re-query cost only triggers on time-range change, not page load.
- [ ] Typecheck passes
- [ ] Tests pass

### US-009: Per-meta-backend + per-data-backend Grafana dashboards
**Description:** As an SRE, I want per-meta-backend + per-data-backend Grafana dashboards so I can isolate Cassandra-vs-TiKV health and RADOS-vs-S3-upstream throughput.

**Acceptance Criteria:**
- [ ] New dashboard `deploy/grafana/dashboards/strata-meta-backends.json`:
  - Cassandra row: LWT conflicts heatmap (`sum by (bucket, shard) (rate(strata_cassandra_lwt_conflicts_total[5m]))`), query duration p50/p99 per table (`histogram_quantile(0.5|0.99, sum by (le, table) (rate(strata_cassandra_query_duration_seconds_bucket[5m])))`).
  - TiKV row: pessimistic-txn churn per op (`rate(strata_tikv_pessimistic_txn_total[5m])` — from US-001), sweeper rate (`rate(strata_meta_tikv_audit_sweep_deleted_total[5m])` — existing).
  - Memory row (lab only): row count via `strata_meta_memory_rows_total` if instrumented; else blank panel + note.
- [ ] New dashboard `deploy/grafana/dashboards/strata-data-backends.json`:
  - RADOS row: op latency p50/p99 per pool (`histogram_quantile(0.5|0.99, sum by (le, pool) (rate(strata_rados_op_duration_seconds_bucket[5m])))`), put/get/del throughput.
  - S3-upstream row: API call rate per cluster (`rate(strata_data_s3_api_calls_total{outcome="success"}[5m])` — from US-001), throttling (`rate(strata_data_s3_throttled_total[5m])` — from US-001), error rate (`rate(strata_data_s3_api_calls_total{outcome="error"}[5m])`).
  - Memory row (lab only): RSS proxy from `process_resident_memory_bytes` (Prom built-in).
- [ ] **`schemaVersion: 39`** pinned on both.
- [ ] Both dashboards `uid: strata-meta-backends` / `strata-data-backends`. Hero drill-down links via `links` array.
- [ ] Typecheck passes
- [ ] Tests pass

### US-010: Dashboard drift-lint Go test extension
**Description:** As a maintainer, I want the existing dashboard drift-lint test extended to cover all 6 dashboards (1 hero + 5 new) so future refactors don't silently break dashboards.

**Acceptance Criteria:**
- [ ] Extend existing `deploy/grafana/dashboard_test.go` to walk every `*.json` file under `deploy/grafana/` AND `deploy/grafana/dashboards/`.
- [ ] Per dashboard: parse JSON → walk every `panel.targets[].expr` (including nested panels via `panel.panels[]`) → extract metric names via regex `strata_[a-z_][a-z0-9_]*`.
- [ ] Cross-check each extracted metric against the set of metric names exported by `internal/metrics/` (walk via `go/parser` + `go/ast` on `internal/metrics/metrics.go` looking for `Name: "<metric>"` literals inside `prometheus.<Counter|Histogram|Gauge>Opts{}` or `prometheus.New<...>Vec` calls).
- [ ] Hard-fail with the dashboard path + panel ID + missing metric name.
- [ ] Assert each dashboard has `schemaVersion >= 39` (Grafana 10 baseline pinned in US-006..US-009).
- [ ] Exempt list for non-Strata metrics: `process_*`, `go_*`, `prometheus_*`, `node_*`.
- [ ] Worker iteration counters (`worker.<name>.tick_total` family from US-006 panel expr — but these are OTel-instrumented spans, not Prom metrics) explicitly exempted with a comment block — or fold into US-001 instrumentation as Prom counter `strata_worker_iteration_total{worker}` if simpler (Ralph decides at impl time; document the decision in commit message).
- [ ] Test runs under `go test ./deploy/grafana/...` (no new build tag).
- [ ] Test currently passes (all dashboards from US-006..US-009 reference real metrics post-US-001).
- [ ] Typecheck passes
- [ ] Tests pass

### US-011: Smoke + docs + ROADMAP create + flip Done
**Description:** As an operator, I want end-to-end smoke validation that all features compose cleanly, plus the cycle ROADMAP entry created and flipped Done.

**Acceptance Criteria:**
- [ ] New script `scripts/smoke-observability.sh`: (a) `promtool check rules deploy/prometheus/alerts.yml` exits 0 (skip with WARN when promtool missing), (b) starts Strata with `STRATA_PPROF_ENABLED=true STRATA_PPROF_LISTEN=127.0.0.1:19002 STRATA_ADMIN_LISTEN=127.0.0.1:19001`, (c) curls `/debug/pprof/heap` from loopback with admin auth → asserts response decodes via Go-native helper from US-004, (d) `go test ./deploy/grafana/...` passes (dashboard drift-lint green), (e) `bin/strata admin slo-report --window 7d --prometheus-url http://localhost:9090 --out /tmp/slo-report.md` produces non-empty output against the bare lab (Prometheus running via `make up-all`).
- [ ] New `make smoke-observability` Makefile target wraps the smoke script.
- [ ] Hero board `deploy/grafana/strata-dashboard.json` updated with 4 drill-down `links` entries pointing at the new dashboards (one per US-006..US-009 dashboard UID — final integration polish; per-story added panel-level links, this finalizes top-of-dashboard links).
- [ ] `/operate/_index.md` card grid extended with entries for `slo.md`, `alerts.md`, `profiling.md` (each created in its owning story US-005/US-003/US-004 — this just adds card grid entries).
- [ ] `/reference/env-vars.md` extended with rows for `STRATA_PPROF_*` (4 new entries) + TOML-key column populated.
- [ ] `deploy/strata.toml.example` extended with `[pprof]` section.
- [ ] Drift-lint test `internal/config/env_toml_parity_test.go` green (new pprof envs accounted for).
- [ ] **ROADMAP entry CREATED** in `ROADMAP.md` under `## Correctness & consistency` in US-001 prep commit: `- **P0/P1 — Cycle B: prod-observability (metric gap-fill + alert rules + burn-rate alerts + pprof endpoint + SLO/SLI doc + 5 new Grafana dashboards + dashboard drift-lint).** In progress on ralph/prod-observability. Closes 4 observability gaps from 2026-05-25 audit. Flipped Done on cycle close (US-011).`
- [ ] **ROADMAP entry FLIPPED to Done** in US-011 closing commit: `~~**P0/P1 — Cycle B: prod-observability ...**~~ — **Done.** Shipped via ralph/prod-observability cycle (US-001..US-011). 9 new metrics. 18 SLO alerts + 12 burn-rate alerts + 3 SLO recording rules. pprof opt-in via STRATA_PPROF_*. SLO/SLI doc + strata admin slo-report subcommand (single-binary preserved). 5 new dashboards (workers/clusters/tenants/meta-backends/data-backends). Drift-lint test extended to all 6 dashboards. (commit <SHA>)`
- [ ] ALL existing smokes still pass.
- [ ] All existing CI jobs still green.
- [ ] Delete `tasks/prd-prod-observability.md` in closing commit per PRD lifecycle.
- [ ] Typecheck + `make test-race` + `make docs-build` pass.

## Functional Requirements

### Metric instrumentation (US-001)
- FR-1: 9 new metrics instrumented at their write sites: `strata_heartbeat_last_write_timestamp`, `strata_rados_cluster_object_count`, `strata_rados_cluster_bytes_used`, `strata_bucket_quota_bytes`, `strata_tikv_pessimistic_txn_total`, `strata_data_s3_api_calls_total`, `strata_data_s3_throttled_total`, `strata_inventory_objects_total`, `strata_worker_leader_events_total`.
- FR-2: All histograms added in this or future cycles MUST use explicit bucket spec per `internal/metrics/metrics.go` package comment.
- FR-3: Live-probe smoke verifies all 9 metrics appear on `/metrics` of `make run-memory`.

### Alert rules + recording rules (US-002 + US-003)
- FR-4: `deploy/prometheus/alerts.yml` shipped with 3 rule groups (`strata.recording`, `strata.<domain>`, `strata.burn_rate`).
- FR-5: 8 recording rules for SLO targets + per-window SLI ratios (availability, latency, durability × 5m/1h/6h/1d windows).
- FR-6: 18 SLO-anchored alert rules — all expressions reference recording rules for SLO numerics (no hard-coded constants).
- FR-7: 12 burn-rate rules (4 windows × 3 SLOs).
- FR-8: `promtool check rules` exits 0; CI `lint-build` job installs promtool + runs `make promtool-check`.

### pprof (US-004)
- FR-9: pprof handlers wired explicitly (no DefaultServeMux side-effect import).
- FR-10: `STRATA_PPROF_ENABLED` default `false`; opt-in even with admin listener present.
- FR-11: `STRATA_PPROF_LISTEN` default empty → use admin listener if set; explicit non-empty value gets dedicated listener.
- FR-12: All pprof handlers gated by admin auth chain on any listener.
- FR-13: Block + mutex profiling gated by `STRATA_PPROF_BLOCK_RATE` + `STRATA_PPROF_MUTEX_RATE` (default 0 = disabled).
- FR-14: pprof decode in tests uses `github.com/google/pprof/profile.Parse` (NOT `golang.org/x/perf/pprof` — doesn't exist).

### SLO + slo-report (US-005)
- FR-15: `/operate/slo.md` defines 3 SLOs with concrete PromQL formulas referencing recording rules.
- FR-16: Worker durability SLO oracled via `strata_gc_terminal_ack_total{reason!="enoent",reason!="ok"}` — always-on, no inventory dependency.
- FR-17: `strata admin slo-report` subcommand at `cmd/strata/admin/slo_report.go` — single-binary invariant preserved (NO new top-level binary, NO bash+curl+jq script).
- FR-18: Subcommand uses official `github.com/prometheus/client_golang/api/prometheus/v1` HTTP client.
- FR-19: Subcommand emits Markdown table with per-SLO compliance, error budget remaining, top-5 5xx paths, top-5 slow paths, optional burn-rate alert firings count.

### Dashboards (US-006..US-009)
- FR-20: 5 new dashboards under `deploy/grafana/dashboards/` — workers / clusters / tenants / meta-backends / data-backends.
- FR-21: All dashboards pin `schemaVersion: 39` (Grafana 10 baseline).
- FR-22: All dashboards auto-loaded via existing `deploy/grafana/dashboard.yaml` provider.
- FR-23: Hero `strata-dashboard.json` gains panel-level + top-of-dashboard `links` entries pointing at each new dashboard via URL pattern `/d/<uid>?var-<name>=$<name>`.
- FR-24: Template variables use `topk(50, ...)` + `refresh: "On Time Range Change"` for high-cardinality dimensions.

### Drift-lint (US-010)
- FR-25: `deploy/grafana/dashboard_test.go` extended to walk every dashboard JSON in both `deploy/grafana/` AND `deploy/grafana/dashboards/`.
- FR-26: PromQL expr metric names cross-checked vs `internal/metrics/metrics.go` exports (parsed via `go/ast`).
- FR-27: Hard-fail with dashboard + panel + missing-metric on mismatch (excluding standard Prom/node exemptions).
- FR-28: Each dashboard `schemaVersion >= 39` asserted.

### Smoke + docs (US-011)
- FR-29: `scripts/smoke-observability.sh` exercises all 7 features end-to-end (instrumentation + alerts + burn-rate + pprof + slo-report + dashboards + drift-lint).
- FR-30: 3 new doc pages shipped: `/operate/slo.md` (US-005), `/operate/alerts.md` (US-003), `/operate/profiling.md` (US-004). Each story owns its page; US-011 only adds card-grid entries to `_index.md`.
- FR-31: ROADMAP entry created in US-001 cycle-prep + flipped Done in US-011 closing commit with SHA backfill.

## Non-Goals (Out of Scope)

- **Alertmanager config + routing tree.** Rules only; operators have their own Alertmanager setups.
- **Continuous profiling integration (Pyroscope / Polar Signals).** pprof endpoint enables ad-hoc capture; continuous profiling deferred.
- **Helm chart Prometheus rule wiring.** Needs ServiceMonitor extension + Prometheus-operator integration; Cycle D.
- **Distributed tracing dashboards.** OTel ringbuf already serves recent-trace browser; Jaeger/Tempo dashboards out of scope.
- **Real-time SLO compliance UI in Web Console.** Deferred to Web UI follow-up cycle.
- **govulncheck / dependabot / CVE scanning.** Cycle C.
- **PDB / NetworkPolicy / HPA.** Cycle D.
- **Chaos / fuzz tests.** Cycle F.
- **Audit-log WORM compliance sink.** Cycle J.
- **Always-on inventory.** Deferred to Cycle G (restore-drill-automation); worker-durability SLO oracled via GC-terminal-ack avoids the dependency in the meantime.

## Design Considerations

- **Opt-in default discipline preserved.** `STRATA_PPROF_*` envs default to off; alert rules and dashboards are static files operators import explicitly. No behavior change without explicit op-side action.
- **SLO targets centralized in recording rules.** Operators tune by editing one rule file location; alerts auto-track.
- **SLO targets are starting points.** Document explicitly that 99.9% / p99 500ms is baseline; operators tune per tenant SLA.
- **Label conventions consistent across alerts.** `severity={critical|warning|info}`, `team=storage`, `slo={availability|latency|durability|<empty>}`. Operators wire Alertmanager routing tree against these labels.
- **Burn-rate windows from Google SRE workbook.** 4-window (5m+1h, 30m+6h, 6h+1d, 24h+3d) shape is industry-standard.
- **Dashboards cross-link to drill-downs via Grafana links array.** Click on a worker name in hero opens per-worker board with `$worker` pre-filled via URL variable.
- **Dashboard cardinality bounded.** Template variables `topk(50, ...)` + `refresh: "On Time Range Change"`.
- **Drift-lint as Go test extension.** Consistent with existing `dashboard_test.go` shape; runs under `go test ./...` — no new toolchain dependency.
- **Single-binary invariant preserved.** `strata admin slo-report` subcommand, NOT new top-level binary, NOT bash+curl+jq script.
- **pprof handlers wired explicitly.** Strata does not use `http.DefaultServeMux`; pprof handlers must be wired via `pprof.Index` / `pprof.Cmdline` / etc. on the Strata-owned mux.

## Technical Considerations

- **`net/http/pprof` side-effect import** registers handlers on `http.DefaultServeMux`. Strata does NOT use DefaultServeMux; pprof handlers must be wired explicitly.
- **`runtime/pprof` block + mutex profiling perf cost.** Per Go runtime docs, block profile at rate=1 samples every event (highest overhead); rate=10000 samples 1 in 10k. Default disabled (0) avoids cost; documented in `/operate/profiling.md`.
- **`promtool` binary** required for CI `make promtool-check`. CI installs via `go install github.com/prometheus/prometheus/cmd/promtool@latest`; `make promtool-check` degrades to WARN + exit 0 when missing (mirrors `make helm-lint` pattern).
- **`github.com/google/pprof/profile.Parse`** used in pprof decode tests (NOT `golang.org/x/perf/pprof` which doesn't exist). Add as direct require in `go.mod` if not already transitive.
- **`github.com/prometheus/client_golang/api/prometheus/v1`** for `strata admin slo-report` Prometheus HTTP client. Already transitive via Strata's Prometheus client export — verify via `go mod why` and promote to direct require if needed.
- **Grafana JSON schema version 39** = Grafana 10 baseline; widely deployed; assert via drift-lint.
- **Burn-rate alert PromQL `for:`** matches longer window per Google SRE workbook: 5m+1h pair uses `for: 2m`; 30m+6h uses `for: 15m`; 6h+1d uses `for: 1h`; 24h+3d uses `for: 3h`.
- **SLO report scraping** uses Prometheus HTTP API. Default `http://localhost:9090`; operator overrides for prod.
- **Histogram bucket boundaries** for new histograms: `prometheus.ExponentialBuckets(0.001, 2, 16)` for sub-second ops; `prometheus.ExponentialBuckets(1024, 4, 10)` for byte counts. Documented in `internal/metrics/metrics.go` package comment.

## Success Metrics

- All 11 user stories complete (`passes=true` in `scripts/ralph/prd.json`).
- ROADMAP `Cycle B: prod-observability` entry created in US-001 + flipped Done in US-011 with SHA backfill.
- `make smoke-observability` green on the TiKV-default lab.
- All existing smokes + CI jobs still green (no regression).
- `make docs-build` green; 3 new doc pages render.
- `make promtool-check` green; 30+ rules (8 recording + 18 alerting + 12 burn-rate) load via promtool.
- `go test ./deploy/grafana/...` green; drift-lint passes for all 6 dashboards.
- `bin/strata admin slo-report` produces non-empty output against the lab.
- 9 new metrics visible on `/metrics`.
- An SRE following `/operate/slo.md` + `/operate/alerts.md` can wire pagers + ship weekly SLO reports from zero in ≤1 hour.

## Open Questions

- **Per-rule runbook depth.** US-003 `/operate/alerts.md` ships per-rule one-paragraph stubs. Full multi-page runbooks per alert deferred.
- **`worker.<name>.tick_total`** — OTel-instrumented spans or Prom counter? US-010 drift-lint will surface the mismatch; Ralph decides at impl time. Recommend Prom counter `strata_worker_iteration_total{worker}` folded into US-001 if grep shows no existing Prom counter.
- **Dashboard auto-export to grafana.com.** Manual import only this cycle. Future enhancement: GitHub Action that publishes dashboards to grafana.com on every merge to main.
- **pprof token-based auth** as alternative to admin-auth-chain. Deferred — admin auth is the canonical control plane already.
- **Worker durability SLO 90-day window** — recording rules use 5m base window. Long-window aggregation (90d) needs Prometheus `count_over_time` + retention policy ≥90 days. Document recommended Prometheus retention setting in `/operate/slo.md`.
