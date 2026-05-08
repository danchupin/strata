---
title: 'Architecture'
weight: 30
bookFlatSection: true
description: 'Layer-by-layer architecture: auth, router, meta-store, data-backend, workers, sharding, observability.'
---

# Architecture

Strata is an S3-compatible gateway built on three swappable tiers: the HTTP S3
surface, an ordered metadata store, and a chunked data backend. The
responsibilities are deliberately narrow at each layer so a backend swap
(memory → Cassandra, RADOS → S3-over-S3) drops in without touching the router
or the workers.

```
                +-----------------------------+
                |  S3 client (aws-cli, mc)    |
                +--------------+--------------+
                               |
                  HTTP S3 (path-style URLs)
                               |
                +--------------v--------------+
                | cmd/strata server           |
                |  -> auth.Middleware (SigV4) |
                |  -> s3api.Server (router)   |
                +-------+--------------+------+
                        |              |
                        v              v
              +--------------------------+   +---------------------+
              | meta.Store               |   | data.Backend        |
              |  memory | cassandra|tikv |   |  memory | rados|s3 |
              +---------+----------------+   +---------+----------+
                        |                              |
                +-------v---------+            +-------v-------+
                | Cassandra/Scylla|            | RADOS         |
                | (sharded fan-out)            | (4 MiB chunks)|
                |   OR             |           +---------------+
                | TiKV (PD+TiKV,   |
                |  ordered scan)   |
                +------------------+
```

A single `cmd/strata server` binary plays both gateway and worker roles. The
HTTP listener serves S3 traffic; `STRATA_WORKERS=` opts the same process into
one or more background loops (gc, lifecycle, notify, replicator, access-log,
inventory, audit-export, manifest-rewriter). Every worker is leader-elected
on a per-name lease so multi-replica deployments scale the gateway without
duplicating background work — see [Workers]({{< ref "/architecture/workers" >}}).

## Per-layer pages

- [Auth]({{< ref "/architecture/auth" >}}) — SigV4, presigned URLs, streaming chunk decoder, virtual-hosted-style routing, identity attribution.
- [Router]({{< ref "/architecture/router" >}}) — `internal/s3api/server.go` query-string dispatch shape, vhost rewriting, admin path carve-out.
- [Meta store]({{< ref "/architecture/meta-store" >}}) — the `meta.Store` contract, LWT semantics, range scans, the optional `RangeScanStore` short-circuit.
- [Data backend]({{< ref "/architecture/data-backend" >}}) — RADOS 4 MiB chunking, manifest format (proto vs JSON sniff, schema-additive evolution), multi-cluster routing, S3-over-S3.
- [Workers]({{< ref "/architecture/workers" >}}) — supervisor model, leader election, panic restart, per-worker pages.
- [Sharding]({{< ref "/architecture/sharding" >}}) — `(bucket_id, shard)` partitioning, fan-out merge, gc fan-out, online reshard.
- [Observability]({{< ref "/architecture/observability" >}}) — slog, audit log, request_id propagation, OTel tracing (tail-sampler + ring buffer), per-storage observers.

## Per-backend deep dives

- [Backends]({{< ref "/architecture/backends" >}}) — TiKV, ScyllaDB, S3-over-S3.
- [Benchmarks]({{< ref "/architecture/benchmarks" >}}) — GC + lifecycle scaling, meta-backend comparison.
- [Migrations]({{< ref "/architecture/migrations" >}}) — binary consolidation, GC + lifecycle Phase 2.
- [Storage]({{< ref "/architecture/storage" >}}) — meta + data backend health surfacing in the operator console.
