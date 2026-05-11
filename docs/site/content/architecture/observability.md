---
title: 'Observability'
weight: 40
description: 'Structured logs (slog), audit log, request_id propagation, OTel tracing (tail-sampler + ring buffer), per-storage observers (Cassandra QueryObserver, RADOS ObserveOp).'
---

# Observability

Strata ships three correlated observability surfaces: structured logs,
an audit log table for state-changing requests, and OTel tracing with a
ring-buffer trace browser embedded in the operator console. All three
key off the same `request_id` so an operator can pivot from one to the
next without rebuilding context.

## Structured logs (slog)

`internal/logging` is the canonical setup. Both binaries (`cmd/strata`,
`cmd/strata-admin`) call `logging.Setup()` first thing to install a
JSON-handler `*slog.Logger` driven by `STRATA_LOG_LEVEL`
(`DEBUG`/`INFO`/`WARN`/`ERROR`; default `INFO`).

- **Workers** (`leader.Session`, `gc.Worker`, `lifecycle.Worker`,
  `notify.Config`, …) take `*slog.Logger` — never `*log.Logger`.
- **HTTP middleware** (`logging.NewMiddleware`) reads / generates
  `X-Request-Id`, sets it on `r.Header` (so downstream middleware like
  `internal/s3api/access_log.go` keeps reading it via
  `r.Header.Get`) and on `w.Header()` (client correlation), and
  attaches a child logger with `request_id` to the request context.
- **Inside handlers**, prefer
  `logging.LoggerFromContext(r.Context()).InfoContext(ctx, msg, "key", value)`.
  The `*Context` form (`InfoContext` / `WarnContext` / `ErrorContext`)
  carries ctx-bound logger updates — use it everywhere.

## Audit log

`internal/s3api.AuditMiddleware` appends one row to the `audit_log`
table per state-changing HTTP request (`PUT` / `POST` / `DELETE`).
`GET` / `HEAD` / `OPTIONS` skip audit. The middleware sits between
the access-log middleware and the API handler so it sees the inner-
handler status (auth-deny rows still get emitted because the audit
middleware is inside `mw.Wrap`).

Row TTL: `STRATA_AUDIT_RETENTION` (Go duration like `720h` or `<N>d`;
default 30 days). Cassandra applies `USING TTL`; the memory backend
prunes lazily on `ListAudit`.

IAM `?Action=` requests carry `BucketID=uuid.Nil` + `Bucket="-"` and
`Resource="iam:<Action>"`.

The middleware is best-effort — meta failures never fail the
underlying request.

### Admin overrides

`/admin/v1/*` is also wrapped in `AuditMiddleware`. Admin handlers
stamp an operator-meaningful audit row by calling

```go
s3api.SetAuditOverride(ctx, action, resource, bucket, principal)
```

inside the handler — `action` is `admin:<Verb>` (e.g.
`admin:CreateBucket`), `resource` is the operator-facing label
(`bucket:<name>`, `iam:<UserName>`, …). The middleware installs the
override pointer in ctx before `Next.ServeHTTP` and reads it back
after. Add the override stamp to every new admin write — listing
handlers (GET) skip audit by `auditableMethod`.

### Long-term retention

`strata server --workers=audit-export` drains audit_log partitions
older than `STRATA_AUDIT_EXPORT_AFTER` (default 30 days) into gzipped
JSON-lines objects in the configured export bucket, then deletes the
source partition. See [Workers]({{< ref "/architecture/workers" >}}).

## Health probes

`internal/health.Handler` serves `/healthz` (always 200) and `/readyz`
(fans out probes concurrently with a 1s timeout). Probes are injected
by the `cmd` binary via type-assertion against `cassandraProber` /
`radosProber` interfaces in `internal/serverapp/serverapp.go::buildHealthHandler`,
so the package stays free of cassandra/rados imports.

- `cassandra.Store.Probe(ctx)` runs `SELECT now() FROM system.local`.
- `rados.Backend.Probe(ctx, oid)` stats a canary OID
  (`STRATA_RADOS_HEALTH_OID`, default `strata-readyz-canary`) and
  treats `goceph.ErrNotFound` as success — only transport/auth errors
  fail.
- Memory backends register no probe, so a pure in-memory gateway is
  always ready.

Both endpoints sit on the mux ahead of `/`, so they bypass auth and
the access-log middleware regardless of `STRATA_AUTH_MODE`.

## Per-storage observers

### Cassandra QueryObserver

`cassandra.SessionConfig{Logger, SlowMS}` installs `gocql.QueryObserver`
(`internal/meta/cassandra/observer.go::SlowQueryObserver`). Queries
over `STRATA_CASSANDRA_SLOW_MS` (default 100) or with errors log at
`WARN` with `request_id` / `table` / `op` / `duration_ms` / `statement`.

### RADOS ObserveOp

`rados.Config.Logger` enables per-op DEBUG (`put` / `get` / `del`) via
`internal/data/rados/observer.go::LogOp`. The helper lives in a
build-tag-free file so it's unit-testable without librados; the
ceph-tagged backend calls it.

Both observers pull `request_id` via
`logging.RequestIDFromContext(ctx)` so per-query / per-op lines
correlate with the gateway request that triggered them.

## OpenTelemetry tracing

`internal/otel.Init(ctx, InitOptions{Logger, RingbufMetrics})` reads:

| Variable | Default | Purpose |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `""` | W3C-spec env var. Empty + ringbuf disabled installs a no-op tracer. |
| `STRATA_OTEL_SAMPLE_RATIO` | `0.01` | Sample ratio. Failing spans always export regardless. |
| `STRATA_OTEL_RINGBUF` | `on` | Toggles the in-process ring-buffer trace browser. |
| `STRATA_OTEL_RINGBUF_BYTES` | `4 MiB` | Ring buffer byte budget (LRU eviction). |

### Tail-sampler

`internal/otel/sampler.go` wraps the OTLP exporter in a tail-sampling
`SpanProcessor` — sampling decides at OnEnd, so failing spans
(`status=Error` OR `http.status_code` >= 500) always export regardless
of `STRATA_OTEL_SAMPLE_RATIO`. Successful spans are sampled at the
configured ratio.

### Ring-buffer trace browser

When `STRATA_OTEL_RINGBUF=on`, `internal/otel/ringbuf.RingBuffer` is
registered as a parallel `SpanProcessor` so it retains every span
(regardless of sample ratio) under a bytes-budgeted LRU. The
`/admin/v1/diagnostics/trace/{requestID}` admin endpoint reads it via
`Provider.Ringbuf()`. The console's Diagnostics page renders the
returned spans as a flame chart — the operator pastes a
`request_id` from a customer ticket and sees the full trace.

### HTTP middleware

`internal/otel.NewMiddleware(provider, next)`:

1. Extracts traceparent via the global propagator.
2. Starts a server-kind span named `<METHOD> <path>`.
3. Captures status via a `responseWriter` shim.
4. Marks the span Error on >= 500.
5. Stamps `request_id` on the span (read from `r.Header["X-Request-Id"]`
   after the inner logging middleware has set it) so the ring buffer
   indexes by request id.

Wired in `internal/serverapp/serverapp.go` ahead of the logging
middleware so the span covers the full request including auth, access-
log, and audit.

### Per-storage span emission

Per-storage span emission piggybacks on the existing observer hooks.
`cassandra.SessionConfig.Tracer` plugs a `trace.Tracer` into
`SlowQueryObserver`; the observer emits one client-kind child span per
gocql query, named `meta.cassandra.<table>.<op>`, timestamped to
`(q.Start, q.End)` so the SDK records the actual query duration even
though `ObserveQuery` runs after the query returns. `rados.Config.Tracer`
threads a tracer onto `Backend`; `ObserveOp(ctx, logger, metrics,
tracer, pool, op, oid, start, err)` emits `data.rados.<op>` spans
(`put` / `get` / `del`) with the same retroactive-timestamp trick.
TiKV has no upstream observer hook, so `internal/meta/tikv/observer.go`
wraps every public `Store` method with `Observer.Start(ctx, op, table)`
that returns a `finish(err)` closure — the wrapper emits
`meta.tikv.<table>.<op>` spans with `db.system=tikv`. The S3-over-S3
data backend installs the upstream
`go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-sdk-go-v2/otelaws`
middleware (v0.68, semconv v1.40) at `connFor` BEFORE the metrics
instrumentation, so otelaws Initialize-after brackets the full retry
loop and emits one client-kind `S3.<Operation>` span per SDK call with
`rpc.system.name=aws-api`, `rpc.method=S3/<op>`, `aws.region`, and
`http.response.status_code` attributes; a custom `AttributeBuilder`
stamps `strata.s3_cluster=<id>` so multi-cluster traces are filterable.
Failing queries / ops set span status to Error so the tail-sampler
exports the full trace regardless of ratio.

Every gateway-side span — HTTP middleware, Cassandra observer, TiKV
observer, RADOS observer, S3 otelaws middleware — also stamps
`strata.component=gateway` via the shared
`internal/otel.AttrComponentGateway` constant. Worker iteration spans
stamp `strata.component=worker` via `AttrComponentWorker`. Operator
filter recipes for `strata.component`, `strata.worker`, and
`strata.s3_cluster` live on the
[Tracing best-practices]({{< ref "/best-practices/tracing" >}}) page.

### SemConv version

`semconv` import version must match the SDK's `resource.Default()`
schema URL — SDK 1.41 → `semconv/v1.39.0`, SDK 1.43 → `semconv/v1.40.0`.
Mismatch fails at runtime with `conflicting Schema URL`. Bump together
when bumping the SDK.

### Local tracing stack

`deploy/docker/docker-compose.yml` ships an OTLP collector + Jaeger
all-in-one behind the `tracing` profile:

```sh
docker compose --profile tracing up otel-collector jaeger
```

Collector config in `deploy/otel/collector-config.yaml` fans incoming
OTLP traces to Jaeger at `jaeger:4317`. Jaeger UI on
`http://localhost:16686`.

### Worker tracing

Every worker registered under `strata server` (gc, lifecycle,
replicator, notify, access-log, inventory, audit-export,
manifest-rewriter, quota-reconcile, usage-rollup) wraps its periodic
iteration entrypoint (`RunOnce`, or `Run` for the one-shot
manifest-rewriter) in `StartIteration` / `EndIteration` from
`internal/otel/iteration_span.go` (re-exported as
`cmd/strata/workers.StartIteration` so the cmd-layer interface stays
`workers.StartIteration(ctx, tracer, name)`). The supervisor passes a
`*strataotel.Provider` to every worker via
`workers.Dependencies.Tracer` (already wired at
`internal/serverapp/serverapp.go:254`), and each worker resolves its
named tracer via `deps.Tracer.Tracer("strata.worker.<name>")`. No
struct change is required when adding a new worker — the dependency
is already there.

Each iteration produces:

- A parent client-kind span named `worker.<name>.tick` with
  `strata.component=worker`, `strata.worker=<name>`, and
  `strata.iteration_id=<atomic.uint64>` (per-worker counter,
  process-local).
- Per-tick sub-op child spans under that parent: `gc.scan_partition` /
  `gc.delete_chunk`; `lifecycle.scan_bucket` / `lifecycle.expire_object` /
  `lifecycle.transition_object`; `replicator.copy_object`;
  `notify.deliver_event`; `access_log.flush_bucket`;
  `inventory.scan_bucket`; `audit_export.export_partition`;
  `manifest_rewriter.rewrite_bucket`; `quota_reconcile.scan_bucket`;
  `usage_rollup.sample_bucket`.

Sub-op failures `RecordError` + `SetStatus(Error)` and flow into a
sync.Mutex-guarded sticky-err accumulator so the iteration parent
flips to Error and the tail-sampler exports the full iteration
regardless of `STRATA_OTEL_SAMPLE_RATIO`. `tracer == nil` falls back
to `strataotel.NoopTracer()` (a real but discarding tracer) so unit
tests without OTel wiring keep working.

`strata-admin rewrap` is a one-shot operator command and stays
untraced.

## Source

- `internal/logging/` — slog setup, request_id middleware.
- `internal/s3api/audit.go` — audit middleware, override surface.
- `internal/health/` — `/healthz`, `/readyz`.
- `internal/otel/` — `Init`, middleware, tail-sampler, ring buffer.
- `internal/meta/cassandra/observer.go` — gocql QueryObserver +
  per-query spans.
- `internal/data/rados/observer.go` — RADOS ObserveOp + per-op spans.
