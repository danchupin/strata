# PRD: Complete OpenTelemetry Tracing Coverage

## Introduction

Tracing today covers HTTP middleware (`internal/otel/middleware.go` — request
spans with traceparent propagation), Cassandra meta backend
(`meta.cassandra.<table>.<op>` via `gocql.QueryObserver`), and RADOS data
backend (`data.rados.<op>` via `ObserveOp`). Three gaps remain that break the
chain on a Jaeger waterfall:

1. **TiKV meta backend** — `internal/meta/tikv/` has no observer.
   Transactional ops (`Begin`/`LockKeys`/`Get`/`Set`/`Commit`) flow through
   `tikv/client-go` without spans. On a lab-tikv stack an operator sees the
   gateway-level HTTP span + `data.rados.<op>` children with the meta-write
   step entirely missing — looks broken.
2. **S3-over-S3 data backend** — `internal/data/s3/` has no observer.
   PutChunk / GetChunk / DeleteChunk hit AWS SDK directly. Secondary data
   path on lab-tikv default (RADOS is primary), but still a gap on
   S3-only deployments.
3. **Workers** — every worker in `cmd/strata/workers/*.go` (10 workers: gc,
   lifecycle, replicator, notify, access-log, inventory, audit-export,
   manifest-rewriter, quota-reconcile, usage-rollup) passes `nil tracer` to
   its Runner per project CLAUDE.md note. Worker iterations are invisible
   in traces.

This PRD closes all three: TiKV observer (P2), S3 SDK-middleware tracing
(P3), and per-iteration worker spans (CLAUDE.md gap) — each with a
component-discriminator attribute (`strata.component={gateway|worker}` +
`strata.worker=<name>`) so Jaeger / tail-sampler can filter worker traffic
separately from gateway requests.

Closes ROADMAP P2 'TiKV meta backend emits no trace spans' + P3 'S3-over-S3
data backend emits no trace spans' on cycle close.

## Goals

- Every public `meta.tikv.*Store` method emits `meta.tikv.<op>` span via a
  thin functional decorator (mirror cassandra observer shape)
- `data.s3.Backend` SDK clients carry `otelaws` middleware so every AWS SDK
  call (`PutObject`, `GetObject`, `HeadObject`, `CopyObject`, `ListObjects`,
  `CreateMultipartUpload`, `UploadPart`, `CompleteMultipartUpload`,
  `AbortMultipartUpload`, `DeleteObject`) lands as a `S3.<op>` span
- Every worker iteration emits a per-tick span named `worker.<name>.tick`
  with attributes `strata.component=worker` + `strata.worker=<name>` +
  `strata.iteration_id=<n>`; failing iterations marked Error so the
  tail-sampler always exports them
- Component discriminator scheme: gateway HTTP spans tag
  `strata.component=gateway`; worker spans tag `strata.component=worker`.
  Operator can filter Jaeger by attribute
- Sampling: worker spans go through the same `STRATA_OTEL_SAMPLE_RATIO`
  tail-sampler as HTTP spans; failing-iteration spans always export
- Closes ROADMAP P2 + P3 on cycle close

## User Stories

### US-001: Gateway-side observers stamp strata.component=gateway
**Description:** As an operator, I want every existing gateway-side span
(HTTP middleware, Cassandra meta observer, RADOS data observer) tagged
with `strata.component=gateway` so the Jaeger filter scheme rolled out in
later stories works on day-one.

**Acceptance Criteria:**
- [ ] `internal/otel/middleware.go` HTTP middleware: after `tracer.Start(ctx, ...)`, call `span.SetAttributes(attribute.String("strata.component", "gateway"))`
- [ ] `internal/meta/cassandra/observer.go::SlowQueryObserver.ObserveQuery`: inside the span emission branch, stamp `strata.component=gateway`
- [ ] `internal/data/rados/observer.go::ObserveOp`: inside the span emission branch, stamp `strata.component=gateway`
- [ ] Add exported constants `internal/otel.AttrComponentGateway` + `AttrComponentWorker` to avoid magic-string duplication
- [ ] Unit tests in each of the three observers extended with one assertion: emitted span carries `strata.component=gateway`
- [ ] Race detector clean
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: TiKV meta backend observer + tracer wiring
**Description:** As an operator inspecting a Jaeger waterfall on the
lab-tikv stack, I want every TiKV transactional op to appear as a child
span of its request so the meta-write step is visible.

**Acceptance Criteria:**
- [ ] Add `internal/meta/tikv/observer.go` with `type Observer struct { logger *slog.Logger; tracer trace.Tracer }` and `WrapOp(ctx, op, table string, fn func(ctx) error) error` helper that starts a child span `meta.tikv.<table>.<op>`, runs `fn`, marks status from `err`, ends the span. Span attributes: `db.system="tikv"`, `db.operation=<op>`, `db.table=<table>`, `strata.component="gateway"`
- [ ] Add `Tracer trace.Tracer` field to `internal/meta/tikv/config.go::Config` (matches `cassandra.SessionConfig.Tracer` shape)
- [ ] `internal/meta/tikv/store.go`'s constructor stores the tracer; every public Store method that hits TiKV (CreateBucket, GetBucket, PutObject, GetObject, DeleteObject, ListObjects, SavePart, CompleteMultipart, abort/complete/get*Multipart, SetBucketBlob/GetBucketBlob/DeleteBucketBlob, PutBucketStats/GetBucketStats, etc — full enumeration via grep) wraps its inner txn body in `observer.WrapOp(ctx, "<op>", "<table>", func(ctx) error { ... })`
- [ ] Wire `cfg.Tracer = tp.Tracer("strata.meta.tikv")` in `internal/serverapp/serverapp.go::buildMetaStore` after `strataotel.Init` runs, mirror cassandra wiring at line ~545
- [ ] Failing op (`err != nil`) sets span status to Error so tail-sampler exports
- [ ] Race detector `go test -race ./internal/meta/tikv/...` clean
- [ ] Unit test `internal/meta/tikv/observer_test.go`: fake tracer captures span name / attributes / status for a synthetic op; success vs error paths verified
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: S3 backend AWS SDK otelaws middleware
**Description:** As an operator running an S3-over-S3 deployment, I want
every AWS SDK call from `data.s3.Backend` to land as a span in the trace
so PutChunk / GetChunk paths are visible.

**Acceptance Criteria:**
- [ ] Add `go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-sdk-go-v2/otelaws` as a project dependency
- [ ] `internal/data/s3/backend.go::connFor` installs the otelaws middleware on the built `*awss3.Client` via `otelaws.AppendMiddlewares(&awsCfg.APIOptions)` BEFORE the client is constructed. Spans emit per SDK call with auto-attributes `rpc.service=S3`, `rpc.method=<op>`, `aws.region=<region>`, `http.status_code=<code>`
- [ ] Add a custom span-attribute hook that stamps `strata.component="gateway"` + `strata.s3_cluster=<cluster-id>` on every span so traces are filterable per cluster
- [ ] Add `Tracer trace.Tracer` field to `internal/data/s3/config.go::Config`; wire `cfg.Tracer = tp.Tracer("strata.data.s3")` in `internal/serverapp/serverapp.go::buildDataBackend` for `case "s3":` (mirror rados wiring at line ~368)
- [ ] If `cfg.Tracer == nil` (tests), skip middleware registration — preserves zero-config test fixture from `s3test.NewFixture`
- [ ] Failing SDK call (`http.status_code >= 400`) marks span status Error so tail-sampler exports
- [ ] Race detector `go test -race ./internal/data/s3/...` clean
- [ ] Unit test `internal/data/s3/observer_test.go`: drive Backend.Put against an httptest fake; assert the otelaws-emitted span carries `rpc.method=PutObject`, `strata.component=gateway`, `strata.s3_cluster=<id>`
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: Worker tracing — iteration helper + gc/lifecycle wiring
**Description:** As an operator, I want each gc / lifecycle worker iteration
to appear as a discrete trace so I can correlate scheduling decisions with
their meta + data ops.

**Acceptance Criteria:**
- [ ] `workers.Dependencies.Tracer` is ALREADY `*strataotel.Provider` (wired at `serverapp.go:254`). No struct change needed — workers just need to USE it via `deps.Tracer.Tracer("strata.worker.<name>")`
- [ ] Add `cmd/strata/workers/iteration_span.go` helper: `StartIteration(ctx context.Context, tracer trace.Tracer, workerName string) (context.Context, trace.Span)` returns a child span named `worker.<name>.tick` with attributes `strata.component="worker"` (via `internal/otel.AttrComponentWorker`), `strata.worker=<name>`, `strata.iteration_id=<atomic-uint64-counter>` (per-worker, `atomic.AddUint64` — safe for fan-out workers). `EndIteration(span trace.Span, err error)` marks Error on `err != nil` and ends the span
- [ ] If `tracer == nil` (unit tests without tracer wiring), iteration spans use `tracenoop.NewTracerProvider().Tracer(...)` — no-panic, no behavior change
- [ ] `cmd/strata/workers/gc.go::Build` extracts `tracer := deps.Tracer.Tracer("strata.worker.gc")` and propagates into gc Runner; gc's main loop wraps each scan-batch tick in `StartIteration` / `EndIteration`. Child spans for `gc.scan_partition`, `gc.delete_chunk` emit under the parent iteration span via `tracer.Start`
- [ ] `cmd/strata/workers/lifecycle.go::Build` same pattern: per-iteration parent span + child spans for `lifecycle.scan_bucket`, `lifecycle.expire_object`, `lifecycle.transition_object`
- [ ] Sampling: worker spans flow through the existing `internal/otel/sampler.go` tail-sampler; failing iterations always export (status=Error)
- [ ] Unit test (gc + lifecycle): drive one iteration with a fake meta backend that errors on the third op; assert the per-iteration span has Error status AND a `gc.delete_chunk` child span with Error status
- [ ] If `deps.Tracer == nil` (unit tests), iteration spans become a no-op tracer call — no-panic, no behavior change
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: Worker tracing — feature batch
**Description:** As an operator, I want every remaining worker (replicator,
notify, access-log, inventory, audit-export, manifest-rewriter,
quota-reconcile, usage-rollup) wired to the same per-iteration span
helper so the trace coverage is uniform.

**Acceptance Criteria:**
- [ ] Each of the 8 workers in `cmd/strata/workers/` (replicator.go, notify.go, access_log.go, inventory.go, audit_export.go, manifest_rewriter.go, quota_reconcile.go, usage_rollup.go) follows the gc/lifecycle pattern from US-003: per-iteration parent span via `StartIteration` / `EndIteration`, child spans for per-tick sub-ops where meaningful (`notify.deliver_event`, `replicator.copy_object`, `access_log.flush_bucket`, `inventory.scan_bucket`, `audit_export.export_partition`, `manifest_rewriter.rewrite_bucket`, `quota_reconcile.scan_bucket`, `usage_rollup.sample_bucket`)
- [ ] All 8 workers pick up `deps.Tracer` from `Dependencies` (no per-worker bootstrap)
- [ ] Each worker's test file (`*_test.go`) gains one trace assertion: drive a single iteration, assert the per-iteration span name + `strata.worker=<name>` attribute + `strata.component=worker` attribute
- [ ] No semantic behavior change — just span emission
- [ ] Race detector `go test -race ./cmd/strata/workers/...` clean
- [ ] Typecheck passes
- [ ] Tests pass

### US-006: Docs + ROADMAP close-flip
**Description:** As a developer, I want operator-facing docs that document
the new component-discriminator scheme + ROADMAP entries flipped in the
same commit when the cycle closes.

**Acceptance Criteria:**
- [ ] Update `docs/site/content/best-practices/web-ui.md` (or add `docs/site/content/best-practices/tracing.md` if it does not yet exist as a separate page — verify) with: full coverage matrix (HTTP middleware / TiKV meta / Cassandra meta / RADOS data / S3 data / Workers), span name conventions (`meta.<backend>.<op>`, `data.<backend>.<op>`, `worker.<name>.tick`), `strata.component=gateway|worker` filter examples for Jaeger, `strata.worker=<name>` per-worker filter examples, sampling behaviour (tail-sampler, failing spans always export)
- [ ] ROADMAP.md close-flip in the same commit per CLAUDE.md Roadmap maintenance rule: flip BOTH P2 entry 'TiKV meta backend emits no trace spans' AND P3 entry 'S3-over-S3 data backend emits no trace spans' to `~~**Px — ...**~~ — **Done.** ... (commit `<pending>`)`
- [ ] Update CLAUDE.md note about "Workers under `strata server` currently pass nil tracer" — replace with the new convention: each worker emits per-iteration spans via the `workers.StartIteration` helper; supervisor wires `deps.Tracer = tp.Tracer("strata.worker." + name)` automatically
- [ ] Reference doc `docs/site/content/reference/_index.md` already mentions OTel envs — verify the `STRATA_OTEL_SAMPLE_RATIO` description reflects worker spans are sampled the same way; update if needed
- [ ] Delete `tasks/prd-complete-tracing.md` per CLAUDE.md PRD lifecycle rule (Ralph snapshot is the canonical record)
- [ ] `make docs-build` clean (Hugo strict-ref resolution catches dangling refs)
- [ ] Closing-SHA backfill follow-up commit on main per established pattern
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- FR-1: TiKV meta observer wraps every public Store method that hits TiKV in `meta.tikv.<op>` spans via `observer.WrapOp`; `Tracer` field on `tikv.Config`; wired by serverapp after `strataotel.Init`
- FR-2: S3 data backend installs `otelaws` AWS SDK middleware in `connFor` before client construction; spans carry `rpc.method`, `rpc.service=S3`, `aws.region`, `http.status_code`, plus stamped `strata.component=gateway` + `strata.s3_cluster=<id>`
- FR-3: `workers.Dependencies` gains `Tracer trace.Tracer`; supervisor wires `tp.Tracer("strata.worker." + name)` per worker
- FR-4: `workers.StartIteration(ctx, name) → (ctx, span)` + `EndIteration(span, err)` helpers in `cmd/strata/workers/iteration_span.go`; spans tag `strata.component="worker"`, `strata.worker=<name>`, `strata.iteration_id=<n>`
- FR-5: Every one of the 10 workers (gc / lifecycle / replicator / notify / access-log / inventory / audit-export / manifest-rewriter / quota-reconcile / usage-rollup) wraps each iteration in `StartIteration`/`EndIteration`; meaningful sub-ops get child spans
- FR-6: Failing TiKV ops, failing S3 SDK calls (status >= 400), failing worker iterations all mark span status Error so the existing tail-sampler in `internal/otel/sampler.go` always exports the parent trace
- FR-7: Component-discriminator attributes (`strata.component=gateway|worker`, `strata.worker=<name>`, `strata.s3_cluster=<id>`) enable per-component / per-worker filtering in Jaeger

## Non-Goals

- **No HTTP middleware changes.** Already covered. This cycle stamps `strata.component=gateway` on existing spans, but does not refactor middleware
- **No Cassandra observer changes.** Already covered. Component attribute stamping is a 1-line addition, not a rewrite
- **No RADOS observer changes.** Already covered. Same 1-line stamping for `strata.component=gateway`
- **No new tracer providers.** Reuse `internal/otel.Provider` and `tp.Tracer(name)` factory
- **No SIGHUP / dynamic-reload of sample ratio.** Already env-time only
- **No exporter changes.** Existing OTLP + ring-buffer wiring is enough
- **No worker introspection UI changes.** ROADMAP P2 'Trace browser list view' is a separate entry; this cycle leaves the existing search-only TraceBrowser as-is
- **No span attribute backfill for old traces.** New spans only; ringbuf evicts old ones in <30 min anyway

## Technical Considerations

### Component discriminator scheme

Operator filter examples (Jaeger Query Language):
- `strata.component=gateway` — all request-path spans (HTTP + meta + data)
- `strata.component=worker` — all background scheduling traffic
- `strata.worker=gc` — gc-worker iterations only
- `strata.s3_cluster=primary` — only the named S3 cluster

The attribute is stamped at span creation time:
- HTTP middleware: `strata.component=gateway`
- meta observers (cassandra + tikv): `strata.component=gateway`
- data observers (rados + s3 otelaws hook): `strata.component=gateway`
- worker `StartIteration` helper: `strata.component=worker` + `strata.worker=<name>`

Same process emits both; the discriminator is per-span attribute, not
Resource attribute (Resource is shared across all spans).

### otelaws middleware vs custom observer

AWS SDK Go v2 provides
`go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-sdk-go-v2/otelaws`
which installs as SDK middleware. Auto-emits per-SDK-call spans with the
standard semconv `rpc.system=aws-api`, `rpc.service=S3`, `rpc.method=<op>`,
`http.status_code=<code>`. Reuses the existing otelaws maintainer
contract. Custom observer would duplicate that work — pick otelaws.

### Per-iteration span name

`worker.<name>.tick` is short, deterministic, and groups well in Jaeger's
service-by-operation view. Per-iteration counter (`strata.iteration_id`)
lets operators correlate adjacent traces from the same worker.

### Sampling behaviour

Tail-sampler in `internal/otel/sampler.go` already exports any span with
status=Error regardless of ratio. Worker spans that fail (transient meta
error, RADOS op error) always export. Sampled-out successful worker spans
still land in the ring buffer (parallel `SpanProcessor`) for short-term
debugging.

### Long-running worker concern

Per-iteration span only — no long-running parent. Avoids the SDK's known
issue with spans living for hours (no late attribute updates, weird
Jaeger timing). Operator wanting "which iteration belongs to which worker
process" filters by `service.instance.id` (Resource attribute) +
`strata.worker`.

### Nil-tracer safety

If `cfg.Tracer == nil` (unit tests), the observer helpers should fall
back to a tracenoop tracer so callers do not need a nil-guard at every
call site. Match existing `rados.ObserveOp` behaviour.

## Success Metrics

- Jaeger query `strata.component=worker AND strata.worker=gc` returns gc-iteration spans during a soak run
- Jaeger query for a failing PUT request shows: HTTP span → meta.tikv.PutObject → data.s3.PutObject (or data.rados.put) — no gaps
- A failing worker iteration produces a span with Error status that the tail-sampler always exports regardless of `STRATA_OTEL_SAMPLE_RATIO`
- Operator can filter all worker traffic out of trace search with `strata.component != worker`
- Race detector clean across all touch points

## Open Questions

- Should `strata.iteration_id` be a monotonic int (simple) or a UUID
  (globally unique across replica restarts)? Default: monotonic int reset
  to 0 on process start; trace.SpanID already provides global uniqueness
- otelaws default attribute set includes `rpc.system=aws-api` and
  `aws.request.id`. Should we also stamp `strata.bucket=<name>` so
  per-bucket S3-side filtering works? Default: yes — add via the custom
  middleware hook in US-002
- For workers that fan-out per shard (gc fan-out per `gc.scan_partition`),
  should the shard ID be a span attribute on the child or part of the
  span name? Default: span attribute `strata.shard=<id>` on the child;
  keeps span-name cardinality bounded
