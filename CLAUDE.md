# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this project is

Strata is a Go-based, S3-compatible object gateway designed as a drop-in replacement for Ceph RGW. Metadata lives in
Cassandra (sharded `objects` table to dodge the bucket-index ceiling that bites RGW), data lives in RADOS as 4 MiB
chunks, and the gateway speaks S3 over HTTP. The compatibility goal is tracked against Ceph's upstream `s3-tests`
suite — see `tasks/prd-s3-compatibility.md` for the active PRD and `ROADMAP.md` for what is shipped vs pending.

The metadata interface (`internal/meta.Store`) is intentionally minimal (LWT semantics, clustering order, range scans).
Strata ships two first-class production backends: **Cassandra** (with **ScyllaDB** as a CQL-compatible drop-in — zero
code changes, gocql works unchanged) and **TiKV** (raw KV via `tikv/client-go`; native ordered range scans short-circuit
Cassandra's 64-way fan-out via the optional `meta.RangeScanStore` interface). Both are benchmarked, documented, and
maintained by the core team — see `docs/site/content/architecture/backends/tikv.md` and `docs/site/content/architecture/benchmarks/meta-backend-comparison.md`. The
in-memory backend is for tests and the smoke pass; no other backends are supported.

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
| Bring up bare-default TiKV stack (pd + tikv + ceph + ceph-b + strata-a/b + LB) | `make up-all && make wait-tikv && make wait-ceph && make wait-strata-lab` |
| Bring up Cassandra-backed regression lab (adds cassandra + strata-cassandra) | `make up-cassandra && make wait-cassandra && make wait-ceph`        |
| Run `strata server` against in-memory backends                         | `make run-memory`                                                        |
| Run `strata server` against Cassandra metadata + memory data           | `make run-cassandra`                                                     |
| Smoke pass                                                             | `make smoke` (signed: `make smoke-signed`)                               |
| Take stack down                                                        | `make down`                                                              |
| S3 compatibility suite                                                 | `scripts/s3-tests/run.sh` (see `scripts/s3-tests/README.md`)             |
| Hugo docs site — local preview on :1313                                | `make docs-serve` (Hugo extended required; theme is a git submodule)     |
| Hugo docs site — produce static bundle under `docs/site/public/`       | `make docs-build`                                                        |

macOS + lima: `make test-integration` needs `DOCKER_HOST=unix:///Users/.../.lima/.../sock/docker.sock` so testcontainers finds the engine.

**Compose shape**: TiKV-default 2-replica multi-cluster lab is canonical. Bare `docker compose up -d` →
`pd + tikv + ceph + ceph-b + strata-a + strata-b + strata-lb-nginx + prometheus + grafana`. Both replicas
`STRATA_META_BACKEND=tikv`, distinct `STRATA_NODE_ID`, share `strata-jwt-shared` volume (cross-replica JWT).
nginx LB `:9999` round-robins; direct ports `:10001`/`:10002`. `make up-cassandra` (profile `cassandra`) adds
`cassandra` + `strata-cassandra` (:9998) — backend stays first-class in code. Feature workers opt-in via
`STRATA_WORKERS` on the same container. Full note:
`docs/site/content/architecture/migrations/tikv-default-lab.md`.

## Big-picture architecture

```
                +-----------------------------+
                |  S3 client (aws-cli, mc)    |
                +--------------+--------------+
                               |
                  HTTP S3 (path-style URLs)
                               |
                +--------------v--------------+
                | cmd/strata server           |
                |  -> auth.Middleware (SigV4) |
                |  -> s3api.Server (router)   |
                +-------+--------------+------+
                        |              |
                        v              v
              +--------------------------+   +---------------------+
              | meta.Store               |   | data.Backend        |
              |  memory | tikv |cassandra|   |  memory | rados    |
              +---------+----------------+   +---------+----------+
                        |                              |
                +-------v---------+            +-------v-------+
                | TiKV (PD+TiKV,  |            | RADOS         |
                |  ordered scan)  |            | (4 MiB chunks)|
                |  -- lab default  |           +---------------+
                |   OR             |
                | Cassandra/Scylla |
                | (sharded fan-out;|
                |  --profile       |
                |   cassandra)     |
                +------------------+

  Leader-elected tick workers (each on `<name>-leader` unless noted):
  lifecycle    -> transitions/expirations/mp-abort. Per-bucket lease
                  `lifecycle-leader-<bid>` gated by `fnv32a(bid)%STRATA_GC_SHARDS`.
  gc           -> GCEntry queue → chunk delete. Per-shard fan-out (SkipLease).
  notify       -> notify_queue + DLQ → webhook/SQS via STRATA_NOTIFY_TARGETS.
  replicator   -> replication_queue → peer Strata HTTP PUT (HTTPDispatcher).
  access-log   -> access_log_buffer → AWS-format log object per flush into
                  PutBucketLogging target bucket.
  inventory    -> per (bucket, configID): walk source → manifest.json + CSV.gz
                  into target bucket.
  strata server --workers=rebalance -> internal/rebalance: migrates chunks off
                          draining clusters. Leader-elected on per-shard leases
                          `rebalance-leader-0..N-1` via `ShardedFanOut`
                          (SkipLease=true; mirrors `internal/gc/fanout.go`).
                          `STRATA_REBALANCE_SHARDS` count, `fnv32a(bucketID) %
                          shards` ownership via `ListBucketsShard`. Scan fires
                          ONLY for `state=evacuating` (readonly = stop-write, no
                          scan). Movers via MoverChain: `ceph`-tag RadosMover
                          (read src → write new OID → manifest CAS via
                          SetObjectStorage, CAS losers → GC queue); tag-free
                          S3Mover (CopyObject same-endpoint, else Get→Put).
                          Safety rails: refuses moves into draining or >90%-full
                          clusters. Completion `>0→0` fires one event/cluster
                          (audit `drain.complete` + notify fan-out).
                          Knobs/metrics/ProgressTracker internals + admin
                          drain/undrain/bucket-references endpoints: see
                          `internal/rebalance/` + cluster-state section below +
                          `docs/site/content/architecture/`.
                          DRAIN INVARIANT (always-strict, no env gate): RADOS+S3
                          `PutChunks` return `data.ErrDrainRefused` → HTTP 503
                          `DrainRefused` + `Retry-After: 300` when resolved
                          cluster draining. PUT-only stop-write — reads/deletes/
                          HEAD/in-flight-multipart keep working. Multipart routing
                          recovered from `BackendUploadID` handle
                          (`cluster\x00bucket\x00key\x00uploadID`), never
                          re-consults picker.
  audit-export -> drains audit_log partitions older than
                  STRATA_AUDIT_EXPORT_AFTER (default 30d) → gzipped JSON-lines
                  in export bucket, then deletes source partition.
  manifest-rewriter -> walks buckets, converts JSON manifest blobs → protobuf
                  in place. Idempotent. Cadence STRATA_MANIFEST_REWRITER_INTERVAL.
  usage-rollup -> nightly samples bucket_stats → one usage_aggregates row per
                  (bucket, storage_class, yesterday-UTC). Feeds billing.
  internal/reshard -> per-bucket online shard-resize, admin-driven one-shot
                  (/admin/bucket/reshard) or daemon — NOT a tick worker.
  strata admin rewrap -> one-shot SSE master-key rotation. Rewraps DEKs to
                  --target-key-id. Idempotent + resumable via rewrap_progress.
```

The S3 router is in `internal/s3api/server.go`. Bucket-scoped queries (`?cors`, `?policy`, `?lifecycle`, …) dispatch via
`handleBucket`; object-scoped (`?uploads`, `?uploadId=`, `?tagging`, …) via `handleObject`. New endpoints follow the
same query-string router pattern.

Auth lives in `internal/auth/`: SigV4 (`sigv4.go`), presigned URLs (`presigned.go`), streaming chunk decoder (
`streaming.go` — chain HMAC validation enforced via `computeChunkSignature` + `prevSig` chaining, mismatch returns
`ErrSignatureInvalid`, shipped under US-022), static credentials store (`static.go`). Identity flows
through context: `auth.FromContext(ctx).Owner`.

Virtual-hosted-style routing (`internal/s3api/vhost.go`): `STRATA_VHOST_PATTERN` is a comma-separated list of
`*.<suffix>` patterns (default `*.s3.local`; set to `-` to disable). Auth middleware runs first and signs the
original `Host` + `URL.Path`; `Server.ServeHTTP` then strips the prefix from `r.Host` and prepends `/<bucket>` to
`r.URL.Path` before path-style routing — never rewrite before SigV4 verification or signatures break.

## RADOS / cephimpl module split

`github.com/ceph/go-ceph` is **NOT** a direct require of the main module's
`go.mod`. The librados-linked backend lives in a separate Go module at
`internal/data/rados/cephimpl/` (`module github.com/danchupin/strata/cephimpl`),
which is the only place that pulls in `go-ceph`. `go.work` at the repo root
unifies main + cephimpl at dev time; IDEs auto-discover both.

`internal/data/rados/` (main module) keeps the shared shape — `Config`,
`ClassSpec`, `ClusterSpec`, `Metrics`, `DefaultCluster`, `ParseClasses`,
`ParseClusters`, `BuildClusters`, `ValidateClusterRefs`, plus exported
helpers used by cephimpl (`PutChunksParallel` + `ChunkPutFn`,
`NewPrefetchReader` + `ChunkGetFn`, `PutConcurrencyFromEnv`,
`GetPrefetchFromEnv`, `BatchOpsFromEnv`, `PoolSizeFromEnv`, `ObserveOp`,
`LogOp`, `BuildPendingPoolStatuses` + `PoolGroup` + `PendingPoolStatus`,
`NextRoundRobin`). `rados.New` is always-on (no build tag) and returns
`data.ErrRADOSNotCompiled`; serverapp + bench branch via `//go:build ceph`
files (`data_rados_ceph.go` + `data_rados_stub.go`,
`bench_rados_ceph.go` + `bench_rados_stub.go`) that either delegate to
`cephimpl.New` or return the not-compiled sentinel.

`cephimpl` exposes its own `RadosCluster` interface (structurally identical
to `internal/rebalance.RadosCluster`) so it does NOT need to import
`internal/rebalance` — the workspace MVS would otherwise re-load the whole
rebalance + tikv transitive closure. The ceph-tagged worker wiring
(`cmd/strata/workers/rebalance_movers_ceph.go`) converts
`map[string]cephimpl.RadosCluster` → `map[string]rebalance.RadosCluster`
at the call site.

**Workspace MVS pitfall**: `tikv/client-go/v2` transitively requires the
pre-split `google.golang.org/genproto@v0.0.0-20230331144136`. Combined with
main's direct requires on the split `googleapis/{api,rpc}` modules under
workspace MVS, the same import paths (`googleapis/api/httpbody`,
`googleapis/rpc/status`) get claimed by two modules → "ambiguous import"
build failure. `go.work` ships with a
`replace google.golang.org/genproto => v0.0.0-20240903143218-8af14fe29dc1`
directive to bump the monolithic module past the split so the conflicting
paths are owned only by the split modules.

**Hermetic check** (default-tag, no workspace): `GOWORK=off go mod graph |
grep -c go-ceph` → 0. Workspace-on graphs include go-ceph because cephimpl
itself requires it — that's expected and not a regression of the hermetic
invariant.

`make build` (default tag) produces a go-ceph-free binary;
`make build-ceph` (docker compose build) drives the ceph image which has
librados available. CI's `lint-build` job vets the main module on
ubuntu-latest without librados; the `e2e` job runs `make test-rados` which
shells into the ceph image and runs `cd internal/data/rados/cephimpl && go
test -tags integration ./...` against the real backend.

## Single-binary invariant

**ALL functionality lives in one `strata` binary.** Admin commands (rewrap, future one-shot tools) are subcommands of `strata`:

- `strata server` — gateway + workers (existing)
- `strata admin rewrap` — SSE master-key rotation
- `strata admin <future>` — operator one-shots

When adding a new operator-facing CLI feature, do NOT create a new top-level binary. Add it under `cmd/strata/admin/<name>.go` or extend the existing subcommand router. The single-binary shape is intentional — one Docker image, one entrypoint, one set of `--help` outputs, consistent flags + env handling.

Consolidation complete: the legacy `cmd/strata-admin` binary has been folded into `cmd/strata/admin` as a subcommand package. Do not regress by reintroducing a second binary.

## Background workers (cmd/strata/workers)

Workers under `strata server` register via `workers.Register(workers.Worker{Name, Build, SkipLease})` from a
per-worker `init()`. The `Build` constructor receives `workers.Dependencies` (Logger, Meta, Data, Tracer,
Locker, Region, EmitLeader) and returns a `workers.Runner`. `workers.Supervisor.Run(ctx, workers)` spins one
goroutine per requested worker; each goroutine acquires a `leader.Session` keyed on `<name>-leader`, builds +
runs the Runner under a supervised context, releases on exit, and recovers from panics. A panic increments
`strata_worker_panic_total{worker=<name>,shard="-"}`, releases the lease, and restarts on an exponential
backoff (1s → 5s → 30s → 2m, reset to 1s after 5 minutes healthy). Lease loss restarts immediately (no
backoff). One worker's panic or lease loss never affects the gateway or sibling workers.

Workers owning leader-election internally (gc fan-out: per-shard `gc-leader-<shardID>` leases, `STRATA_GC_SHARDS`)
register `SkipLease: true` — supervisor skips the outer `<name>-leader` lease but STILL owns panic recovery +
backoff. The runner manages its own leases and MUST call `deps.EmitLeader(name, acquired)` on each transition so
the heartbeat `leader_for=<name>` chip flips. Per-shard panics increment
`strata_worker_panic_total{worker,shard="<i>"}` (shard `"-"` for non-fan-out).

`cmd/strata/server.go::runServer` validates `STRATA_WORKERS` (or `--workers=`) via `workers.Resolve` BEFORE any
backend is built — unknown names exit 2 immediately. The resolved `[]workers.Worker` is then handed to
`internal/serverapp.Run`, which builds the leader-election locker (cassandra → LWT lease, memory →
process-local) and spawns `workers.Supervisor.Run` in a goroutine alongside the gateway. When adding a new
worker, register from `cmd/strata/workers/<name>.go`'s `init()` and let the binary pick it up; do not spawn a
goroutine ad-hoc inside `internal/serverapp` — the supervisor owns the lifecycle. `gc` reads
`STRATA_GC_INTERVAL` / `STRATA_GC_GRACE` / `STRATA_GC_BATCH_SIZE` / `STRATA_GC_CONCURRENCY` /
`STRATA_GC_SHARDS` (default 1, range [1, 1024]) from env at Build time (no per-worker flags); other workers
follow the same env-only convention.

Every tick-loop worker MUST wrap its iteration body in `metrics.ObserveWorkerTick(name, err, time.Since(start))`
at the same site as `strataotel.StartIteration` / `strataotel.EndIteration` so the per-worker Grafana dashboard
(`deploy/grafana/dashboards/strata-workers.json`) gets `strata_worker_iteration_total{worker, status}` +
`strata_worker_tick_duration_seconds{worker}` populated 1:1 with the OTel iteration span. The helper coerces
empty `name` → `"unknown"` and routes nil err → `status="success"` / non-nil → `"error"`. When adding a new
worker, also extend `internal/metrics/metrics.go::registeredWorkerNames` so the prewarm seeds zero-valued
series and the dashboard's `label_values(strata_worker_iteration_total, worker)` picker exposes it on boot.
Reshard is intentionally absent — it is admin-driven one-shot, not a continuous tick.

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
is selected by `data.SetManifestFormat("proto"|"json")`, default `proto`. `internal/serverapp` (the shared
gateway entrypoint) reads `STRATA_MANIFEST_FORMAT` once at startup and calls `SetManifestFormat`. Reads always go
through `data.DecodeManifest` which sniffs the first non-whitespace byte (`{` → JSON, anything else → proto3 wire
format) so JSON-vs-proto migrations are transparent. New fields tagged `json:",omitempty"` (and a fresh `protobuf` tag
in `manifest.proto` + helper updates in `manifest_codec.go`) are schema-additive — old rows decode with zero-values,
and you avoid an `ALTER`. Use this for per-object metadata the GET path reads but Cassandra never filters on (e.g.
`Manifest.PartChunkCounts` for the SSE multipart locator, `Manifest.PartChunks []PartRange` for ?partNumber=N GET).
**Field-rename gotcha**: when the new field collides with an existing JSON key, rename the old Go field, drop its
JSON tag, and write a custom `UnmarshalJSON` on `Manifest` that sniffs `json.RawMessage` of the colliding key —
try the new shape first, fall back to the legacy shape. The proto side stays wire-compatible if you keep the
field number and only rename the label. To convert pre-existing JSON rows to proto, run
`strata server --workers=manifest-rewriter` (leader-elected, idempotent — re-runs skip already-proto rows).

## pprof endpoint (US-004 prod-observability)

`/debug/pprof/*` opt-in via `STRATA_PPROF_ENABLED=true` (default off). Routes wired explicitly in
`serverapp/pprof.go` — **NEVER `_ "net/http/pprof"` side-effect import** (Strata doesn't serve
`http.DefaultServeMux`). Attach precedence: `STRATA_PPROF_LISTEN` (dedicated listener) > `STRATA_ADMIN_LISTEN`
(admin mux) > both-empty errors at boot (MUST NOT share S3 hot path). Auth via `WrapWithAdminAuth`. Block/mutex
profiles need rate flips (`STRATA_PPROF_BLOCK_RATE`/`_MUTEX_RATE`, default 0).
**`google/pprof` must stay a direct require** — referenced from non-test `internal/pprofutil/decode.go` so
`go mod tidy` doesn't re-mark it `// indirect`. Operator config (modes, smoke scripts): see `docs/`.

## Logging (slog)

`internal/logging` is canonical. `cmd/strata` calls `logging.Setup()` first thing → JSON `*slog.Logger` driven by
`STRATA_LOG_LEVEL` (default INFO). Workers take `*slog.Logger`, never `*log.Logger`. Gateway wraps mux with
`logging.NewMiddleware` — reads/generates `X-Request-Id`, sets it on both `r.Header` (downstream middlewares read
via `r.Header.Get`) and `w.Header()`, attaches a `request_id`-bound child logger to ctx. In handlers use
`logging.LoggerFromContext(r.Context()).InfoContext(...)` and the `*Context` variants so ctx-bound loggers ride through.

Audit log: `s3api.AuditMiddleware` appends one `audit_log` row per state-changing request (PUT/POST/DELETE;
GET/HEAD/OPTIONS skipped). Sits inside `mw.Wrap` so auth-deny rows still emit. TTL `STRATA_AUDIT_RETENTION`
(default 30d; Cassandra `USING TTL`, memory prunes lazily). IAM `?Action=` rows carry `BucketID=uuid.Nil` +
`Resource="iam:<Action>"`. Best-effort — meta failure never fails the request. `/admin/v1/*` also wrapped:
handlers stamp `s3api.SetAuditOverride(ctx, action, resource, bucket, principal)` with `action=admin:<Verb>`;
empty Action → path-derived fallback. **Add the override stamp to every new admin write** (GET listings skip
via `auditableMethod`).

Health probes: `internal/health.Handler` serves `/healthz` (always 200) + `/readyz` (fans probes, 1s timeout).
Probes injected via type-assertion on `cassandraProber`/`radosProber` in `serverapp.go::buildHealthHandler` (keeps
package import-free). `cassandra.Store.Probe` = `SELECT now()`; `rados.Backend.Probe` stats canary OID
(`STRATA_RADOS_HEALTH_OID`), `ErrNotFound`=success. Memory backends register no probe (always ready). Both endpoints
ahead of `/` on the mux → bypass auth + access-log.

Per-storage observers: `cassandra.SessionConfig{Logger, SlowMS}` → `SlowQueryObserver` logs WARN for queries over
`STRATA_CASSANDRA_SLOW_MS` (default 100) or with errors. `rados.Config.Logger` → per-op DEBUG via `LogOp`. Both pull
`request_id` via `logging.RequestIDFromContext` for correlation. RADOS observer helper is build-tag-free (unit-testable
without librados).

OpenTelemetry tracing: `internal/otel.Init` reads `OTEL_EXPORTER_OTLP_ENDPOINT` + `STRATA_OTEL_SAMPLE_RATIO`
(default 0.01) + ringbuf toggle (`STRATA_OTEL_RINGBUF` default `on`). Tail-sampler decides at OnEnd so failing
spans (`status=Error` OR `http.status_code >= 500`) always export regardless of ratio. Ringbuf retains every
span under a bytes-budgeted LRU; `/admin/v1/diagnostics/trace/{requestID}` reads it. HTTP middleware
(`internal/otel.NewMiddleware`) wired ahead of logging middleware so the span covers the full request. Empty
endpoint + ringbuf off → noop provider (callers stay nil-free). Per-storage spans piggyback on the observer
hooks (`cassandra/rados/tikv` `.Tracer`); S3-backend uses upstream `otelaws` middleware. Wiring in
`serverapp.go::buildMetaStore`/`buildDataBackend` after `strataotel.Init`.

**semconv import version MUST match the SDK's `resource.Default()` schema URL** — SDK 1.41 → `semconv/v1.39.0`,
SDK 1.43 → `semconv/v1.40.0`. Mismatch fails at runtime with "conflicting Schema URL". Bump together.

Workers emit per-iteration spans. **Adding a worker: call `deps.Tracer.Tracer("strata.worker.<name>")` in
`Build` and wrap the tick body in `workers.StartIteration`/`EndIteration` — no struct change.** `tracer == nil`
→ `strataotel.NoopTracer()`. Every gateway observer stamps `strata.component=gateway`, workers
`=worker` (`internal/otel/component.go`) for one-query Jaeger filtering. Coverage matrix + span-name list +
per-worker filter recipes: [docs/site/content/best-practices/tracing.md](docs/site/content/best-practices/tracing.md).
`strata admin rewrap` stays untraced.

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

## TiKV gotchas (real ones, hit during the US-001..US-018 cycle)

- **Plain `Put` on a key with prior LWT history breaks read-after-write coherence — same lesson as Cassandra LWT-on-LWT.**
  Any RMW that needs read-after-write coherence must use a pessimistic txn (`Begin(pessimistic) → LockKeys → Get → Set →
  Commit`), not a plain `Put`. `SetBucketVersioning` / `SetBucketACL` / `SetReshardState` / IAM access-key flips all use
  the `updateBucket`-shaped helper. Bypassing the abstraction is how this gets reintroduced.
- **TiKV has no native TTL.** Cassandra's `USING TTL` lets the storage tier expunge expired rows for free; on TiKV every
  expirable row carries `ExpiresAt` in the payload, readers lazy-skip expired rows on `Get`/`List`, and a leader-elected
  sweeper goroutine (`internal/meta/tikv/sweeper.go`) eager-deletes them in the background. Both halves are required —
  lazy filter alone leaks disk; sweeper alone leaves a window where reads see stale rows.
- **Pessimistic txns with EARLY-RETURN paths must call `txn.Rollback()` explicitly.** `defer rollbackOnError(txn, &err)`
  fires only when `err != nil`. A CAS-reject path that returns `applied=false, nil` (e.g. `SetObjectStorage`) leaks the
  `LockKeys` lease for the txn lifetime — and in the in-process `memBackend` (used by unit tests) it deadlocks the next
  caller forever. Any non-error early return MUST `txn.Rollback()` first.
- **`testutils.NewMockTiKV` is NOT a full transactional fake.** Pessimistic-txn `Commit` hangs on heartbeat indefinitely
  against the in-process mock, even though `LockKeys`/`Get`/`Set` succeed. Use real PD+TiKV containers for any
  contract-level exercise; reserve the mock for low-level RPC bench (single-Get RPC shape, no commit). The memory
  backend (`internal/meta/memory`) is the parity oracle for surface contract.
- **Variable-length string segments in keys use FoundationDB-style byte-stuffing** (`0x00 → 0x00 0xFF`, terminator
  `0x00 0x00`) to preserve lex ordering across heterogeneous lengths. Never add length-prefixed encoding to a key — it
  breaks "all keys starting with prefix X" scans. See `internal/meta/tikv/keys.md`.
- **Object version-DESC ordering** uses a 24-byte suffix `[MaxUint64-ts8-BE][raw-uuid-16]`. Inverted ts makes ascending
  range scan emit latest version first (free GET-without-versionId path). Null sentinel UUID (timestamp 0) sorts last
  among versions of a key — gateway resolves `?versionId=null` by exact lookup, not scan position.
- **Fixed-width integer fields** (`partNumber`, day-epoch, shardID) use big-endian uintN so forward range scans return
  them in ascending numerical order. Never use little-endian or varint there.
- **`testcontainers-go`'s `host.docker.internal` advertise pattern** works on Docker Desktop and Linux CI runners (via
  `ExtraHosts: host.docker.internal:host-gateway`) but does NOT resolve from the macOS host on Lima docker contexts.
  Tests must `t.Skipf` when the gateway alias misses. The CI workflow (`.github/workflows/ci-tikv.yml`) sidesteps this
  by using `STRATA_TIKV_TEST_PD_ENDPOINTS` against a docker-compose-managed PD.
- **PD ≥3 in production for raft majority.** Two PD nodes survive no failure (split-brain risk on partition). PD is
  small; the cost is negligible. TiKV ≥3 is the default region raft factor.
- **`docker-compose profiles` cannot toggle env on a single service.** Mutually-exclusive shapes (cassandra-backed
  strata vs tikv-backed strata) get distinct service names sharing the same image. See `strata-tikv` (profile `tikv`)
  vs `strata` (default) in `deploy/docker/docker-compose.yml`.
- **TiKV's upstream container image has no clean HTTP healthcheck contract.** The status server returns plain text on a
  non-stable contract and the alpine-glibc base ships no curl. PD's `/pd/api/v1/health` is HTTP-shaped and stable;
  downstream consumers wait on `pd: service_healthy` and `tikv: service_started`. The TiKV client retries until PD
  assigns regions, so transient boot races are absorbed in the application layer.
- **`koanf` env provider stores env values as raw strings — no comma-split into `[]string`.** Multi-value config (TiKV
  PD endpoints) keeps `Config.TiKV.Endpoints` as `string` and splits with `strings.Split` + `TrimSpace` + drop-empty
  at use-site. Cleaner than wiring a custom mapstructure decode hook.
- **`bucket_stats` live counter is fan-out, not single-key.** TiKV stores 8 shards `s/B/<bid>/bs/<shard>`;
  `BumpBucketStats` picks via `fnv32a(uuid.NewString()) % 8` (FRESH uuid per call — hashing on a stable key
  collapses the fan-out); `GetBucketStats` sums all 8 in one snapshot txn. Cassandra unchanged (LWT CAS loop
  `maxAttempts=32`). Distribution via `strata_bucket_stats_shard_writes_total{shard}` — one shard dominating =
  picker misused.

## Cluster state machine — 5 states + per-cluster weight

`cluster_state` rows model the cross-cluster lifecycle. 5 states (`meta.ClusterState*`) + per-cluster `weight ∈ [0,100]`.
Picker behavior is what matters for code:

- `pending` — new env cluster, no chunks yet. Excluded from weight wheel; explicit bucket policy still routes there. `/activate` → live.
- `live` — `weight` drives default-routing share. `weight=0+live` legal (reads + explicit policy work, no new default writes). `/weight` adjusts.
- `draining_readonly` / `evacuating` — both excluded from picker; PUT landing here → 503 `DrainRefused`. `evacuating` additionally scanned+migrated by rebalance worker (`deregister_ready` when chunks→0). `/drain {mode}`, `/undrain` deletes row (absence==live).
- `removed` — tombstone, excluded everywhere.

Transitions rejected with `409 InvalidTransition`. Boot reconcile auto-inits `(no row)→pending` (or `→live weight=100` if `bucket_stats` already references it). States snapshot via `placement.DrainCache.States(ctx)` (30s TTL, admin handlers invalidate synchronously).

**Two weight layers — NEVER COMBINE (picker call sites `rados.Backend.PutChunks` + `s3.Backend.clusterForPlacement`
MUST short-circuit on `Placement != nil` BEFORE consulting weights — else bucket-policy-wins breaks):**

- `bucket.Placement != nil` → policy = bucket.Placement, cluster.weight IGNORED.
- `Placement == nil` AND class spec.Cluster == "" → synthesise `{<live>: <weight>}` via `placement.DefaultPolicy`.
- Class env `@cluster` suffix sets `spec.Cluster` → bypass synthesis (explicit per-class pin).

**Per-bucket `PlacementMode` ∈ {`weighted` (default), `strict`}** — opt-in compliance pin atop the
bucket-policy-wins invariant. Resolution in `placement.EffectivePolicy(bucketPolicy, mode, clusterWeights,
clusterStates)` (`internal/data/placement/policy.go`): live-subset of bucket policy wins; if empty AND policy
non-empty AND `strict` → return nil (compliance refuse → 503 `DrainRefused`, preserves data-sovereignty pin);
else fall back to live-subset of cluster-weights wheel. `mode==""` coerced to `weighted` via
`meta.NormalizePlacementMode`. Both PUT picker sites route through `EffectivePolicy`; rebalance classifier
`ClassifyBucket` derives `migratable | stuck_single_policy | stuck_no_policy` from the same triple (drives
`/drain-impact` + `<BulkPlacementFixDialog>` "flip to weighted"). Persisted all 3 backends; admin
`PUT /admin/v1/buckets/{name}/placement` body takes optional `mode`, audit `admin:UpdateBucketPlacementMode`.

Boot reconcile (`internal/serverapp/cluster_reconcile.go`) is idempotent — re-running it creates no duplicates and
overwrites nothing. Lives between drain-cache wiring and listener start; fail-soft on transient meta errors so a
gocql hiccup doesn't block gateway startup.

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
- **`multipart_uploads.cluster` column** (US-004 drain-followup) carries the leading cluster id component of
  `BackendUploadID` (`<cluster>\x00<bucket>\x00<key>\x00<uploadID>` — see `internal/data/s3/multipart.go`). The
  drain-progress safety gate probes it via `ListMultipartUploadsByCluster` so the `deregister_ready=true` flip
  refuses to fire while any S3-pass-through multipart session is still bound to the cluster about to be removed.
  Chunk-based RADOS uploads leave `BackendUploadID` empty and thus persist `NULL` — these never match a
  per-cluster probe (the chunk-based router has no init-time cluster binding). Pre-US-004 rows have `NULL` in
  the column and are tolerated on read (NULL never matches any clusterID) — no one-shot migration is required.
- **Denormalised `_by_cluster` lookup tables** (US-005 drain-followup): `gc_entries_by_cluster` mirrors
  `gc_entries_v2` and `multipart_uploads_by_cluster` mirrors `multipart_uploads`, both partitioned on
  `(cluster)` so `ListChunkDeletionsByCluster` / `ListMultipartUploadsByCluster` are single-partition scans —
  no `ALLOW FILTERING` on the drain hot path. **Dual-write rule**: every `EnqueueChunkDeletion` /
  `AckGCEntry` / `CreateMultipartUpload` / `CompleteMultipartUpload` / `AbortMultipartUpload` MUST keep the
  primary table and the lookup row in lockstep (skip the dual-write when cluster id is empty — chunk-based
  uploads + legacy rows). Boot reconcile in `internal/serverapp` (`metacassandra.Store.ReconcileLookupTables`)
  backfills missing lookup rows once per process from the legacy tables; idempotent re-runs are upserts with
  the same payload.

## Where to look when adding S3 surface

1. Read the corresponding entry in `tasks/prd-s3-compatibility.md` (or `ROADMAP.md` for older items).
2. Wire the route into `handleBucket` / `handleObject` in `internal/s3api/server.go`.
3. Add a sentinel error in `internal/meta/store.go` and an `APIError` in `internal/s3api/errors.go`.
4. Add the `Set/Get/Delete` triple to `meta.Store` and implement in both backends. Use the blob-config helpers if it's a
   single-document config.
5. Add a Cassandra schema migration via `alterStatements` or a new entry in `tableDDL`.
6. Tests: round-trip happy path, malformed body, not-configured 404. Bucket name `/bkt`.

## Engineering rules (`.claude/rules/`)

Consult these before writing tests or reviewing — they own the substance the
caveman style does not:

- `.claude/rules/test-discipline.md` — never weaken an assertion to go green (a
  red test usually found a real bug — fix the source first, not the test); bug
  fixes need red/green proof; every `t.Skip` cites a ROADMAP P-item or a concrete
  env condition + reason (no bare/stale skips — re-validate when you touch the
  area); table-driven canons (inputs-first, `expected`-prefixed, one concern per
  case, name = intent); reuse harnesses (`newHarness`/`storetest`/`racetest`),
  don't fork; CI (Linux) is the source of truth.
- `.claude/rules/review.md` — severity-gate findings (blocking / suggestion / nit,
  suppress nits unless asked); never silently drop a review aspect (name it);
  Strata's high-value correctness lenses (LWT/CAS coherence, drain stop-write,
  GC double-delete, concurrency, leaks, auth boundary); pre-launch = ignore
  backwards-compat / rolling-upgrade / migration concerns (noise here).

## Commits and PRs

- Subject is `<area>: <imperative summary>` (e.g. `s3api: implement DeleteObjects`).
- Co-authored-by trailers are present on AI-assisted commits.
- Don't push `main` without an explicit ask. CI (`.github/workflows/ci.yml`) runs lint+build, unit (`-race`), Cassandra
  integration (testcontainers), Docker build (proves librados linking), and a full e2e via `smoke.sh` +
  `smoke-signed.sh` + RADOS integration tests.
- `s3-tests` pass-rate is the headline compatibility number. After meaningful surface changes, re-run
  `scripts/s3-tests/run.sh` and update `scripts/s3-tests/README.md` baseline section.

## Ralph autonomous runs

`scripts/ralph/` drives `claude --print` through `prd.json`, commits per story, writes `progress.txt`.
`scripts/ralph/CLAUDE.md` is Ralph's *task prompt* — project knowledge goes HERE (root, auto-loaded), not there.

**PRD lifecycle — canonical record is the Ralph snapshot.** Markdown `tasks/prd-<feature>.md` is a disposable
design draft. After cycle prep, truth is the auto-archived `scripts/ralph/archive/<date>-<branch>/{prd.json,progress.txt}`
(via `archive_cycle`). On close-flip: DELETE the markdown from `tasks/` (don't copy to `tasks/archive/`).
`tasks/archive/` is only for design intent with no Ralph snapshot.

## Roadmap maintenance

`ROADMAP.md` is the canonical shipped-vs-pending list at every SHA (PRDs in `tasks/` are cycle-scoped, don't mirror).
Applies to Ralph + human commits alike.

- **Close an item** in the same commit: `~~**P<n> — <title>.**~~ — **Done.** <summary>. (commit `<sha>`)`.
- **Surface a new gap/bug** → add a `ROADMAP.md` entry same commit, severity P1 (correctness/prod-blocker) / P2
  (meaningful gap) / P3 (DX) / `Known latent bugs`.
- Code-only commits touching neither need no roadmap edit.
