---
title: 'Workers'
weight: 25
description: 'Supervisor model, registration via init(), leader-election shape, panic restart with backoff, per-worker pages.'
---

# Workers

Background loops run inside the same `cmd/strata server` binary as the
gateway. `STRATA_WORKERS=` (or `--workers=`) selects which to run; an
empty list runs the gateway only. Each worker is leader-elected, panic-
recovered, and supervised — one worker's panic or lease loss never affects
the gateway or sibling workers.

## Registry shape

Each worker has a per-worker file under `cmd/strata/workers/<name>.go`
that calls `workers.Register` from `init()`:

```go
func init() {
    workers.Register(workers.Worker{
        Name: "gc",
        Build: func(deps Dependencies) (Runner, error) { ... },
        SkipLease: true, // gc fan-out manages its own per-shard leases
    })
}
```

`Build` constructs the per-worker runner from the shared `workers.Dependencies`
struct (`Logger`, `Meta`, `Data`, `Tracer`, `Locker`, `Region`, `EmitLeader`).
Per-worker tunables (`STRATA_GC_INTERVAL`, `STRATA_LIFECYCLE_*`, …) are read
inside `Build` directly so the dependency surface stays small.

`workers.Resolve` runs **before any backend is built** in
`cmd/strata/server.go::runServer`, so an unknown worker name fails fast
(exit 2) before Cassandra / RADOS connections are opened. Typos surface in
under a second.

## Supervisor

`workers.Supervisor.Run(ctx, workers)` spins one goroutine per requested
worker. Each goroutine:

1. Acquires the worker's leader lease (`leader.Session` keyed
   `<name>-leader`).
2. Calls `Build(deps)` to construct the runner.
3. Runs `runner.Run(ctx)` under a supervised context.
4. On `Run` return: releases the lease, restarts on backoff (`1s → 5s → 30s
   → 2m`, reset to `1s` after 5 minutes healthy).
5. On panic: increments `strata_worker_panic_total{worker=<name>,shard="-"}`,
   releases the lease, and restarts on backoff.
6. On lease loss (eviction / network partition): restarts immediately
   without backoff.

The leader-election locker is built in `internal/serverapp/serverapp.go`:
Cassandra → LWT-backed lease, memory → process-local lock. TiKV reuses
the Cassandra-shape lease through a thin shim.

## SkipLease workers

Workers that own their own leader-election internally register with
`SkipLease: true`. The supervisor still owns panic recovery and backoff
— only the outer `<name>-leader` lease is skipped. The runner manages
its own leases and must call `deps.EmitLeader(name, acquired)` from each
acquire/release transition so the heartbeat `leader_for=<name>` chip
still flips on the supervisor's `LeaderEvents()` channel.

The canonical SkipLease worker is **gc fan-out** (US-004). Per-shard
`gc-leader-<shardID>` leases let one replica drain multiple shards in
parallel and lose only one shard's lease on a panic. Shard count is
`STRATA_GC_SHARDS` (default 1, range `[1, 1024]`).

The fan-out folds multi-shard ownership inside one replica into a
single heartbeat-level acquired/released pair: the chip flips at most
twice per cycle even though the underlying fan-out may hold N shards.
Per-shard panics increment
`strata_worker_panic_total{worker="gc",shard="<i>"}`.

## Per-worker reference

| Worker | Lease key | Source |
|---|---|---|
| `gc` | `gc-leader-<shardID>` (per-shard, SkipLease) | `cmd/strata/workers/gc.go` + `internal/gc/` |
| `lifecycle` | `lifecycle-leader-<bucketID>` (per-bucket, gated by `fnv32a(bucketID) % STRATA_GC_SHARDS`) | `cmd/strata/workers/lifecycle.go` + `internal/lifecycle/` |
| `notify` | `notify-leader` | `cmd/strata/workers/notify.go` + `internal/notify/` |
| `replicator` | `replicator-leader` | `cmd/strata/workers/replicator.go` + `internal/replicator/` |
| `access-log` | `access-log-leader` | `cmd/strata/workers/access_log.go` + `internal/accesslog/` |
| `inventory` | `inventory-leader` | `cmd/strata/workers/inventory.go` + `internal/inventory/` |
| `audit-export` | `audit-export-leader` | `cmd/strata/workers/audit_export.go` + `internal/auditexport/` |
| `manifest-rewriter` | `manifest-rewriter-leader` | `cmd/strata/workers/manifest_rewriter.go` + `internal/manifestrewriter/` |

### gc

Drains the GC entry queue (chunks scheduled for deletion by lifecycle
transitions, multipart aborts, version overwrites) and removes them
from the data backend. Fans out across `STRATA_GC_SHARDS` logical shards
(1024 wide internally; runtime shard count modulates the fan-out
factor). Tunables: `STRATA_GC_INTERVAL`, `STRATA_GC_GRACE`,
`STRATA_GC_BATCH_SIZE`, `STRATA_GC_CONCURRENCY`, `STRATA_GC_SHARDS`.

### lifecycle

Walks bucket lifecycle rules and applies transitions / expirations /
multipart-aborts. Phase 2 (US-005 of the binary-consolidation cycle)
gates per-bucket leases on
`fnv32a(bucketID) % STRATA_GC_SHARDS == min(GCFanOut.HeldShards())`,
so multi-replica deployments distribute lifecycle work in lockstep
with the gc fan-out. The legacy global `lifecycle-leader` lease is
retired.

### notify

Drains `notify_queue` + DLQ → webhook / SQS sinks via
`STRATA_NOTIFY_TARGETS`. Backoff + DLQ semantics in
`internal/notify/`.

### replicator

Drains `replication_queue` → peer Strata via HTTP PUT
(`HTTPDispatcher`). Cross-region replication endpoints come from the
bucket's replication configuration.

### access-log

Drains `access_log_buffer` per source bucket and writes one AWS-format
log object per flush into the target bucket configured by
`PutBucketLogging`.

### inventory

Ticks per `(bucket, configID)`, walks the source bucket, and writes
`manifest.json` + CSV.gz pairs into the configured target bucket.
Driven by `InventoryConfiguration` blobs on each bucket.

### audit-export

Drains `audit_log` partitions older than `STRATA_AUDIT_EXPORT_AFTER`
(default 30 days) into gzipped JSON-lines objects in the configured
export bucket, then deletes the source partition. Long-term audit
retention without keeping every row hot in Cassandra.

### manifest-rewriter

Walks every bucket and converts any JSON-encoded `objects.manifest`
blob to protobuf in place (US-049). Idempotent — re-runs skip
already-proto rows. Cadence via `STRATA_MANIFEST_REWRITER_INTERVAL`
(default 24h). See [Data backend]({{< ref "/architecture/data-backend" >}}).

## Source

- `cmd/strata/workers/registry.go` — `Worker`, `Register`, `Resolve`.
- `cmd/strata/workers/supervisor.go` — leader acquisition, panic
  recovery, backoff loop.
- `cmd/strata/workers/<name>.go` — per-worker `init()` registration.
- `internal/<name>/` — per-worker `Runner` implementation.
- `internal/leader/` — leader-election sessions (Cassandra LWT,
  memory).
