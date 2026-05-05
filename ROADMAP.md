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

S3-compatibility headline: **84.7% (150/177)** on the executable subset of `ceph/s3-tests`. See
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
- **P1 — s3-tests 80% → 90%+.** Lifted to 84.7% (150/177) by the `ralph/s3-compat-90`
  cycle (US-001..US-010 — multipart per-part offset tracking, ?partNumber=N GET, per-part
  composite-checksum, multipart copy FlexibleChecksum, listing delimiter+prefix folding,
  V2 opaque continuation token, versioning literal "null", multipart Complete
  preconditions + size-too-small + composite-checksum input validation). 16 real
  failures remain, clustered in multipart copy edges, ?partNumber=N quoted-ETag shape,
  CRC32-family composite checksum, multipart If-Match-on-missing-object. See
  `scripts/s3-tests/README.md` for the per-test gap breakdown.
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

- **P2 — Multipart copy edges (UploadPartCopy).** US-004 closed the boto SDK
  `FlexibleChecksum` path on the data plane, but four upstream s3-tests still fail:
  `test_multipart_copy_small`, `_improper_range`, `_special_names`, `_multiple_sizes`.
  Need: copy-source-range parser handling `bytes=0-0` / out-of-bounds inputs cleanly,
  per-special-char (`+`, space, `?`) copy-source URL decoding, and the resulting copy
  ETag wire shape on the multi-part response. Surfaced under `ralph/s3-compat-90`.

- **P2 — `?partNumber=N` GET quoted-ETag wire shape.** US-002 ships per-part ETag
  echo via `Manifest.PartChunks[N-1].ETag`, but three upstream s3-tests
  (`test_multipart_get_part`, `_sse_c_get_part`, `_single_get_part`) still fail
  because the boto-side compares the response `ETag` header, double-quoted, against
  the part-upload response's quoted ETag. Strata emits `"<hex>"` (single quotes
  around hex) but `internal/s3api/server.go::getObject` may strip or re-quote
  inconsistently. Audit the partNumber path's ETag header set vs the response_quote
  helper used by the regular GET.

- **P2 — Multipart composite checksum CRC32 / CRC32C / CRC64NVME.** US-003 closed
  SHA1 / SHA256 composite formula; the CRC32-family is still off by a fold:
  `test_multipart_use_cksum_helper_crc32`, `_crc32c`, `_crc64nvme`. Boto3 ≥1.35
  ships CRC64NVME as the auto-default, so this is the most-noticed remaining gap.
  Composite formula for CRC algos differs from the SHA ones — needs the BoringSSL-
  style fold over per-part raw checksums.

- **P2 — Multipart `If-Match`-on-missing-object error code.** US-008 aligned
  CompleteMultipartUpload's If-Match path with putObject's RFC 7232 §3.1
  contract (return `412 PreconditionFailed`), but the upstream s3-test
  `test_multipart_put_object_if_match` / `_put_current_object_if_match`
  asserts `404 NoSuchKey` for the missing-object branch (matches AWS S3
  rather than RFC). Revert the alignment for this specific branch.

- **P1 — Cassandra null-version timeuuid sentinel.** Cassandra's `objects.version_id`
  column is `timeuuid`, which the server rejects on INSERT with the all-zero
  `meta.NullVersionID` (v0 not a valid v1). US-007 + US-028 + US-029 broke every
  PUT to a Disabled-or-Suspended bucket against the Cassandra backend (~70 s3-tests
  red). Fixed in this cycle by introducing a cassandra-internal v1 sentinel
  (`00000000-0000-1000-8000-000000000000`) translated at every gocql.ParseUUID /
  .String() boundary; see `internal/meta/cassandra/store.go::versionToCQL`. Caught
  end-to-end via the `ralph/s3-compat-90` rerun. The contract test
  `caseVersioningNullSentinel` did not catch this (CI cassandra-integration ran
  the test but apparently passed — needs a follow-up to verify the test actually
  exercises an INSERT with `o.IsNull=true && o.VersionID==NullVersionID` against a
  real Cassandra container).

- **P1 — `koanf` env provider does not skip empty env values.** When a non-empty
  TOML default exists for a duration/integer field and the docker-compose
  `${VAR:-}` shape passes through with VAR unset, koanf overrides the default
  with `""` and unmarshal fails (`'gc.interval' time: invalid duration`). Fixed
  in this cycle by switching from `env.Provider` → `env.ProviderWithValue` with
  a callback that returns an empty key for empty values; see
  `internal/config/config.go::Load`. The bug blocked `make up-all` startup of the
  cassandra-backed `strata` service for the entire `ralph/s3-compat-90` cycle
  before discovery.

- **P2 — Multipart Complete leaks `completing` state on per-part ETag mismatch.**
  `meta.Store.CompleteMultipartUpload` flips the upload row to `completing` *before*
  validating per-part ETags in all 3 backends (memory `store.go:1692`, cassandra
  `store.go:2347` LWT, tikv `multipart.go:257`). When the client supplies a stale
  ETag (e.g. after a part resend), the request returns `InvalidPart` but the upload
  is now stuck in `completing`. A subsequent retry with corrected ETags maps to
  `ErrMultipartInProgress` → `NoSuchUpload`, breaking idempotency. Fix: defer the
  status flip until after the per-part ETag scan succeeds, or revert the flip on
  ETag-mismatch return paths in all 3 backends. Surfaced by US-009's resend tests.

- **P3 — Object Lock `COMPLIANCE` audit log.** `audit_log` (US-022) records all
  state-changing requests, but a denied DELETE under `COMPLIANCE` is not flagged
  distinctly. Regulated customers want a queryable "blocked retention violation" feed —
  add a typed `audit.Event.Reason` field that `audit_log` reads to filter.

## Auth

- **P3 — STS / assume-role.** Temporary credentials with expiry. Useful for multi-tenant
  deployments. Today an access key in Cassandra (US-036 family of IAM stories) is the
  only way to authenticate.
- **P3 — Per-bucket request signing keys (KMS-backed).** Rotate the signing material on
  a schedule, reject keys older than `STRATA_KEY_MAX_AGE`. Hooks onto the existing
  Vault provider.

## Scalability & performance

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
- **P3 — OTel ring-buffer eviction tuning under burst load.** The 4 MiB default + per-trace 256-span cap was sized by hand. Run a burst-load harness (`hey -z 60s -c 100 …` against `make run-memory` with ringbuf=on) and measure (a) eviction rate, (b) p99 trace retention age, (c) memory ceiling vs configured budget. Document the observed cap and either bump the default or expose `STRATA_OTEL_RINGBUF_BYTES` more prominently in `docs/ui.md`.
- ~~**P3 — Web UI — TiKV heartbeat backend.**~~ — **Done.** `internal/meta/tikv/heartbeat.go` implements `heartbeat.Store` against the TiKV transactional client. Rows live under `s/hb/<nodeID>` with a JSON payload carrying `ExpiresAt = LastHeartbeat + DefaultTTL`; readers lazy-skip expired rows and writers eager-delete up to 16 expired rows per write so the prefix does not leak disk. Wired in `internal/serverapp.buildHeartbeatStore`. (commit `c37487b`)

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
