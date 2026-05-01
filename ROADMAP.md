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

S3-compatibility headline: **78.5% (139/177)** on the executable subset of `ceph/s3-tests`
(2026-05-01 measurement, commit `6e122903`, `make up-all` stack). See
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
- **P1 — Race harness as a real test, not a gate.** `internal/racetest` (US-035) landed but
  has not been run at load against the full `make up-all` stack. Run it for ≥1 hour against
  Cassandra+RADOS, record observed inconsistencies (or zero, with the workload that proves
  it). Add the run to CI on a nightly schedule so regressions surface.
- **P1 — s3-tests 80% → 90%+.** Cycle `ralph/s3-tests-90` (US-001..US-011) shipped the
  per-cluster meta + handler surface (per-part offset tracking, ?partNumber=N GET, composite
  checksum response shape, FlexibleChecksum copy, listing delimiter+prefix, V2 opaque
  continuation token, versioning literal "null", multipart preconditions, size-too-small,
  Complete checksum input validation). 2026-05-01 baseline run after the cycle (`make up-all`,
  Cassandra + Ceph RADOS) lands at **78.5% (139/177)** — below the 90% floor. 30 real
  cluster-level interop failures remain (8 SigV2 deliberate; see
  `scripts/s3-tests/README.md`'s 2026-05-01 baseline section for the per-cluster breakdown).
  Per-story unit tests under `internal/s3api` pass; the gap is in the boto + aws-cli SDK
  envelope shapes (FlexibleChecksum digest match, ContentLength of ?partNumber=N GET, V1
  NextMarker shape, Cassandra null-version DELETE coherence). Filed as the follow-ups below.
- **P1 — s3-tests follow-up: ?partNumber=N GET ContentLength + single-part 416.**
  `test_multipart_get_part`, `test_multipart_sse_c_get_part`, `test_multipart_single_get_part`
  fail at cluster level: `ContentLength` echoes the whole-object size instead of the part
  size, and a single-part object does not 416 on out-of-range `?partNumber=`. US-002 wired
  the meta shape; the s3api response writer still emits the wrong size header when the
  underlying chunk stream is shorter than the part window.
- **P1 — s3-tests follow-up: versioning literal "null" Cassandra coherence.**
  `test_versioning_obj_plain_null_version_removal`,
  `test_versioning_obj_plain_null_version_overwrite`,
  `test_versioning_obj_plain_null_version_overwrite_suspended`,
  `test_versioning_obj_suspend_versions`, `test_versioning_obj_suspended_copy` all fail
  cluster-level on Cassandra. US-007 wired memory + cassandra meta lockstep; the cassandra
  path still leaves a deleted null version readable on subsequent GET. Likely a missing LWT
  on the DELETE path or an `IF EXISTS` guard skip.
- **P1 — s3-tests follow-up: multipart FlexibleChecksum SDK envelope.**
  `test_multipart_use_cksum_helper_{sha256,sha1,crc32,crc32c,crc64nvme}` (5),
  `test_multipart_copy_{small,improper_range,special_names,multiple_sizes}` (4),
  `test_multipart_checksum_sha256` (1) all fail SDK-side `FlexibleChecksumError`. US-003,
  US-004, US-010 wired the recompute paths; the SDK still rejects the digest because the
  composite shape we emit on `CompleteMultipartUploadResult` does not match the boto
  helper's expected envelope. CRC64NVME is a new algorithm not yet wired.
- **P2 — s3-tests follow-up: listing edge cases (delimiter+prefix V1, V2 continuation
  token).** `test_bucket_list_delimiter_prefix`, `test_bucket_list_delimiter_prefix_underscore`
  (US-005), `test_bucket_listv2_continuationtoken`,
  `test_bucket_listv2_both_continuationtoken_startafter` (US-006). Memory + cassandra unit
  tests pass; the cluster-level fixture exposes a NextMarker-vs-NextContinuationToken
  divergence on the `boo/bar`+`cquux/*` shape with `max-keys=1` paging.
- **P2 — s3-tests follow-up: multipart preconditions + size-too-small + resend ordering.**
  `test_multipart_put_current_object_if_match` (US-008),
  `test_multipart_upload_size_too_small` (US-009),
  `test_multipart_resend_first_finishes_last` (US-009). Per-story unit tests pass; the
  cluster path still rejects a Complete with `InvalidPartOrder` on a body the spec accepts,
  and the s3-tests size-too-small fixture exposes a non-last-part shape we don't reject.
- **P2 — s3-tests follow-up: anonymous list configuration.**
  `test_bucket_list_objects_anonymous`, `test_bucket_listv2_objects_anonymous` need
  `auth=optional` plus a bucket policy / ACL allow-anonymous shape — out of `s3-tests-90`
  scope, lands with the bucket-policy P-item.
- **P3 — s3-tests follow-up: object delete on missing bucket.**
  `test_object_delete_key_bucket_gone` — error code drift on a bucket-gone shape.
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
- ~~**P1 — S3-over-S3 first-class data backend.**~~ — **Done.** `internal/data/s3`
  implements the full `data.Backend` surface against any S3-compatible endpoint
  (AWS S3, MinIO, Ceph RGW, Garage, Wasabi, B2-S3) via `aws-sdk-go-v2`. Native
  shape: one Strata object = one backend object via backend multipart upload
  (no N-chunks-per-object request amplification). Defensive backend-versioning:
  per-object `VersionId` captured in `Manifest.BackendRef` at PUT/Complete,
  passed back on Delete — versioned backends do not silently leak storage into
  delete-markers. Wired through `STRATA_DATA_BACKEND=s3` (US-009),
  docker-compose `s3-backend` profile + MinIO sidecar (US-011), smoke +
  CI matrix (US-012, US-018), full SSE config flag (passthrough/strata/both,
  US-013), bidirectional lifecycle mapping (US-014), CORS passthrough (US-015),
  presigned URL passthrough (US-016). The previously listed community-tier
  data backends (filesystem, Azure Blob, GCS) are dropped from the roadmap —
  operators front non-S3 storage with MinIO / s3-proxy / GCS-S3-interop API
  instead. See `docs/backends/s3.md` for the operator guide. (commit `5914f34`)

## Correctness & consistency

- **P3 — Object Lock `COMPLIANCE` audit log.** `audit_log` (US-022) records all
  state-changing requests, but a denied DELETE under `COMPLIANCE` is not flagged
  distinctly. Regulated customers want a queryable "blocked retention violation" feed —
  add a typed `audit.Event.Reason` field that `audit_log` reads to filter.

## Auth

- **P2 — Per-chunk signature validation in streaming payload.**
  `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` body chunks carry per-chunk signatures —
  `internal/auth/streaming.go` decodes the framing but does NOT verify the chained
  HMAC. An attacker that intercepts a signed request can mutate the body without
  detection (the outer SigV4 covers headers + query but not chunk bodies).
  Implement the chain: `sig(chunk_n) = HMAC(signing_key, "AWS4-HMAC-SHA256-PAYLOAD\n
  <date>\n<scope>\n<prev-sig>\n<hash("")>\n<hash(chunk)>")`. Reject mismatched
  chunks with 403 `SignatureDoesNotMatch`. Mandatory — no opt-out flag — every
  AWS SDK already sends the correct chain. See
  `tasks/prd-auth-per-chunk-signature.md` for the implementation plan.
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
