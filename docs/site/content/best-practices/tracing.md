---
title: 'Tracing'
weight: 35
description: 'OpenTelemetry coverage matrix, span name conventions, component / worker filters, tail-sampling behaviour.'
---

# Tracing

Strata emits OpenTelemetry spans across every meaningful tier of the
request and worker paths. Each span carries the `strata.component`
attribute so an operator can filter the entire gateway path or the
entire worker path in one Jaeger query, and each worker iteration
appears as a discrete trace so a slow / failing tick is easy to
correlate with the meta + data ops it triggered.

The wire-up env vars + tail-sampler + ring buffer behaviour live in
[Monitoring]({{< ref "/best-practices/monitoring#opentelemetry-tracing" >}}).
This page is the operator-facing reference for **what spans exist,
how they are named, and how to filter them**.

## Coverage matrix

| Tier | Span name shape | Emitter | `strata.component` | Extra attributes |
|---|---|---|---|---|
| HTTP server | `<METHOD> <path>` | `internal/otel/middleware.go` | `gateway` | `http.method`, `http.target`, `http.status_code`, `request_id` |
| Cassandra meta | `meta.cassandra.<table>.<op>` | `internal/meta/cassandra/observer.go` (`gocql.QueryObserver`) | `gateway` | `db.system=cassandra`, `db.operation`, `db.cassandra.table`, `request_id`, retroactive `(q.Start, q.End)` timestamps |
| TiKV meta | `meta.tikv.<table>.<op>` | `internal/meta/tikv/observer.go` (Store-method decorator) | `gateway` | `db.system=tikv`, `db.operation`, `db.tikv.table` |
| RADOS data | `data.rados.<op>` | `internal/data/rados/observer.go::ObserveOp` | `gateway` | `pool`, `oid`, retroactive `(start, end)` timestamps |
| S3-over-S3 data | `S3.<Operation>` | `go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-sdk-go-v2/otelaws` v0.68 (semconv v1.40) installed via `internal/data/s3/observer.go::installOTelMiddleware` | `gateway` | `rpc.system.name=aws-api`, `rpc.method=S3/<op>`, `aws.region`, `http.response.status_code`, `strata.s3_cluster=<id>` |
| Worker iteration (parent) | `worker.<name>.tick` | `internal/otel.StartIteration` / `EndIteration` (re-exported as `cmd/strata/workers.StartIteration`) | `worker` | `strata.worker=<name>`, `strata.iteration_id=<atomic.uint64>` |
| Worker sub-ops | see below | per-worker `tracer.Start(ctx, …)` under the iteration parent | `worker` | `strata.worker=<name>` + worker-specific keys |

### Worker sub-op spans

| Worker | Sub-op span(s) |
|---|---|
| `gc` | `gc.scan_partition`, `gc.delete_chunk` |
| `lifecycle` | `lifecycle.scan_bucket`, `lifecycle.expire_object`, `lifecycle.transition_object` |
| `replicator` | `replicator.copy_object` |
| `notify` | `notify.deliver_event` |
| `access-log` | `access_log.flush_bucket` |
| `inventory` | `inventory.scan_bucket` |
| `audit-export` | `audit_export.export_partition` |
| `manifest-rewriter` | `manifest_rewriter.rewrite_bucket` |
| `quota-reconcile` | `quota_reconcile.scan_bucket` |
| `usage-rollup` | `usage_rollup.sample_bucket` |
| `rebalance` | `rebalance.scan_bucket`, `rebalance.move_chunk` |

## Tracer names

Every tier installs a named tracer via `tp.Tracer("strata.<area>")`:

| Tracer name | Owner |
|---|---|
| `strata.http` | HTTP server middleware |
| `strata.meta.cassandra` | Cassandra QueryObserver |
| `strata.meta.tikv` | TiKV Store decorator |
| `strata.data.rados` | RADOS ObserveOp |
| `strata.data.s3` | S3 backend (otelaws middleware) |
| `strata.worker.<name>` | each worker (gc, lifecycle, replicator, notify, access-log, inventory, audit-export, manifest-rewriter, quota-reconcile, usage-rollup, rebalance) |

`internal/serverapp/serverapp.go` wires the meta + data tracers after
`strataotel.Init` runs (so the provider exists before backends are
built); the supervisor passes `*strataotel.Provider` to every worker
via `workers.Dependencies.Tracer`, and each worker resolves its named
tracer with `deps.Tracer.Tracer("strata.worker.<name>")`.

## Filter recipes

### "Everything that ran on the gateway request path"

```
strata.component=gateway
```

Catches HTTP server spans + every meta / data child span underneath
them. Pair with `request_id=<uuid>` to scope to one customer ticket.

### "Everything any background worker emitted"

```
strata.component=worker
```

One Jaeger query covers all ten workers. Layer `strata.worker=<name>`
to scope to one worker.

### "GC iterations on this replica that failed in the last hour"

```
strata.component=worker
strata.worker=gc
status=Error
service.instance.id=<hostname>
```

The iteration span flips to Error if the iteration body returns an
error OR any sub-op span recorded an error (sticky-err accumulator
inside `gc.Worker.drainCount` / `lifecycle.Worker.iterErr`). The
tail-sampler always exports failing spans regardless of
`STRATA_OTEL_SAMPLE_RATIO`, so failures land in Jaeger even at a
0.01 sample ratio.

### "All S3 SDK calls against the secondary backend"

```
strata.component=gateway
strata.s3_cluster=secondary
```

`strata.s3_cluster` is stamped per-cluster by the otelaws
`AttributeBuilder` in `internal/data/s3/observer.go::stampStrataAttrs`,
so multi-cluster routing (see
[S3 multi-cluster]({{< ref "/best-practices/s3-multi-cluster" >}}))
is filterable end-to-end.

### "Which TiKV table is hot on this trace"

```
strata.component=gateway
db.system=tikv
```

Each `meta.tikv.<table>.<op>` span carries `db.tikv.table=<table>`;
sort Jaeger by duration to find the offender.

## Sampling

The tail-sampler (`internal/otel/sampler.go`) decides at OnEnd:

- `status=Error` → exported.
- `http.status_code >= 500` → exported.
- Otherwise → kept at `STRATA_OTEL_SAMPLE_RATIO` (default `0.01`).

Worker iteration + sub-op spans flow through the same sampler, so
**failing iterations always export** regardless of the configured
ratio. The ring buffer (`STRATA_OTEL_RINGBUF=on`, default) retains
every span in-process under a bytes budget regardless of the
sampler, so the operator console's `/admin/v1/diagnostics/trace/{requestID}`
endpoint always has the full trace for any recent request — even
sampled-out ones.

## Iteration-id semantics

`strata.iteration_id` is a per-worker `atomic.Uint64` counter
(map keyed on worker name, mutated under a sync.Mutex). The counter
is process-local, NOT cluster-wide — different replicas can emit the
same `strata.iteration_id=42` for the same worker. The intended
filter shape is:

```
strata.worker=gc
service.instance.id=<hostname>
strata.iteration_id=42
```

…which scopes to one iteration on one replica. The supervisor + gc
fan-out share one counter per worker name; fan-out shards do NOT get
distinct counters (so the chip-level "leader heartbeat" stays the
heartbeat-level acquired / released pair documented in the project
CLAUDE.md).

## See also

- [Monitoring]({{< ref "/best-practices/monitoring#opentelemetry-tracing" >}})
  — env knobs + ring buffer + bundled tracing stack.
- [Observability deep dive]({{< ref "/architecture/observability" >}})
  — implementation rationale (tail-sampler, semconv version, observer
  retroactive-timestamp trick).
