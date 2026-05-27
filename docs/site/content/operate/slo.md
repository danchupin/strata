---
title: 'SLO / SLI'
weight: 23
description: 'Strata service-level objectives — availability / latency / durability baselines, SLI formulas, weekly reporting workflow.'
---

# SLO / SLI

Strata ships three production SLOs as a starting point. Operators can
re-tune each target by editing one line in
`deploy/prometheus/alerts.yml` — every single-window alert and
multi-burn-rate alert from
[/operate/alerts]({{< relref "/operate/alerts" >}}) references the same
recording rule so the change ripples cleanly.

| SLO          | Target | Window  | SLI source                                                      |
|--------------|-------:|---------|-----------------------------------------------------------------|
| Availability | 99.9%  | 30 days | `strata:availability:ratio_rate5m` (recording rule)             |
| Latency p99  | GET/PUT < 500 ms, LIST < 2 s, multipart Complete < 1 s | 30 days | `strata:latency_get_put:p99_rate5m` + per-op equivalents        |
| Durability   | 0 non-OK terminal GC acks | 90 days | `strata:durability:error_rate5m` (always-on; no inventory dep)  |

## Availability

> 99.9% of S3 requests over a rolling 30-day window return a non-5xx
> status. Admin paths (`bucket="_admin"`) excluded — admin error rate is
> tracked separately under the per-tenant dashboard.

SLI formula (verbatim from the `strata.recording` group):

```promql
strata:availability:ratio_rate5m =
  1 - (
    sum(rate(strata_http_requests_total{code=~"5..",bucket!="_admin"}[5m]))
    /
    clamp_min(sum(rate(strata_http_requests_total{bucket!="_admin"}[5m])), 1)
  )
```

Target rule: `strata:slo_availability:target = vector(0.999)`. Burn-rate
alerts compare 5m / 1h / 6h / 1d windows against the same target — see
[Burn-rate philosophy]({{< relref "/operate/alerts#burn-rate-alert-philosophy" >}}).

**Why 99.9%.** Three nines is the standard cloud-object-store starting
point — leaves ~43m monthly error budget which is enough headroom to
absorb a single-replica restart without paging. Tenants with stricter
SLAs (four nines + customer-side retry budget) can tighten the target by
editing the recording rule; alerts auto-track.

## Latency

> p99 GET + PUT under 500 ms, LIST under 2 s, multipart Complete under
> 1 s, measured at the gateway boundary across all S3 traffic.

SLI formulas:

```promql
strata:latency_get_put:p99_rate5m =
  histogram_quantile(
    0.99,
    sum by (le) (rate(strata_http_request_duration_seconds_bucket{method=~"GET|PUT"}[5m]))
  )
```

Target rules:

```promql
strata:slo_latency_get_put_seconds:target          = vector(0.5)
strata:slo_latency_list_seconds:target             = vector(2)
strata:slo_latency_multipart_complete_seconds:target = vector(1)
```

The `StrataLatencyP99Above500ms` alert (US-002) pages when the 5m
recording rule crosses the GET/PUT target. Burn-rate alerts (US-003) page
on sustained breaches over 4 window pairs.

**Why these numbers.** GET/PUT p99 < 500 ms matches RGW's published
median latency budget on equivalent hardware and is a no-regression
ceiling for the S3-test suite. LIST p99 < 2 s is the customer-visible
bound on browser-driven file pickers. Multipart Complete < 1 s is the
finalise-coordination ceiling — 4 MiB chunk fan-in plus an LWT commit.

## Durability

> Zero non-ENOENT, non-OK terminal GC acks over a rolling 90-day window.

SLI formula:

```promql
strata:durability:error_rate5m =
  sum(rate(strata_gc_terminal_ack_total{reason!="enoent",reason!="ok"}[5m]))
```

Target rule: `strata:slo_durability_error_rate:target = vector(0)`.

**Why `gc_terminal_ack_total` is the oracle.** Every chunk delete that
the GC worker attempts emits exactly one terminal ack with a `reason`
label. `ok` = chunk deleted cleanly. `enoent` = chunk already gone
(idempotent re-run, not a durability incident). Anything else
(`backend_error`, `cas_lost`, `unknown_error`, …) means a chunk that
SHOULD be live got touched in a way the worker could not reconcile —
the canonical durability-loss signal. The metric is always-on, costs
nothing to scrape, and does NOT depend on the operator configuring an
inventory worker.

**Zero-tolerance semantics + burn-rate.** Target = 0 means any non-zero
error rate is a budget breach. The single-window alert
(`StrataDurabilityErrorRateNonZero` in US-002) fires immediately;
burn-rate alerts (US-003) escalate severity by window pair —
short+fast = critical/page, long+slow = info/trend-watch — so steady
low-grade errors flow into ticket queues while sudden onset pages
on-call. See the burn-rate philosophy section for the full table.

**Why 90 days.** 30-day windows miss slow background corruption that
only surfaces on long-tail GC scans. 90 days matches the standard
backup-restore drill cadence + lifecycle expiration horizon. Keep Prom
TSDB retention at least 90 days for this to compute (see below).

## Prometheus retention

The 90-day durability SLO requires `--storage.tsdb.retention.time=90d`
minimum on the Prometheus instance that scrapes Strata. The lab
`deploy/prometheus/prometheus.yml` ships with the default retention
(15 days) — fine for the 30-day availability + latency SLOs, too short
for durability long-window aggregation. Override in production:

```
--storage.tsdb.retention.time=90d
--storage.tsdb.retention.size=200GB   # back-pressure cap
```

## Weekly compliance report

`strata admin slo-report` queries Prometheus, renders a markdown table
of each SLO + top-5 5xx paths + top-5 slow paths, and (optionally)
counts burn-rate alert firings over the window. The subcommand lives
inside the single `strata` binary per the single-binary invariant —
no bash + curl + jq shell script to maintain.

```bash
# 7-day report against the local lab.
strata admin slo-report --window 7d --prometheus-url http://localhost:9090

# 30-day report joined with Alertmanager firings, JSON for downstream tooling.
strata admin slo-report \
  --window 30d \
  --prometheus-url http://prometheus.internal:9090 \
  --alertmanager-url http://alertmanager.internal:9093 \
  --format json --out /tmp/slo-30d.json
```

`make slo-report` wraps the subcommand with the lab defaults
(`http://localhost:9090`, 7-day window, markdown to stdout).

### Output shape

The markdown report has three sections:

1. **SLO status table** — one row per SLO with target, actual, status emoji.
   `✅` = within target, `⚠️` = within 10% of breach, `🔥` = breached.
2. **Top-5 5xx paths** — `topk(5, sum by (path) (rate(strata_http_requests_total{code=~"5.."}[<window>])))`.
3. **Top-5 slow paths** — `topk(5, ...)` over the p99 latency histogram.

When `--alertmanager-url` is set the report appends a fourth section
listing burn-rate alert firings count per SLO over the window.

## Re-tuning a target

To tighten or loosen an SLO:

1. Edit the matching `strata:slo_*:target` recording rule in
   `deploy/prometheus/alerts.yml`.
2. `make promtool-check` to validate.
3. Reload Prometheus (`SIGHUP` or `/-/reload`).
4. Every single-window alert + burn-rate alert that references the
   target rule picks up the new value on the next evaluation cycle.

The recording rule indirection is intentional — never hard-code SLO
numbers into alert expressions. The
[`strata.recording` group]({{< relref "/operate/alerts#recording-rules" >}})
is the canonical place to read or change them.

## See also

- [Monitoring]({{< relref "/operate/monitoring" >}}) — scrape config,
  per-metric definitions, Grafana dashboard provisioning.
- [Alerts]({{< relref "/operate/alerts" >}}) — full rule catalog,
  Alertmanager routing recipe, burn-rate philosophy.
- [Profiling]({{< relref "/operate/profiling" >}}) — pprof workflow
  when latency SLO breaches need a perf root-cause.
