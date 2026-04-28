# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this project is

Strata is a Go-based, S3-compatible object gateway designed as a drop-in replacement for Ceph RGW. Metadata lives in
Cassandra (sharded `objects` table to dodge the bucket-index ceiling that bites RGW), data lives in RADOS as 4 MiB
chunks, and the gateway speaks S3 over HTTP. The compatibility goal is tracked against Ceph's upstream `s3-tests`
suite — see `tasks/prd-s3-compatibility.md` for the active PRD and `ROADMAP.md` for what is shipped vs pending.

The metadata interface (`internal/meta.Store`) is intentionally minimal and Cassandra-flavoured (LWT, clustering order,
fan-out paging). Cassandra is the primary backend; ScyllaDB is a drop-in. The in-memory backend is for tests and the
smoke pass.

## Common commands

| Task                                                                   | Command                                                                  |
|------------------------------------------------------------------------|--------------------------------------------------------------------------|
| Build everything                                                       | `make build` (`go build ./...`)                                          |
| Build with RADOS data backend                                          | `go build -tags ceph ./...`                                              |
| Vet                                                                    | `make vet`                                                               |
| Unit tests                                                             | `make test` (`go test ./...`)                                            |
| Race-detector tests                                                    | `make test-race`                                                         |
| Cassandra integration tests (testcontainers, needs Docker)             | `make test-integration` (`go test -tags integration -timeout 10m ./...`) |
| RADOS integration tests (in-container, requires `make up-all` running) | `make test-rados`                                                        |
| Run a single test                                                      | `go test -run TestBucketCRUD ./internal/s3api`                           |
| Bring up Cassandra only                                                | `make up && make wait-cassandra`                                         |
| Bring up full stack (Cassandra + Ceph + gateway)                       | `make up-all && make wait-cassandra && make wait-ceph`                   |
| Run gateway against in-memory backends                                 | `make run-memory`                                                        |
| Run gateway against Cassandra metadata + memory data                   | `make run-cassandra`                                                     |
| Smoke pass                                                             | `make smoke` (signed: `make smoke-signed`)                               |
| Take stack down                                                        | `make down`                                                              |
| S3 compatibility suite                                                 | `scripts/s3-tests/run.sh` (see `scripts/s3-tests/README.md`)             |

macOS + lima Docker note: `make test-integration` needs `DOCKER_HOST=unix:///Users/.../.lima/.../sock/docker.sock` for
testcontainers to find the engine.

## Big-picture architecture

```
                +-----------------------------+
                |  S3 client (aws-cli, mc)    |
                +--------------+--------------+
                               |
                  HTTP S3 (path-style URLs)
                               |
                +--------------v--------------+
                | cmd/strata-gateway          |
                |  -> auth.Middleware (SigV4) |
                |  -> s3api.Server (router)   |
                +-------+--------------+------+
                        |              |
                        v              v
              +-------------------+   +---------------------+
              | meta.Store        |   | data.Backend        |
              |  memory | cassandra|   |  memory | rados    |
              +---------+----------+   +---------+----------+
                        |                        |
                +-------v-------+        +-------v-------+
                | Cassandra     |        | RADOS         |
                | (sharded)     |        | (4 MiB chunks)|
                +---------------+        +---------------+

  strata server --workers=lifecycle -> meta.Store + data.Backend (transitions /
                          expirations / mp-abort). (US-006: legacy
                          cmd/strata-lifecycle deleted; runs as a worker inside
                          the unified strata binary, leader-elected on
                          `lifecycle-leader`)
  strata server --workers=gc -> meta.Store (GCEntry queue) + data.Backend (chunk delete)
                          (US-005: legacy cmd/strata-gc deleted; gc now runs as a worker
                          inside the unified strata binary, leader-elected on `gc-leader`)
  strata server --workers=notify -> meta.Store (notify_queue + DLQ) -> webhook / SQS
                          sinks via STRATA_NOTIFY_TARGETS. (US-007: legacy
                          cmd/strata-notify deleted; runs inside the unified
                          strata binary, leader-elected on `notify-leader`)
  strata server --workers=replicator -> meta.Store (replication_queue) +
                          data.Backend, copies to peer Strata via HTTP PUT
                          (HTTPDispatcher). (US-008: legacy cmd/strata-replicator
                          deleted; runs inside the unified strata binary,
                          leader-elected on `replicator-leader`)
  strata server --workers=access-log -> meta.Store (access_log_buffer) +
                          data.Backend, drains buffered rows per source bucket
                          and writes one AWS-format log object per flush into
                          the target bucket configured by PutBucketLogging.
                          (US-009: legacy cmd/strata-access-log deleted; runs
                          inside the unified strata binary, leader-elected on
                          `access-log-leader`)
  strata server --workers=inventory -> meta.Store (bucket InventoryConfiguration
                          blobs) + data.Backend, ticks per (bucket, configID),
                          walks the source bucket and writes manifest.json +
                          CSV.gz pairs into the configured target bucket.
                          (US-010: legacy cmd/strata-inventory deleted; runs
                          inside the unified strata binary, leader-elected on
                          `inventory-leader`)
  internal/reshard      -> per-bucket online shard-resize worker (US-045); driven
                          synchronously via /admin/bucket/reshard or as a daemon
  cmd/strata-audit-export -> internal/auditexport: drains audit_log partitions
                          older than STRATA_AUDIT_EXPORT_AFTER (default 30d) into
                          gzipped JSON-lines objects in the configured export
                          bucket, then deletes the source partition (US-046).
                          Leader-elected, daily tick.
  cmd/strata-manifest-rewriter -> internal/manifestrewriter: walks every bucket
                          and converts any JSON-encoded objects.manifest blob
                          to protobuf in place (US-049). Leader-elected,
                          single-pass, idempotent. Run once after flipping
                          STRATA_MANIFEST_FORMAT=proto on the gateway to
                          shrink existing rows.
```

The S3 router is in `internal/s3api/server.go`. Bucket-scoped queries (`?cors`, `?policy`, `?lifecycle`, …) dispatch via
`handleBucket`; object-scoped (`?uploads`, `?uploadId=`, `?tagging`, …) via `handleObject`. New endpoints follow the
same query-string router pattern.

Auth lives in `internal/auth/`: SigV4 (`sigv4.go`), presigned URLs (`presigned.go`), streaming chunk decoder (
`streaming.go`, **chain HMAC validation TODO** — see ROADMAP P2), static credentials store (`static.go`). Identity flows
through context: `auth.FromContext(ctx).Owner`.

Virtual-hosted-style routing (`internal/s3api/vhost.go`): `STRATA_VHOST_PATTERN` is a comma-separated list of
`*.<suffix>` patterns (default `*.s3.local`; set to `-` to disable). Auth middleware runs first and signs the
original `Host` + `URL.Path`; `Server.ServeHTTP` then strips the prefix from `r.Host` and prepends `/<bucket>` to
`r.URL.Path` before path-style routing — never rewrite before SigV4 verification or signatures break.

## Background workers (cmd/strata/workers)

Workers under `strata server` register via `workers.Register(workers.Worker{Name, Build})` from a per-worker
`init()`. The `Build` constructor receives `workers.Dependencies` (Logger, Meta, Data, Tracer, Locker, Region) and
returns a `workers.Runner`. `workers.Supervisor.Run(ctx, workers)` spins one goroutine per requested worker;
each goroutine acquires a `leader.Session` keyed on `<name>-leader`, builds + runs the Runner under a supervised
context, releases on exit, and recovers from panics. A panic increments `strata_worker_panic_total{worker=<name>}`,
releases the lease, and restarts on an exponential backoff (1s → 5s → 30s → 2m, reset to 1s after 5 minutes
healthy). Lease loss restarts immediately (no backoff). One worker's panic or lease loss never affects the
gateway or sibling workers.

`cmd/strata/server.go::runServer` validates `STRATA_WORKERS` (or `--workers=`) via `workers.Resolve` BEFORE any
backend is built — unknown names exit 2 immediately. The resolved `[]workers.Worker` is then handed to
`internal/serverapp.Run`, which builds the leader-election locker (cassandra → LWT lease, memory →
process-local) and spawns `workers.Supervisor.Run` in a goroutine alongside the gateway. When adding a new
worker, register from `cmd/strata/workers/<name>.go`'s `init()` and let the binary pick it up; do not spawn a
goroutine ad-hoc inside `internal/serverapp` — the supervisor owns the lifecycle. `gc` reads
`STRATA_GC_INTERVAL` / `STRATA_GC_GRACE` / `STRATA_GC_BATCH_SIZE` from env at Build time (no per-worker
flags); other workers follow the same env-only convention.

## meta.Store interface — the contract

`internal/meta/store.go` is the abstraction every backend must satisfy. **Both `internal/meta/memory`
and `internal/meta/cassandra` implement it in lockstep** — adding a method to the interface means updating both, plus
the contract tests.

`internal/meta/storetest/contract.go` defines `Run(t, factory)` — the shared test suite. New methods that have a
backend-agnostic semantic should grow a `case<Name>` here so both backends are exercised. Memory tests live in
`internal/meta/memory/store_test.go`; Cassandra integration tests in
`internal/meta/cassandra/store_integration_test.go` (build tag `integration`).

There is a generic blob-config helper pattern for "bucket has one XML/JSON document of kind X" endpoints (lifecycle,
CORS, policy, public-access-block, ownership-controls). Reuse `setBucketBlob` / `getBucketBlob` / `deleteBucketBlob` in
both backends instead of writing fresh CRUD per endpoint.

`data.Manifest` is encoded into the `objects.manifest` blob column via `data.EncodeManifest` (US-049): the format
is selected by `data.SetManifestFormat("proto"|"json")`, default `proto`. `cmd/strata-gateway` (and other binaries
that touch the encoder) read `STRATA_MANIFEST_FORMAT` once at startup and call `SetManifestFormat`. Reads always go
through `data.DecodeManifest` which sniffs the first non-whitespace byte (`{` → JSON, anything else → proto3 wire
format) so JSON-vs-proto migrations are transparent. New fields tagged `json:",omitempty"` (and a fresh `protobuf` tag
in `manifest.proto` + helper updates in `manifest_codec.go`) are schema-additive — old rows decode with zero-values,
and you avoid an `ALTER`. Use this for per-object metadata the GET path reads but Cassandra never filters on (e.g.
`Manifest.PartChunks` for the SSE multipart locator). To convert pre-existing JSON rows to proto, run
`cmd/strata-manifest-rewriter` (leader-elected, idempotent — re-runs skip already-proto rows).

## Logging (slog)

`internal/logging` is the canonical setup. Every `cmd/<binary>/main.go` should call `logging.Setup()` first thing
to install a JSON-handler `*slog.Logger` driven by `STRATA_LOG_LEVEL` (DEBUG/INFO/WARN/ERROR; default INFO) and use
the returned logger for binary-level errors. Workers (`leader.Session`, `gc.Worker`, `lifecycle.Worker`,
`notify.Config`, etc.) take `*slog.Logger`, never `*log.Logger`. The HTTP gateway wraps its mux handler with
`logging.NewMiddleware(logger, next)` which reads / generates `X-Request-Id`, sets it on both `r.Header` (so
downstream middlewares like `internal/s3api/access_log.go` keep reading it via `r.Header.Get`) and `w.Header()`
(client correlation), and attaches a child logger with `request_id` to the request context. Inside handlers, prefer
`logging.LoggerFromContext(r.Context()).InfoContext(ctx, msg, "key", value)` — passing the bound logger keeps lines
correlated without additional plumbing. Use `WarnContext`/`InfoContext`/`ErrorContext` (not the no-context variants)
so future ctx-bound loggers ride through.

Audit log: `internal/s3api.AuditMiddleware` appends one row to the `audit_log`
table per state-changing HTTP request (US-022). GET/HEAD/OPTIONS are skipped;
PUT/POST/DELETE always emit. The middleware lives between the access-log
middleware and the API handler so it sees the inner-handler status (auth-deny
rows are still emitted because the audit middleware sits inside `mw.Wrap`).
Row TTL is `STRATA_AUDIT_RETENTION` (Go duration like `720h` or `<N>d`;
default 30 days). Cassandra applies TTL via `USING TTL`; the memory backend
prunes lazily on `ListAudit`. IAM `?Action=` requests carry `BucketID=uuid.Nil`
+ `Bucket="-"` and `Resource="iam:<Action>"`. The middleware is best-effort —
meta failures never fail the underlying request.

Health probes: `internal/health.Handler` serves `/healthz` (always 200) and `/readyz` (fans out probes
concurrently with a 1s timeout). Probes are injected by the cmd binary via type-assertion against
`cassandraProber` / `radosProber` interfaces in `cmd/strata-gateway/main.go::buildHealthHandler`, so the package
stays free of cassandra/rados imports. `cassandra.Store.Probe(ctx)` runs `SELECT now() FROM system.local`;
`rados.Backend.Probe(ctx, oid)` stats a canary OID (`STRATA_RADOS_HEALTH_OID`, default `strata-readyz-canary`)
and treats `goceph.ErrNotFound` as success — only transport/auth errors fail. Memory backends register no probe,
so a pure in-memory gateway is always ready. Both endpoints sit on the mux ahead of `/`, so they bypass auth
and the access-log middleware regardless of `STRATA_AUTH_MODE`.

Per-storage observers: `cassandra.SessionConfig{Logger, SlowMS}` installs `gocql.QueryObserver`
(`internal/meta/cassandra/observer.go::SlowQueryObserver`) — queries over `STRATA_CASSANDRA_SLOW_MS` (default 100) or
with errors log WARN with `request_id`/`table`/`op`/`duration_ms`/`statement`. `rados.Config.Logger` enables per-op DEBUG
(`put`/`get`/`del`) via `internal/data/rados/observer.go::LogOp`. Both observers pull `request_id` via
`logging.RequestIDFromContext(ctx)` so per-query/per-op lines correlate with the gateway request. The RADOS observer
helper lives in a build-tag-free file so it's unit-testable without librados; the ceph-tagged backend calls it.

OpenTelemetry tracing: `internal/otel.Init(ctx)` reads `OTEL_EXPORTER_OTLP_ENDPOINT` (W3C-spec env var) and
returns a `*Provider`. Empty endpoint installs a `tracenoop.NewTracerProvider` and a no-op Shutdown so callers stay
nil-free. Endpoint set builds an OTLP/HTTP exporter wrapped in a tail-sampling `SpanProcessor`
(`internal/otel/sampler.go`) — sampling decides at OnEnd, so failing spans (`status=Error` OR
`http.status_code` >= 500) always export regardless of `STRATA_OTEL_SAMPLE_RATIO` (default 0.01). The HTTP
middleware `internal/otel.NewMiddleware(provider, next)` extracts traceparent via the global propagator,
starts a server-kind span named `<METHOD> <path>`, captures status via a `responseWriter` shim and marks the
span Error on >= 500. Wired in `cmd/strata-gateway/main.go` ahead of the logging middleware so the span
covers the full request including auth/access-log/audit. **semconv import version must match the SDK's
`resource.Default()` schema URL** — SDK 1.41 → `semconv/v1.39.0`; mismatch fails at runtime with
"conflicting Schema URL". Bump together when bumping the SDK.

Per-storage span emission piggybacks on the existing observer hooks. `cassandra.SessionConfig.Tracer`
plugs a `trace.Tracer` into `SlowQueryObserver`; the observer emits one client-kind child span per
gocql query, named `meta.cassandra.<table>.<op>`, timestamped to `(q.Start, q.End)` so the SDK records
the actual query duration even though `ObserveQuery` runs after the query returns. `rados.Config.Tracer`
threads a tracer onto `Backend`; `ObserveOp(ctx, logger, metrics, tracer, pool, op, oid, start, err)`
emits `data.rados.<op>` spans (`put`/`get`/`del`) with the same retroactive-timestamp trick. Failing
queries / ops set span status to Error so the tail-sampler exports the full trace regardless of ratio.
Tracer wiring happens in `cmd/strata-gateway/main.go::buildMetaStore` + `buildDataBackend` after
`strataotel.Init` runs (move OTel init ahead of meta/data construction; its lifetime spans the whole
process). For tracing-only deploy, `deploy/docker/docker-compose.yml` ships an OTLP collector + Jaeger
all-in-one behind the `tracing` profile (`docker compose --profile tracing up otel-collector jaeger`);
collector config in `deploy/otel/collector-config.yaml` fans incoming OTLP traces to Jaeger at
`jaeger:4317`. Other binaries (gc/lifecycle/replicator/access-log/notify/rewrap) currently pass nil
tracer — add `Tracer: tp.Tracer("strata.<binary>")` to their config when their own /readyz / metrics
story matures.

## Cassandra gotchas (real ones, hit during this codebase's lifetime)

- **No subqueries.** CQL does not support `WHERE name IN (SELECT name FROM ... WHERE id=?)`. If you need that,
  denormalise (store the natural PK in the row you need to update) or do a two-step round trip.
- **LWT (`IF EXISTS` / `IF NOT EXISTS`) is required for read-after-write coherence on the same row.**
  `SetBucketVersioning` learned this the hard way: a plain `UPDATE` after an `INSERT … IF NOT EXISTS` left Paxos state
  stale and `LOCAL_QUORUM` reads observed the pre-update value. Any UPDATE on a row that may be read with quorum after a
  previous LWT must itself be LWT.
- **`MapScanCAS` not `ScanCAS(nil...)` for `INSERT … IF NOT EXISTS`.** `CreateBucket` had a column-count bug fixed via
  `MapScanCAS`.
- **`ALLOW FILTERING` on a non-PK column is an antipattern.** If you need to look up by a non-PK, denormalise into a
  second table or add a secondary index — but secondary indexes are also a smell at this scale. Prefer denormalisation.
- **Schema migrations are additive.** `internal/meta/cassandra/schema.go` has `tableDDL` (idempotent
  `CREATE TABLE IF NOT EXISTS`) and `alterStatements` (idempotent `ALTER TABLE ADD column`, swallowed by
  `isColumnAlreadyExists`). Existing keyspaces need to upgrade in place — never write a destructive migration.
- **Two UUID flavours coexist.** Outside the cassandra package use `github.com/google/uuid` (`uuid.UUID`). Inside, gocql
  exposes its own `gocql.UUID`. Convert via the `gocqlUUID()` / `uuidFromGocql()` helpers at the boundary.

## Sharded objects table — listing fans out

The `objects` table is partitioned by `(bucket_id, shard)` where `shard = hash(key) % N` (default `N=64`, configurable
via `STRATA_BUCKET_SHARDS` at bucket creation). `ListObjects` therefore queries `N` partitions concurrently and
heap-merges by clustering order (key ASC, version_id DESC). See `cassandra/store.go: ListObjects` and the `cursorHeap` /
`versionHeap` types. A new range-scan-capable backend (e.g. TiKV) would short-circuit this via the optional
`RangeScanStore` interface (see ROADMAP).

## S3-specific conventions in this repo

- **Bucket names must be ≥3 chars, lowercase, DNS-safe.** `internal/s3api/validate.go: validBucketName` enforces this on
  `PUT /<bucket>`. Tests use `/bkt`, never `/b` — the latter rejects with 400 InvalidBucketName.
- **Test harness:** `internal/s3api/testutil_test.go` exposes `newHarness(t)` returning a server hooked up to in-memory
  `data` + `meta`. Drive it with `h.doString(method, path, body, headers...)` and `h.mustStatus(resp, code)`.
- **Conditional headers (RFC 7232) on GET** flow through `checkConditional` in `internal/s3api/conditional.go`. PUT-side
  `If-Match` / `If-None-Match` checks live inline in `putObject`.
- **Lifecycle worker uses CAS on transition** via
  `Store.SetObjectStorage(ctx, …, expectedClass, newClass, manifest) (applied bool, err error)`. If `applied=false`, the
  worker discards the freshly written tier-2 chunks via the GC queue — this is intentional, the concurrent client write
  wins.
- **Multipart Complete uses LWT** — `IF status='uploading'` flips to `completing` so concurrent retries get
  `ErrMultipartInProgress` rather than racing to write the object row twice.

## Where to look when adding S3 surface

1. Read the corresponding entry in `tasks/prd-s3-compatibility.md` (or `ROADMAP.md` for older items).
2. Wire the route into `handleBucket` / `handleObject` in `internal/s3api/server.go`.
3. Add a sentinel error in `internal/meta/store.go` and an `APIError` in `internal/s3api/errors.go`.
4. Add the `Set/Get/Delete` triple to `meta.Store` and implement in both backends. Use the blob-config helpers if it's a
   single-document config.
5. Add a Cassandra schema migration via `alterStatements` or a new entry in `tableDDL`.
6. Tests: round-trip happy path, malformed body, not-configured 404. Bucket name `/bkt`.

## Commits and PRs

- Subject is `<area>: <imperative summary>` (e.g. `s3api: implement DeleteObjects`).
- Co-authored-by trailers are present on AI-assisted commits.
- Don't push `main` without an explicit ask. CI (`.github/workflows/ci.yml`) runs lint+build, unit (`-race`), Cassandra
  integration (testcontainers), Docker build (proves librados linking), and a full e2e via `smoke.sh` +
  `smoke-signed.sh` + RADOS integration tests.
- `s3-tests` pass-rate is the headline compatibility number. After meaningful surface changes, re-run
  `scripts/s3-tests/run.sh` and update `scripts/s3-tests/README.md` baseline section.

## Ralph autonomous runs

`scripts/ralph/` contains a Ralph loop runner that drives `claude --print` (or Amp) through `scripts/ralph/prd.json`. It
commits per story and writes `progress.txt`. `scripts/ralph/CLAUDE.md` is Ralph's *task prompt* — do not put project
knowledge there. This root `CLAUDE.md` is the project memory and is auto-loaded by every Claude Code invocation,
including Ralph's. Update this file (not Ralph's) when you discover something a future iteration should know.

## Roadmap maintenance

`ROADMAP.md` is the canonical project state list. It MUST stay an honest reflection of what is shipped vs pending at
every SHA. The PRDs in `tasks/` (and `scripts/ralph/prd.json`) are scoped to specific cycles and do NOT need to mirror
the roadmap — only `ROADMAP.md` is canonical.

This rule applies to **all** work — Ralph autonomous runs and human-driven commits alike.

**Closing a roadmap item.** Every commit that closes a `ROADMAP.md` item MUST flip the bullet to the format:

```
~~**P<n> — <title>.**~~ — **Done.** <one-line summary>. (commit `<sha>`)
```

…in the same commit. If the closing SHA is needed inline (commit-then-amend is undesirable here — see "Commits and PRs"
above), the immediate follow-up commit may carry the SHA edit instead.

Example diff shape (close-flip):

```diff
-- **P1 — Single-binary `strata` (CockroachDB-shape).** Today there are 10 `cmd/` binaries,
--  most of them background workers. Collapse `cmd/strata-{gateway,gc,...}` into a single
--  `cmd/strata` ...
++ ~~**P1 — Single-binary `strata` (CockroachDB-shape).**~~ — **Done.** Two binaries
++  (`strata`, `strata-admin`); workers selected via `STRATA_WORKERS=`. (commit `abc1234`)
```

**Discovering a new gap, latent bug, or regression.** Every commit that surfaces something not yet on the roadmap MUST
add a new entry in `ROADMAP.md` in the same commit. Place it under the appropriate severity section: P1 for correctness
or production-blockers, P2 for meaningful gaps expected by serious deployments, P3 for nice-to-haves and DX, or
`Known latent bugs` for live bugs.

Example diff shape (new discovery):

```diff
 ## Correctness & consistency

++- **P2 — Multipart UploadPart `Content-MD5` not validated.** `s3api.uploadPart` accepts the
++  client-supplied `Content-MD5` header but never recomputes/compares; mismatches silently
++  succeed. Add the check on the streaming-decoder hot path.
+
 - **P3 — Object Lock `COMPLIANCE` audit log.** ...
```

Code-only commits that touch neither a roadmap item nor a new gap do not need a roadmap edit.
