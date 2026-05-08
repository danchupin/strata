---
title: 'Sharding'
weight: 35
description: 'objects-table partitioning (bucket_id, shard), cursorHeap / versionHeap fan-out merge, gc fan-out (1024 logical shards × runtime shardCount), online reshard worker.'
---

# Sharding

Sharding is the single biggest divergence from Ceph RGW: every bucket's
metadata is split across `N` partitions instead of living in a single
bucket-index object. The split avoids RGW's bucket-index ceiling at
large object counts and lets ListObjects scale linearly with `N`.

## Objects table partition key

The cassandra `objects` table is partitioned by `(bucket_id, shard)`:

```
PRIMARY KEY ((bucket_id, shard), key, version_id)
WITH CLUSTERING ORDER BY (key ASC, version_id DESC)
```

Where `shard = fnv32a(key) % N` and `N` is per-bucket
(`STRATA_BUCKET_SHARDS` at bucket creation, default 64). `N` must be a
power of two — `meta.IsValidShardCount(n)` enforces it. Power-of-two
constraint matters for the [reshard worker]({{< ref "#online-reshard" >}}) below: when `N` doubles, every old
shard either stays under the new modulo or splits cleanly into two new
ones, never three.

Per-shard partition shape:

| Field | Role |
|---|---|
| `(bucket_id, shard)` | partition key — one Cassandra partition per shard |
| `key` | clustering column ASC — listing order |
| `version_id` | clustering column DESC — newest version first |

Per-shard size is bounded by your bucket's per-shard fanout target
(typically <= 100k keys per shard). The
[meta-backend benchmark]({{< ref "/architecture/benchmarks/meta-backend-comparison" >}})
covers the per-shard read/write profile.

## ListObjects fan-out + heap merge

`ListObjects` queries `N` partitions concurrently and merges results by
clustering order. The merge logic is in `internal/meta/cassandra/store.go`:

- `cursorHeap` — min-heap by `key`. Each cursor advances one row at a
  time within its shard partition. The top of the heap is the next
  globally-ordered key.
- `versionHeap` — heap by `(key, version_id DESC)` for
  `ListObjectVersions`. Latest version of a key emerges first; null-
  versioned rows sort last by encoded sentinel.

Pagination cookies carry per-shard cursor positions so the next page
resumes exactly where the previous left off. This is the
N-way-fan-out path the gateway uses when the meta backend does NOT
implement `RangeScanStore`.

## RangeScanStore short-circuit

Backends with a globally-ordered keyspace skip the fan-out. `meta.RangeScanStore`
is the optional capability — see [Meta store]({{< ref "/architecture/meta-store" >}}).
TiKV implements it because its byte-string key encoding (FoundationDB-style
stuffing for variable segments, big-endian fixed-width integer fields,
inverted-timestamp version suffix) gives a globally lex-ordered keyspace.
ListObjects under TiKV is one continuous scan.

The dispatch decision is at `internal/s3api/server.go::listObjects`:

```go
if rs, ok := s.Meta.(meta.RangeScanStore); ok {
    return rs.ScanObjects(ctx, bucketID, opts)
}
return s.Meta.ListObjects(ctx, bucketID, opts)
```

## GC fan-out

GC entries (chunks scheduled for deletion) are partitioned across **1024
logical shards** in the meta store. The gc worker's runtime shard count
(`STRATA_GC_SHARDS`, default 1, range `[1, 1024]`) modulates how many of
those logical shards a single replica owns:

```
entry belongs to runtime shard i  iff  entry.LogicalShardID % STRATA_GC_SHARDS == i
```

Per-shard `gc-leader-<shardID>` leases let one replica drain multiple
shards in parallel and lose only one shard's lease on a panic. Multi-
replica deployments scale linearly: with 3 replicas and
`STRATA_GC_SHARDS=3`, each replica owns ~1/3 of the GC queue.

The lifecycle worker reuses the same shard distribution: per-bucket
`lifecycle-leader-<bucketID>` leases are gated by
`fnv32a(bucketID) % STRATA_GC_SHARDS == min(GCFanOut.HeldShards())` so
lifecycle work distributes in lockstep with the gc fan-out.

## Online reshard

`internal/reshard` is a per-bucket online shard-resize worker (US-045).
It drains the source shards, rewrites every key under the new modulo,
and flips the bucket's `shardCount` once the rewrite catches up. The
worker is driven synchronously via `/admin/bucket/reshard` or as a
daemon under `STRATA_WORKERS=`.

The power-of-two constraint matters here: doubling from `N=64` to
`N=128` means every old shard either stays in place (keys whose
`fnv32a(key) % 128 < 64`) or moves to its new sibling shard (`+ 64`).
No three-way splits. The reshard worker exploits this — it reads each
old shard once and either keeps the row in place or writes it to the
sibling, never to two destinations.

## Per-bucket / per-shard observability

`bucketstats.Sampler` (see [Storage status]({{< ref "/architecture/storage" >}}))
emits per-(bucket, shard) gauges
(`strata_bucket_shard_bytes`, `strata_bucket_shard_objects`) so
operators can spot hotshards before they take down a partition.
Cardinality is capped at `STRATA_BUCKETSTATS_TOPN` (default 100) — the
cluster-wide totals are unaffected by the cap.

## Source

- `internal/meta/store.go` — `IsValidShardCount`, `RangeScanStore`.
- `internal/meta/cassandra/store.go` — `shardOf`, `cursorHeap`,
  `versionHeap`, `ListObjects` fan-out + merge.
- `internal/meta/tikv/keys.md` — TiKV byte-level key encoding.
- `internal/gc/fanout.go` — `FanOut`, runtime shard ownership, panic
  metrics.
- `internal/lifecycle/distribute.go` — per-bucket distribution gate.
- `internal/reshard/` — online reshard worker.
