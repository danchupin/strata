# Strata roadmap

MVP (phases 0–8) is complete. This document tracks known gaps and the direction of work that follows.

Items are labeled by rough priority:
- **P1** — correctness or production-blockers
- **P2** — meaningful gaps; expected for serious deployments
- **P3** — nice-to-have, visibility, DX

---

## Correctness & consistency

- ~~**P1 — CAS on transition FLIPPED.**~~ **Done.** `SetObjectStorage(ctx, …, expectedClass, newClass, manifest)` now returns `(applied bool, err error)` — Cassandra impl uses `UPDATE ... IF storage_class=?` LWT; memory impl compares in-memory class. Lifecycle worker discards the already-written new chunks (via GC queue) when `applied=false`, preserving concurrent client writes.
- ~~**P1 — `NoncurrentVersionTransition` / `NoncurrentVersionExpiration` lifecycle actions.**~~ **Done.** `lifecycle.worker.applyNoncurrentActions` iterates `ListObjectVersions`, computes "noncurrent since" as the mtime of the next-newer version, applies transition or expiration per the rule. Transition re-uses the existing CAS-guarded `SetObjectStorage` path. Expiration calls `DeleteObject(versionID)` + enqueues chunks into GC.
- ~~**P1 — Multipart upload abandonment cleanup.**~~ **Done.** Lifecycle rule `<AbortIncompleteMultipartUpload><DaysAfterInitiation>N</DaysAfterInitiation></AbortIncompleteMultipartUpload>` is now parsed and executed by `strata-lifecycle` — it scans pending uploads and aborts those older than `N * STRATA_LIFECYCLE_UNIT`.
- **P2 — Per-chunk signature validation in streaming payload.** `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` body chunks carry per-chunk signatures — we decode the framing but don't verify the chained HMAC. An attacker that intercepts a signed request can alter the body without detection (the outer signature only covers headers/query). Implement the chain: `sig(chunk_n) = HMAC(signing_key, "AWS4-HMAC-SHA256-PAYLOAD\n<date>\n<scope>\n<prev-sig>\n<hash("")>\n<hash(chunk)>")`.
- **P2 — Bucket policy / ACL enforcement.** Today any authenticated identity can do anything. Owner on `buckets.owner_id` is stored but not checked on subsequent requests.
- **P2 — Idempotency for multipart Complete on retry.** If client retries CompleteMultipartUpload after network failure, second call races with LWT and can return misleading errors. Record the completion result for N minutes, replay on duplicate.
- **P3 — Object Lock `COMPLIANCE` audit log.** Currently a DELETE under COMPLIANCE is blocked but no persistent record. Regulated customers need an immutable audit trail.

## Auth

- ~~**P1 — Presigned URLs.**~~ **Done.** `internal/auth/presigned.go` parses the `X-Amz-*` query parameters; middleware routes requests without `Authorization` header but with `X-Amz-Signature` query param through `validatePresigned`. Canonical request built with `X-Amz-Signature` removed from the query and body hash hardcoded to `UNSIGNED-PAYLOAD` per AWS spec. Expiry window verified (must be within `X-Amz-Expires` seconds of `X-Amz-Date`, capped at 7 days upstream). Verified: valid URL → 200, expired → 403, tampered signature → 403.
- **P2 — Cassandra-backed credentials store.** Replace `StaticStore` with a `cassandra.Store` implementation on the `access_keys` table (already in schema). Include an admin HTTP endpoint to create/rotate keys.
- **P2 — IAM-style policy attachment.** Per-bucket JSON policy that restricts actions by principal / resource / condition. Minimally: `AllowList`, `DenyGet`.
- **P3 — STS / assume-role.** Temporary credentials with expiry. Useful for multi-tenant deployments.
- **P3 — MFA delete.** Per-bucket flag that requires an MFA token header on DELETE-version. Low demand but in the S3 spec.

## Scalability

- **P1 — Multi-cluster RADOS.** `data.ChunkRef.Cluster` exists; `rados.Backend` still holds a single `*rados.Conn`. Lift to `map[clusterID]*rados.Conn`, keyring per cluster. Needed for real geo-sep cold tier.
- ~~**P1 — Leader election for workers.**~~ **Done.** New `internal/leader` package + `internal/meta/cassandra/locker.go` implements LWT-based lease on `worker_locks` table. Row has a 30s TTL refreshed every 10s; loss-of-lease cancels the worker's context. Memory backend has a same-process `Locker` that makes the smoke-level no-op work without Cassandra. Each worker runs `AwaitAcquire → Supervise → w.Run → Release` in a retry loop.
- **P2 — Per-bucket shard_count resize.** Buckets are created with N=64 partitions. As a bucket grows past tens of millions of objects, the heap-merge fan-out becomes expensive. Add a split/rehash flow — online rewrite of rows into a larger N. This is the *other* reshard we inherit from RGW; design it better (background job, no downtime, LWT on the active N).
- **P2 — Cross-region replication.** `x-amz-replication-status` headers, `ReplicationConfiguration` XML, a replicator that mirrors object writes to a peer gateway (which may target a different Cassandra keyspace / RADOS cluster).
- **P3 — Erasure-code aware manifests.** For EC pools, track k+m parameters in manifest for restore-path optimizations.

## Operations & observability

- **P2 — Prometheus metrics — more coverage.** Current metrics are minimal. Add: Cassandra query latency per table, RADOS op latency, multipart active count, per-storage-class byte counts, gc_queue depth (as a gauge sampled by the gc worker).
- **P2 — Structured logging.** Swap `log` for a structured logger (zap/slog). Correlation IDs in per-request logs.
- **P2 — Health and readiness endpoints.** `/healthz` (liveness), `/readyz` (Cassandra+RADOS reachable). Used by k8s probes.
- **P2 — Grafana dashboards.** Ship JSON dashboards alongside the compose so `docker compose up` includes a Grafana container pre-wired to the three metrics endpoints.
- **P3 — OpenTelemetry tracing.** Spans through Gateway → Meta (Cassandra query) → Data (RADOS op). Propagation from S3 client via `traceparent` if set.
- **P3 — Audit log.** Who-did-what for bucket/object/key ops. Write to a separate Cassandra table with TTL.

## S3 API surface

- **P2 — CORS.** Bucket-level `PutBucketCors` / `GetBucketCors`, and the `OPTIONS` preflight handling on object endpoints.
- **P2 — Static website hosting.** `PutBucketWebsite`, `GetBucketWebsite`, index/error doc routing on GET.
- **P2 — Notifications.** `PutBucketNotificationConfiguration`, background publisher to SNS/SQS/Kafka/Webhook for PUT/DELETE events.
- **P3 — Inventory + analytics.** Scheduled manifest exports.
- **P3 — Intelligent-Tiering.** Access-time tracking + auto-transition. Needs hot/cold access counters per object.
- **P3 — Select / Select Object Content.** SQL over CSV/JSON/Parquet in place. Large effort for narrow win.
- **P3 — Object Lambda.** Out of scope for a storage layer.

## Testing & CI

- **P1 — Go integration tests.** Two waves landed:
  - Unit layer: ~30 cases across `internal/s3api`, `internal/auth` (incl. AWS SigV4 "get-vanilla" vector), `internal/leader`, `internal/lifecycle`, `internal/meta/memory`.
  - Integration layer: `internal/meta/cassandra/store_integration_test.go` under `-tags integration` spins up Cassandra 5.0 via `testcontainers-go` and runs the same `storetest.Run` contract against a live cluster. Each subtest gets its own fresh keyspace for isolation. Running `make test-integration` on macOS with lima requires `DOCKER_HOST=unix:///Users/.../.lima/.../sock/docker.sock`.
  - This wave caught two real Cassandra bugs: `CreateBucket`'s `ScanCAS(nil...)` had the wrong column count (fixed via `MapScanCAS`); `SetBucketVersioning` with a plain UPDATE after a LWT INSERT caused read-after-write anomaly (fixed by upgrading the UPDATE to LWT `IF EXISTS`).

  Third wave landed:
  - `internal/data/rados/backend_integration_test.go` under `//go:build ceph && integration`. Tests run **inside** the Dockerfile build stage (which has librados + Go toolchain) via `make test-rados`; they exercise PutChunks → GetChunks → Delete round-trip, ranged reads, and storage-class validation against the live Ceph that `make up-all` brings up. Skips cleanly when `/etc/ceph/ceph.conf` is not reachable.
  - `.github/workflows/ci.yml` runs five jobs on every push / PR: `lint-build`, `unit` (with `-race`), `integration-cassandra` (testcontainers-based), `docker-build` (proves librados linking), and `e2e` which brings up the full compose stack and runs `smoke.sh` + `smoke-signed.sh` + the RADOS integration tests.
- **P1 — Ceph `s3-tests` compatibility suite.** Runner landed at `scripts/s3-tests/run.sh` — bootstraps a Python venv, clones upstream `ceph/s3-tests`, writes `s3tests.conf` pointing at the local gateway, runs pytest with a feature-matched filter, parses `junit.xml` for a machine-readable pass rate. Baseline: **11/19 (~58%) on the executable subset** of `bucket_create | bucket_list | object_write | object_read | object_delete | multipart | versioning`; 1046 tests collected overall with 3 passing without filters. The remaining P-items each move a chunk of these errors into passes — the number in `scripts/s3-tests/README.md` is the honest "am I S3-compatible" score to track.
- **P2 — Concurrency / race tests.** Hammer Gateway with parallel PUT/DELETE, verify final state in Cassandra + RADOS is consistent.
- **P2 — Chaos test harness.** Kill Cassandra node mid-PUT, kill RADOS OSD mid-PUT, verify the gateway behaves correctly (retries, 503 after timeout).
- **P2 — CI via GitHub Actions.** Matrix on `-tags ceph` and no-tag. Run Go tests, bash smoke, s3-tests subset.

## Performance

- **P2 — Parallel chunk upload in PutChunks.** Today chunks are written sequentially. A bounded worker pool (32–64) would hide RADOS latency on multi-chunk objects.
- **P2 — Parallel chunk read in GetChunks.** Prefetch next chunk while current is streamed to client.
- **P3 — ReadOp / WriteOp batching.** Bundle head xattr read + first chunk read in one OSD op (single round-trip for small objects).
- **P3 — Connection pool tuning.** Benchmark: one `*rados.Conn` vs several for write-heavy workloads; measure CGO contention inside librados.
- **P3 — Benchmarks.** `aws-s3-benchmark` + `cosbench` + `warp`. Publish numbers alongside RGW baseline.

## Developer experience

- **P2 — `make dev` for one-command cluster.** Single command that bootstraps Cassandra + Ceph + gateway + lifecycle + gc and streams logs.
- **P3 — Admin CLI (`strata-admin`).** Wrapper around a small admin API: create access key, flush GC queue, force lifecycle tick, inspect a bucket's shard distribution.
- **P3 — Protobuf manifest.** Switch from JSON-encoded Manifest in `objects.manifest` to protobuf, for smaller row size and forward compatibility.
- **P3 — Go module tags cleanup.** Right now `github.com/ceph/go-ceph` is in `go.mod` regardless of `-tags ceph`. A `go mod tidy` without the tag removes it, breaking reproducibility. Fix by wrapping the import in a default-on tag file, or committing a `go.mod` that pins it as an explicit `require`.
- **P3 — Examples directory.** Sample `aws-cli` / `mc` / `boto3` / `s3cmd` invocations matched to Strata configs.

---

## Alternative metadata backends

Strata's primary production backend is **Cassandra** (and **ScyllaDB** as a drop-in CQL-compatible replacement — zero code changes, gocql works unchanged). The core team benchmarks, documents, and maintains this path. Everything else is community-maintained without feature-parity or latency guarantees.

The `meta.Store` interface stays intentionally minimal and is driven by the primary backend's idioms (LWT semantics, clustering-order reads, NetworkTopologyStrategy). Backends that cannot match these capabilities are free to implement `meta.Store` with documented caveats; **we do not water down the interface to accommodate the weakest backend**.

Capability-specific features (e.g. native range scans across partitions) should land behind **optional interfaces** that a backend opts into:

```go
// In internal/meta. Optional, not required by Store.
type RangeScanStore interface {
    Store
    ScanRange(ctx, bucketID, start, end string, limit int) (*ListResult, error)
}
```

Gateway code would use type-assertion (`if rs, ok := store.(RangeScanStore); ok {...}`) to pick the better code path when available, falling back to the fan-out/heap-merge default otherwise.

Currently envisioned alternatives:

- **P1 — ScyllaDB** as first-class primary (same docs, same gocql). Main win: Raft-based LWT is 3–5× faster than Cassandra's Paxos path, which matters for our bucket-create / versioning-flip / multipart-complete hot paths. Expected migration effort: zero code, update docs and run benchmarks.
- **P3 — TiKV** as a community backend. Native ordered key range means `ListObjects` could become a single range scan instead of a 64-way fan-out. Requires a new `meta/tikv/` implementation; LWT becomes TiKV transactions. Good fit for teams already running TiKV for other workloads.
- **P3 — FoundationDB** as a community backend. Best fit for exabyte-scale deployments (the Snowflake / Apple pattern). Strong serializable transactions, range scans. Requires learning curve.
- **P3 — PostgreSQL + Citus / Yugabyte** as a community backend. SQL familiarity. Needs advisory-lock-based emulation of LWT and custom sharding logic; not a natural fit but useful for small single-node deployments.

Non-goals:
- A backend that cannot honor at least `LOCAL_QUORUM`-equivalent semantics. Single-node-consistent-only stores (Redis standalone, SQLite) will never be a supported production path.
- Backends that cannot represent the `(bucket_id, shard, key, version_id DESC)` clustering natively. Anything slower than O(page_size) per page on ListObjects is not acceptable.

## Alternative data backends

Strata's primary production data backend is **RADOS** via `go-ceph`. The S3 backend is an **equal-tier alternative** built on `aws-sdk-go-v2` for operators who already run an S3-compatible store (AWS S3, MinIO, Ceph RGW, Garage, Wasabi, B2-S3). Both are core-team-maintained, benchmarked, and documented; everything else falls under the same "no community slots" policy as Alternative metadata backends.

The supported set is exactly two: **`rados`** and **`s3`** (plus `memory` for tests). Filesystem / Azure Blob / GCS are explicitly **not planned** — operators needing those use Strata's S3 backend pointed at any S3-compatible service (MinIO over filesystem, s3-proxy over Azure, GCS S3-interop API). We do not water down the `data.Backend` interface to accommodate the weakest backend, and we do not maintain backends that duplicate this design.

The `data.Backend` interface stays minimal and stream-shaped (`Put` / `Get` / `GetRange` / `Delete`). Capability-specific features that some backends do natively (multipart pass-through, lifecycle translation, CORS mirror, presigned-URL passthrough) live behind **optional interfaces** that a backend opts into:

```go
// In internal/data. Optional, not required by Backend.
type MultipartBackend interface {
    Backend
    CreateBackendMultipart(ctx, key string) (handle string, err error)
    UploadBackendPart(ctx, handle string, part int, body io.Reader, size int64) (etag string, err error)
    CompleteBackendMultipart(ctx, handle string, parts []BackendCompletedPart) (*Manifest, error)
    AbortBackendMultipart(ctx, handle string) error
}
```

Gateway code uses type-assertion (`if mb, ok := backend.(data.MultipartBackend); ok {...}`) to pick the better code path when available, falling back to the chunk-based / worker-based default otherwise. Same pattern for `LifecycleBackend` (US-014), `CORSBackend` (US-015), and `PresignBackend` (US-016).

Currently envisioned alternatives:

- ~~**P1 — S3-over-S3 (any S3-compatible endpoint).**~~ **Done** (cycle `ralph/s3-over-s3-backend`, commit pending). Strata's data plane gains an equal-tier alternative to RADOS that stores object bytes in any S3-compatible endpoint via `aws-sdk-go-v2`. Native shape: one Strata object = one backend object via backend multipart upload (NOT N-chunks-per-object — request-count amplification kills it). Defensive backend-versioning support: per-object VersionId captured in `Manifest.BackendRef` at PUT/Complete time, passed back on Delete, so versioned-bucket backends do not silently leak storage into delete-markers. Wires `STRATA_DATA_BACKEND=s3` dispatch (US-009), docker-compose `s3-backend` profile with MinIO sidecar (US-011), smoke + CI matrix entry (US-012, US-018), full SSE config flag (passthrough/strata/both, US-013), bidirectional lifecycle mapping (US-014), CORS passthrough (US-015), presigned URL passthrough (US-016). Operator pre-creates the backend bucket; Strata refuses to start on missing bucket. See [`docs/backends/s3.md`](docs/backends/s3.md) for the operator guide (capability matrix, tested-against backends, pitfalls).

Non-goals:
- A native filesystem backend (POSIX, FUSE, NFS). Operators needing local-disk storage point the S3 backend at MinIO (or any S3-compatible service that wraps a filesystem). Owning a separate filesystem backend would duplicate maintenance for zero capability we cannot already deliver.
- Native Azure Blob / GCS backends. Both clouds expose S3-compatible interop endpoints (s3-proxy / GCS S3-interop API); the S3 backend already covers these paths.
- A backend that splits a Strata object into N small backend objects. Multiplies request count by N at every read; defeats the point of running on top of an S3 store. Native shape (one Strata object = one backend object) is non-negotiable.

## Known latent bugs

- GET with `Range: bytes=start-` where `start >= size` returns `416` — same as AWS. `Range: bytes=-N` with `N > size` returns full body — matches AWS. Edge cases around zero-length objects: not tested.
- Streaming chunked decoder assumes `\r\n` strictly and reads via `bufio`. Does not handle `aws-chunked-trailer` (newer aws-cli variants). aws-cli 2.22 observed to use plain `x-amz-content-sha256: <hex>` for `s3api put-object` and STREAMING for `s3 cp`, both tested working.
- Lifecycle worker has no retry on transient failures — next tick re-tries.

---

## Design notes captured during MVP

Documented in `memory/project_strata.md` (internal) and in commit messages. A few that deserve a dedicated doc:

- Why we skip RADOS omap entirely (the thing RGW uses and we are replacing).
- Why `IsLatest` is derived at read-time from clustering order, not flipped on every PUT.
- Why `go-ceph.NewConnWithUser("admin")` takes the short ID, not `client.admin`.
- Why the runtime image is based on `quay.io/ceph/ceph:v19.2.3` (matching librados version, multi-arch) instead of stock debian librados (stale at v16).

These should land in `docs/adr/` as ADRs when we start taking external contributions.
