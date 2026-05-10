---
title: 'Quotas + billing'
weight: 50
description: 'Per-bucket / per-user quotas, the live bucket_stats counter, the reconcile worker, and the nightly usage_aggregates rollup that feeds external billing.'
---

# Quotas + billing

Strata enforces hard per-bucket and per-user storage quotas at PUT-validate
time and emits a nightly per-(bucket, storage class) usage aggregate that an
external invoice generator can consume. The shape mirrors AWS / RGW so a
RADOS Gateway tenant migrating to Strata sees the same `QuotaExceeded` 403
on overage.

The cycle stops at the usage feed. Invoice ledger / payment integration
lives in a separate ops-layer service that consumes `usage_aggregates`.

## Quota shape

Two quota objects, both stored in `meta.Store` and addressable via the
admin API:

| Type | Fields | Scope |
|---|---|---|
| `BucketQuota` | `MaxBytes`, `MaxObjects`, `MaxBytesPerObject` (`int64`; `0 ⇒ unlimited`) | One per bucket. |
| `UserQuota` | `MaxBuckets` (`int32`), `TotalMaxBytes` (`int64`) | One per IAM user. |

`BucketQuota.MaxBytesPerObject` caps a single object's size; the other
fields cap aggregate usage. `UserQuota.TotalMaxBytes` is enforced as the
sum of `bucket_stats.UsedBytes` across every bucket the user owns.

## What triggers `QuotaExceeded`

The gateway-side check (`internal/s3api/quota.go::checkQuota`) runs BEFORE
any chunk write on:

- `PutObject` — `Content-Length` is checked against `MaxBytesPerObject`,
  bucket bytes / objects, and the user's `TotalMaxBytes`.
- `UploadPart` — running bucket / user byte budget only (no per-object
  cap until Complete).
- `CompleteMultipartUpload` — sum-of-part-sizes vs `MaxBytesPerObject` +
  bucket bytes + bucket objects + user bytes.
- `CreateBucket` — `UserQuota.MaxBuckets` via `ListBuckets(ctx, owner)`.

On exceeded, the gateway returns:

```http
HTTP/1.1 403 Forbidden
Content-Type: application/xml

<?xml version="1.0" encoding="UTF-8"?>
<Error>
  <Code>QuotaExceeded</Code>
  <Message>Quota exceeded for bucket / user</Message>
</Error>
```

`QuotaExceeded` is the RGW shape, not standard AWS. It is documented as
the cross-vendor convention for "quota rejection at the S3 surface" and
S3 SDKs surface it through the standard error path.

Unversioned-overwrite calls subtract the prior object's size before
checking — replacing a 100 MiB object with a 100 MiB object never trips
`MaxBytes`. Versioned overwrites add the new version's size to the bucket
total because both versions persist.

## Live usage source — `bucket_stats`

`bucket_stats{bucket_id, used_bytes, used_objects, updated_at}` is the
denormalised counter the quota check reads. It is updated atomically on
every successful `PutObject` / `DeleteObject` / `CompleteMultipartUpload`
through `meta.Store.BumpBucketStats(ctx, bucketID, ΔBytes, ΔObjects)`.

Backend-specific atomicity:

- **Memory:** mutex-guarded read-modify-write.
- **Cassandra:** `UPDATE … IF used_bytes = ? AND used_objects = ?` LWT
  retry loop. Required because read-after-write coherence on a
  previously-LWT-written row demands LWT writes (CLAUDE.md gotcha).
- **TiKV:** pessimistic txn (`Begin → LockKeys → Get → Set → Commit`)
  on key `bs/<bucketID>`. CAS-reject early-return paths call
  `txn.Rollback()` explicitly.

Delete markers contribute `(0, 0)` — they occupy no bytes and the row
count is absorbed into the marker, which sits on top of versions without
removing them.

## Drift reconcile — `--workers=quota-reconcile`

`bucket_stats` can drift from the actual sum of object sizes when
operators run direct meta repairs, when LWT timeouts mis-replicate
under partition, or when the lifecycle worker bulk-expires objects
during a counter race window. The reconcile worker corrects drift
periodically.

```bash
strata server --workers=gateway,quota-reconcile
```

| Env | Default | Meaning |
|---|---|---|
| `STRATA_QUOTA_RECONCILE_INTERVAL` | `6h` | Cadence between full bucket scans. |

Per tick the worker fans out across every bucket: paginated
`ListObjects` walk → sum bytes + objects (latest non-delete-marker
only — that is exactly what `bucket_stats` represents) → re-sample
stats AFTER the walk → `drift = walk - statsAfter`. Sampling
post-walk is the canonical pattern for any future denormalised-counter
consistency check (concurrent activity during the walk composes safely
because `BumpBucketStats` only adds a delta on top — never overwrites).

The worker corrects only when drift trips a threshold:

```
correctIf objectDrift != 0
       OR |byteDrift| > max(MinDriftBytes, used * MinDriftRatio)
```

Defaults: `MinDriftBytes = 1 MiB`, `MinDriftRatio = 0.5 %`. Tiny drift is
intentionally swallowed — re-bumping every tick churns LWT for no gain.

Correction itself goes through `BumpBucketStats(driftBytes, driftObjects)`,
so a concurrent client PUT during the correction does not lose its update.

The worker is leader-elected on `quota-reconcile-leader` so a single
replica reconciles at a time and double-correction is impossible.

### Metric

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `strata_quota_reconcile_drift_bytes` | gauge | `bucket` | Last observed `byteDrift` per bucket. Positive = stats undercount; negative = overcount. |

Alert on sustained non-zero drift on a bucket — it points at a bumper
path that is not wired through `BumpBucketStats` (e.g. a new write that
bypasses the gateway).

## Usage rollup — `--workers=usage-rollup`

The rollup worker writes one `usage_aggregates` row per
`(bucket_id, storage_class, day)` covering the previous UTC day. It is
the feed the external invoice generator consumes.

```bash
strata server --workers=gateway,usage-rollup
```

| Env | Default | Meaning |
|---|---|---|
| `STRATA_USAGE_ROLLUP_AT` | `00:00` | UTC hour:minute the daily tick fires. |
| `STRATA_USAGE_ROLLUP_INTERVAL` | `24h` | Cadence between rollup ticks. |

Per tick the worker walks every bucket, samples `GetBucketStats` once,
and writes:

```
(bucket_id, storage_class, yesterday-UTC,
   byte_seconds  = used_bytes  * 86400,
   object_count_avg = used_objects,
   object_count_max = used_objects)
```

Leader-elected on `usage-rollup-leader`. Days are normalised to UTC
midnight at the write boundary (Cassandra `date` codec, TiKV's
`day-Unix / 86400` BE epoch, and the memory `int64` key all require
midnight — reads and writes miss each other otherwise).

### `byte_seconds × 24h` approximation — caveat

v1 samples `bucket_stats` once per day. `byte_seconds = used_bytes × 86400`
under-counts intra-day growth and over-counts intra-day shrinkage. A
bucket that grew from 0 → 1 TiB at 12:00 UTC bills as if it had 1 TiB
all day — overstates by 12 h × 1 TiB = 12 TiB·s. Inverse for shrinkage.

The error band is bounded by the daily delta, which is small for steady
workloads and accepted for v1. Continuous integration of `bucket_stats`
across the day is a P3 follow-up tracked in `ROADMAP.md`.

### `usage_aggregates` schema

Cassandra:

```cql
CREATE TABLE usage_aggregates (
  bucket_id     uuid,
  storage_class text,
  day           date,
  byte_seconds  bigint,
  object_count_avg bigint,
  object_count_max bigint,
  computed_at   timestamp,
  PRIMARY KEY ((bucket_id, storage_class), day)
) WITH CLUSTERING ORDER BY (day ASC);
```

A sibling `usage_aggregates_classes((bucket_id), storage_class)` index
keeps "list all classes seen for this bucket" cheap. TiKV uses the
parallel pattern `s/B/<uuid16>/ua/<escClass>\x00\x00<day4-BE>` plus
a `s/B/<uuid16>/uc/<escClass>\x00\x00` presence row. The memory backend
is an in-process map keyed on the same tuple.

## Admin API

All endpoints stamp the audit log with an operator-meaningful action
(`admin:PutBucketQuota`, `admin:DeleteUserQuota`, …) via
`s3api.SetAuditOverride`. OpenAPI contract:
`internal/adminapi/openapi.yaml`.

### Bucket quota

| Endpoint | Body | Response |
|---|---|---|
| `GET /admin/v1/buckets/{name}/quota` | — | `200 {maxBytes, maxObjects, maxBytesPerObject}` or `404 NoSuchBucketQuota`. |
| `PUT /admin/v1/buckets/{name}/quota` | `{maxBytes, maxObjects, maxBytesPerObject}` (zero = unlimited) | `204` |
| `DELETE /admin/v1/buckets/{name}/quota` | — | `204` (idempotent) |
| `GET /admin/v1/buckets/{name}/usage?start=YYYY-MM-DD&end=YYYY-MM-DD` | — | `200 {rows: [{day, storageClass, byteSeconds, objectCountAvg, objectCountMax}]}` |

`start` / `end` are inclusive UTC days. Defaults: `end = today`,
`start = end - 30d`. The handler translates to the meta.Store half-open
`[from, to)` shape internally.

### User quota

| Endpoint | Body | Response |
|---|---|---|
| `GET /admin/v1/iam/users/{user}/quota` | — | `200 {maxBuckets, totalMaxBytes}` or `404`. |
| `PUT /admin/v1/iam/users/{user}/quota` | `{maxBuckets, totalMaxBytes}` | `204` |
| `DELETE /admin/v1/iam/users/{user}/quota` | — | `204` |
| `GET /admin/v1/iam/users/{user}/usage?start=&end=` | — | `200 {rows: [{bucket, day, storageClass, byteSeconds, …}], totals: {bytes, objects}}` |

User-scope usage fans out via `ListBuckets(owner)` + per-bucket
`ListUsageAggregates`, so each row carries its source bucket name.

## Web UI

The embedded operator console (`/console/`) surfaces both:

- **Per-bucket Usage tab** (`web/src/components/BucketUsageTab.tsx`):
  live `bucket_stats` progress bar against `BucketQuota.MaxBytes`
  (gray when no quota set), 30-day chart of `byteSeconds / 86400` per
  day, per-storage-class breakdown table, Edit / Remove quota buttons.
- **Per-user Billing page** at `/iam/users/{user}/billing`
  (`web/src/pages/UserBilling.tsx`): cross-bucket per-day chart,
  per-bucket byte-seconds-ranked table, Edit / Remove user quota
  buttons.

Reach the user page via the **Billing** button on the IAM user detail
header.

## Audit + observability

- Every quota / billing admin write writes a row to `audit_log` (kept
  for `STRATA_AUDIT_RETENTION`, default 30 d). See
  [Monitoring]({{< ref "/best-practices/monitoring" >}}#audit-log).
- `strata_quota_reconcile_drift_bytes{bucket}` exposes the last drift
  observation for alerting.
- `403 QuotaExceeded` is counted in the standard
  `strata_http_requests_total{code="403"}` counter — alert on a sudden
  spike if a tenant's automation is mid-runaway.

## Common pitfalls

- **Operator-side mutation of `objects` rows that bypasses `BumpBucketStats`.**
  Drift accumulates until the next reconcile tick. If you must edit the
  meta backend directly, run a one-off reconcile via the worker (or
  re-derive `bucket_stats` and `BumpBucketStats` manually).
- **Setting `STRATA_USAGE_ROLLUP_AT` to a non-UTC time.** The worker
  treats the value as UTC. For Pacific 17:00 use `00:00` (next-day
  UTC). Mismatching this just shifts the sample point — the resulting
  row is still keyed on UTC midnight.
- **Forgetting `MaxBytesPerObject` when capping per-tenant uploads.**
  `MaxBytes` does not stop a single 10 TiB object that fits the
  remaining budget. Pair the two when you want both an aggregate and
  per-upload ceiling.
- **Relying on `bucket_stats` for second-by-second accuracy in the
  rollup.** The single-sample approximation is bounded by daily delta;
  if billing accuracy matters more than that, the continuous-integration
  follow-up (P3) is the right fix — not pushing
  `STRATA_USAGE_ROLLUP_INTERVAL` to 1 h.

## See also

- [Monitoring]({{< ref "/best-practices/monitoring" >}}) for the alert
  set and audit-log shape.
- [Capacity planning]({{< ref "/best-practices/capacity-planning" >}})
  for sizing before quota.
- [Architecture — Workers]({{< ref "/architecture/workers" >}}) for the
  supervisor + leader-election shape behind every leader-elected
  worker.
