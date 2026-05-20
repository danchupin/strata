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
| Bucket index                | Sharded Cassandra / TiKV, ordered range scans            | RADOS omap, dynamic resharding stalls |
| Online resharding           | Yes — `strata admin reshard`, no read pause              | Re-shard locks the bucket             |
| Multi-cluster routing       | Per-bucket policies + weighted default routing           | Multisite zonegroups only             |
| Cluster drain               | First-class workflow with live progress + ETA            | Manual `rados cppool` dance           |
| Admin surface               | REST API + embedded web console                          | `radosgw-admin` CLI                   |
| Deployment shape            | One Docker image, one binary                             | RGW + osd + mon + mgr stack           |
| Observability               | OTel traces, Prometheus, Grafana, request-id correlation | Native metrics, partial tracing       |
| License                     | Apache 2.0                                               | LGPL 2.1                              |

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
