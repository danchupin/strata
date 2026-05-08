---
title: 'Monitoring'
weight: 30
description: 'Prometheus scrape config, key metrics, Grafana dashboards, OTel collector wire-up, in-process trace browser.'
---

# Monitoring

Strata exposes three observability surfaces and one operator console
embedded in the gateway binary:

1. **Prometheus metrics** at `/metrics` (every replica).
2. **Structured slog logs** to stdout, JSON-shaped, correlated by
   `request_id`.
3. **OpenTelemetry traces** exported via OTLP/HTTP, sampled tail-first
   with an in-process ring buffer for failed-trace replay.
4. **Audit log** in the metadata backend (`audit_log` table or TiKV
   prefix), one row per state-changing request.

This page covers the wire-up; the
[Observability deep dive]({{< ref "/architecture/observability" >}})
covers the implementation rationale.

## Prometheus

### Scrape config

Strata serves `/metrics` on the gateway HTTP port (default 9000 in
docker, 8080 on `make run-memory`). Scrape every replica; worker
counters carry a `worker` label so one job covers gateway + all
in-process workers.

```yaml
scrape_configs:
  - job_name: strata
    static_configs:
      - targets:
          - "strata-1:9000"
          - "strata-2:9000"
          - "strata-3:9000"
        labels:
          binary: strata
```

The bundled `deploy/prometheus/prometheus.yml` covers the docker-compose
shapes (`strata`, `strata-features`, `strata-tikv-{a,b}`); use it as a
template.

### Key metrics

Every metric is documented in `internal/metrics/metrics.go`; the
Prometheus `Help` strings are authoritative. The operator-facing
shortlist:

| Metric | Type | Meaning | Alert shape |
|---|---|---|---|
| `strata_http_requests_total` | counter, labels `method,code,bucket,access_key` | Per-request counter. `bucket="_admin"` covers `/admin/v1`, `/metrics`, `/healthz`, `/readyz`, `/console`. | Sustained 5xx rate above baseline. |
| `strata_http_request_duration_seconds` | histogram, labels `method,path,status` | Latency. `path` is templated (`/{bucket}/{key}`). | p99 above SLO for ≥ 5 min. |
| `strata_worker_panic_total` | counter, labels `worker,shard` | Panics caught + recovered by the supervisor. `shard` is `"-"` outside the gc fan-out. | Any non-zero rate. |
| `strata_replication_queue_age_seconds` | gauge, label `bucket` | Oldest pending replication row per source bucket. Backs the per-bucket Replication tab. | > 600 s for ≥ 10 min. |
| `strata_replication_queue_depth` | gauge, label `rule_id` | Pending replication queue rows per rule. | Sustained growth without drain. |
| `strata_cassandra_lwt_conflicts_total` | counter, labels `table,bucket,shard` | LWT conflicts (compare-and-set rejects). Backs the Hot Shards heatmap. | Spikes correlate with bucket-shard hot keys. |
| `strata_gc_queue_depth` | gauge, label `region` | Pending gc_queue rows per region. | Sustained growth without drain. |
| `strata_gc_processed_chunks_total` | counter | Chunks deleted by the GC worker. | Drain rate visibility. |
| `strata_gc_enqueued_chunks_total` | counter | Chunks enqueued for async deletion. | Pair with `processed_total` to compute net depth. |
| `strata_lifecycle_tick_total` | counter, labels `action,status` | Per-action outcomes. `action ∈ {transition,expire,expire_noncurrent,abort_multipart}`, `status ∈ {success,error,skipped}`. | `error` rate spike. |
| `strata_notify_delivery_total` | counter, labels `sink,status` | Notification delivery outcomes. `status ∈ {success,failure,dlq}`. | DLQ growth. |
| `strata_cassandra_query_duration_seconds` | histogram, labels `table,op` | Per-query latency from the gocql QueryObserver. | LWT op p99. |
| `strata_rados_op_duration_seconds` | histogram, labels `pool,op` | RADOS op latency from `internal/data/rados.ObserveOp`. | put / get p99 spikes. |
| `strata_otel_ringbuf_traces` / `strata_otel_ringbuf_evicted_total` | gauge / counter | In-process OTel ring-buffer occupancy + evictions. | Eviction rate > 0 means raise `STRATA_OTEL_RINGBUF_BYTES`. |
| `strata_audit_stream_subscribers` | gauge | Live subscribers on `/admin/v1/audit/stream`. | Diagnostic only. |
| `strata_meta_tikv_audit_sweep_deleted_total` | counter | Audit rows expunged by the TiKV retention sweeper (TiKV has no native TTL). | Steady-state non-zero on TiKV. |
| `strata_bucket_bytes` | gauge, labels `bucket,storage_class` | Per-bucket bytes, sampled hourly. | Capacity dashboards. |
| `strata_bucket_shard_bytes` / `strata_bucket_shard_objects` | gauge, labels `bucket,shard` | Per-shard distribution for the top-N largest buckets. | Hot-shard detection. |

### Grafana dashboard

`deploy/grafana/strata-dashboard.json` ships with the repo and is
auto-loaded by the docker-compose Grafana service via
`deploy/grafana/dashboard.yaml` + `deploy/grafana/datasource.yaml`. It
covers the request-rate / latency / error-rate 4-up plus the worker
panel. Import it into a standalone Grafana via Dashboards → Import →
Upload JSON.

A regression test (`deploy/grafana/dashboard_test.go`) keeps the panel
queries in lockstep with the metrics; bumping a metric name without
updating the dashboard fails CI.

## OpenTelemetry tracing

`internal/otel.Init` reads the standard OTLP env vars at startup:

| Env | Default | Meaning |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset → no-op | OTLP/HTTP collector endpoint (e.g. `http://otel-collector:4318`). Empty + ringbuf disabled installs a no-op tracer. |
| `STRATA_OTEL_SAMPLE_RATIO` | `0.01` | Head-sample ratio. Failing spans (`status=Error` or `http.status_code >= 500`) bypass the ratio via tail sampling. |
| `STRATA_OTEL_RINGBUF` | `on` | Toggle the in-process ring buffer (retains every span regardless of ratio). |
| `STRATA_OTEL_RINGBUF_BYTES` | `4 MiB` | Bytes budget for the ring buffer; LRU-evicted on pressure. |

The `internal/otel.NewMiddleware` wraps the gateway and starts a
server-kind span per request, stamped with `request_id` so traces and
logs cross-link. Per-storage observers emit child spans:

- `meta.cassandra.<table>.<op>` from the gocql QueryObserver
  (Cassandra path).
- `data.rados.<op>` (`put` / `get` / `del`) from
  `internal/data/rados.ObserveOp` (RADOS path).

### Bundled tracing stack

`deploy/docker/docker-compose.yml` ships an OTLP collector + Jaeger
all-in-one behind the `tracing` profile:

```bash
docker compose -f deploy/docker/docker-compose.yml \
  --profile tracing up otel-collector jaeger
```

The collector config in `deploy/otel/collector-config.yaml` fans
incoming OTLP spans to Jaeger at `jaeger:4317`. Point
`OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318` and Jaeger UI
on `http://localhost:16686` shows the traces.

### In-process trace browser

The ring buffer (`internal/otel/ringbuf`) retains every span the
process emits, indexed by `request_id`. The operator console exposes
`/admin/v1/diagnostics/trace/{requestID}` to look up the full trace for
any recent request — this is the lowest-friction debug path for "why
was THIS request slow" without leaving the cluster.

`STRATA_OTEL_RINGBUF=off` disables it (memory budget reclaim) at the
cost of losing the on-cluster trace browser.

## Logs

Every replica writes JSON-shaped slog lines to stdout. The gateway
middleware bound to `request_id` emits one access-log line per request
plus per-handler debug / info as the handlers choose. Workers
correlate by `worker=<name>` plus `request_id` if the worker action was
triggered by a request (e.g. lifecycle transitions stamp the source
PUT's request id).

Ship via the standard sidecar (Fluent Bit, Promtail, Vector). No
Strata-specific config beyond `STRATA_LOG_LEVEL` (`DEBUG` / `INFO` /
`WARN` / `ERROR`; default `INFO`).

## Audit log

State-changing requests (PUT / POST / DELETE on S3 paths, plus admin
write actions) append a row to the `audit_log` table. Retention is set
via `STRATA_AUDIT_RETENTION` (Go duration like `720h` or `<N>d`,
default 30 days):

- Cassandra applies the TTL via `USING TTL`; rows expunge for free.
- TiKV's audit-export worker (`--workers=audit-export`) drains
  partitions older than `STRATA_AUDIT_EXPORT_AFTER` (default 30 d) into
  gzipped JSON-lines objects in the configured export bucket, then
  deletes the source partitions.

The `/admin/v1/audit/stream` SSE endpoint tails the audit log live for
operator debugging; the gauge `strata_audit_stream_subscribers` reports
the live subscriber count.

## Health probes

- `GET /healthz` — always 200; lives ahead of auth + middleware.
- `GET /readyz` — fans out probes (Cassandra `SELECT now() FROM
  system.local`, RADOS canary OID stat) with a 1 s timeout. Failed
  probe → 503 with the failure reason.

Wire `/readyz` to the load balancer / Ingress / Kubernetes
readinessProbe; wire `/healthz` to the liveness probe. Both endpoints
bypass `STRATA_AUTH_MODE` regardless of mode.

## Recommended alerts

A minimal alert set:

- p99 `strata_http_request_duration_seconds` > 200 ms for 5 min.
- 5xx rate >  1 % of total RPS for 5 min.
- `strata_worker_panic_total` increased in the last 5 min.
- `strata_replication_queue_age_seconds` > 600 s for 10 min on any
  active rule.
- `strata_gc_queue_depth` growing without drain for 10 min.
- `strata_otel_ringbuf_evicted_total` increased — bump
  `STRATA_OTEL_RINGBUF_BYTES`.
- Cassandra cluster's own latency / availability alerts (upstream).

Pair every alert with the runbook entry in
[GC + Lifecycle tuning]({{< ref "/best-practices/gc-lifecycle-tuning" >}})
or
[Capacity planning]({{< ref "/best-practices/capacity-planning" >}}).
