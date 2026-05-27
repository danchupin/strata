---
title: 'Alerts'
weight: 22
description: 'Shipped Prometheus alert rules — label conventions, Alertmanager routing recipe, per-rule runbooks, 4-window multi-burn-rate philosophy.'
---

# Alerts

Strata ships a curated alert rule set in `deploy/prometheus/alerts.yml`.
The file declares **47 rules** — 17 SLO recording rules, 18 single-window
alert rules, and 12 multi-window burn-rate alerts (Google SRE workbook
ch.5). This page is the operator-facing companion: what each alert means,
when it fires, and what to do.

Validate the file via `make promtool-check`. CI installs `promtool` and
runs the target on every push; locally the target degrades to WARN when
the binary is missing (mirrors `make helm-lint`). Lab Prometheus
auto-loads the file via the `rule_files: [alerts.yml]` directive in
`deploy/prometheus/prometheus.yml`.

## Label conventions

Every rule carries three labels so an Alertmanager routing tree can
sort by ownership + impact + SLO without inspecting the rule name:

| Label      | Values                                | Purpose                                                                       |
|------------|---------------------------------------|-------------------------------------------------------------------------------|
| `severity` | `critical` / `warning` / `info`       | Pager grade — `critical` pages on-call, `warning` opens a ticket, `info` logs |
| `team`     | `storage`                             | Single team owns Strata today; expand if you split workers / gateway crews    |
| `slo`      | `availability` / `latency` / `durability` (omitted on non-SLO rules) | Roll-up axis — group SLO breaches in one tree branch                          |

Burn-rate alerts add two more labels for window selection:

| Label         | Values                                | Purpose                                  |
|---------------|---------------------------------------|------------------------------------------|
| `burn_window` | `5m_1h` / `30m_6h` / `6h_1d` / `1d_3d` | Window pair that triggered               |
| `burn_rate`   | `14.4` / `6` / `3` / `1`              | Multiplier — distinguishes fast vs slow burn |

## Alertmanager routing recipe

Wire the labels above into your Alertmanager routing tree so paging,
ticketing, and SLO dashboards stay decoupled:

```yaml
route:
  receiver: default-ticket
  group_by: ['alertname', 'cluster']
  routes:
    - matchers: [team="storage", severity="critical"]
      receiver: storage-pagerduty
      continue: true
    - matchers: [team="storage", severity="warning"]
      receiver: storage-jira
      continue: true
    - matchers: [team="storage", slo="availability"]
      receiver: slo-board
    - matchers: [team="storage", slo="latency"]
      receiver: slo-board
    - matchers: [team="storage", slo="durability"]
      receiver: slo-board

receivers:
  - name: storage-pagerduty
    pagerduty_configs:
      - service_key: '<PD integration key>'
  - name: storage-jira
    webhook_configs:
      - url: '<Jira webhook>'
  - name: slo-board
    webhook_configs:
      - url: '<SLO dashboard ingestion endpoint>'
  - name: default-ticket
    webhook_configs:
      - url: '<catch-all>'
```

`continue: true` on the first two routes lets the same alert reach both
the pager and the SLO board — useful when the SLO breach also
self-explains the page.

## Burn-rate alert philosophy

Single-threshold alerts (e.g. "5xx ratio > X") catch sudden breaches
but miss slow leaks. Multi-window multi-burn-rate (MWMBR) — popularized
by Google SRE workbook ch.5 — pairs a short window with a long window
and fires only when **both** windows breach the multiplier-scaled
threshold. The short window keeps latency-to-alert tight; the long
window suppresses noise from one-off blips.

Strata ships 4 window pairs per SLO so operators see the same SLO from
4 escalation angles:

| Window pair | Burn rate | Severity   | Time to budget exhaustion (30d) | Expected action |
|-------------|-----------|------------|---------------------------------|------------------------------------------------------|
| 5m + 1h     | 14.4×     | `critical` | ~2 days                         | Page on-call now — investigate within 15 min        |
| 30m + 6h    | 6×        | `critical` | ~5 days                         | Page on-call — escalate before fast-burn pair fires |
| 6h + 1d     | 3×        | `warning`  | ~10 days                        | Open ticket — schedule investigation this week      |
| 1d + 3d     | 1×        | `info`     | Tracks SLO floor exactly        | Trend-watch; ticket if pattern persists             |

12 burn-rate alerts = 4 pairs × 3 SLOs (availability, latency,
durability). Recording rules in `strata.recording` centralize SLO
targets — change `strata:slo_*:target` once and every burn-rate alert
auto-tracks the new objective.

Durability SLO note: target = 0 errors/s (zero-tolerance). Multiplier
× 0 = 0, so the literal multi-window check reduces to "error rate is
non-zero over both windows" — multi-window framing still suppresses
single-blip noise while preserving zero-tolerance semantics. Severity
escalation by window pair distinguishes steady-state pre-existing
errors (info / warning) from sudden onset (critical pages).

## Recording rules

The `strata.recording` group emits 17 instant-vector rules:

| Rule | Purpose |
|------|---------|
| `strata:slo_availability:target` | 0.999 — change once, every alert tracks |
| `strata:slo_latency_get_put_seconds:target` | 0.5 — GET/PUT p99 budget |
| `strata:slo_latency_list_seconds:target` | 2 — LIST p99 budget |
| `strata:slo_latency_multipart_complete_seconds:target` | 1 — Complete p99 budget |
| `strata:slo_durability_error_rate:target` | 0 — zero-tolerance |
| `strata:availability:ratio_rate{5m,1h,6h,1d}` | SLI ratio per window |
| `strata:latency_get_put:p99_rate{5m,1h,6h,1d}` | p99 latency per window |
| `strata:durability:error_rate{5m,1h,6h,1d}` | terminal-ack error rate per window |

30m + 3d windows are inlined directly in the burn-rate alert
expressions — kept off recording rules to bound the per-scrape compute.

## Per-rule runbooks

The runbook stubs below answer "the alert fired — now what?" in one
paragraph. Each is keyed by the lowercased alert name so the
`runbook_url` annotation on each rule resolves to the exact section
below.

### Single-window SLO alerts

#### strata5xxratehigh

**Strata5xxRateHigh** (`critical`, `slo=availability`). 5xx ratio over
5m exceeds 14.4× the SLO error budget. Check
`/admin/v1/diagnostics/recent-errors` for repeated stack signatures.
Common root causes: backend (RADOS / TiKV / Cassandra) outage, auth
middleware regression, or a single client driving an error pattern. If
it tracks one bucket, narrow via
`sum by (bucket) (rate(strata_http_requests_total{code=~"5.."}[5m]))`.

#### stratalatencyp99above500ms

**StrataLatencyP99Above500ms** (`warning`, `slo=latency`). GET/PUT p99
above 500ms over 5m. Inspect the latency dashboard's per-bucket /
per-method heatmap and the backend op-latency panel
(`strata_rados_op_duration_seconds` / `strata_cassandra_query_duration_seconds`).
Hot key on a single bucket → check `strata_cassandra_lwt_conflicts_total`
and consider bucket reshard (see `/operate/scaling`).

### Worker alerts

#### strataworkerpanic

**StrataWorkerPanic** (`critical`). A worker goroutine panicked and the
supervisor is restarting it on exponential backoff (1s→5s→30s→2m). Pull
the stack from logs: `request_id` is empty for worker spans, filter by
`worker=<label>` instead. Persistent panics block worker progress —
investigate before backoff caps out. Sibling workers + gateway
unaffected (single-worker fault isolation invariant — see CLAUDE.md
`Background workers`).

#### stratagcqueuegrowth

**StrataGCQueueGrowth** (`warning`). GC queue depth is growing — chunk
deletion can't keep up with producer rate. Two causes: (a) deletes are
failing — inspect `strata_gc_terminal_ack_total` by `reason`; (b)
producer rate is high — consider scaling `STRATA_GC_CONCURRENCY`
(parallel workers per shard) or `STRATA_GC_SHARDS` (parallel leases).

#### stratalifecycleerrorspike

**StrataLifecycleErrorSpike** (`warning`). Lifecycle worker error rate
> 0.1/s. Break down by action:
`sum by (action) (rate(strata_lifecycle_tick_total{status="error"}[5m]))`.
Common offenders: storage-class transitions hitting backend errors,
expirations racing concurrent client writes (CAS rejects are expected
— inspect distinct reasons).

#### stratanotifydlqgrowth

**StrataNotifyDLQGrowth** (`warning`). Notify worker is shipping events
to DLQ. Check sink reachability + recent DLQ contents via
`/admin/v1/notify/dlq`. Webhook 5xx / timeouts most common; inspect
`STRATA_NOTIFY_TARGETS` config + per-target health.

#### strataheartbeatstale

**StrataHeartbeatStale** (`critical`). No gateway replica has written
heartbeat in >60s. Either the meta backend is unhealthy (every
replica's heartbeat tick depends on it) or every replica has stalled.
Check `/readyz` on each replica + meta backend health (`Probe(ctx)`
log lines). Immediate page — read path is potentially down too.

### Replication alerts

#### stratareplicationqueueage

**StrataReplicationQueueAge** (`warning`, per-bucket). Oldest
`replication_queue` row in the bucket is >10m old. Peer reachability
likely degraded, or replicator throughput insufficient. Inspect peer
endpoint health (`STRATA_REPLICATOR_PEERS`) + replicator worker
iteration spans.

#### stratareplicationqueuegrowth

**StrataReplicationQueueGrowth** (`warning`, per-rule). Replication
queue depth slope > 0 over 15m. Producer (PUT traffic) exceeds
replicator consumption. Inspect bandwidth headroom + peer CPU; consider
scaling the replicator worker fleet.

### Backend alerts

#### stratacassandralwtconflictspike

**StrataCassandraLwtConflictSpike** (`warning`, per `(bucket, shard)`).
LWT conflict rate > 10/s on a single bucket-shard pair — hot key. Heat
map: `sum by (bucket, shard) (rate(strata_cassandra_lwt_conflicts_total[5m]))`.
Consider bucket reshard (see `/operate/scaling`) if the hot shard
correlates with high-traffic prefix.

#### stratacassandraqueryp99spike

**StrataCassandraQueryP99Spike** (`warning`, per `(table, op)`).
Cassandra p99 query latency > 100ms. Inspect the slow-query log
(controlled by `STRATA_CASSANDRA_SLOW_MS`, default 100ms) and the
Cassandra cluster health (compaction backlog, GC pauses, network).

#### strataradosopp99spike

**StrataRADOSOpP99Spike** (`warning`, per `(pool, op)`). RADOS p99 op
latency > 500ms. Investigate cluster OSD health, recovery state, scrub
backlog. The lab Grafana ships a per-pool latency panel — start there.

#### stratabucketstatsshardimbalance

**StrataBucketStatsShardImbalance** (`info`). One bucket_stats shard
absorbed >50% of writes over 30m. The picker should hash on a fresh
uuid per call (see CLAUDE.md `bucket_stats live counter is fan-out, not
single-key`); if you see this firing, somebody likely changed the
picker to hash on a stable key. Inspect
`strata_bucket_stats_shard_writes_total` distribution across shards.

#### strataotelringbufhorizonlow

**StrataOTelRingbufHorizonLow** (`info`). OTel ring buffer's oldest
trace age < 5min — trace browser will miss older requests under load.
Grow `STRATA_OTEL_RINGBUF_BYTES` (default 4 MiB) if you need a longer
replay horizon.

### Security alerts

#### stratabackendtlsskipverify

**StrataBackendTLSSkipVerify** (`critical`). One or more backend TLS
clients have `InsecureSkipVerify=true`. Traffic to that backend is
vulnerable to MITM. Re-issue the trusted CA bundle (`STRATA_*_TLS_CA_FILE`)
and unset `STRATA_*_TLS_SKIP_VERIFY` immediately. Cycle A gauge — see
`/best-practices/security`.

#### strataratelimitrefusalspike

**StrataRateLimitRefusalSpike** (`warning`). Ingress rate limiter is
refusing > 10 requests/s. Inspect by `reason`:
`sum by (reason) (rate(strata_ingress_rate_limit_refused_total[5m]))`.
Either a misbehaving client / DDoS or limits set too tight. Tune
`STRATA_RATE_LIMIT_PER_KEY` / `STRATA_RATE_LIMIT_PER_IP`.

#### strataauditstreamsubscriberleak

**StrataAuditStreamSubscriberLeak** (`warning`). Audit stream
broadcaster has > 50 live subscribers. Operator dashboards rarely
exceed ~5 simultaneous; this indicates leaked
`/admin/v1/audit/stream` SSE connections. Restart the gateway replica
that owns the broadcaster if rotation is impossible.

### Drain alerts

#### stratadrainprogressstalled

**StrataDrainProgressStalled** (`warning`). Rebalance worker moved
zero chunks over 15m and no drain completion event fired. Inspect
`/admin/v1/clusters/{id}/drain-progress` for stuck buckets
(`stuck_single_policy` / `stuck_no_policy`). Common fix: flip
`stuck_single_policy` buckets from `strict` to `weighted` placement
mode via the `<BulkPlacementFixDialog>` UI shortcut.

### Burn-rate alerts (4-window MWMBR)

Each burn-rate alert annotation carries an `error_budget_remaining_pct`
template — a PromQL formula operators can paste into Prometheus to
read the 30-day budget remaining as a percentage. Strata does NOT
emit this value live (it would require a 30-day range vector on every
evaluation, which gets expensive); the annotation is a recipe.

#### strataavailabilityburnratefast

**StrataAvailabilityBurnRateFast** (`critical`, 5m+1h, 14.4×). The
fastest-burn variant — at this rate the 30-day availability budget
exhausts in ~2 days. Page on-call immediately. Investigate the same
way as `Strata5xxRateHigh` (which fires at the 5m window standalone);
this alert adds the 1h confirmation to suppress single-spike noise.

#### strataavailabilityburnratemedium

**StrataAvailabilityBurnRateMedium** (`critical`, 30m+6h, 6×). Budget
exhausts in ~5 days. Page on-call before the alert escalates to the
14.4× fast-burn page. 30m window is inlined (not recorded) so the
expression is a touch longer; same investigation playbook as fast-burn.

#### strataavailabilityburnrateslow

**StrataAvailabilityBurnRateSlow** (`warning`, 6h+1d, 3×). Budget
exhausts in ~10 days at this pace. Ticket-grade — schedule a
root-cause investigation this week. Useful for catching slow
regressions a deploy may have introduced.

#### strataavailabilityburnratebaseline

**StrataAvailabilityBurnRateBaseline** (`info`, 1d+3d, 1×). Budget
consumption tracks the SLO target exactly. Trend-watch — no immediate
action. Open a ticket only if this pattern persists across multiple
budget cycles.

#### stratalatencyburnratefast

**StrataLatencyBurnRateFast** (`critical`, 5m+1h, 14.4×). GET/PUT p99
exceeds 14.4× the 500ms SLO target on both windows. Severe slowness —
page on-call. Same investigation as `StrataLatencyP99Above500ms` plus
the 1h confirmation.

#### stratalatencyburnratemedium

**StrataLatencyBurnRateMedium** (`critical`, 30m+6h, 6×). Sustained
slowness across 30m + 6h windows. 30m window inlined; same playbook.

#### stratalatencyburnrateslow

**StrataLatencyBurnRateSlow** (`warning`, 6h+1d, 3×). p99 at 3× SLO
target over 6h + 1d. Ticket-grade — investigate hot buckets, slow
backend ops, or capacity headroom.

#### stratalatencyburnratebaseline

**StrataLatencyBurnRateBaseline** (`info`, 1d+3d, 1×). p99 at SLO
target floor on both windows. Trend-watch.

#### stratadurabilityburnratefast

**StrataDurabilityBurnRateFast** (`critical`, 5m+1h). Non-ENOENT
non-OK terminal GC acks observed in both 5m + 1h windows. Chunk
deletion is failing — data could be at risk. Page on-call. Break down
by reason:
`sum by (reason) (rate(strata_gc_terminal_ack_total{reason!="enoent",reason!="ok"}[1h]))`.

#### stratadurabilityburnratemedium

**StrataDurabilityBurnRateMedium** (`critical`, 30m+6h). Errors
persist across 30m + 6h. Escalate before the 5m+1h fast-burn fires.

#### stratadurabilityburnrateslow

**StrataDurabilityBurnRateSlow** (`warning`, 6h+1d). Steady-state
errors across 6h + 1d. Ticket-grade — schedule per-reason root-cause
this week.

#### stratadurabilityburnratebaseline

**StrataDurabilityBurnRateBaseline** (`info`, 1d+3d). Long-tail trend.
Observe; investigate if the pattern survives multiple budget cycles.

## See also

- [Monitoring]({{< relref "/operate/monitoring" >}}) — Prometheus
  scrape config, dashboards, OTel wire-up.
- [Drain a cluster]({{< relref "/operate/drain-cluster" >}}) — drain
  workflow that the drain-progress alert references.
- [Scaling]({{< relref "/operate/scaling" >}}) — bucket reshard +
  worker fleet sizing pointers for the queue-growth / LWT-conflict
  alerts.
