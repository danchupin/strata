---
title: 'Meta store'
weight: 15
description: 'meta.Store interface — LWT semantics, clustering order, range scans, sharded objects table; backend parity (memory / Cassandra / TiKV); RangeScanStore short-circuit on TiKV.'
---

# Meta store

`internal/meta/store.go` defines `meta.Store`, the interface every metadata
backend implements. The contract is intentionally narrow: only the
operations the S3 surface needs, and only with the consistency primitives
the backends can all support without bolting a coordinator in front.

Three production-eligible backends satisfy `meta.Store`:

| Backend | When to pick | Notes |
|---|---|---|
| `memory` | Tests, smoke pass, single-process demos | In-process tree-map. Naturally ordered. No durability. |
| `cassandra` | Multi-replica, scale tested against the s3-tests suite | ScyllaDB drops in unchanged (CQL-compatible). Sharded objects table, fan-out + heap-merge listing. |
| `tikv` | Multi-replica, prefer ordered scans | Native KV via `tikv/client-go`. Implements `RangeScanStore` so `ListObjects` is a single scan. |

A new backend MUST satisfy `meta.Store` and pass the contract suite at
`internal/meta/storetest/contract.go`. The suite is shared across all
backends and is the parity oracle.

## Consistency primitives

The interface is built on three primitives every backend must support:

- **LWT (compare-and-set).** `CreateBucket` is `INSERT … IF NOT EXISTS`,
  `DeleteBucket` is `IF EXISTS`. Multipart `Complete` flips
  `status='uploading' → 'completing'` under `IF status='uploading'`
  semantics. Lifecycle transitions use `SetObjectStorage(…,
  expectedClass, newClass) → applied bool` so concurrent client writes
  win. See `cassandra/store.go` and `tikv/store.go` for the LWT shape;
  the [Cassandra gotchas]({{< ref "/architecture/backends/scylla" >}})
  and [TiKV gotchas]({{< ref "/architecture/backends/tikv" >}}) sections
  cover the per-backend traps.
- **Read-after-write coherence on the same row.** Once a row has been
  written under LWT, every subsequent UPDATE on that row must also be
  LWT — bypassing LWT after an LWT-write leaves Paxos / pessimistic-txn
  state stale, and quorum reads can observe the pre-update value.
  `SetBucketVersioning`, `SetBucketACL`, and the access-key flips all
  use the LWT helper for this reason.
- **Ordered range scan over `(bucket, key, version)`.** ListObjects
  needs every key in a bucket in lex order, with `version_id` clustering
  order DESC. The cassandra backend gets this from the per-shard
  primary key `((bucket_id, shard), key, version_id DESC)` and
  heap-merges across shards. TiKV gets it from the global byte-string
  ordering of its key encoding. Memory uses an in-process tree-map.

## Sharded objects table

The cassandra `objects` table is partitioned by `(bucket_id, shard)`
where `shard = fnv32a(key) % N` and `N` is per-bucket
(`STRATA_BUCKET_SHARDS`, default 64, must be a power of two — see
`meta.IsValidShardCount`). The fan-out keeps each partition under the
RGW bucket-index ceiling that bites Ceph at large object counts.

`ListObjects` therefore queries `N` partitions concurrently and
heap-merges by clustering order. The merge logic is in
`internal/meta/cassandra/store.go` (`cursorHeap`, `versionHeap`). See
the [Sharding deep dive]({{< ref "/architecture/sharding" >}}) for the
full picture, including the online reshard worker.

## RangeScanStore — the short-circuit

`meta.RangeScanStore` is an optional capability surface:

```go
type RangeScanStore interface {
    Store
    ScanObjects(ctx, bucketID, opts) (*ListResult, error)
}
```

Backends with a globally-ordered keyspace implement it and the gateway
type-asserts at the dispatch site (`internal/s3api/server.go::listObjects`).
With the assertion, `ListObjects` becomes one continuous scan instead of
N-way fan-out + heap merge.

**Cassandra deliberately does NOT implement `RangeScanStore`.** Its
physical layout requires the fan-out, and hoisting it under a "single
range scan" name would just hide the same code. Memory and TiKV both
implement it because their layouts naturally support it — TiKV via the
US-002 byte-string key encoding (FoundationDB-style stuffing for variable
segments, big-endian fixed-width integer fields, version-DESC suffix).
See [TiKV backend]({{< ref "/architecture/backends/tikv" >}}) for the
byte-level layout.

## Adding a new method

When the S3 surface gains an operation that needs metadata persistence:

1. Add a sentinel error in `internal/meta/store.go` (e.g.
   `ErrMultipartInProgress`).
2. Add the `Set/Get/Delete` triple to `meta.Store`. Use the blob-config
   helpers (`setBucketBlob` / `getBucketBlob` / `deleteBucketBlob`) for
   the "bucket has one XML/JSON document of kind X" shape — every
   bucket-scoped sub-resource (CORS, policy, lifecycle, public-access,
   ownership-controls) reuses these. Don't write fresh CRUD per
   sub-resource.
3. Implement in `memory`, `cassandra`, and `tikv` in lockstep.
4. Add a `case<Name>` in `internal/meta/storetest/contract.go` so all
   three backends are exercised by the shared suite.
5. Cassandra schema additions are idempotent — append to `tableDDL` (new
   table) or `alterStatements` (`ALTER TABLE ADD column`, swallowed by
   `isColumnAlreadyExists`). Never write a destructive migration.

## Manifest blob — schema-additive evolution

`data.Manifest` is encoded into the `objects.manifest` column via
`data.EncodeManifest`. The wire format is selected by
`data.SetManifestFormat("proto"|"json")` (default `proto`); reads always
go through `data.DecodeManifest` which sniffs the first non-whitespace
byte (`{` → JSON, anything else → proto3 wire). New fields tagged
`json:",omitempty"` (and a fresh `protobuf` tag in `manifest.proto`) are
schema-additive — old rows decode with zero-values, no `ALTER` needed.

The `strata server --workers=manifest-rewriter` worker walks every
bucket once a day and converts JSON-encoded blobs to proto in place
(idempotent — re-runs skip already-proto rows). See [Workers]({{< ref "/architecture/workers" >}}).

## Source

- `internal/meta/store.go` — interface, ListOptions / ListResult,
  `RangeScanStore`, `IsValidShardCount`.
- `internal/meta/memory/` — in-process backend.
- `internal/meta/cassandra/` — Cassandra / Scylla backend.
- `internal/meta/tikv/` — TiKV backend.
- `internal/meta/storetest/contract.go` — shared contract suite.
