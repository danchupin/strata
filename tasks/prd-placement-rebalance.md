# PRD: Per-bucket Placement Policy + Cross-cluster Rebalance Worker

## Introduction

Strata now supports multiple RADOS and S3 data-side clusters (`STRATA_RADOS_CLUSTERS` / `STRATA_S3_CLUSTERS` + `STRATA_S3_CLASSES`), but every chunk PUT still lands on `cluster=DefaultCluster` unless the bucket's storage class maps elsewhere. There is no weighted spread, no fill-aware placement, and no way to migrate old chunks when a new cluster joins. Operators wanting to drain an old cluster have no path forward.

This PRD adds (a) a per-bucket placement policy persisted alongside `meta.Bucket`, (b) chunk-PUT routing that consults the policy with a stable hash-mod scheme so retries land on the same cluster, (c) a new leader-elected `rebalance` worker that scans manifests and copies chunks A→B until the actual distribution matches the policy, and (d) safety rails (refuse mover dispatch when target cluster is >90% full or when source is mid-drain).

Closes ROADMAP P2 *Per-bucket placement policy + cross-cluster rebalance worker* (line 185).

## Goals

- Per-bucket `Placement` policy: `map[cluster]int` weights (0..100), stored in `meta.Bucket.Placement`, round-trippable via admin API
- Chunk PUT routes via `fnv32a("<bucketID>/<key>/<chunkIdx>") % sum(weights)` → weight wheel → stable per-(bucket, key, chunk) cluster pick
- New `strata server --workers=rebalance` periodic scanner copies chunks A→B and CAS-updates manifests until actual distribution matches policy
- Throttle via `STRATA_REBALANCE_RATE_MB_S` (default 100) + `STRATA_REBALANCE_INFLIGHT` (default 4)
- Safety rails: refuse mover dispatch when target cluster usage > 90% (RADOS df) or when source cluster is in `draining` state
- Backward compatible: buckets without `Placement` continue to route to `$defaultCluster` — zero behavior change for existing deploys
- Operator workflow: register new cluster (env + rolling restart) → set Placement weighting old→0, new→1 → rebalance worker drains → mark old cluster `draining` → deregister via env after manifest scan shows 0 chunks remain — zero-downtime end-to-end

## User Stories

### US-001: `meta.Bucket.Placement` field + admin API
**Description:** As an operator, I want to set and inspect a placement policy on a bucket so I can control how its chunks spread across clusters.

**Acceptance Criteria:**
- [ ] `meta.Bucket` gets `Placement map[string]int` field with `json:"placement,omitempty"` tag
- [ ] New `meta.Store` triple: `SetBucketPlacement(ctx, name, map[string]int) error`, `GetBucketPlacement(ctx, name) (map[string]int, error)`, `DeleteBucketPlacement(ctx, name) error`
- [ ] Implementation in `internal/meta/memory` + `internal/meta/cassandra` (via `setBucketBlob` helper) + `internal/meta/tikv`
- [ ] Validation: weight range `[0, 100]` per cluster, `sum > 0` else `ErrInvalidPlacement`, at least one cluster name must match a live entry in `STRATA_RADOS_CLUSTERS` / `STRATA_S3_CLUSTERS` (error `ErrUnknownCluster`)
- [ ] Admin API: `PUT /admin/v1/buckets/{name}/placement` (body `{cluster: weight}`), `GET /admin/v1/buckets/{name}/placement` returns 404 when unset, `DELETE /admin/v1/buckets/{name}/placement` reverts to default routing
- [ ] OpenAPI spec at `internal/adminapi/openapi.yaml` updated
- [ ] Audit row stamped via `s3api.SetAuditOverride(ctx, "admin:PutBucketPlacement", ...)` on PUT/DELETE
- [ ] Contract test in `internal/meta/storetest/contract.go::caseBucketPlacement` exercises all three backends
- [ ] Backward compat: `GetBucketPlacement` on a bucket without policy returns `(nil, nil)` — NOT an error — so the routing path knows to fall back to `$defaultCluster`
- [ ] Typecheck passes, `go vet ./...` passes, `go test ./internal/meta/...` passes

### US-002: PutChunks consults policy with hash-mod stable routing
**Description:** As a developer, I need the chunk PUT path to route via the placement policy so chunks spread per the operator's weights and the same chunk always lands on the same cluster across retries.

**Acceptance Criteria:**
- [ ] New helper `internal/data/placement/router.go::PickCluster(bucketID uuid.UUID, key string, chunkIdx int, policy map[string]int) string` using `fnv32a("<bucketID>/<key>/<chunkIdx>")` mod `sum(weights)` walking the weight wheel
- [ ] Deterministic: same inputs → same output across 1000 random key/idx pairs (unit test)
- [ ] Distribution: 10000 chunks with weights `{a:1, b:1}` split ~50/50 (within 5%), weights `{a:1, b:3}` split ~25/75 (within 5%)
- [ ] Empty/nil policy → returns `""` so caller falls back to existing `$defaultCluster` behavior (no behavior change for unconfigured buckets)
- [ ] Zero-weight cluster in policy: never picked
- [ ] `internal/data/rados/Backend.PutChunks` reads `Bucket.Placement` (passed via existing call-site that already has the bucket) and routes each chunk via `placement.PickCluster`
- [ ] `internal/data/s3/Backend.PutChunks` same
- [ ] Both backends keep their existing `$defaultCluster` fallback when policy is nil/empty
- [ ] Integration test with two RADOS clusters: 100 chunks with `{c1:1, c2:0}` all land on `c1`; flip to `{c1:0, c2:1}` and 100 NEW chunks all land on `c2` (old chunks stay on `c1` — they migrate via rebalance worker, not PUT)
- [ ] Typecheck passes, `go test ./internal/data/placement/...` + RADOS integration test pass

### US-003: Rebalance worker scaffold + leader election
**Description:** As an operator, I want a background worker that periodically scans buckets and reports actual-vs-target chunk distribution so I can see migration progress without manual SSH.

**Acceptance Criteria:**
- [ ] New file `cmd/strata/workers/rebalance.go` registers a worker named `rebalance` via `init()` calling `workers.Register`
- [ ] `internal/rebalance/worker.go::Worker` implements `workers.Runner`
- [ ] Leader-elected via outer `rebalance-leader` lease (NOT `SkipLease`)
- [ ] Envs at constructor time: `STRATA_REBALANCE_INTERVAL` (Go duration, default `1h`, range `[1m, 24h]`), `STRATA_REBALANCE_RATE_MB_S` (default `100`, range `[1, 10000]`), `STRATA_REBALANCE_INFLIGHT` (default `4`, range `[1, 64]`)
- [ ] Per-iteration: walks `meta.Store.ListAllBuckets()`, for each bucket with a non-nil `Placement` runs `scanDistribution(bucket)` → returns `map[cluster]chunkCount` + `[]Move{ObjectKey, ChunkIdx, FromCluster, ToCluster}`
- [ ] No moves dispatched yet (US-003 is scaffold only — `executeMoves` is a stub returning nil); worker emits info log `"rebalance plan"` with `bucket=<name>`, `moves=<n>`, `actual=...`, `target=...`
- [ ] Iteration span: `tracer.Start(ctx, "worker.rebalance.tick")` via the shared `iteration_span.go` helper (US-004 from the tracing cycle); sub-op span per bucket `rebalance.scan_bucket`
- [ ] Worker registered in supervisor list at `internal/serverapp.Run`; visible to `STRATA_WORKERS=rebalance` and validated by `workers.Resolve`
- [ ] Per-iteration metric `strata_rebalance_planned_moves_total{bucket}` incremented per planned move (counter)
- [ ] Typecheck passes, unit tests cover the scan loop with an in-memory meta store + a fake Bucket{Placement}, no real data backend needed

### US-004: RADOS-side mover — Read + Write + manifest CAS + GC enqueue
**Description:** As an operator, I want the rebalance worker to actually move chunks between RADOS clusters so the distribution converges on the policy without manual `rados cp` ceremony.

**Acceptance Criteria:**
- [ ] `internal/rebalance/rados_mover.go::Move(ctx, src, tgt *rados.Cluster, plan []Move) error`
- [ ] Per move: Read chunk bytes from `src` ioctx, Write to `tgt` ioctx (preserve length + chunk size), then collect into per-object batches keyed by `(bucketID, objectKey)`
- [ ] Per-object batched manifest CAS via `meta.Store.SetObjectStorage(ctx, bucket, key, versionID, expectedClass, newClass, updatedManifest)` (already exists for lifecycle path) — single LWT per object even when multiple chunks moved
- [ ] On manifest CAS success: enqueue old `{Pool, OID}` into the GC queue via existing `meta.Store.EnqueueGCEntry` — old blob deleted by gc worker per existing grace window
- [ ] On manifest CAS reject (`applied=false`): worker discards the freshly written target chunks via GC queue and increments `strata_rebalance_cas_conflicts_total{bucket}` — concurrent client write wins (mirrors lifecycle's CAS-loses pattern)
- [ ] Bandwidth throttle: token-bucket limiter at `STRATA_REBALANCE_RATE_MB_S` (golang.org/x/time/rate) — reads + writes both consume from same bucket
- [ ] Inflight concurrency: bounded `errgroup` at `STRATA_REBALANCE_INFLIGHT` — one in-flight move per slot
- [ ] Metrics: `strata_rebalance_bytes_moved_total{from,to}` (counter), `strata_rebalance_chunks_moved_total{from,to,bucket}` (counter)
- [ ] Two-cluster integration test (`make up-all` lab style): plant 100 chunks across cluster `a` via PUT, set Placement to `{a:0, b:1}`, run one rebalance tick, verify (a) 100 chunks now present on `b` via `rados -p data ls | grep`, (b) old OIDs enqueued in GC queue, (c) manifests reference `Pool: data-b`
- [ ] Typecheck passes, integration test green under `make test-rados`

### US-005: S3-side mover — CopyObject + Get/Put fallback
**Description:** As an operator running multi-cluster S3-over-S3, I want the rebalance worker to move chunks between S3 clusters using the cheapest available SDK path.

**Acceptance Criteria:**
- [ ] `internal/rebalance/s3_mover.go::Move(ctx, src, tgt *s3.Cluster, plan []Move) error`
- [ ] If `src.Region == tgt.Region` AND `src.Endpoint == tgt.Endpoint` → `s3.CopyObject` (server-side copy, no bytes through gateway)
- [ ] Else → `GetObject` from src streaming into `PutObject` to tgt (chunk-size buffered, no full-object materialisation)
- [ ] Per-object batched manifest CAS via existing `meta.Store.SetObjectStorage` — same shape as RADOS mover
- [ ] On CAS success: enqueue old `{Pool, OID}` into GC queue
- [ ] On CAS reject: discard target-side chunks via GC queue, increment `strata_rebalance_cas_conflicts_total{bucket}`
- [ ] Throttle: same token-bucket + errgroup primitives as RADOS mover (shared `internal/rebalance/throttle.go`)
- [ ] Metrics: same `_bytes_moved_total` + `_chunks_moved_total` counters (label `from`/`to` carry cluster ID)
- [ ] Integration test against `testcontainers` minio (two minio instances): plant 100 chunks on cluster `c1`, set Placement `{c1:0, c2:1}`, run rebalance, verify chunks on `c2` and GC queue contains old keys
- [ ] Typecheck passes, integration test green under `make test-integration` (tagged `integration`)

### US-006: Safety rails — cluster fill probe + drain sentinel
**Description:** As an operator, I want the rebalance worker to refuse moves when the target is nearly full or when the source is mid-drain so I don't trigger a cascade or fight an in-flight deregistration.

**Acceptance Criteria:**
- [ ] **Cluster fill probe (RADOS):** `data.Backend.ClusterStats(ctx, clusterID) (used, total int64, err error)` for RADOS uses `MonCommand({"prefix":"df"})` parsed JSON; for S3 returns `ErrNotSupported` (S3 has no native cluster-level fill semantics — operator opts into per-cluster byte-ceiling envs in a follow-up P3)
- [ ] **Refuse rebalance dispatch if `used/total > 0.90` on target** — log WARN `"rebalance refused: target full"` + bucket/target labels, increment `strata_rebalance_refused_total{reason="target_full",target}`
- [ ] **Drain sentinel:** new `cluster_state` row keyed on `clusterID` with `state ∈ {"live", "draining", "removed"}` stored in `meta.Store` via `setBucketBlob`-style helpers; default state is `"live"` (implicit — no row required)
- [ ] Admin API: `POST /admin/v1/clusters/{id}/drain` flips state to `"draining"`; `POST /admin/v1/clusters/{id}/undrain` flips back to `"live"`; `GET /admin/v1/clusters` lists every cluster registered in env with state + chunk-count snapshot from the latest rebalance scan
- [ ] **Refuse rebalance dispatch if `tgt.state == "draining"`** — log WARN `"rebalance refused: target draining"`, increment `strata_rebalance_refused_total{reason="target_draining",target}`
- [ ] **Allow rebalance dispatch if `src.state == "draining"`** — this is exactly the operator workflow (drain old cluster off into new ones)
- [ ] **Refuse PUT chunk routing to a `draining` cluster** — `placement.PickCluster` skips entries where the cluster state is `"draining"` and walks past them; if all clusters in policy are draining → returns `""` so caller falls back to `$defaultCluster`
- [ ] Audit row on drain/undrain admin call
- [ ] Integration test: register 2 clusters, drain `c1`, verify (a) `GET /admin/v1/clusters` shows `c1.state=draining`, (b) new PUTs route to `c2` even when Placement is `{c1:1, c2:0}`, (c) rebalance worker moves chunks `c1 → c2`, (d) attempting to drain a `c2` that is at 95% full is refused-by-warning
- [ ] Typecheck passes, integration test green

### US-007: Docs + ROADMAP close-flip + PRD removal
**Description:** As a future operator, I need the operator workflow + tuning knobs documented so I can run a real cluster drain without code-diving.

**Acceptance Criteria:**
- [ ] New page `docs/site/content/best-practices/placement-rebalance.md`: operator workflow (register → set Placement → drain → rebalance → deregister), env tuning table (`STRATA_REBALANCE_*`), troubleshooting (CAS conflict storms, target-full refusals)
- [ ] `docs/site/content/architecture/observability.md` adds the `strata_rebalance_*` metric family + `worker.rebalance.tick` span shape
- [ ] `docs/site/content/best-practices/_index.md` index entry
- [ ] Project `CLAUDE.md` "Background workers" section gets a new bullet describing `--workers=rebalance` (env names, leader lease name, what it touches)
- [ ] `ROADMAP.md` P2 line 185 close-flip: `~~**P2 — Per-bucket placement policy + cross-cluster rebalance worker.**~~ — **Done.** <one-line summary>. (commit \`<pending>\`)` — backfill SHA in follow-up commit on main
- [ ] `tasks/prd-placement-rebalance.md` REMOVED per CLAUDE.md PRD lifecycle rule (Ralph snapshot is canonical)
- [ ] `make docs-build` succeeds
- [ ] Typecheck passes, `make vet`, `make test` pass

## Functional Requirements

- **FR-1:** `meta.Bucket.Placement map[string]int` field stores the per-bucket policy; absent/empty == fall back to `$defaultCluster`
- **FR-2:** Admin API `PUT/GET/DELETE /admin/v1/buckets/{name}/placement` round-trips the policy
- **FR-3:** Weight validation: each weight `∈ [0, 100]`, `sum(weights) > 0`, every cluster key must resolve to a live cluster in env
- **FR-4:** Chunk PUT (both RADOS + S3) consults the policy via `placement.PickCluster(bucketID, key, chunkIdx, policy) string` — same inputs always return the same cluster
- **FR-5:** Hash function: `fnv32a("<bucketID>/<key>/<chunkIdx>") % sum(weights)` walking the weight wheel
- **FR-6:** Empty/nil policy → router returns `""` → caller falls back to existing `$defaultCluster` behavior (no breaking change)
- **FR-7:** `strata server --workers=rebalance` runs leader-elected on `rebalance-leader` with periodic scan at `STRATA_REBALANCE_INTERVAL` (default `1h`)
- **FR-8:** Rebalance worker scans `meta.Store.ListAllBuckets`, computes actual-vs-target distribution per bucket, copies chunks A→B until actual matches target ±1 chunk
- **FR-9:** RADOS mover: Read from src ioctx + Write to tgt ioctx + per-object batched manifest CAS + GC enqueue of old OID
- **FR-10:** S3 mover: `CopyObject` if same endpoint+region, else `GetObject` + `PutObject` streaming + per-object batched manifest CAS + GC enqueue of old OID
- **FR-11:** Throttle via `STRATA_REBALANCE_RATE_MB_S` (token bucket) + `STRATA_REBALANCE_INFLIGHT` (bounded errgroup)
- **FR-12:** Cluster fill probe: refuse mover dispatch if `used/total > 0.90` on target (RADOS only — S3 returns `ErrNotSupported`)
- **FR-13:** Drain sentinel: `cluster_state` row with `state ∈ {live, draining, removed}`; admin API to flip it; PUT routing skips draining clusters; rebalance worker refuses moves INTO a draining cluster but allows moves OUT of one
- **FR-14:** Metrics: `strata_rebalance_planned_moves_total`, `_chunks_moved_total{from,to,bucket}`, `_bytes_moved_total{from,to}`, `_cas_conflicts_total{bucket}`, `_refused_total{reason,target}`
- **FR-15:** Spans: per-iteration `worker.rebalance.tick` (via existing helper) + per-bucket `rebalance.scan_bucket` + per-move `rebalance.move_chunk`

## Non-Goals

- **No per-class placement override.** Storage-class → cluster mapping stays in `STRATA_S3_CLASSES`; per-bucket `Placement` is the only new dimension.
- **No dynamic cluster registry / runtime add-remove.** Cluster set stays env-driven per the prior won't-do decision (ROADMAP line 183).
- **No S3-side cluster fill probe.** `ClusterStats` returns `ErrNotSupported` for S3 in this cycle; per-cluster byte-ceiling envs are a P3 follow-up.
- **No automatic rebalance trigger on Placement change.** Operator waits for the periodic scan; on-demand `POST /admin/v1/buckets/{name}/rebalance` is a P3 follow-up (user picked `3A`).
- **No cross-region erasure-coded chunks.** EC remains intra-cluster; this cycle moves whole `{Pool, OID}` pairs between clusters, not chunk fragments.
- **No quota-aware placement.** Bucket quota enforcement (`internal/s3api/quota.go`) is orthogonal — policy only governs where chunks land, not how many.
- **No bucket-level migration UI.** Web UI surface beyond the placement-edit dialog (e.g. live rebalance progress page) is out of scope for this PRD.

## Design Considerations

- **Reuse the blob-config helper pattern** (`setBucketBlob` / `getBucketBlob` / `deleteBucketBlob`) for `cluster_state` — it's a single document keyed on cluster ID, identical shape to lifecycle/CORS/policy.
- **Worker shape**: register via `cmd/strata/workers/rebalance.go::init()`, follow the iteration-span + leader-elected supervisor pattern documented in CLAUDE.md "Background workers". Do NOT use `SkipLease` — this is a simple outer lease (one leader cluster-wide).
- **Span naming**: `worker.rebalance.tick` (iteration parent) + `rebalance.scan_bucket` + `rebalance.move_chunk` (sub-ops) — matches the convention established in the complete-tracing cycle.
- **Manifest CAS shape**: reuse `meta.Store.SetObjectStorage` (already used by lifecycle for transition). It enforces `IF storage_class = expected` semantics; on rebalance we pass `expectedClass == currentClass` because we're not changing class, just chunk locations. The signature already accepts a `data.Manifest` arg.
- **Web UI**: a single `<EditPlacement>` dialog on `BucketDetail` (next to existing Quota / Lifecycle / CORS dialogs) that round-trips `PUT /admin/v1/buckets/{name}/placement`. Defer cluster-list view + drain controls to a P3 follow-up.

## Technical Considerations

- **Bucket-scoped chunk identity.** The hash key `"<bucketID>/<key>/<chunkIdx>"` includes `bucketID` (UUID) so two buckets with the same key and policy don't co-locate on the same cluster (avoids unintentional hot spots).
- **Weight wheel walk** (per cluster pick): given `weights = [3, 1, 2]` and hash `h ∈ [0, 6)`, iterate sorted cluster IDs accumulating `[3, 4, 6]` and pick the first index where `h < accumulated`. Deterministic + O(N) where N = cluster count (small, typically <16).
- **Drain-sentinel read on PUT hot path.** Reading the drain state on every PUT would round-trip Cassandra/TiKV. Cache it in-process with a 30s TTL — drain is a slow operator action, eventual consistency for 30s is fine.
- **Throttle semantics.** Token bucket budgets `bytes_per_second`; one read + one write each consume `chunkSize` tokens (so a 4 MiB chunk move at 100 MB/s consumes 8 MiB of tokens → ~12 chunks/sec under default throttle).
- **GC interaction.** Manifest CAS must precede GC enqueue — if we enqueue first and CAS fails, GC may delete the still-live source chunk. Order: copy → CAS → if applied: enqueue old, if rejected: enqueue target (the unused copy).
- **Two-cluster lab.** `deploy/docker/docker-compose.yml` ships only one Ceph cluster today; integration tests can use the existing `ceph` service as `c1` and stand up a second `ceph-b` profile-gated service for `c2`. Or use the in-memory data backend's existing multi-cluster shape if available; check `internal/data/memory/` before adding compose complexity.
- **Backward compatibility on Placement absence**: every existing bucket has `Placement == nil`; the entire PUT routing path must short-circuit to `$defaultCluster` when this is true, with zero schema migration required.

## Success Metrics

- 100% of chunks PUT to a bucket with `Placement={a:0, b:1}` land on cluster `b` (verified by lab test)
- After rebalance scan with N=100 chunks on `a` and policy flipped to `{a:0, b:1}`, 100 chunks present on `b` and 100 GC entries enqueued for `a`'s OIDs within `STRATA_REBALANCE_INTERVAL`
- Refused-mover counter increments when target cluster crosses 90% used
- Drain sentinel correctly skips PUT routing to a `draining` cluster within 30s (drain-cache TTL window)
- Zero behavior change for buckets without a Placement policy (regression-test: existing smoke tests pass)

## Open Questions

- Per-bucket Placement editor in Web UI: where exactly on `BucketDetail`? Best fit is alongside the existing Lifecycle / CORS dialogs. (Deferred — not blocking.)
- Token-bucket sharing across multiple rebalance worker iterations vs per-iteration fresh bucket: pick per-iteration fresh for simplicity (no leaked rate budget across ticks).
- On-demand rebalance trigger: deferred to P3 (user picked `3A`).
- Per-cluster S3 byte-ceiling envs (e.g. `STRATA_S3_CLUSTERS_MAX_BYTES`): deferred to P3 follow-up.
