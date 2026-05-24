# Strata

![CI](https://github.com/danchupin/strata/actions/workflows/ci.yml/badge.svg)
![License](https://img.shields.io/badge/license-Apache--2.0-blue)

> ⚠️ **Alpha software.** Pre-launch; no production deploys. APIs and schemas may change without notice.

S3-compatible object gateway — drop-in replacement for Ceph RGW. Metadata in Cassandra or TiKV; data in RADOS, S3, or memory.

## What is Strata?

Strata speaks the S3 HTTP API and serves objects out of one or more storage backends. Compared with Ceph RGW, it
moves the bucket index off RADOS omap so listing and resharding stop being the operator's worst day. Buckets can pin
their data to specific clusters; a drain workflow lets you decommission a cluster without downtime. One static binary
runs the gateway and every background worker; an embedded web console handles day-to-day operator tasks.

## Key features

- S3 surface: buckets, objects, multipart, versioning, ACLs, lifecycle, SSE-S3/KMS, replication, Object Lock, tagging.
- Two first-class metadata backends: Cassandra (and ScyllaDB drop-in) or TiKV. Memory backend ships for tests + smoke.
- Multi-cluster RADOS routing with per-bucket placement policies and weighted default routing.
- Online drain + rebalance — move a cluster's data off without taking the bucket offline.
- One static binary: `strata server` runs the gateway plus opt-in workers (GC, lifecycle, replication, notifications…).
- Embedded operator console at `/console/` — buckets, drain progress, metrics, audit log.
- OpenTelemetry tracing, Prometheus metrics, Grafana dashboards out of the box.

## Strata vs Ceph RGW

| Capability                  | Strata                                                   | Ceph RGW                              |
|-----------------------------|----------------------------------------------------------|---------------------------------------|
| Bucket index                | 208k ops/s on a 100k-key bucket; RGW saturates the OSD on the seed phase[^list-100k] | RADOS omap, dynamic resharding stalls[^list-100k] |
| Concurrent 1 KiB PUT (p99)  | ~211 ms @ c=8 — sharded `bucket_stats` fan-out absorbs writes[^put-small] | 11–22 s @ c=8 — omap-index serialises every PUT[^put-small] |
| Single-object PUT/GET (p99) | ~12 ms @ c=1 (user-space SigV4)[^get-small] | ~2 ms @ c=1 (in-process auth)[^get-small] |
| Multipart 5 GB (throughput) | 4 MiB chunk write, manifest LWT on Complete[^multipart-5g] | 4 MiB striping, omap-index bookkeeping[^multipart-5g] |
| Online resharding           | Yes — `strata admin reshard`, no read pause              | Re-shard locks the bucket             |
| Multi-cluster routing       | Per-bucket policies + weighted default routing           | Multisite zonegroups only             |
| Cluster drain               | First-class workflow with live progress + ETA            | Manual `rados cppool` dance           |
| Admin surface               | REST API + embedded web console                          | `radosgw-admin` CLI                   |
| Deployment shape            | One Docker image, one binary                             | RGW + osd + mon + mgr stack           |
| Observability               | OTel traces, Prometheus, Grafana, request-id correlation | Native metrics, partial tracing       |
| License                     | Apache 2.0                                               | LGPL 2.1                              |

Bench numbers from `make bench-rgw-comparison` on lima/macOS M3 Pro; see [Limitations](https://danchupin.github.io/strata/architecture/benchmarks/rgw-comparison/#limitations).

[^list-100k]: ListObjects 100k-key p99 — see [rgw-comparison#list-100k](https://danchupin.github.io/strata/architecture/benchmarks/rgw-comparison/#list-100k).
[^put-small]: 1 KiB PUT concurrency sweep (c=1/8/32/128) — see [rgw-comparison#put-small](https://danchupin.github.io/strata/architecture/benchmarks/rgw-comparison/#put-small).
[^get-small]: 1 KiB GET concurrency sweep (c=1/8/32/128) — see [rgw-comparison#get-small](https://danchupin.github.io/strata/architecture/benchmarks/rgw-comparison/#get-small).
[^multipart-5g]: Multipart 5 GB per-part p99 + aggregate throughput — see [rgw-comparison#multipart-5g](https://danchupin.github.io/strata/architecture/benchmarks/rgw-comparison/#multipart-5g).

## Status & maturity

Strata is in alpha — pre-launch, no production deploys yet. Shipped features below; open work tracked in [ROADMAP.md](ROADMAP.md).

- **Shipped**: S3 surface (92.7% of `ceph/s3-tests` pass), Cassandra + TiKV backends, multi-cluster RADOS, drain & rebalance, lifecycle, replication, notifications, SSE-S3/KMS, Object Lock, operator console, OTel tracing.
- **In flight**: benchmark harness vs upstream RGW, ScyllaDB performance numbers, documentation polish.
- **Parked**: alternative metadata backends (FoundationDB, Postgres). Cassandra + TiKV cover the design space.

## Quickstart

### In-memory smoke (fastest)

```bash
make run-memory
make smoke
```

Listens on `:9999` with memory metadata + memory data. Round-trips a `PUT/GET` over the gateway.

### Full TiKV lab (PD + TiKV + two Ceph clusters + two Strata replicas behind nginx)

```bash
make up-all
make wait-tikv
make wait-ceph
make wait-strata-lab
make smoke
open http://localhost:9999/console/
```

Bare `docker compose up -d` is the canonical 2-replica TiKV lab. Cassandra-backed regression lab layers on with
`make up-cassandra` (adds the Cassandra-backed Strata replica at `:9998` behind the same compose stack).

## Documentation

Full guides — Get Started, Concepts, Deploy, Operate, Best Practices, Architecture deep dive, Reference, S3
Compatibility — live at **[danchupin.github.io/strata](https://danchupin.github.io/strata/)**.

For source-tree readers: `make docs-serve` runs the Hugo site locally on `:1313`.

## License

Apache 2.0 — see [LICENSE](LICENSE).
