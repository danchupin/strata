---
title: 'GC + lifecycle tuning'
weight: 40
description: 'STRATA_GC_CONCURRENCY / STRATA_LIFECYCLE_CONCURRENCY (Phase 1) plus STRATA_GC_SHARDS (Phase 2) tuning, with the bench curve and the dual-write cutover playbook.'
---

# GC + lifecycle tuning

The gc and lifecycle workers run inside every gateway replica. The
operator-facing knobs split into two layers:

- **Phase 1 — per-replica fan-out:** `STRATA_GC_CONCURRENCY` /
  `STRATA_LIFECYCLE_CONCURRENCY` cap the goroutine count inside one
  replica's worker.
- **Phase 2 — multi-replica sharding:** `STRATA_GC_SHARDS` shards the
  leader-election space so N replicas process disjoint slices of the
  queue in parallel.

Both layers compose: a 3-replica deploy with `STRATA_GC_SHARDS=3` and
per-replica `STRATA_GC_CONCURRENCY=64` runs three independent fan-outs
against disjoint shards.

For the architectural rationale + the underlying primitives, see
[Architecture — Workers]({{< ref "/architecture/workers" >}}) and
[Architecture — Sharding]({{< ref "/architecture/sharding" >}}). For
the migration mechanics, see
[Architecture — GC + lifecycle Phase 2]({{< ref "/architecture/migrations/gc-lifecycle-phase-2" >}}).

## Phase 1 — single-replica fan-out

### `STRATA_GC_CONCURRENCY` (default 64)

Bounds the goroutine count inside one gc.Worker drain pass. Each
goroutine performs one `AckGCEntry` pessimistic-txn round-trip plus
one `Data.Delete` call.

The bench curve from
[GC + Lifecycle scaling]({{< ref "/architecture/benchmarks/gc-lifecycle" >}})
(memory data backend, TiKV meta, single replica):

| concurrency | throughput (chunks/s) | speedup vs c=1 |
|------------:|----------------------:|---------------:|
|           1 |                11,108 |          1.00× |
|           4 |                25,220 |          2.27× |
|          16 |                46,397 |          4.18× |
|          64 |                90,098 |          8.11× |
|         256 |               100,275 |          9.03× |

Knee at c=64 (per-doubling yield drops to +11 % above that). Recommended
production default: **`STRATA_GC_CONCURRENCY=64`**. High-churn workloads
can probe up to 128–256; diminishing returns above 64 are TiKV
region-heat-bound (Phase 2 territory).

The lab's RADOS path is **not** measured in the curve above; production
deploys with RADOS data backend will see a knee at lower concurrency
because the per-entry cost includes a RADOS `remove` round-trip.
Re-measure with `STRATA_DATA_BACKEND=rados` on your cluster after
fixing the
[gc-ack-on-ENOENT bug]({{< ref "/architecture/benchmarks/gc-lifecycle" >}}#latent-bug-discovered-while-running)
(`ROADMAP.md` → `Known latent bugs`).

### `STRATA_LIFECYCLE_CONCURRENCY` (default 64)

Bounds the goroutine count inside one lifecycle.Worker bucket scan.
Each goroutine performs `DeleteObject` + `EnqueueChunkDeletion` + the
audit-row pessimistic-txn for one expired/transitioned object.

| concurrency | throughput (objects/s) | speedup vs c=1 |
|------------:|-----------------------:|---------------:|
|           1 |                    485 |          1.00× |
|           4 |                  1,238 |          2.55× |
|          16 |                  3,022 |          6.23× |
|          64 |                  6,496 |         13.40× |
|         256 |                  9,150 |         18.87× |

No knee in the swept range — lifecycle still yields +41 % from c=64 to
c=256. Recommended production default: **`STRATA_LIFECYCLE_CONCURRENCY=64`**
for safety (memory + connection-pool footprint). Operators with large
expiration / transition windows can push to 128–256 if the meta backend
has headroom.

### Other Phase 1 knobs

| Env | Default | Meaning |
|---|---|---|
| `STRATA_GC_INTERVAL` | `30s` | Cadence between drain passes. |
| `STRATA_GC_GRACE` | `60s` | Minimum age of a queue row before it's eligible for delete (covers in-flight writers). |
| `STRATA_GC_BATCH_SIZE` | `100` | Rows per `ListGCEntries` page. Larger pages amortise round-trips at the cost of memory. |
| `STRATA_LIFECYCLE_INTERVAL` | `1h` | Cadence between bucket scans. |
| `STRATA_LIFECYCLE_UNIT` | `Day` | The S3 lifecycle expiration unit. Override to `Minute` for tests. |

## Phase 2 — multi-replica sharding

`STRATA_GC_SHARDS=N` (default `1`, range `[1, 1024]`) shards the
leader-election space across replicas:

- **gc:** N per-shard leases (`gc-leader-0..N-1`). Each replica races
  for one or more leases and drains only the queue slice it owns.
- **lifecycle:** per-bucket leases (`lifecycle-leader-<bucketID>`)
  plus a distribution gate `fnv32a(bucketID) % STRATA_GC_SHARDS ==
  myReplicaID`, where `myReplicaID = min(GCFanOut.HeldShards())`.

`STRATA_GC_SHARDS=1` reproduces Phase 1 byte-for-byte; one replica
wins both leases and runs the full drain. Existing `make smoke` /
`make smoke-tikv` / `make smoke-lab-tikv` continue to pass under the
default.

### Sizing matrix

| Replica count | `STRATA_GC_SHARDS` | Notes |
|---|---|---|
| 1 | 1 (default) | Phase 1 shape; nothing changes. |
| 2 | 2 | Each replica owns half the gc shards + half the lifecycle buckets. |
| 3 | 3 | Canonical multi-replica deploy. Aggregate ≈ 3× Phase 1 ceiling. |
| N (≤ 1024) | N | One-to-one with replicas. |
| N > active-bucket-count | active-bucket-count | Lifecycle distribution-gate hash collisions cap the gain at distinct-bucket count. |

Going **beyond** replica count is a no-op (extra leases unfilled).
Going **below** replica count leaves replicas idle (no shard owned, no
work).

### Per-shard concurrency budget

The Phase 1 knobs `STRATA_GC_CONCURRENCY` / `STRATA_LIFECYCLE_CONCURRENCY`
still cap the fan-out **inside** one replica's drain pipeline. Aggregate
chunk throughput on a Phase 2 deploy is roughly `N × per-replica-cap`
until one of these saturates first:

1. **TiKV region heat** on the gc queue partition (~16–32 runtime
   shards on a 3-node TiKV cluster).
2. **RADOS round-trip** on chunk delete (~8–16 shards before OSD pool
   heat shows up).
3. **Bucket count vs replica count** for lifecycle: gain plateaus at
   `min(N, distinct-bucket-count)`.

See
[GC + Lifecycle scaling]({{< ref "/architecture/benchmarks/gc-lifecycle" >}})
(Phase 2 — multi-leader section) for the cap-shape analysis.

## `STRATA_GC_DUAL_WRITE` cutover playbook

`STRATA_GC_DUAL_WRITE` (default `on` during the Phase 2 cycle) gates
the writer half of the schema cutover:

- **`on`:** `EnqueueChunkDeletion` writes both the legacy
  (`gc_queue` / TiKV `s/qg/`) and the v2 (`gc_entries_v2` / TiKV
  `s/qG/`) row. Readers prefer v2 with a legacy-prefix top-up when the
  v2 result is short. `AckGCEntry` deletes both sides.
- **`off`:** writers write only v2; readers stop the legacy fallback;
  ack-deletes target v2 only.

### Operator runbook

1. **Roll the Phase 2 binary across all replicas.** Default
   `STRATA_GC_DUAL_WRITE=on` keeps writers fanning out to both shapes;
   the queue accepts both workloads.
2. **Set `STRATA_GC_SHARDS` to match replica count.** For a 3-replica
   deploy set `STRATA_GC_SHARDS=3` on every replica. Each replica grabs
   one or more of `gc-leader-0..2`. The default `1` keeps Phase 1
   semantics.
3. **Wait for the legacy queue to drain.** Operator-confirmed via:
   - Cassandra: `SELECT COUNT(*) FROM gc_queue WHERE region = '<r>'`
     across all regions.
   - TiKV: scan the legacy `s/qg/` prefix; an admin diagnostic endpoint
     or one-shot `strata-admin` probe walks the prefix and returns the
     count.
   Conservative target: legacy queue depth zero for ≥ one full
   `STRATA_GC_INTERVAL`.
4. **Flip `STRATA_GC_DUAL_WRITE=off` and roll the gateway tier.**
   Writers stop populating the legacy row; readers stop the legacy
   fallback. Storage layer is now v2-only.
5. **(Optional) Drop the legacy table / prefix.** The legacy
   `gc_queue` table and `s/qg/` prefix are retained indefinitely for
   forensic reasons — they are empty, take negligible space, and
   surviving cutover with the data in place is the safest rollback
   shape. Operators who want the disk back can issue
   `DROP TABLE gc_queue` (Cassandra) or a range-delete on the `s/qg/`
   prefix (TiKV) once a Phase 1 rollback is no longer in play.

### Rollback shape

- **During the dual-write window** (`STRATA_GC_DUAL_WRITE=on`):
  reverting to a Phase 1 binary is a clean binary roll-back. The older
  binary keeps reading + writing the legacy queue; v2 partitions /
  prefixes are ignored.
- **After cutover** (`STRATA_GC_DUAL_WRITE=off`): rollback is one-way.
  Writers stop populating the legacy queue, so the legacy row stream
  goes stale. Reverting to Phase 1 from this state is supported but
  loses any GC entries enqueued post-flip.

Operators who need a safer rollback should keep
`STRATA_GC_DUAL_WRITE=on` for an extended period (one full release
cycle) so the legacy queue stays warm.

For the underlying schema + key-shape changes, see
[Architecture — GC + lifecycle Phase 2]({{< ref "/architecture/migrations/gc-lifecycle-phase-2" >}}).

## Common pitfalls

- **Setting `STRATA_GC_SHARDS > replica count`.** Extra leases sit
  unfilled forever. Harmless but confusing in logs.
- **Setting `STRATA_GC_SHARDS=1` on a multi-replica deploy.** All
  replicas race for the single lease; only one drains, the rest idle.
  Phase 1 behaviour, scaled poorly.
- **Forgetting to flip `STRATA_GC_DUAL_WRITE=off`.** The dual-write
  overhead is small but non-zero; once cutover is verified, flip it
  off so writers stop the redundant write.
- **Pushing concurrency above the bench knee with no measurement.**
  Above c=64 the gain is meta-backend-bound; profile your meta first.
- **Using `STRATA_LIFECYCLE_UNIT=Minute` in production.** Test-only
  knob; expirations fire too often otherwise.

## See also

- [Monitoring]({{< ref "/best-practices/monitoring" >}}) for the
  worker metrics shortlist.
- [Capacity planning]({{< ref "/best-practices/capacity-planning" >}})
  for lifecycle cadence vs storage growth.
- [Architecture — Workers]({{< ref "/architecture/workers" >}}) for
  the supervisor + leader-election shape.
