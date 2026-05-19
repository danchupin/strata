---
title: 'ADR-0001: Skip RADOS omap for bucket index'
weight: 1
---

# ADR-0001: Skip RADOS omap for bucket index

## Status

Accepted — April 2026

## Context

Strata is positioned as a drop-in replacement for Ceph RGW. RGW stores
the bucket index in a small set of RADOS objects, each carrying an
`omap` (ordered key→value map) that lists the bucket's objects in lex
order. The omap is convenient — listing is a native ordered scan —
but it has hard scale ceilings:

- The omap of a single index object lives in a single placement group.
  All listing traffic and all index mutations for that shard land on
  one OSD, capped by that OSD's IOPS budget.
- RGW's only mitigation is bucket-index resharding (`radosgw-admin
  bucket reshard`). Resharding rewrites the entire omap, must
  quiesce or rate-limit write traffic during the cut-over, and the
  shard count tops out before the largest production buckets do —
  beyond roughly 100M objects a single bucket exhausts the
  resharding ceiling and starts taking IOPS hits regardless.
- The contract is opaque to the layer above. We cannot trade
  consistency for throughput, partition by a different key, or fan
  out across heterogeneous storage tiers without re-implementing the
  scan path.

We considered (a) keeping omap with aggressive resharding and (b)
moving the index to a separate ordered store. (a) inherits RGW's
ceiling; the project goal is to lift it, not match it.

## Decision

We do not use RADOS omap. The bucket index is held in a dedicated
metadata tier — Cassandra (or ScyllaDB / TiKV as drop-in CQL or
ordered-KV replacements) — modelled as a single `objects` table
sharded by `(bucket_id, shard)` where `shard = hash(key) %
STRATA_BUCKET_SHARDS` (default 64). RADOS is used only for the data
plane, as a chunk store keyed by manifest-derived OIDs.

## Consequences

- **Listing scales horizontally.** `ListObjects` fans out across
  `STRATA_BUCKET_SHARDS` partitions concurrently and heap-merges by
  clustering order (`key ASC, version_id DESC` — see
  [ADR-0002]({{< ref "/adr/0002-islatest-read-time" >}})). Buckets
  with >1B objects are addressable at the cost of a constant
  fan-out wide; no single partition is a hot shard. The fan-out
  shape lives in `cassandra/store.go: ListObjects` and the
  `cursorHeap` / `versionHeap` types. A range-scan-native backend
  (TiKV) short-circuits the fan-out via the optional
  `meta.RangeScanStore` interface.
- **Two-store consistency invariant.** Object existence is the
  manifest row in metadata; the data chunks in RADOS are merely
  referenced by it. The PUT path writes data chunks first, then
  inserts the manifest row via Cassandra LWT (or TiKV pessimistic
  txn for backends with RMW coherence requirements). Failed
  manifest inserts leak chunks that the GC worker reaps via the
  `gc_entries_v2` queue. Manifest CAS on lifecycle transitions
  (`Store.SetObjectStorage`) keeps tier-2 writes from racing
  concurrent client PUTs.
- **Two-tier operational footprint.** Operators now run two storage
  systems (metadata + RADOS) instead of one. This is the explicit
  trade-off — Cassandra / TiKV are both well-understood operational
  shapes, both ship native multi-region replication, and the
  combined deploy is no harder than a production RGW with its own
  separate index pool. The benchmarks comparing the two metadata
  backends live under
  [Architecture → Benchmarks → Meta backend comparison]({{< ref
  "/architecture/benchmarks/meta-backend-comparison" >}}).
- **No bucket reshard cliff.** Adding capacity is a Cassandra /
  TiKV cluster expansion, not a per-bucket maintenance task. The
  online per-bucket shard-resize worker (`internal/reshard`) handles
  the rare case where `STRATA_BUCKET_SHARDS` for an existing bucket
  needs to grow.
