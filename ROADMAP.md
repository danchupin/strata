# Strata roadmap

MVP (phases 0–8) is complete. The "modern complete" Ralph cycle (US-001..US-049 on
`ralph/modern-complete`) closed the bulk of the original P1/P2/P3 backlog: SSE-S3 + SSE-KMS,
notifications, replicator, access-log delivery, structured logs, full Prometheus + Grafana,
health probes, virtual-hosted URLs, audit log + export, per-part GET, versioning null literal,
website redirects, OTel tracing through Cassandra and RADOS, admin CLI, race harness, Inventory,
Access Points, ScyllaDB CI, multi-cluster RADOS, online shard resize, examples, protobuf manifest.

Next phase is **consolidation and validation**, not feature breadth. Items below are labeled
as before:
- **P1** — correctness or production-blockers
- **P2** — meaningful gaps; expected for serious deployments
- **P3** — nice-to-have, visibility, DX

S3-compatibility headline: **91.5% (162/177)** on the executable subset of `ceph/s3-tests`. See
`scripts/s3-tests/README.md` for the gap breakdown.

---

## Consolidation & validation (the new top of the stack)

These are not feature work. The codebase shipped a lot of surface in a short window; before
adding more, prove what is there.

- ~~**P1 — Single-binary `strata` (CockroachDB-shape).**~~ — **Done.** Two binaries
  (`strata`, `strata-admin`); `strata server` runs the gateway plus an opt-in subset of
  workers (gc, lifecycle, notify, replicator, access-log, inventory, audit-export,
  manifest-rewriter) selected via `STRATA_WORKERS=`. Each worker keeps its own
  `internal/leader` lease keyed on `<name>-leader`. SSE master-key rotation moved to
  `strata-admin rewrap`. One Docker image, one compose service. (commit `ae4e338`)
- **P1 — Race harness as a real test, not a gate.** `internal/s3api/race_test.go`
  exists as an integration scenario (drives mixed PUT/GET/DELETE/multipart concurrency),
  but the dedicated `internal/racetest` package + `cmd/strata-racecheck` binary
  outlined in `tasks/prd-race-harness.md` were never shipped. Land the duration-bounded
  binary, run it ≥1 h against Cassandra+RADOS, record observed inconsistencies (or
  zero, with the workload that proves it). Add the run to CI on a nightly schedule
  so regressions surface.
- ~~**P1 — s3-tests 80% → 90%+.**~~ — **Done.** Lifted to **91.5% (162/177)** by the
  `ralph/s3-compat-95` cycle (US-001..US-006 — multipart copy range-parser + special-char
  URL handling, ?partNumber=N GET wire shape flipped to whole-object multipart ETag,
  CRC32 / CRC32C / CRC64NVME `FULL_OBJECT` composite combine math, multipart Complete
  `If-Match`-on-missing-object → 404 NoSuchKey AWS-parity, suspended-bucket GET
  stale-row dual-probe, missing-bucket DELETE → 404 ahead of auth, ListObjectVersions
  Owner DisplayName, validate-then-flip on Complete in cassandra+memory). 4 real failures
  remain (3 multipart-copy GET-side checksum echo, 1 duplicate-PartNumber Complete) —
  filed as separate P2 entries. See `scripts/s3-tests/README.md` for the per-test gap
  breakdown. (commit `494b62b`)
- **P2 — Benchmarks vs RGW.** "Drop-in RGW replacement" is unproven without numbers. Run
  `warp` and `cosbench` against both gateways on the same RADOS cluster. Publish absolute
  latency / throughput per workload class (small-object PUT, large-object GET, listing,
  multipart) in a dedicated `docs/benchmarks/` directory. Update on each release.
- **P2 — ScyllaDB benchmarks.** `docs/backends/scylla.md` (US-042) documents the path; the
  expected 3–5× LWT speedup on Paxos hot paths (bucket-create, versioning-flip,
  multipart-complete) needs measurement. Same harness as the RGW benches, swap the
  metadata backend.
- ~~**P3 — Drop unused background daemons.**~~ — **Done.** Default compose stack
  is `cassandra + ceph + strata` (gateway + gc + lifecycle); the feature workers
  (notify / replicator / access-log / inventory / audit-export) live behind a
  single `--profile features` `strata-features` replica. (commit `5841043`)
- ~~**P1 — TiKV first-class metadata backend.**~~ — **Done.** `internal/meta/tikv`
  implements the full `meta.Store` surface; native ordered range scans via the
  optional `meta.RangeScanStore` interface short-circuit Cassandra's 64-way
  fan-out on `ListObjects` (~5× faster on a 100k-object bucket per
  `docs/benchmarks/meta-backend-comparison.md`). Wired through
  `STRATA_META_BACKEND=tikv` + `STRATA_TIKV_PD_ENDPOINTS`; ships with
  `--profile tikv` in compose, `make up-tikv` / `make smoke-tikv`,
  `.github/workflows/ci-tikv.yml`, race-soak coverage, contract suite parity,
  and `docs/backends/tikv.md`. Memory is now tests-only; the previously listed
  community backends (FoundationDB, PostgreSQL+Citus / Yugabyte) are dropped
  from the roadmap. (commit `40b45de`)

## Correctness & consistency

- ~~**P2 — Multipart copy edges (UploadPartCopy).**~~ — **Done.** US-001 closed the
  copy-source-range parser (`internal/s3api/multipart.go::parseCopySourceRangeStrict`
  splits 400 InvalidArgument syntax errors from 416 InvalidRange out-of-bounds) and
  the special-char URL handling (`copy_object.go::parseCopySource` splits on `?`
  before `url.PathUnescape` so literal `?` in keys round-trips). `_improper_range`
  passes. The `_small` / `_special_names` / `_multiple_sizes` tests still fail on a
  separate axis (GET-side checksum echo on the destination — see new entry below).
  (commit `968a32a`)

- **P2 — Multipart copy GET-side checksum echo divergence.** Three s3-tests still
  fail (`test_multipart_copy_small`, `_special_names`, `_multiple_sizes`): GET on
  the multipart-copied destination object emits an `x-amz-checksum-*` header that
  does not match the actual stored bytes, so boto3's `FlexibleChecksumError` fires
  on response read (`Expected checksum X did not match calculated checksum: Y`).
  Source has random content; dest is built via `UploadPartCopy`. The data plane
  needs to recompute the destination's per-part / whole-object checksum from the
  actual stored bytes rather than echoing the source manifest's composite. Surfaced
  by `ralph/s3-compat-95` rerun.

- ~~**P2 — `?partNumber=N` GET quoted-ETag wire shape.**~~ — **Done.** US-002
  flipped the wire shape: `?partNumber=N` GET / HEAD now echoes the WHOLE-OBJECT
  multipart ETag (`"<32hex>-<count>"`), matching `complete_multipart_upload`'s
  response — not the per-part ETag. Out-of-range `partNumber` returns
  `400 InvalidPart` (was 416 InvalidRange). Non-multipart `?partNumber=1` returns
  the whole object 200 OK; `?partNumber=N` for N≥2 returns 400 InvalidPart. Three
  s3-tests pass: `test_multipart_get_part`, `_sse_c_get_part`, `_single_get_part`.
  (commit `6b4e304`)

- ~~**P2 — Multipart composite checksum CRC32 / CRC32C / CRC64NVME.**~~ — **Done.**
  US-003 added the AWS `FULL_OBJECT` composite shape for the CRC family in
  `internal/s3api/checksum.go::combineCRCParts` — standard zlib `crc32_combine` /
  `crc64_combine` math (gf2 matrix square+times) over per-part (CRC, size) yields
  the whole-stream CRC. SHA1 / SHA256 stay COMPOSITE (`BASE64(HASH(concat(raw_i)))-N`).
  Three s3-tests pass: `test_multipart_use_cksum_helper_crc32`, `_crc32c`,
  `_crc64nvme`. (commit `798ab58`)

- ~~**P2 — Multipart `If-Match`-on-missing-object error code.**~~ — **Done.** US-004
  reverted CompleteMultipartUpload's `If-Match`-on-missing-object branch from
  RFC 7232 §3.1's `412 PreconditionFailed` to AWS S3's `404 NoSuchKey`. The
  ETag-mismatch-on-existing-object path stays `412`. `putObject`'s single-PUT path
  is unchanged (per RFC). Two s3-tests pass: `test_multipart_put_object_if_match`,
  `_put_current_object_if_match`. (commit `8b5ca84`)

- ~~**P1 — Cassandra null-version timeuuid sentinel.**~~ — **Done.** Fixed in the
  prior `ralph/s3-compat-90` cycle (cassandra-internal v1 sentinel
  `00000000-0000-1000-8000-000000000000` translated at every gocql.ParseUUID /
  .String() boundary; `internal/meta/cassandra/store.go::versionToCQL`). The
  `ralph/s3-compat-95` cycle's US-006 added the missing CI coverage:
  `internal/meta/cassandra/store_integration_test.go::TestCassandraNullSentinelOnDisk`
  asserts the on-disk `objects.version_id` is the v1 sentinel while
  `meta.NullVersionID` surfaces the canonical zero UUID to clients, plus a
  Suspended-mode INSERT step in the storetest contract `caseVersioningNullSentinel`.
  CI integration job runs `go test -v` so the test name prints, catching skip
  regressions. (commit `494b62b`)

- ~~**P1 — `koanf` env provider does not skip empty env values.**~~ — **Done.**
  Fixed in the prior `ralph/s3-compat-90` cycle (`env.Provider` →
  `env.ProviderWithValue` with an empty-skip callback;
  `internal/config/config.go::Load`). The `ralph/s3-compat-95` cycle's US-006 added
  `internal/config/config_test.go::TestLoadEmptyEnvDoesNotOverrideTOMLValue` that
  routes through `STRATA_CONFIG_FILE` (TOML default `gc.interval=30s`) +
  `STRATA_GC_INTERVAL=""` and asserts `cfg.GC.Interval == 30*time.Second`. Locks
  the empty-skip callback against future regression. (commit `494b62b`)

- ~~**P2 — Multipart Complete leaks `completing` state on per-part ETag mismatch.**~~
  — **Done.** US-005 flipped the cassandra + memory backends' `CompleteMultipartUpload`
  to validate-then-flip: ListParts + per-part ETag walk runs first, only then does
  the LWT (cassandra) / in-place mutation (memory) flip status to `completing`. A
  stale-ETag retry now leaves the upload in `uploading` and a corrected retry
  succeeds. TiKV was already correct (deferred `rollbackOnError(txn, &err)` rolls
  the flip back on validation failure). Note: `test_multipart_resend_first_finishes_last`
  still fails on a separate axis (duplicate PartNumber in Complete XML — see new
  entry below). (commit `ba9368a`)

- **P2 — Multipart Complete rejects duplicate PartNumber in Parts list.**
  `internal/s3api/multipart.go::completeMultipartUpload` enforces strict-ascending
  PartNumber via `if p.PartNumber <= prev` and returns `InvalidPartOrder`. The
  upstream s3-test `test_multipart_resend_first_finishes_last` re-uploads
  `PartNumber=1` while a prior `PartNumber=1` upload is mid-stream and then sends
  Complete with a `Parts` list containing BOTH ETags under the same PartNumber.
  AWS / RGW dedupe duplicates by taking the latest storage-side write; Strata
  rejects. Fix: relax the strict-ascending check to `p.PartNumber < prev` (allow
  equal) and dedupe in-handler before the per-part validation, OR validate the
  LAST ETag for each unique PartNumber against the latest stored part. Surfaced
  by `ralph/s3-compat-95` rerun.

- **P3 — Object Lock `COMPLIANCE` audit log.** `audit_log` (US-022) records all
  state-changing requests, but a denied DELETE under `COMPLIANCE` is not flagged
  distinctly. Regulated customers want a queryable "blocked retention violation" feed —
  add a typed `audit.Event.Reason` field that `audit_log` reads to filter.

## Auth

- ~~**P3 — STS / assume-role.**~~ — **Done.** Minimal AssumeRole endpoint at `?Action=AssumeRole` (`internal/s3api/sts.go` + `internal/auth/sts.go`). Issues a temporary credential triple (`STSSession{AccessKey, Secret, SessionToken}`) with `DefaultSTSDuration` validity; verifier honours `SessionToken` on subsequent SigV4 requests. (commit `cec9c06`)
- **P3 — Per-bucket request signing keys (KMS-backed).** Rotate the signing material on
  a schedule, reject keys older than `STRATA_KEY_MAX_AGE`. Hooks onto the existing
  Vault provider.

## Scalability & performance

- **P1 — gc / lifecycle workers serialise inside a single goroutine; throughput cap ~50–500 ops/s.** `internal/gc/worker.go::drainCount` and `internal/lifecycle/worker.go` walk entries with `for _, e := range entries { … }` — every chunk delete or object expire blocks on a sequential RADOS round-trip + meta ack. At one chunk/object per round-trip, a single worker tops out around 50–200 chunks/s (gc) and 100–500 objects/s (lifecycle). For prod-scale churn (10k object PUTs/s × ~4 chunks each = 40k chunks/s) the queue grows linearly forever. Phase 1: bounded `errgroup` inside the existing single-leader worker (`STRATA_GC_CONCURRENCY` / `STRATA_LIFECYCLE_CONCURRENCY`, default 1, max ~256) hides the round-trip latency without changing leader-election semantics. Phase 2 (separate cycle): sharded leader-election (`gc-leader-0..N-1`, per-bucket `lifecycle-leader-<bucket>`) so multiple replicas process disjoint slices in parallel — needs new `Meta.ListGCEntriesShard(ctx, region, shardID, shardCount, …)` API + per-bucket lease keying. Bench harness lands with Phase 1 to quantify the speedup curve.
- **P2 — Parallel chunk upload in `PutChunks`.** Chunks are written sequentially. A
  bounded worker pool (32–64) hides RADOS latency on multi-chunk objects. Same shape on
  the multi-cluster path (US-044) — fan out per cluster.
- **P2 — Parallel chunk read / prefetch in `GetChunks`.** Stream chunk N to the client
  while chunk N+1 is being fetched. Memory-bounded; abort the prefetch on client cancel.
- **P3 — Erasure-code aware manifests.** For EC pools, track k+m parameters in the
  manifest for restore-path optimizations and accurate space accounting.
- **P3 — `ReadOp` / `WriteOp` batching in RADOS.** Bundle the head xattr read with the
  first chunk read in one OSD op (single round-trip for small objects).
- **P3 — Connection pool tuning.** Benchmark one `*rados.Conn` vs several for write-heavy
  workloads; measure CGO contention inside librados.

## Web UI

- ~~**P2 — Web UI — Foundation (Phase 1).**~~ **Done.** Embedded React+TS console served at `/console/` on the gateway port (`go:embed` + SPA fallback). Versioned `/admin/v1/*` JSON API + OpenAPI 3.1 spec at `internal/adminapi/openapi.yaml`. Session-cookie auth (HS256 JWT, 24 h, `HttpOnly`+`SameSite=Strict`+`Path=/admin`) backed by the existing static-credentials store, with SigV4 fallback for programmatic clients. Pages: login, cluster overview (CockroachDB-shape hero + nodes table + top-buckets + top-consumers widgets), buckets list (search/sort/paginate), bucket detail (read-only object browser with folder navigation + object detail panel), metrics dashboard (request rate / latency p50/p95/p99 / error rate / bytes — 15m/1h/6h/24h/7d ranges). Heartbeat infra in `internal/heartbeat` (memory + Cassandra; 10 s write, 30 s TTL). TanStack Query 5 polling (5 s default, per-range overrides on Metrics). Recharts 2 lazy-loaded. Bundle ≤500 KiB gzipped initial. Critical-path Playwright e2e (`web/e2e/critical-path.spec.ts`) running in CI under the `e2e-ui` job. Operator guide at `docs/ui.md`. (commit `e27cf21`)
- ~~**P3 — Web UI — Phase 2 (admin).**~~ — **Done.** 22 stories: bucket admin (create / delete with force-empty job / versioning + object-lock toggle / lifecycle / CORS / policy / ACL / inventory / access-log), IAM users + access keys + managed policies (attach/detach), object upload (per-part presigned + Web Worker progress) / delete / tags / retention / legal-hold, multipart watchdog (cluster-wide list + bulk abort), audit-log viewer, settings (JWT secret rotation + S3-backend config + BackendPresign toggle). Playwright e2e: `web/e2e/admin.spec.ts` covers the five Phase 2 critical paths. `docs/ui.md` capability matrix lists all 20 admin surfaces. (commit `5a6058b`)
- ~~**P3 — Web UI — Phase 3 (debug).**~~ — **Done.** 15 stories: SSE audit-tail (broadcaster + live-tail page with virtualised list, pause/resume, reconnect backoff), slow-queries (`total_time_ms` audit column + `ListSlowQueries` across memory/cassandra/tikv + filter/histogram UI), OTel trace ring buffer (in-process bytes-budgeted LRU with per-trace span cap, ringbuf-served via `/admin/v1/diagnostics/trace/{requestID}`) + waterfall renderer (depth-first bar layout, span detail sheet, recent-trace history, optional Jaeger deep link), hot-buckets heatmap (PromQL `sum by (bucket) (rate(...))` + custom canvas heatmap component, no @nivo dep), hot-shards heatmap (`strata_cassandra_lwt_conflicts_total{bucket,shard}` instrumentation + per-bucket tab with s3-backend explainer + drill panel), per-node drilldown drawer (5 PromQL sparklines via `instance="<addr>"` filter), bucket-shard distribution (per-shard sampler in `bucketstats` + Distribution tab with skew detection), replication-lag chart (`strata_replication_queue_age_seconds{bucket}` gauge + Replication tab gated on `replication_configured`). Playwright e2e `web/e2e/debug.spec.ts` covers five Phase 3 critical paths. `docs/ui.md` capability matrix lists the eight new debug surfaces. (commit `7677cdd`)
- **P2 — Trace browser has no list view.** `internal/otel/ringbuf.RingBuffer` exposes `GetByRequestID` / `GetByTraceID` only — no `List(limit) []TraceSummary`. The UI's "Recent traces" panel reads from `localStorage`, populated only when the operator successfully opens a trace by id. Without an id, the page is search-only — operators can't discover what's in the ringbuf. Fix scope: add `RingBuffer.List(limit, offset)` returning the LRU front-N as `{request_id, trace_id, root_name, started_at, duration_ms, status}` summaries; expose via `GET /admin/v1/diagnostics/traces?limit=50`; render in the existing TraceBrowser page above the search box (sortable by start time, click → load full trace by request_id).
- **P2 — TiKV meta backend emits no trace spans.** `internal/meta/cassandra/observer.go` wires a `gocql.QueryObserver` that emits `meta.cassandra.<table>.<op>` child spans on every query, but `internal/meta/tikv/` has no equivalent — TiKV transactional ops (`Begin / LockKeys / Get / Set / Commit`) flow through `tikv/client-go` without a span around them. Operator inspecting a trace on the lab-tikv stack sees only the gateway-level HTTP span + `data.rados.<op>` children, with the meta-write step entirely missing — the chain looks broken. Fix scope: new `internal/meta/tikv/observer.go` mirroring the cassandra observer shape, wrapping the public `Store` methods (CreateBucket, PutObject, GetObject, …) in `meta.tikv.<table>.<op>` spans via a thin functional decorator; wire `Tracer` field on `tikv.Config` mirroring `cassandra.SessionConfig.Tracer`; passed in from `internal/serverapp.buildMetaStore` after `strataotel.Init`.
- **P3 — S3-over-S3 data backend emits no trace spans.** `internal/data/s3/` has no observer; PutChunk / GetChunk / DeleteChunk hit AWS SDK directly. Same shape fix as the TiKV one but on the data side — wrap the SDK calls in `data.s3.<op>` spans. Lower priority because the lab-tikv default uses RADOS + the s3-over-s3 backend is the secondary data path.
- **P3 — OTel ring-buffer eviction tuning under burst load.** The 4 MiB default + per-trace 256-span cap was sized by hand. Run a burst-load harness (`hey -z 60s -c 100 …` against `make run-memory` with ringbuf=on) and measure (a) eviction rate, (b) p99 trace retention age, (c) memory ceiling vs configured budget. Document the observed cap and either bump the default or expose `STRATA_OTEL_RINGBUF_BYTES` more prominently in `docs/ui.md`.
- ~~**P3 — Web UI — TiKV heartbeat backend.**~~ — **Done.** `internal/meta/tikv/heartbeat.go` implements `heartbeat.Store` against the TiKV transactional client. Rows live under `s/hb/<nodeID>` with a JSON payload carrying `ExpiresAt = LastHeartbeat + DefaultTTL`; readers lazy-skip expired rows and writers eager-delete up to 16 expired rows per write so the prefix does not leak disk. Wired in `internal/serverapp.buildHeartbeatStore`. (commit `c37487b`)
- ~~**P3 — Heartbeat `leader_for` chip wired to actual lease state.**~~ — **Done.** `cmd/strata/workers.Supervisor` now exposes a buffered (cap 8) `LeaderEvents()` channel emitting `(workerName, acquired)` on every per-worker lease acquire/release; `internal/heartbeat.Heartbeater.SetLeaderFor(worker, owned)` mutates `Node.LeaderFor` under a mutex and the next write tick (~10 s) propagates to the cluster_nodes row consumed by Cluster Overview. `internal/serverapp.Run` wires a goroutine from `Supervisor.LeaderEvents()` into `hb.SetLeaderFor`. (commit `6f81734`)
- ~~**P3 — Multi-replica lab (TiKV).**~~ — **Done.** New `lab-tikv` compose profile spins up two TiKV-backed Strata replicas (`strata-tikv-a`, `strata-tikv-b`) behind an `nginx` LB at host port 9999, sharing a JWT secret via the `strata-jwt-shared` named volume (file-based atomic bootstrap via POSIX `O_EXCL`). `Supervisor.LeaderEvents()` → `Heartbeater.SetLeaderFor` propagates lease rotation into the Cluster Overview `leader_for` chip within ~35 s of a holder kill. `scripts/multi-replica-smoke.sh` (target `make smoke-lab-tikv`) drives 5 host-side scenarios; `web/e2e/multi-replica.spec.ts` mirrors the same in a `[multi-replica]`-gated CI job (`e2e-ui-multi-replica`). Operator guide at `docs/multi-replica.md`. (commit `9e36975`)
- ~~**P3 — Web UI — Storage status (meta + data backend observability).**~~ — **Done.** New `/storage` page (Meta + Data tabs + per-class card), Cluster Overview "Storage" hero card, and top-level `<StorageDegradedBanner>` above the AppShell. Backed by `meta.HealthProbe` (Cassandra `system.peers`+`system.local` merge with 10 s in-process cache; TiKV bootstrap-only `pdclient` against `/pd/api/v1/stores`; memory single-row), `data.HealthProbe` (RADOS `(*IOContext).GetPoolStats()` + `(*Conn).MonCommand({"prefix":"status"})`; S3-over-S3 `HeadBucket`; memory RSS proxy), and the `bucketstats` sampler extended with optional `ClassSink`+`Snapshot` for cluster-wide per-(class) totals (cardinality bound by `STRATA_BUCKETSTATS_TOPN`; cadence via new `STRATA_BUCKETSTATS_INTERVAL`). New endpoints `/admin/v1/storage/{meta,data,classes,health}`; aggregate `/health` honors `STRATA_STORAGE_HEALTH_OVERRIDE` for e2e. Playwright spec `web/e2e/storage.spec.ts` exercises page render, hero chip, and degraded-banner dismissal. Operator guide at `docs/storage.md`. (commit `cde5581`)

## S3 API surface

- **P3 — Intelligent-Tiering.** Access-time tracking + auto-transition. Needs hot/cold
  access counters per object.
- **P3 — Select / Select Object Content.** SQL over CSV/JSON/Parquet in place. Large
  effort for narrow win.
- **(out of scope) — Object Lambda.** Storage layer should not host user code.

## Developer experience

- **P3 — Module tags cleanup.** `github.com/ceph/go-ceph` is in `go.mod` regardless of
  `-tags ceph`. A `go mod tidy` without the tag removes it, breaking reproducibility. Fix
  by wrapping the import in a default-on tag file, or pinning it as an explicit `require`.
- **P3 — `make dev` for one-command developer cluster.** Single command that bootstraps
  Cassandra + Ceph + the consolidated `strata` binary and streams logs.
- **P3 — Architecture decision records.** Move the design notes captured below into
  `docs/adr/` once external contributions start.
- **P2 — Full project documentation site (GitHub Pages).** `docs/` is a flat collection of operator-facing notes (`ui.md`, `multi-replica.md`, `storage.md`, `backends/*.md`, etc.) — no navigation, no cross-linking, no rendered home page. New contributors and operators can't get a guided tour of "what is Strata, how do I deploy it, where do I look for X". Fix scope: introduce `mkdocs-material` (or `docusaurus`) under `docs/`, organise existing `.md` into a sectioned tree (Getting started → Architecture → Operators → Developers → API reference → ADRs), wire a GitHub Action that builds + publishes to `gh-pages` on every merge to main, register `https://danchupin.github.io/strata/` (or repo-org equivalent) as the project home page. Auto-link from README.md. Bonus: an OpenAPI viewer for `internal/adminapi/openapi.yaml` (Redoc / Swagger UI) embedded in the API reference section.

---

## Alternative metadata backends

Strata supports two production metadata backends: **Cassandra** (with **ScyllaDB** as a
CQL-compatible drop-in — zero code changes, gocql works unchanged, CI matrix landed in
US-042) and **TiKV** (raw KV via `tikv/client-go`, native ordered range scans short-circuit
Cassandra's 64-way fan-out via the optional `meta.RangeScanStore` interface; ships with
`docs/backends/tikv.md`, `docs/benchmarks/meta-backend-comparison.md`, and
`.github/workflows/ci-tikv.yml`). Both are first-class — the core team benchmarks,
documents, and maintains both paths. Memory is for tests only.

Headline gap from `docs/benchmarks/meta-backend-comparison.md`: TiKV's native ordered
range scan finishes a 100k-object `ListObjects` in **30–50 ms** vs Cassandra's
64-way fan-out + heap-merge at **150–300 ms** — **~5× faster** on the listing hot path.
LWT-equivalent operations (`CreateBucket`, `CompleteMultipartUpload`) are ~1.5–2× faster
on TiKV pessimistic-txn vs Cassandra Paxos; small-object Get hot paths
(`GetObject`, `GetIAMAccessKey`) are dominated by network RTT and look comparable.
Cassandra wins on audit retention (`USING TTL` is free; TiKV runs an explicit
sweeper).

The `meta.Store` interface stays intentionally minimal (LWT semantics, clustering-order
reads, range scans). Capability-specific features (e.g. native range scans across
partitions) land behind **optional interfaces** that a backend opts into:

```go
// In internal/meta. Optional, not required by Store.
type RangeScanStore interface {
    Store
    ScanRange(ctx, bucketID, start, end string, limit int) (*ListResult, error)
}
```

Gateway code uses type-assertion (`if rs, ok := store.(RangeScanStore); ok {...}`) to
pick the better code path when available, falling back to the fan-out/heap-merge default
otherwise. TiKV implements `RangeScanStore`; Cassandra explicitly does not (the fan-out
path is the only shape Cassandra serves efficiently).

Non-goals:
- A backend that cannot honor at least `LOCAL_QUORUM`-equivalent semantics. Single-node-
  consistent-only stores (Redis standalone, SQLite) will never be a supported production
  path.
- Backends that cannot represent the `(bucket_id, shard, key, version_id DESC)` clustering
  natively. Anything slower than O(page_size) per page on `ListObjects` is not acceptable.

## Known latent bugs

- GET with `Range: bytes=start-` where `start >= size` returns `416` — same as AWS.
  `Range: bytes=-N` with `N > size` returns full body — matches AWS. Edge cases around
  zero-length objects: not tested.
- Streaming chunked decoder assumes `\r\n` strictly and reads via `bufio`. Does not handle
  `aws-chunked-trailer` (newer aws-cli variants). aws-cli 2.22 observed to use plain
  `x-amz-content-sha256: <hex>` for `s3api put-object` and STREAMING for `s3 cp`, both
  tested working.
- Lifecycle worker has no retry on transient failures — next tick re-tries.

---

## Design notes captured during MVP and "modern complete"

Documented in `memory/project_strata.md` (internal) and in commit messages. A few that
deserve a dedicated `docs/adr/` entry:

- Why we skip RADOS omap entirely (the thing RGW uses and we are replacing).
- Why `IsLatest` is derived at read-time from clustering order, not flipped on every PUT.
- Why `go-ceph.NewConnWithUser("admin")` takes the short ID, not `client.admin`.
- Why the runtime image is based on `quay.io/ceph/ceph:v19.2.3` (matching librados version,
  multi-arch) instead of stock debian librados (stale at v16).
- Why `data.Manifest` lives in a JSON-encoded blob column instead of normalised columns —
  schema-additive evolution without `ALTER`.
- Why each background worker is leader-elected separately rather than co-locating them in
  a single supervisor (and why that choice is being reconsidered — see Consolidation
  section above).
- Why the protobuf manifest (US-048/049) ships behind a decoder-first migration: every
  reader handles both shapes for one full release before the writer flips.
