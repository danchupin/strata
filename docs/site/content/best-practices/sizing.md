---
title: 'Sizing'
weight: 20
description: 'CPU / RAM / disk per replica based on object PUTs/s, plus Cassandra / TiKV cluster sizing pointers.'
---

# Sizing

Strata replicas are stateless gateways — they own no on-disk state, so
sizing is a function of three signals: peak request rate (RPS), the
working-set bytes the gateway holds in flight (RAM), and the cores the
SigV4 + crypto path consumes (CPU). The metadata + data tiers
(Cassandra / TiKV / RADOS) follow their own upstream sizing guides; this
page covers the gateway tier and points operators at the upstream
references.

The numbers below are anchored to the
[GC + Lifecycle scaling]({{< ref "/architecture/benchmarks/gc-lifecycle" >}})
benchmark — the only end-to-end profile we have committed to the repo
today. RPS / latency rules of thumb come from staging measurements on
the canonical `lab-tikv` and `lab-tikv-3` profiles in
`deploy/docker/docker-compose.yml`.

## Gateway replica — per-replica budget

Strata gateways are stateless, so horizontal scale is the answer once
one replica saturates. The per-replica budget below assumes the
canonical mix (path-style + vhost, ~30 % multipart, average object size
~1 MiB, SigV4 on every request, RADOS data backend, `STRATA_GC_SHARDS=1`
default).

| Workload tier | RPS (sustained) | CPU (cores) | RAM (resident) | Notes |
|---|---|---|---|---|
| Small (lab / dev) | ≤ 200 | 1–2 | 512 MiB | `make run-memory` profile. SigV4 dominates. |
| Medium (single-tenant prod) | 200 – 2,000 | 4 | 1.5 GiB | Single-replica `deploy/docker-compose`. Cassandra LWT is the next ceiling. |
| Large (multi-tenant prod) | 2,000 – 8,000 | 8 | 4 GiB | 3-replica deploy with `STRATA_GC_SHARDS=3`. Add LB. |
| Very large | > 8,000 | scale out | 4 GiB / replica | Each additional replica adds ~3,000 RPS headroom; LB and TiKV scale with it. |

Disk on the gateway pod is **ephemeral only** — `/etc/ceph` (Secret),
`/etc/strata/jwt-shared` (emptyDir), `/tmp` for multipart staging.
Allocate ~10 GiB per replica for `/tmp` if you accept very large
multipart parts (chunk size × pending parts × concurrent uploads); the
default is fine.

### CPU cost shape

The hot-path costs per request break down roughly as:

- **SigV4 verification (HMAC-SHA256 + canonicalisation):** ~30 µs per
  request on a single core. Streaming chunk-decoder verification adds
  ~20 µs per 16 KiB chunk (chain HMAC + comparison). At 1,000 RPS the
  SigV4 path takes ~3 % of one core; this scales linearly.
- **Manifest encode / decode:** proto encoding ~10 µs per object,
  unconditional on every PUT / GET. Negligible at any RPS.
- **RADOS round-trip orchestration:** the gateway does no chunk
  hashing; RADOS handles placement. The CPU cost on the gateway is
  goroutine bookkeeping — ~5 µs per 4 MiB chunk.
- **TLS termination:** if the gateway terminates TLS itself (vs an
  Ingress / LB doing it), add another ~50 µs per handshake-resumed
  request, ~500 µs for a fresh handshake. Prefer LB-tier termination
  for any deploy above the medium tier.

### RAM cost shape

The bulk of resident memory is per-request buffers, not caches:

- **Multipart part buffer:** Strata streams multipart parts directly to
  RADOS without buffering the full part in RAM, but the chunk
  serialiser holds one 4 MiB chunk per concurrent in-flight part.
- **HTTP read / write buffers:** ~64 KiB per concurrent connection,
  bounded by the Go net/http server.
- **Gocql / TiKV connection pool:** ~32 connections × ~256 KiB = ~8 MiB
  steady-state per replica.
- **OTel ring buffer (`STRATA_OTEL_RINGBUF=on`):** default 4 MiB
  (`STRATA_OTEL_RINGBUF_BYTES`). Tunable upward for heavier trace
  retention; bump RAM budget proportionally.
- **slog JSON encoder:** allocation-bounded, no caches.

A medium-tier replica at 2,000 RPS with ~200 concurrent in-flight
requests sits around 1.2–1.5 GiB resident.

## Worker fan-out

Workers run inside the gateway binary and share its CPU / RAM budget.
Phase 1 (single-replica) caps:

- `STRATA_GC_CONCURRENCY` default 64 — bench knee for gc fan-out at
  ~64 (90k chunks/s on TiKV+memory). Per-additional goroutine yield
  drops below 10 % above 64.
- `STRATA_LIFECYCLE_CONCURRENCY` default 64 — lifecycle still scales
  past 64 in the bench (no knee inside the swept range), bounded by
  meta-backend headroom.

Phase 2 (multi-replica) shards the leader-election space; see
[GC + Lifecycle tuning]({{< ref "/best-practices/gc-lifecycle-tuning" >}})
for the full tuning matrix and
[GC + Lifecycle scaling]({{< ref "/architecture/benchmarks/gc-lifecycle" >}})
for measured numbers.

The replicator / notify / inventory / audit-export / manifest-rewriter
workers each add a single goroutine when active; the supervisor folds
them into the gateway's process. Reserve ~50 MiB extra RAM and ~0.2
cores per active worker beyond gc / lifecycle.

## Metadata backend sizing

### Cassandra

Three-node Cassandra cluster (RF=3, LOCAL_QUORUM) is the floor. Per
node:

- 8 cores, 16 GiB RAM, NVMe SSD ≥ 500 GiB.
- `concurrent_writes: 64`, `concurrent_reads: 64`, `concurrent_counter_writes: 32`.
- The objects table is sharded `(bucket_id, shard)`; the 64-way default
  partition fan-out keeps any single partition under ~10 GiB.

ScyllaDB is a CQL-compatible drop-in — the same gocql client works
unchanged. ScyllaDB sizes more aggressively per node (more cores, more
RAM); follow the upstream sizing guide. Strata's LWT path is the load
shape that matters; budget for ~5× the read path's IOPS for LWT
conflicts under contention.

Cluster sizing target: aim for ≤ 60 % CPU at peak load on every node;
LWT has a steep tail.

### TiKV

Three-node PD cluster (raft majority, small footprint) + three-node
TiKV cluster (region raft factor 3) is the floor. Per node:

- PD: 4 cores, 8 GiB RAM, 100 GiB SSD (tiny).
- TiKV: 16 cores, 64 GiB RAM, NVMe SSD ≥ 1 TiB. Tune
  `storage.scheduler-worker-pool-size`, `raftstore.apply-pool-size`,
  `raftstore.store-pool-size` per the upstream TiKV operator guide.

TiKV's pessimistic-txn round-trip is the dominant per-op cost on the
gateway side; latency targets are the same as Cassandra (p99 < 20 ms
for `Get`, p99 < 50 ms for a pessimistic-txn commit).

### Local backends (memory)

`memory` is for tests and the smoke pass only. Not supported in
production; see
[Architecture — Meta Store]({{< ref "/architecture/meta-store" >}}).

## Data backend sizing

### RADOS (Ceph)

Strata writes 4 MiB chunks to a Ceph pool. Per chunk → one RADOS object
→ ~3 round-trips (write, ack, journal flush). Sizing the Ceph cluster
is upstream territory; the Strata-specific knobs:

- **Pool replication:** EC 4+2 or replicated 3 are both supported.
  Replicated is simpler; EC saves ~50 % storage at the cost of ~2× the
  write IOPS per chunk.
- **Pool count:** Strata supports multi-cluster RADOS routing
  (`STRATA_RADOS_POOLS=...`) so per-class pools can land on different
  Ceph clusters. See
  [Architecture — Data Backend]({{< ref "/architecture/data-backend" >}}).
- **OSDs per node:** rule of thumb ≥ 8 OSDs per node so a single OSD
  failure stays under 12.5 % of node IO.

### S3-over-S3

Strata can target an upstream S3 bucket as the data backend (e.g. AWS
S3 or another S3-compatible store). Sizing falls out of the upstream
service; budget the Strata-side connection pool size to match peak
concurrent PUTs.

## Cross-region replication

The replicator worker is single-leader and queue-driven. One active
replicator goroutine per replica (leader-elected on
`replicator-leader`). Throughput is bounded by the peer S3 endpoint's
PUT rate; sizing matches the peer's accepted RPS. See
[Backup + restore]({{< ref "/best-practices/backup-restore" >}}).

## Headroom + scaling triggers

Trigger replica scale-out when:

- `strata_http_request_duration_seconds` p99 > 200 ms for ≥ 5 min, AND
  per-replica CPU > 70 %.
- Worker queue depth (`strata_replication_queue_depth` /
  `strata_gc_queue_depth`) climbs faster than drain rate for ≥ 10 min.
- `strata_worker_panic_total` recovers but the panic rate exceeds the
  baseline (typically 0).

Trigger metadata-backend scale-out when:

- p99 of `strata_cassandra_query_duration_seconds{op="LWT"}` > 50 ms,
  OR Cassandra read latency from the cluster's own metrics breaches
  upstream guidelines.
- TiKV PD reports region-rebalance pressure (region-leader churn > 5 %
  per minute).

See [Monitoring]({{< ref "/best-practices/monitoring" >}}) for the full
metric set and the
[architecture deep dive]({{< ref "/architecture" >}}) for why each
ceiling exists.
