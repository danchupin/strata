---
title: 'Billing'
weight: 80
description: 'Byte-seconds trapezoid math, intra-day sampling cadence, and the usage_aggregates feed external invoice generators consume.'
---

# Billing

Strata does not ship an invoice generator. It ships the **inputs an
external invoice generator can consume**: a daily per-(bucket, storage
class) row in `usage_aggregates` whose `byte_seconds` field integrates
the bucket-usage counter across the UTC day with the trapezoid rule.

This page documents the integration math, the sampling cadence, and the
env knob that tunes accuracy vs. meta-backend cost. For the live quota
counter, the `QuotaExceeded` shape, the reconcile worker, and the admin
API surface, see [Quotas + billing]({{< ref "/best-practices/quotas-billing" >}}).

## Why byte-seconds

Storage billing in S3-shape services is almost always per byte-second:
a 1 GiB object stored for 12 hours costs the same as a 2 GiB object
stored for 6 hours. Daily roll-up gives the invoice generator one row
per (bucket, class, day) — small, indexed, easy to join against tenant
metadata.

Strata's `usage_aggregates` row carries three fields per day:

| Field | Meaning |
|---|---|
| `byte_seconds` | Trapezoid integral of `used_bytes` over the day. The headline billing input. |
| `object_count_avg` | Mean object count across intra-day samples. Useful for per-object-fee tiers. |
| `object_count_max` | Maximum object count across intra-day samples. Useful for SLA reporting. |

The schema and `(bucket_id, storage_class, day)` partition shape are
documented in [Quotas + billing — usage_aggregates schema]({{< ref "/best-practices/quotas-billing#usage_aggregates-schema" >}}).

## Trapezoid integration

`byte_seconds = Σ_{i=0..N-2} (s[i] + s[i+1]) / 2 × Δt + s[N-1] × Δt`,
where `Δt = 86400 / N` and `s[i]` is the `used_bytes` snapshot of the
bucket-usage counter at the i-th intra-day sample.

Each adjacent pair of samples is averaged (trapezoid rule); the last
segment carries the tail value forward as a rectangle, so a flat day
integrates exactly back to `used_bytes × 86400`. The math degrades
gracefully under low sample counts — `N=1` collapses to the v0
single-sample formula `used_bytes × 86400`.

### Worked example

A bucket grows from `100` B to `200` B at noon UTC and stays flat
through end of day. With `STRATA_USAGE_ROLLUP_SAMPLES_PER_DAY=24`
(hourly samples), the trapezoid math bills:

```
byte_seconds = (Σ over first 12 hours of 100 B samples)
             + (Σ over last 12 hours of 200 B samples)
             + one trapezoid transition pair at noon
            ≈ 13_140_000 B·s
```

The ideal integral is `100 × 43_200 + 200 × 43_200 = 12_960_000 B·s`
— the trapezoid result lands within ~1.4 % of the closed form.

The pre-trapezoid `N=1` math would have billed `200 × 86400 =
17_280_000 B·s` — over-counted by ~33 % because it credited the full
day to the post-growth value.

## Sampling cadence

The rollup worker fires intermediate ticks every
`STRATA_USAGE_ROLLUP_INTERVAL / STRATA_USAGE_ROLLUP_SAMPLES_PER_DAY`
and stores each snapshot of the bucket-usage counter in an in-memory
per-process ring keyed by `(bucketID, storageClass)`. The daily
roll-up tick drains the ring per bucket and writes the trapezoid
integral into `usage_aggregates`.

| Env | Default | Range | Effect |
|---|---|---|---|
| `STRATA_USAGE_ROLLUP_AT` | `00:00` | UTC `HH:MM` | Wall-clock the daily flush fires at. |
| `STRATA_USAGE_ROLLUP_INTERVAL` | `24h` | Go duration | Period between daily flushes. Lower only for test rigs. |
| `STRATA_USAGE_ROLLUP_SAMPLES_PER_DAY` | `24` | `[1, 1440]` | Intra-day sample count. Higher = finer trapezoid. Out-of-range clamped + WARN-logged. |

Sizing notes:

- `SAMPLES_PER_DAY=24` (hourly) is the production default — within ~2 %
  of the closed-form integral for any reasonable bucket-growth curve;
  one sample tick costs one `GetBucketStats` round-trip per bucket,
  which is cheap.
- `SAMPLES_PER_DAY=1440` (every minute) brings the trapezoid error
  below 0.1 % at the cost of 60× the per-bucket meta load per tick.
  Useful only when tenants are billed at sub-1 % precision and an
  audit relies on the integral matching wall-clock observations.
- `SAMPLES_PER_DAY=1` reproduces the legacy single-sample math. Useful
  as a panic switch — set this if the sample fan-out starts blocking
  the meta backend.

## Leader election + leader-flip behaviour

The usage-rollup worker is leader-elected on `usage-rollup-leader`. Only
the leader replica fires intra-day samples and writes the daily row;
non-leader replicas keep their per-process ring empty so the meta load
stays single-replica.

A leader flip mid-day starts a fresh ring on the new leader. The first
day after the flip will have fewer samples + degrade gracefully (worst
case 1 sample → v0 math). Subsequent days recover to the configured
`SAMPLES_PER_DAY`.

## Downstream invoice generator contract

The external invoice generator consumes `usage_aggregates` over a
half-open day range `[from, to)`. The admin API surfaces this via
`GET /admin/v1/buckets/{name}/usage?start=&end=` and
`GET /admin/v1/iam/users/{user}/usage?start=&end=` — both translate
inclusive `start` / `end` UTC days to the meta-store half-open shape.

Strata commits to:

1. **Stable schema** — adding a new aggregate column is additive only.
   Existing readers ignore unknown columns.
2. **Idempotent writes** — re-running the rollup worker for a day that
   already has a row overwrites the row in place.
3. **UTC midnight day boundaries** — every backend (Cassandra `date`,
   TiKV BE day-epoch, memory `int64`) normalises to UTC midnight at
   write boundary. Mixed-time-zone reads do not appear.

What's NOT in scope for this cycle:

- **Invoice generation, payment integration, pricing rules.** Out of
  scope by design — billing rules vary per tenant and live in an
  ops-layer service.
- **Continuous integration** (sub-minute byte-seconds). Tracked as a
  P3 follow-up; the trapezoid is the production design point.

## Common pitfalls

- **Setting `SAMPLES_PER_DAY` very high without measuring meta load.**
  Each sample costs one `GetBucketStats` round-trip per bucket. A
  10k-bucket deploy at `SAMPLES_PER_DAY=1440` issues ~14 M
  meta-reads/day from a single replica. Stay at the default unless
  you have headroom.
- **Mistaking the v0 fallback for a bug.** A bucket created mid-day
  has fewer samples than `SAMPLES_PER_DAY` for that first UTC day; the
  trapezoid integral simply uses whatever samples accumulated. The row
  is still committed — just at slightly coarser fidelity.
- **Setting `STRATA_USAGE_ROLLUP_AT` to a non-UTC wall-clock.** The
  worker treats the value as UTC. Pacific 17:00 is `00:00` next-day
  UTC. The row is keyed on UTC midnight either way — mis-setting the
  knob only shifts when the flush fires, not which day it lands on.
- **Reading `usage_aggregates` against a day older than
  `STRATA_AUDIT_EXPORT_AFTER`.** Audit-export retention is independent
  of `usage_aggregates` (which has its own retention). Verify both
  retention windows match your billing-cycle archive policy.

## See also

- [Quotas + billing]({{< ref "/best-practices/quotas-billing" >}}) for
  the live counter, `QuotaExceeded`, the reconcile worker, and the
  full admin API surface.
- [Monitoring]({{< ref "/operate/monitoring" >}}) for the alert set
  on drift + rollup lag.
- [Capacity planning]({{< ref "/operate/capacity-planning" >}}) for
  sizing the meta backend under a high `SAMPLES_PER_DAY`.
- [Architecture — Workers]({{< ref "/architecture/workers" >}}) for
  the supervisor + leader-election shape behind the usage-rollup
  worker.
