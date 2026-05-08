---
title: 'Capacity planning'
weight: 60
description: 'Chunk fan-out math, lifecycle cadence vs storage growth, when to scale shards / replicas, dedup roadmap.'
---

# Capacity planning

Capacity on a Strata deploy splits across three independently-scaled
tiers:

- **Gateway tier:** stateless replicas. Capacity = peak RPS / SigV4 +
  routing budget per replica. Scale horizontally; see
  [Sizing]({{< ref "/best-practices/sizing" >}}).
- **Metadata tier:** Cassandra / TiKV. Capacity = total object-row
  count × per-row overhead, plus the LWT / pessimistic-txn rate during
  writes.
- **Data tier:** RADOS / S3-over-S3. Capacity = total object bytes ×
  pool replication factor, plus chunk fan-out overhead.

This page covers the math; pair with
[Backup + restore]({{< ref "/best-practices/backup-restore" >}}) for
the backup overhead and
[GC + lifecycle tuning]({{< ref "/best-practices/gc-lifecycle-tuning" >}})
for the drain rate.

## Object size + chunk fan-out

Strata writes 4 MiB chunks to RADOS. Per-object overhead:

- **Manifest row** in the metadata backend (one row in the `objects`
  table). The proto-encoded manifest carries per-chunk `(Pool, OID,
  Length)` triplets — ~40 bytes per chunk.
- **Chunk objects** in RADOS. One RADOS object per 4 MiB slice.

Per object size class:

| Object size | Chunks | Manifest size (proto) | RADOS objects |
|---|---|---|---|
| ≤ 4 MiB | 1 | ~80 B | 1 |
| 16 MiB | 4 | ~200 B | 4 |
| 100 MiB | 25 | ~1.1 KiB | 25 |
| 1 GiB | 256 | ~10.5 KiB | 256 |
| 5 GiB (largest single PUT) | 1,280 | ~52 KiB | 1,280 |

Multipart uploads write a chunk per upload-part, so a 1 GiB object
uploaded as 100 MiB parts produces 256 chunks (same as a single-PUT
1 GiB) plus per-part bookkeeping rows during the upload window. The
multipart bookkeeping rows expunge on Complete or Abort.

### RADOS pool sizing

`(total object bytes) × (pool replication factor) ÷ 0.7` gives a
conservative sized pool — the 70 % headroom covers the rebalance
window during OSD additions / removals, plus pgmap overhead. EC 4+2
saves ~50 % vs replicated 3 at the cost of doubled write IOPS per
chunk.

### Metadata pool sizing

The `objects` table is sharded `(bucket_id, shard)` with default 64
shards per bucket. Per-row overhead in Cassandra is ~200 B
(SSTable-compressed). For 1 B objects across 1,000 buckets:

- 1 B rows × 200 B = ~200 GB raw. RF=3 → ~600 GB across the cluster,
  ~200 GB per node on a 3-node cluster.
- Per partition (64 shards × 1,000 buckets = 64k partitions) ≈ 15 KB
  rows on average. Far under Cassandra's per-partition ceiling.

TiKV stores rows in regions. Default 96 MiB per region; the same 1 B
rows × 200 B = ~200 GB raw → ~2,000 regions. PD spreads regions across
TiKV nodes; the per-node disk math is the same.

## Lifecycle cadence vs storage growth

Without lifecycle, every PUT adds bytes monotonically. The lifecycle
worker reclaims storage by:

- **Expiration:** delete objects (and noncurrent versions) past their
  configured age.
- **Transition:** move objects between storage classes (e.g. hot →
  cold). The cold class typically points at a different RADOS pool
  with cheaper EC + slower OSDs.
- **Multipart abort:** clean up incomplete multipart uploads past
  their abort timeout.

Per-bucket lifecycle is configured via
`PUT /<bucket>?lifecycle` (S3 LifecycleConfiguration). The worker ticks
every `STRATA_LIFECYCLE_INTERVAL` (default 1 h) and processes one
bucket per goroutine up to `STRATA_LIFECYCLE_CONCURRENCY` (default 64).

### Drain rate vs PUT rate

For a steady-state deploy, lifecycle drain rate must exceed the PUT
rate of expiring objects, or the gateway accumulates orphans
indefinitely. The bench numbers from
[GC + Lifecycle scaling]({{< ref "/architecture/benchmarks/gc-lifecycle" >}}):

- Single-replica lifecycle worker at c=64: ~6,500 objects/s on
  TiKV+memory.
- Three-replica with `STRATA_GC_SHARDS=3` at c=64 each: ~18,000–19,500
  objects/s expected (≈3× single-replica, minus per-bucket
  lease-acquire overhead).

If the expiring-object PUT rate exceeds the drain rate for an extended
window, the queue depth grows. Watch
`strata_lifecycle_tick_total{action="expire",status="success"}` against
the inferred PUT rate; correct via `STRATA_LIFECYCLE_CONCURRENCY` /
replica scale-out.

### GC drain rate

Every lifecycle expire / transition + every PUT-overwriting-existing
enqueues the old chunks into the GC queue. The gc worker drains the
queue and issues RADOS `remove` per chunk:

- Single-replica gc at c=64: ~90,000 chunks/s on TiKV+memory; far less
  on RADOS (RADOS round-trip dominates).
- Three-replica with `STRATA_GC_SHARDS=3`: ~250,000–270,000 chunks/s
  expected on TiKV+memory.

Saturated lifecycle traffic + saturated GC sets the storage-reclaim
ceiling. If `strata_gc_queue_depth` grows without drain for ≥ 10 min,
investigate per the
[GC + lifecycle tuning runbook]({{< ref "/best-practices/gc-lifecycle-tuning" >}}).

## When to scale shards

Shard count is set per bucket at creation time via
`STRATA_BUCKET_SHARDS` (default 64). Scaling drivers:

- **Hot-shard contention** on the `(bucket_id, shard)` partition key.
  Backed by `strata_bucket_shard_bytes{bucket=...}` /
  `strata_bucket_shard_objects{bucket=...}` for the top-N largest
  buckets, plus `strata_cassandra_lwt_conflicts_total{bucket,shard}`
  for LWT conflict heatmaps.
- **Per-partition size limit.** Cassandra partitions over ~100 MB
  start hurting compaction; tune up the shard count for buckets that
  approach this.
- **List throughput.** `ListObjects` heap-merges N partitions
  concurrently; doubling shard count doubles the list cost. Don't
  over-shard small buckets.

The online reshard worker (`internal/reshard`) lets operators bump
shard count on a live bucket without downtime; trigger via
`POST /admin/v1/bucket/<name>/reshard`. See
[Architecture — Sharding]({{< ref "/architecture/sharding" >}}).

## When to scale replicas

The Sizing page covers the per-replica budget. Common triggers for
horizontal scale-out:

- **CPU > 70 %** sustained for 5 min on every replica.
- **p99 latency > SLO** with no obvious metadata-tier or RADOS-tier
  bottleneck.
- **Worker queue depth growing without drain** — adding replicas plus
  bumping `STRATA_GC_SHARDS` to match buys parallel drain capacity.
- **Concurrent connection cap** — the Go net/http server scales
  cheaply but at very high concurrency the OS-level fd budget caps
  out per replica. Add replicas before bumping fd limits.

When scaling out:

1. Add the replica behind the LB.
2. Wait for `/readyz` to flip green.
3. Bump `STRATA_GC_SHARDS` on **every** replica to match the new
   replica count (rolling restart). Going beyond replica count is a
   no-op (extra leases unfilled).

When scaling in:

1. Drain the replica from the LB.
2. Wait for in-flight requests to drain (gateway sets readiness
   `false` on shutdown signal).
3. Stop the replica.
4. Bump `STRATA_GC_SHARDS` down to match (otherwise some shards have
   no leader and stall their drain).

## Dedup roadmap (P2)

`ROADMAP.md` carries a P2 entry for **content-addressed object
deduplication**: chunk OID becomes `dedup/<sha256(content)>`, a new
`chunk_refcount` table tracks references, PUT increments + skips RADOS
write on hit, DELETE / lifecycle-expire decrement, GC only deletes the
underlying RADOS blob when refcount hits 0.

When this lands, the data-tier sizing math changes:

- **Storage savings depend on duplication rate.** Workloads with high
  dedup rates (e.g. backup tiers, container image layers) can see
  3–10× reduction; cold-storage / archive workloads with random bytes
  see ~0 % savings.
- **CPU cost on the PUT path.** ~500 MB/s per core sha256 — a 10 GiB
  PUT spends ~20 s of one core on hashing. Acceptable on the
  multi-core deploys this page targets.
- **Crypto independence.** SSE-S3 / KMS encrypts each object's chunks
  with a unique DEK by default, so the same plaintext encrypts
  differently per object — dedup is incompatible with default SSE.
  The roadmap calls out an opt-in `dedup-friendly` mode where the DEK
  is derived from `hash(plaintext)`; this weakens crypto independence
  and operators should opt in only for known-safe workloads.

Pre-dedup capacity planning should size the data tier for **no**
dedup; treat post-dedup savings as a future optimisation, not a budget.

## Lifecycle cadence rules of thumb

A few cadence patterns worth documenting:

- **Aggressive expiration (data-warehouse-style):** expire after 7 d.
  Lifecycle drain rate must clear ~1/7 of the daily PUT volume per
  day; pair with `STRATA_GC_SHARDS≥3` on a high-PUT deploy.
- **Tiered archive:** transition STANDARD → STANDARD_IA after 30 d,
  STANDARD_IA → GLACIER after 90 d. Each transition writes the new
  manifest + enqueues the old chunks for GC. Storage growth is
  bounded by the slowest tier's expiration policy.
- **Compliance hold:** Object Lock retention pins objects past their
  lifecycle expiration. Combine retention rules with lifecycle so
  expirations only fire on releasable objects; otherwise the gateway
  ignores the lifecycle rule and the operator sees no reclaim.

## See also

- [Sizing]({{< ref "/best-practices/sizing" >}}) for per-replica
  budgets.
- [GC + lifecycle tuning]({{< ref "/best-practices/gc-lifecycle-tuning" >}})
  for tuning the drain rate.
- [Backup + restore]({{< ref "/best-practices/backup-restore" >}}) for
  the storage cost of backup snapshots + replication.
- [Architecture — Sharding]({{< ref "/architecture/sharding" >}}) for
  the per-shard fan-out math.
