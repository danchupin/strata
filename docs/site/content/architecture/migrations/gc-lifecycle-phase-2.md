---
title: 'GC + lifecycle Phase 2'
weight: 20
description: 'Sharded leader-election cutover — multi-replica gc / lifecycle workers.'
---

# Migrating to gc / lifecycle Phase 2 (sharded leader-election)

Phase 1 (cycle `ralph/gc-lifecycle-scale`, commit `6561845`) lifted the
per-leader concurrency cap via bounded `errgroup` fan-out
(`STRATA_GC_CONCURRENCY` / `STRATA_LIFECYCLE_CONCURRENCY`). The leader was
still single-replica.

Phase 2 (cycle `ralph/gc-lifecycle-scale-phase-2`) shards the leader-election
space so multiple replicas process disjoint slices of the queue in parallel:

- gc gets `gc-leader-0..N-1` lease keys driven by `STRATA_GC_SHARDS` (default
  `1`, range `[1, 1024]`). Each replica races for one or more of the
  per-shard leases and drains only the entries it owns via
  `Meta.ListGCEntriesShard`. The legacy global `gc-leader` lease is retired.
- lifecycle gets per-bucket leases (`lifecycle-leader-<bucketID>`) plus a
  distribution gate (`fnv32a(bucketID) % STRATA_GC_SHARDS == myReplicaID`,
  where `myReplicaID = min(GCFanOut.HeldShards())`). The legacy global
  `lifecycle-leader` lease is retired.
- Backwards-compat: `STRATA_GC_SHARDS=1` (default) reproduces Phase 1
  behaviour byte-for-byte. Existing `make smoke` / `make smoke-tikv` /
  `make smoke-lab-tikv` continue to pass without configuration changes.

This guide is the operator-facing checklist for the schema cutover and the
multi-replica rollout.

## Storage shape changes

### Cassandra

A new table `gc_entries_v2` is added to `internal/meta/cassandra/schema.go`
under `tableDDL`:

```cql
CREATE TABLE IF NOT EXISTS gc_entries_v2 (
    region        text,
    shard_id      int,
    enqueued_at   timestamp,
    oid           text,
    cluster       text,
    PRIMARY KEY ((region, shard_id), enqueued_at, oid)
);
```

The migration is **idempotent + additive** — re-running schema bootstrap
against an existing keyspace creates the new table without touching
`gc_queue`. No destructive migration, no operator action required at upgrade
time other than rolling the binary. The legacy `gc_queue` table is retained
across the dual-write window.

### TiKV

A new key prefix `s/qG/<escaped(region)>\x00\x00<shardID2BE><tsNano8-BE><escaped(oid)>`
is introduced alongside the legacy `s/qg/<escaped(region)>\x00\x00<tsNano8-BE><escaped(oid)>`
prefix. The fixed 2-byte BE shard segment between the region terminator and
the timestamp preserves lex ordering across the 1024 logical shards so a
per-shard prefix scan returns one shard's queue in order. No destructive
migration — TiKV scans both prefixes during the dual-write window.

## Dual-write cutover

`STRATA_GC_DUAL_WRITE` (default `on`) gates the writer half of the cutover:

- **`on` (default during Phase 2 cycle):** `EnqueueChunkDeletion` writes both
  the legacy and the v2 row in one atomic batch (Cassandra
  `LoggedBatch` / TiKV optimistic txn). Readers prefer v2 with a
  legacy-prefix top-up when the v2 result is short. `AckGCEntry` deletes
  both sides.
- **`off` (post-cutover):** writers write only v2; readers stop the legacy
  fallback; ack-deletes target v2 only.

### Operator runbook for cutover

1. **Roll Phase 2 binary across all replicas.** Default
   `STRATA_GC_DUAL_WRITE=on` keeps writers fanning out to both shapes; the
   queue accepts both workloads.
2. **Set `STRATA_GC_SHARDS` to match replica count.** For a 3-replica deploy
   set `STRATA_GC_SHARDS=3` on every replica. Each replica grabs one (or
   more, depending on contention) of `gc-leader-0..2`. Sub-1 (idle) replicas
   skip lifecycle work that cycle (the distribution gate sees no
   defensible stake).
3. **Wait for the legacy queue to drain.** Operator-confirmed via:
   - Cassandra: `SELECT COUNT(*) FROM gc_queue WHERE region = '<r>'`
     (zero across all regions under load).
   - TiKV: scan the legacy `s/qg/` prefix; admin diagnostic endpoint or a
     one-shot `strata admin` probe walks the prefix and returns the count.
   The legacy queue depth is bounded above by Phase 1's drain rate (~90k
   chunks/s on TiKV), so a saturated drain converges within minutes; a
   conservative target is "legacy queue depth is zero for ≥ one full
   `STRATA_GC_INTERVAL`".
4. **Flip `STRATA_GC_DUAL_WRITE=off` and roll the gateway tier.** Writers
   stop writing the legacy row; readers stop the legacy fallback. Storage
   layer is now v2-only. Phase 2 cutover is complete.
5. **(Optional) Drop the legacy table / prefix.** The legacy `gc_queue`
   table and `s/qg/` prefix are retained indefinitely for forensic reasons —
   they are empty, take negligible space, and surviving the cutover with the
   data in place is the safest rollback shape. Operators who need the disk
   back can issue a manual `DROP TABLE gc_queue` (Cassandra) or a
   range-delete on the `s/qg/` prefix (TiKV) once they are comfortable that
   no rollback to Phase 1 is in play.

## Rollback

Phase 2 is **not** a one-way migration during the dual-write window. As long
as `STRATA_GC_DUAL_WRITE=on`, both halves of the queue are kept in lockstep,
and reverting to a Phase 1 binary is a binary-roll-back: the older binary
keeps reading and writing the legacy `gc_queue` / `s/qg/` prefix as it always
has, and the v2 partitions / prefixes are simply ignored.

After `STRATA_GC_DUAL_WRITE=off` is flipped, **rollback is one-way**: writers
stop populating the legacy queue, so the legacy row stream goes stale.
Reverting to Phase 1 from this state is supported but loses any GC entries
enqueued post-flip. Operators who need a safer rollback should keep
`STRATA_GC_DUAL_WRITE=on` for an extended period (one full release cycle) so
the legacy queue stays warm.

If a rollback is needed during Phase 2:

1. Set `STRATA_GC_SHARDS=1` on every replica (or omit — `1` is the default).
   Phase 2 binary at `STRATA_GC_SHARDS=1` reproduces Phase 1 behaviour
   byte-for-byte; only one replica's lease wins each cycle.
2. Roll back the binary to the pre-Phase-2 release. Both v2 partition and
   the new TiKV prefix are ignored by the older code path; new writes
   continue against the legacy queue (Phase 2 binary writes both sides via
   dual-write; older Phase 1 binary writes only the legacy side).
3. Drain v2 manually if disk pressure is a concern: a one-shot
   `strata admin` probe can iterate the v2 partition / prefix and re-enqueue
   each row into the legacy queue. Not provided as a packaged tool — the
   shape mirrors a 30-line scan loop.

## Multi-leader replica sizing

`STRATA_GC_SHARDS` should match replica count up to the bucket-shard
cardinality limit (1024). Concretely:

- **3-replica deploy:** `STRATA_GC_SHARDS=3`,
  `STRATA_GC_CONCURRENCY=64` per replica. Aggregate ceiling ≈ 3× Phase 1
  per-replica cap.
- **N-replica deploy:** `STRATA_GC_SHARDS=N`. Going beyond replica count is
  a no-op (extra leases unfilled). Going below leaves replicas without
  work (idle).
- **Lifecycle:** ensure `STRATA_GC_SHARDS ≤ active-bucket-count` or hash
  collisions cap the gain. The per-bucket distribution gate maps
  `fnv32a(bucketID) % STRATA_GC_SHARDS`; with fewer buckets than shards,
  some replicas idle.

See the canonical bench numbers + cap-shape analysis in
[GC + Lifecycle scaling]({{< ref "/architecture/benchmarks/gc-lifecycle" >}}) (Phase 2 — multi-leader section).

## Observability

- `strata_worker_panic_total{worker="gc",shard="<i>"}` — gc fan-out exposes
  the per-shard panic counter alongside the legacy aggregate. Non-fan-out
  workers continue to use `shard="-"`.
- The supervisor's `LeaderEvents()` channel emits one
  `leader_for=gc acquired=true` event when the fan-out picks up its first
  shard, and one `acquired=false` event when it releases its last shard —
  ownership of multiple shards inside one replica is folded into a single
  acquire/release pair so the heartbeat chip flips at most twice per cycle.
- `gc.FanOut.HeldShards()` returns the currently-held shard IDs; lifecycle
  reads it via `lifecycleReplicaInfo(STRATA_GC_SHARDS)` and exposes
  `myReplicaID = min(HeldShards())` to the per-bucket distribution gate.

## What did not change

- `STRATA_GC_INTERVAL`, `STRATA_GC_GRACE`, `STRATA_GC_BATCH_SIZE` are
  unchanged. The drain pipeline inside one shard still uses Phase 1's
  bounded errgroup (`STRATA_GC_CONCURRENCY`).
- `STRATA_LIFECYCLE_INTERVAL`, `STRATA_LIFECYCLE_UNIT`,
  `STRATA_LIFECYCLE_CONCURRENCY` are unchanged. Per-bucket lease + the
  distribution gate are layered on top — within a bucket scan, Phase 1's
  bounded errgroup still drives the parallelism.
- `make smoke` / `make smoke-tikv` / `make smoke-lab-tikv` are unchanged;
  the default `STRATA_GC_SHARDS=1` reproduces Phase 1 behaviour
  byte-for-byte.
