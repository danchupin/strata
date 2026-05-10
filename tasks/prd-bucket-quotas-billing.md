# PRD: Bucket / user quotas + usage-based billing

## Introduction

Strata today has zero quota enforcement — a runaway client can fill the cluster
with no S3-level pushback. This cycle adopts the AWS / RGW shape: per-bucket
`BucketQuota{MaxBytes, MaxObjects, MaxBytesPerObject}` and per-user
`UserQuota{MaxBuckets, TotalMaxBytes}`, both persisted in `meta.Store` and
enforced at PUT-validate time with a hard `QuotaExceeded` 403 response. Live
usage is tracked via a denormalised `bucket_stats` row updated atomically on
every PUT / DELETE; a leader-elected reconcile worker fixes drift. A nightly
usage rollup worker aggregates `(bucket_id, storage_class, day, byte_seconds,
object_count)` into `usage_aggregates`, which together with the existing
`strata_http_requests_total{bucket,access_key}` Prometheus counters feeds an
external invoice generator (out of scope this cycle — stop at the usage feed).

Operator-facing: admin endpoints for getting / setting quotas and reading usage.
UI: a per-bucket Usage tab and a per-user Billing summary.

## Goals

- Hard quota enforcement at PUT-validate time. `BucketQuota` covers
  `MaxBytes` / `MaxObjects` / `MaxBytesPerObject`; `UserQuota` covers
  `MaxBuckets` / `TotalMaxBytes`.
- Live `bucket_stats{bucket_id, used_bytes, used_objects}` row updated
  atomically on every successful PUT / DELETE.
- Reconcile worker corrects drift between `bucket_stats` and the actual sum of
  object sizes (LWT-based, leader-elected).
- Nightly usage rollup worker writes `usage_aggregates` rows per
  `(bucket_id, storage_class, day)`.
- Admin API endpoints: `GET/PUT /admin/v1/buckets/{name}/quota`,
  `GET /admin/v1/iam/users/{user}/quota` + `PUT`,
  `GET /admin/v1/buckets/{name}/usage?start=&end=`,
  `GET /admin/v1/iam/users/{user}/usage?start=&end=`.
- Web UI: per-bucket Usage tab (current usage + quota bar + 30-day chart) and
  per-user Billing summary (per-bucket usage table + total).
- All three meta backends (memory / Cassandra / TiKV) implement the new
  surfaces in lockstep; contract tests cover them.

## User Stories

### US-001: `BucketQuota` + `UserQuota` schema + `meta.Store` API (memory + contract)
**Description:** As a developer I need quota types persisted in `meta.Store`
on the memory backend so the rest of the surface has something to hang off.

**Acceptance Criteria:**
- [ ] New types in `internal/meta/store.go`:
  `BucketQuota{MaxBytes, MaxObjects, MaxBytesPerObject int64}` (zero ⇒
  unlimited), `UserQuota{MaxBuckets int32, TotalMaxBytes int64}`.
- [ ] New `meta.Store` methods: `GetBucketQuota(ctx, bucketID) (BucketQuota,
  bool, error)` (bool = configured), `SetBucketQuota(ctx, bucketID, q)`,
  `DeleteBucketQuota(ctx, bucketID)`; same triple for `UserQuota` keyed on
  user name (`GetUserQuota` / `SetUserQuota` / `DeleteUserQuota`).
- [ ] Memory backend implements all six methods. Stored under
  `bucketBlobKindQuota` / `userBlobKindQuota` via the existing
  `setBucketBlob` / `getBucketBlob` helpers.
- [ ] Contract test in `internal/meta/storetest/contract.go::caseBucketQuota`
  + `caseUserQuota`: round-trip set → get → delete → get-missing.
- [ ] Contract test runs against memory backend.
- [ ] `go vet ./...` clean; `go test ./...` clean.

### US-002: Quota schema + API on Cassandra backend
**Description:** As a developer I need the quota API live on Cassandra with no
destructive migration.

**Acceptance Criteria:**
- [ ] Cassandra schema in `internal/meta/cassandra/schema.go`: `bucket_blobs`
  + `user_blobs` (or whichever existing per-bucket-/per-user-blob table is
  canonical) gain quota blob kinds. Reuse the blob-config helper pattern; no
  new tables needed if the existing blob tables fit.
- [ ] Cassandra `GetBucketQuota` / `SetBucketQuota` / `DeleteBucketQuota` +
  the user triple implemented via the existing blob helpers.
- [ ] Integration test (build tag `integration`): contract suite green on
  Cassandra container.
- [ ] `make test-integration` passes.
- [ ] `go vet ./...` clean; `go test ./...` clean.

### US-003: Quota schema + API on TiKV backend
**Description:** As a developer I need the quota API live on TiKV.

**Acceptance Criteria:**
- [ ] TiKV key shapes: `bq/<bucketID>` for BucketQuota, `uq/<userName>` for
  UserQuota. Variable-length user-name segment uses the FoundationDB
  byte-stuffing already in `internal/meta/tikv/keys.go`.
- [ ] TiKV `Get/Set/Delete` triple for bucket + user quota implemented.
- [ ] Integration test against PD+TiKV containers passes (CI workflow
  `ci-tikv.yml`) — same contract assertions as US-001/US-002.
- [ ] `go vet ./...` clean; `go test ./...` clean.

### US-004: `bucket_stats` denormalised counter (memory + contract)
**Description:** As a developer I need a per-bucket live counter row that
PUT / DELETE updates atomically so quota checks are O(1).

**Acceptance Criteria:**
- [ ] New type `BucketStats{UsedBytes int64, UsedObjects int64, UpdatedAt
  time.Time}` in `internal/meta/store.go`.
- [ ] New `meta.Store` methods: `GetBucketStats(ctx, bucketID)
  (BucketStats, error)`, `BumpBucketStats(ctx, bucketID, deltaBytes int64,
  deltaObjects int64) (BucketStats, error)` — atomic increment that returns
  the post-update value.
- [ ] Existing `PutObject` / `DeleteObject` / `multipart Complete` /
  `lifecycle expire` paths in memory backend wrapped in `BumpBucketStats`
  calls so the live counter stays coherent. Negative deltas allowed (for
  DELETE).
- [ ] Contract test `caseBucketStats`: 100 sequential PUTs → assert
  `UsedBytes` matches expected sum; 100 concurrent PUTs → assert no lost
  updates (memory backend must be safe under concurrent
  `BumpBucketStats`).
- [ ] Contract test runs against memory backend.
- [ ] `go vet ./...` clean; `go test ./...` clean.

### US-005: `bucket_stats` on Cassandra + TiKV
**Description:** As a developer I need the live counter on the production
backends with read-after-write coherence under concurrent PUTs.

**Acceptance Criteria:**
- [ ] Cassandra: new table `bucket_stats{bucket_id uuid PRIMARY KEY,
  used_bytes bigint, used_objects bigint, updated_at timestamp}` added to
  `tableDDL`. `BumpBucketStats` uses `UPDATE … SET used_bytes = used_bytes +
  ?, used_objects = used_objects + ? WHERE bucket_id = ? IF EXISTS` (LWT to
  ensure read-after-write coherence per the CLAUDE.md gotcha). First call
  upserts via INSERT IF NOT EXISTS.
- [ ] TiKV: pessimistic txn (`Begin → LockKeys → Get → Set → Commit`) on key
  `bs/<bucketID>` so concurrent bumps serialize correctly. Empty key reads
  as zero stats.
- [ ] PUT / DELETE / multipart Complete / lifecycle expire paths wired
  through `BumpBucketStats` on both backends.
- [ ] Cassandra integration test passes; TiKV integration test passes.
- [ ] `go vet ./...` clean; `go test ./...` clean.

### US-006: PUT-validate quota enforcement + `QuotaExceeded` 403
**Description:** As an operator I want hard quota rejection at PUT time so a
runaway client cannot fill the cluster.

**Acceptance Criteria:**
- [ ] New sentinel error `meta.ErrQuotaExceeded` in `internal/meta/store.go`.
- [ ] New `APIError` in `internal/s3api/errors.go`: HTTP 403, S3 code
  `QuotaExceeded`, message "Quota exceeded for bucket / user". (Non-AWS but
  matches RGW shape — drop-in compatibility).
- [ ] `internal/s3api.putObject` (and multipart `UploadPart` /
  `CompleteMultipartUpload` size-bumping points) check
  `BucketQuota.MaxBytes`, `BucketQuota.MaxObjects`,
  `BucketQuota.MaxBytesPerObject`, `UserQuota.TotalMaxBytes` BEFORE writing
  data. If quota would be exceeded, reject with 403 `QuotaExceeded`. Live
  usage source: `bucket_stats` (bucket-scoped) +
  sum-of-`bucket_stats`-across-user-buckets (user-scoped).
- [ ] `MaxBytesPerObject` checked against `Content-Length` (single PUT) or
  declared multipart total at `CompleteMultipartUpload` time.
- [ ] `MaxBuckets` enforced at `CreateBucket` time: reject 403
  `QuotaExceeded` if user has more than `UserQuota.MaxBuckets` already (sum
  via `ListBuckets(ctx, owner)` or denormalised `user_bucket_count`).
- [ ] Unit tests in `internal/s3api`: PUT under quota → 200; PUT exactly at
  quota → 200; PUT just past quota → 403; multipart that would exceed
  quota at Complete → 403; CreateBucket past MaxBuckets → 403.
- [ ] `go vet ./...` clean; `go test ./...` clean.

### US-007: Quota reconcile worker
**Description:** As an operator I want a leader-elected reconcile worker that
periodically corrects drift between `bucket_stats` and the actual sum of
object sizes.

**Acceptance Criteria:**
- [ ] New worker `cmd/strata/workers/quota-reconcile.go` registered via
  `workers.Register` in `init()`. Build receives `workers.Dependencies`,
  returns `workers.Runner`.
- [ ] Leader-elected on `quota-reconcile-leader` lease (single replica
  reconciles at a time to avoid double-correction).
- [ ] Periodic tick (`STRATA_QUOTA_RECONCILE_INTERVAL`, default 6h) walks
  every bucket: sum object sizes via existing object-list APIs, compare
  against `bucket_stats`, log + correct if drift > 0.5 % or > 1 MB.
- [ ] Drift correction uses LWT/pessimistic-txn shape: read current stats,
  compute delta, apply via `BumpBucketStats(delta)`. Concurrent live
  PUT/DELETE during reconcile must not lose updates.
- [ ] New Prometheus metric
  `strata_quota_reconcile_drift_bytes{bucket}` (gauge, last observed drift
  per bucket).
- [ ] Worker registered in `cmd/strata/workers/registry.go`'s recognised
  list; `STRATA_WORKERS=quota-reconcile` resolves cleanly.
- [ ] Unit test: seed memory backend with stats showing fake drift; run one
  reconcile tick; assert stats now match object sum.
- [ ] `go vet ./...` clean; `go test ./...` clean.

### US-008: Usage rollup worker — nightly per (bucket, storage_class)
**Description:** As an operator I want a leader-elected nightly worker that
aggregates `(bucket_id, storage_class, day, byte_seconds, object_count)` into
the `usage_aggregates` table for billing.

**Acceptance Criteria:**
- [ ] New `usage_aggregates` table / KV shape in all three backends:
  `(bucket_id, storage_class, day) → {byte_seconds, object_count_avg,
  object_count_max, computed_at}`.
- [ ] Cassandra schema: `usage_aggregates{bucket_id uuid, storage_class
  text, day date, byte_seconds bigint, object_count_avg bigint,
  object_count_max bigint, computed_at timestamp, PRIMARY KEY ((bucket_id,
  storage_class), day)}` added to `tableDDL`. Range scans by day work via
  the clustering key.
- [ ] TiKV key shape: `ua/<bucketID>/<storageClass>/<day-epoch-BE>` →
  CBOR/JSON-encoded payload. Day uses fixed-width BE so range scans return
  ascending.
- [ ] New `meta.Store` methods: `WriteUsageAggregate(ctx, bucketID,
  storageClass, day, agg)`, `ListUsageAggregates(ctx, bucketID,
  storageClass, dayFrom, dayTo)`, `ListUserUsage(ctx, userName, dayFrom,
  dayTo)` (sums across all user's buckets per (storage_class, day)).
- [ ] New worker `cmd/strata/workers/usage-rollup.go`. Leader-elected on
  `usage-rollup-leader`. Cadence: `STRATA_USAGE_ROLLUP_AT` (default
  `00:00 UTC`) + `STRATA_USAGE_ROLLUP_INTERVAL` (default 24h).
- [ ] On tick: for each bucket, sample current `bucket_stats` once (good
  enough for byte_seconds × 24h approximation in v1 — the simple model
  trades absolute precision for code simplicity; doc this in
  `docs/site/content/best-practices/quotas-billing.md`). Write
  `(bucket_id, storage_class, yesterday-UTC, byte_seconds, object_count)`.
- [ ] Worker registered in `registry.go`; `STRATA_WORKERS=usage-rollup`
  resolves.
- [ ] Unit test: seed bucket_stats; trigger rollup; assert
  `usage_aggregates` row written for yesterday with correct
  `byte_seconds = used_bytes * 86400`.
- [ ] `go vet ./...` clean; `go test ./...` clean.

### US-009: Admin endpoints — quota + usage
**Description:** As an operator I want JSON admin endpoints to read / set
quotas and read usage history.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/buckets/{name}/quota` → 200
  `{maxBytes, maxObjects, maxBytesPerObject}` or 404 if not configured.
- [ ] `PUT /admin/v1/buckets/{name}/quota` body
  `{maxBytes, maxObjects, maxBytesPerObject}` → 204. Zero ⇒ unlimited.
- [ ] `DELETE /admin/v1/buckets/{name}/quota` → 204.
- [ ] `GET /admin/v1/iam/users/{user}/quota` → 200
  `{maxBuckets, totalMaxBytes}` or 404. `PUT` + `DELETE` mirror.
- [ ] `GET /admin/v1/buckets/{name}/usage?start=YYYY-MM-DD&end=YYYY-MM-DD`
  → 200
  `{rows: [{day, storageClass, byteSeconds, objectCountAvg,
  objectCountMax}]}`.
- [ ] `GET /admin/v1/iam/users/{user}/usage?start=…&end=…` → 200
  `{rows: [{bucket, day, storageClass, byteSeconds, …}], totals: {bytes,
  objects}}`.
- [ ] Each admin handler stamps `s3api.SetAuditOverride(ctx,
  "admin:GetBucketQuota" / "admin:PutBucketQuota" / …, …)` so audit-log
  rows are operator-meaningful.
- [ ] OpenAPI contract `internal/adminapi/openapi.yaml` updated with the
  new endpoints + schemas.
- [ ] Handler unit tests: golden-path + 404 + invalid-body 400.
- [ ] `go vet ./...` clean; `go test ./...` clean.

### US-010: Web UI — per-bucket Usage tab + per-user Billing summary
**Description:** As an operator using the embedded console I want a Usage tab
on each bucket and a Billing summary for each user.

**Acceptance Criteria:**
- [ ] New tab in the per-bucket page (`web/src/pages/BucketDetail.tsx` or
  equivalent): "Usage" — current `bucket_stats` (used_bytes /
  used_objects) shown as a progress bar against the bucket's
  `BucketQuota.MaxBytes` (gray bar if no quota set), 30-day usage chart
  (line chart of `byteSeconds / 86400` per day from
  `/admin/v1/buckets/{name}/usage`), per-storage-class breakdown table.
- [ ] "Edit Quota" button on the Usage tab opens a modal to set
  `maxBytes` / `maxObjects` / `maxBytesPerObject`. Saves via `PUT /admin/v1/
  buckets/{name}/quota`, refreshes the tab.
- [ ] New page in IAM section: per-user Billing summary
  (`web/src/pages/UserBilling.tsx`) — per-bucket usage table + 30-day
  chart aggregated across the user's buckets, "Edit User Quota" button
  for `UserQuota`.
- [ ] Both pages handle 404-not-configured gracefully (show "No quota set"
  + the Edit button).
- [ ] Playwright e2e test in `web/e2e/quota-billing.spec.ts`: set bucket
  quota via UI → reload → quota persists; PUT object that exceeds quota
  via aws-cli → S3 returns 403 QuotaExceeded.
- [ ] Verify in browser using dev-browser skill (set quota, observe
  enforcement, check the chart renders).
- [ ] `go vet ./...` clean; `go test ./...` clean; web typecheck passes;
  Playwright e2e passes.

### US-011: Docs + ROADMAP close-flip
**Description:** As a maintainer I need the ROADMAP P2 entry flipped Done with
a pointer to the operator docs.

**Acceptance Criteria:**
- [ ] New page
  `docs/site/content/best-practices/quotas-billing.md` covers: quota
  shape (BucketQuota / UserQuota), what triggers `QuotaExceeded`, how to
  set quotas via admin API + UI, how the rollup worker turns
  `bucket_stats` into `usage_aggregates`, the `byte_seconds × 24h`
  approximation caveat, the reconcile worker's drift-correction shape,
  pointer to the audit log + Prometheus metrics.
- [ ] `docs/site/content/reference/_index.md` (or its env-vars subpage if
  it lands first) gains entries for `STRATA_QUOTA_RECONCILE_INTERVAL`,
  `STRATA_USAGE_ROLLUP_AT`, `STRATA_USAGE_ROLLUP_INTERVAL`.
- [ ] `ROADMAP.md` line 180 (current P2 — Bucket / user quotas +
  usage-based billing) flipped to `~~**P2 — Bucket / user quotas +
  usage-based billing.**~~ — **Done.** <one-line summary citing the
  surface shipped + link to best-practices doc>. (commit \`<pending>\`)`.
- [ ] If any ROADMAP discoveries surface during the cycle (e.g. a missing
  metric, a known approximation), add new entries in the same close-flip
  per CLAUDE.md "Discovering a new gap" rule.
- [ ] `tasks/prd-bucket-quotas-billing.md` REMOVED in this commit per
  CLAUDE.md PRD lifecycle rule.
- [ ] No regressions: `go vet ./...`, `go test ./...`, `make smoke` all
  clean.
- [ ] Closing-SHA backfill follow-up commit on main per established
  convention.

## Functional Requirements

- FR-1: `meta.Store` interface gains `GetBucketQuota` / `SetBucketQuota` /
  `DeleteBucketQuota` + `GetUserQuota` / `SetUserQuota` / `DeleteUserQuota`.
- FR-2: `meta.Store` gains `GetBucketStats` and `BumpBucketStats(deltaBytes,
  deltaObjects)` — atomic increment with read-after-write coherence on
  Cassandra (LWT) + TiKV (pessimistic txn) + memory.
- FR-3: All three backends (memory / Cassandra / TiKV) implement the new
  surfaces in lockstep; contract tests verify parity.
- FR-4: PUT / multipart Complete / DELETE / lifecycle-expire paths wrapped
  in `BumpBucketStats`. Object lifecycle transitions move bytes between
  storage classes — out of scope for live counter (rollup handles
  per-class breakdown via `usage_aggregates`).
- FR-5: `internal/s3api.putObject` + multipart endpoints + CreateBucket
  reject with 403 `QuotaExceeded` when the new size would exceed
  `BucketQuota.MaxBytes` / `MaxObjects` / `MaxBytesPerObject` /
  `UserQuota.TotalMaxBytes` / `UserQuota.MaxBuckets`. Hard limit; no soft
  warning.
- FR-6: `quota-reconcile` worker (leader-elected, default 6h) corrects drift
  between `bucket_stats` and actual object-size sum. Drift threshold:
  > 0.5 % OR > 1 MB.
- FR-7: `usage-rollup` worker (leader-elected, default 24h at 00:00 UTC)
  writes `(bucket_id, storage_class, day, byte_seconds, object_count)` to
  `usage_aggregates`. v1 approximation: `byte_seconds = used_bytes ×
  86400` based on a single sample at rollup time.
- FR-8: Admin endpoints under `/admin/v1/buckets/{name}/quota` and
  `/admin/v1/iam/users/{user}/quota` (GET / PUT / DELETE) and
  `/admin/v1/buckets/{name}/usage` + `/admin/v1/iam/users/{user}/usage`
  (GET with `start` / `end` query params).
- FR-9: OpenAPI contract `internal/adminapi/openapi.yaml` updated.
- FR-10: Web UI: per-bucket Usage tab + per-user Billing summary page.
- FR-11: Audit log rows for every quota mutation
  (`admin:PutBucketQuota` / `admin:DeleteBucketQuota` / etc).

## Non-Goals

- **No invoice ledger / payment integration.** The cycle stops at the
  `usage_aggregates` feed; downstream billing service is a separate
  ops-layer concern.
- **No soft limits.** Hard 403 only.
- **No per-storage-class quota.** A single `MaxBytes` covers all classes;
  per-class breakdowns surface only in `usage_aggregates` (read-only).
- **No live byte_seconds counter.** v1 approximation is sample-based at
  rollup time. Switching to a continuous integral is a P3 follow-up if the
  approximation proves too coarse.
- **No quota-on-DELETE-failure.** `BumpBucketStats(-N)` is best-effort;
  the reconcile worker fixes drift.
- **No quota-aware lifecycle**. Lifecycle expirations don't bypass quota
  (they just decrement `bucket_stats`).
- **No multipart-aware quota during upload**. Live counter only updates at
  Complete; mid-upload abandonment is rare and the next reconcile catches
  any leak.
- **No new third backend.** Cycle ships memory + Cassandra + TiKV — same
  as today.

## Design Considerations

- **`bucket_stats` row contention:** every PUT writes the same row. On
  TiKV, pessimistic-txn serialises but the bucket's hot-shard test will
  reveal contention bottleneck — acceptable since RGW behaves the same.
  Hot-shard mitigation (sharded counter rows) is a P3 follow-up if the
  cycle's bench surfaces it.
- **Quota config UI** reuses the existing per-bucket Settings tab pattern
  (modal form, save → toast, refresh on success).
- **Billing summary chart** reuses the chart component used by the existing
  Web UI replication / hot-buckets pages — no new chart library.

## Technical Considerations

- **Cassandra LWT on counter:** `UPDATE … SET used_bytes = used_bytes + ?
  IF EXISTS` is the canonical RAW-coherent counter shape; the CLAUDE.md
  gotcha "LWT is required for read-after-write coherence" applies.
- **TiKV pessimistic txn early-return rule:** `BumpBucketStats` paths must
  call `txn.Rollback()` on every non-error early return per the CLAUDE.md
  TiKV gotcha.
- **MaxBytesPerObject** check must cover both single-PUT (Content-Length)
  and multipart (Complete-time computed total). The latter avoids
  multipart-bypass attacks.
- **MaxBuckets** check at `CreateBucket` time benefits from a
  denormalised `user_bucket_count` row — but a `ListBuckets(ctx, owner)`
  walk is acceptable at v1 since CreateBucket isn't on the hot path.
  Document the tradeoff in code.
- **Reconcile drift threshold (0.5 % / 1 MB):** picked to absorb
  in-flight PUTs that haven't yet bumped the counter at scan time.
- **`byte_seconds × 24h` rollup** is intentionally crude. A future P3
  follow-up can integrate continuously if billing precision demands it.

## Success Metrics

- A runaway client hits a 403 within one PUT of breaching quota.
- `bucket_stats` drift < 0.5 % steady-state under synthetic mixed
  PUT/DELETE load (verified by reconcile worker logs).
- Admin endpoints + UI ship with audit-log rows so quota mutations are
  traceable.
- `usage_aggregates` table has a row per `(bucket, class, day)` after the
  first 24 h.
- No regression in S3 PUT throughput more than 5 % vs Phase-2 baseline
  (counter LWT contention should not dominate).

## Open Questions

- Hot-shard contention on `bucket_stats` under sustained 10k PUT/s for a
  single bucket — measure during the cycle bench. If problematic, add
  P3 follow-up for sharded counter rows.
- `MaxBytesPerObject` for multipart: enforcement at Complete time means
  the client has already uploaded the parts. Should we reject earlier
  (UploadPart-N when running total exceeds)? Probably yes — track as a
  follow-up if v1 shape is too lenient.
- Cross-account quota (one user's quota covers another user's buckets via
  policy) — out of scope; users own their buckets in Strata's IAM model.
- Time zone of `usage-rollup` boundary — `STRATA_USAGE_ROLLUP_AT` defaults
  to UTC; operators in other zones can override. Document the default.
