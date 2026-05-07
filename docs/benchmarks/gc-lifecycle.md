# GC + Lifecycle worker concurrency benchmark (Phase 1)

Quantifies the throughput curve for `internal/gc.Worker` and
`internal/lifecycle.Worker` across the per-worker bounded-errgroup
fan-out introduced in cycle `ralph/gc-lifecycle-scale` (US-001 / US-002).
The harness lands as `strata-admin bench-gc` + `strata-admin bench-lifecycle`
(US-003) and is wired into `make bench-gc` / `make bench-lifecycle` to drive
the canonical lab-tikv stack.

## Harness

```bash
# Bring up the lab-tikv stack (PD + TiKV + ceph) but stop the bundled
# strata-tikv-{a,b} replicas first so their gc/lifecycle workers don't
# compete for the bench region.
make up-lab-tikv && make wait-strata-lab
docker stop strata-tikv-a strata-tikv-b strata-lb-nginx

# Sweep concurrency=1,4,16,64,256 (default).
make bench-gc          # -> bench-gc-results.jsonl
make bench-lifecycle   # -> bench-lifecycle-results.jsonl
```

Each invocation pre-seeds N synthetic rows under a unique
`bench-<uuid>` region (gc) or `lcbench-<uuid>` bucket (lifecycle), runs
`Worker.RunOnce`, prints one JSON line on stdout, and cleans up the
seeded rows on exit. Override `BENCH_GC_ENTRIES`,
`BENCH_LC_OBJECTS`, `BENCH_CONCURRENCY_LEVELS` to widen the sweep.

The Makefile targets default to `STRATA_DATA_BACKEND=rados` against the
lab's ceph pool. The numbers below were captured with
`STRATA_DATA_BACKEND=memory` to isolate the **meta-side** worker
throughput from RADOS round-trip — see Methodology + Latent bug below
for the rationale.

## Methodology

- **Stack:** lab-tikv profile from `deploy/docker/docker-compose.yml`
  (PD v8.5, TiKV v8.5, single replica each). Same docker network as
  the bench container.
- **Hardware:** macOS host, lima Docker context, single-node PD+TiKV.
  Numbers are **directional** — the absolute throughput will rise on
  multi-node TiKV and bare-metal hardware. The shape of the curve
  (relative speedup per doubling) is the load-bearing signal.
- **Workload size:** N=10000 entries / objects per level. Wall-clock
  noise dominates below ~100 ms; trust the per-level deltas for
  c≤64, treat c=256 as a saturation indicator rather than a precise
  number.
- **Data backend:** `memory`. Real production lab-tikv would use
  RADOS; we deliberately bypass RADOS here because the gc worker
  **infinite-loops on `Data.Delete` failure** (latent bug, see below).
  Memory backend deletes succeed instantly so the bench measures the
  worker's meta-side fan-out cap. Once the gc-ack-on-ENOENT bug is
  fixed, re-running with `STRATA_DATA_BACKEND=rados` is mechanical.
- **Meta backend:** TiKV. AckGCEntry / DeleteObject /
  EnqueueChunkDeletion all use pessimistic txns; that's the path
  Phase 1 parallelises.

## Results — bench-gc

| concurrency | elapsed (ms) | throughput (chunks/s) | speedup vs c=1 |
|------------:|-------------:|----------------------:|---------------:|
|           1 |          900 |                11,108 |          1.00× |
|           4 |          396 |                25,220 |          2.27× |
|          16 |          215 |                46,397 |          4.18× |
|          64 |          110 |                90,098 |          8.11× |
|         256 |           99 |               100,275 |          9.03× |

**Per-doubling yield:** 1→4 +127 %, 4→16 +84 %, 16→64 +94 %, 64→256
**+11 %**.

## Results — bench-lifecycle

| concurrency | elapsed (ms) | throughput (objects/s) | speedup vs c=1 |
|------------:|-------------:|-----------------------:|---------------:|
|           1 |       20,638 |                    485 |          1.00× |
|           4 |        8,080 |                  1,238 |          2.55× |
|          16 |        3,309 |                  3,022 |          6.23× |
|          64 |        1,539 |                  6,496 |         13.40× |
|         256 |        1,092 |                  9,150 |         18.87× |

**Per-doubling yield:** 1→4 +155 %, 4→16 +144 %, 16→64 +115 %, 64→256
+41 %.

## Bottleneck per tier

| Concurrency band | gc bottleneck | lifecycle bottleneck |
|---|---|---|
| 1 (sequential) | single-threaded `AckGCEntry` pessimistic txn round-trip to TiKV | per-object `DeleteObject` + `EnqueueChunkDeletion` + audit row pessimistic txns serialised on one goroutine |
| 4–16 | TiKV pipeline depth filling; near-linear scaling | same; lifecycle has more meta ops per entry so the absolute floor is ~25× lower |
| 64 | starting to contend on the GC-region key prefix (`gc/<region>/<ts>/<oid>`) — same TiKV region for all writes | starting to contend on the bucket-objects key prefix (`obj/<bucketID>/<shard>/<key>`) at the single-shard default |
| 256 | saturated; +11 % yield ≈ measurement noise. RADOS round-trip would shift the knee right by hiding meta latency. | TiKV pessimistic-txn region heat dominates; +41 % yield says headroom remains in single-leader land. |

The lab-tikv RADOS path is **not** measured here; in production the gc
worker's per-entry cost is dominated by RADOS round-trip to delete the
chunk. Once `internal/gc.Worker` is fixed to ack on ENOENT (latent bug
below), the same harness can re-run with `STRATA_DATA_BACKEND=rados` to
quantify the RADOS contribution.

## Knee / recommended defaults

The knee is the concurrency above which per-additional-goroutine yield
falls below 10 %.

- **gc:** 64→256 yields +11 %, **just above the 10 % threshold**. The
  gc knee on this lab is at **c=64**. Recommended production default:
  `STRATA_GC_CONCURRENCY=64`. Operators serving very-high-churn
  workloads can probe up to 128–256 — diminishing returns from there
  are TiKV-region-heat bound (Phase 2 territory).
- **lifecycle:** 64→256 still yields +41 %, well above the 10 %
  threshold; no knee inside the swept range. Recommended production
  default: `STRATA_LIFECYCLE_CONCURRENCY=64` for safety (memory + TiKV
  connection pool footprint stays bounded). Operators with large
  expiration / transition windows can push to 128–256 if the meta
  backend has headroom.

Both defaults assume the metadata backend can absorb the extra
pessimistic-txn fan-out. On Cassandra the same envs translate into
parallel LWT round-trips; the LWT throughput cap on a single partition
will dominate the curve. Re-run the bench against a Cassandra-backed
lab and update this file when that becomes a target.

## Tradeoffs at very-high concurrency

- **Memory:** each errgroup goroutine carries the `meta.GCEntry` /
  `meta.Object` plus a TiKV pessimistic-txn lease. At c=256 with
  in-flight 256 acks, RSS grows by a few hundred KiB — negligible.
- **Connection pressure:** `tikv/client-go` uses a fixed-size gRPC
  pool; at c=256 the pool can saturate and incoming work queues on the
  client side, which manifests as throughput plateau. Tune
  `STRATA_TIKV_CLIENT_*` envs (none plumbed yet — Phase 2 hook) only
  if profiling shows pool starvation.
- **TiKV region heat:** the GC queue is keyed by region; concentrating
  256 concurrent writes on one region will trigger TiKV's split + load
  balance, briefly stalling the worker. In production the bench
  `bench-<uuid>` region prefix avoids real-traffic collision; production
  workers run against the canonical `region=default` so the same
  contention shape applies.
- **RADOS pool heat (production):** the missing tier in this bench.
  When `STRATA_DATA_BACKEND=rados`, every gc.Worker goroutine issues a
  RADOS `remove` round-trip; at c=256 against a single Ceph monitor
  this becomes the new dominant cost.

## Latent bug discovered while running

`internal/gc.Worker.drainCount` does **not** ack a GC entry when
`Data.Delete` returns an error (see `internal/gc/worker.go:123-126`).
Combined with the outer `for {}` loop that re-issues
`ListGCEntries` until it returns `< Batch`, any persistent
`Data.Delete` failure (RADOS pool not found, ENOENT for an OID that
was already swept by a concurrent leader, transient timeout) **spins
the worker forever** on the same batch. The bench surfaced this
immediately when run against the real `strata.rgw.buckets.data` pool
with synthetic OIDs — every `rados-2 (No such file or directory)`
warning kept the entry in the queue, and `RunOnce` never returned.

The fix is local: ack on ENOENT (the chunk *is* gone — that's the
desired terminal state), and probably ack on any non-retryable error
class to avoid the spin. Tracked under `Known latent bugs` in
`ROADMAP.md` and out of scope for this cycle's Phase 1 closure.

## Reproducing

The Makefile defaults to RADOS data backend, which trips the bug
above; for a clean reproduction of these numbers use the memory data
backend instead:

```bash
make up-lab-tikv && make wait-strata-lab
docker stop strata-tikv-a strata-tikv-b strata-lb-nginx

for c in 1 4 16 64 256; do
  docker run --rm --network docker_default \
    -v docker_strata-ceph-etc:/etc/ceph:ro \
    -e STRATA_META_BACKEND=tikv \
    -e STRATA_DATA_BACKEND=memory \
    -e STRATA_TIKV_PD_ENDPOINTS=pd:2379 \
    -e STRATA_LOG_LEVEL=ERROR \
    --entrypoint /usr/local/bin/strata-admin strata:ceph \
    bench-gc --entries=10000 --concurrency=$c
done
```

Same shape for `bench-lifecycle` with `--objects=10000`.
