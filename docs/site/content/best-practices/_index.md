---
title: 'Best Practices'
weight: 40
bookFlatSection: true
description: 'Operational guidance — sizing, monitoring, GC + lifecycle tuning, backup, capacity planning.'
---

# Best Practices

Operator-facing guidance for running Strata in production. Each page
covers one operational concern end-to-end with the env knobs, metrics,
and runbook shape an on-call operator needs.

| Page | When to read it |
|---|---|
| [Sizing]({{< ref "/best-practices/sizing" >}}) | Picking CPU / RAM / disk per replica, plus Cassandra / TiKV cluster sizing pointers. |
| [Monitoring]({{< ref "/best-practices/monitoring" >}}) | Wiring Prometheus, Grafana, OTel collector, and the in-process trace browser. |
| [GC + lifecycle tuning]({{< ref "/best-practices/gc-lifecycle-tuning" >}}) | Tuning `STRATA_GC_CONCURRENCY` / `STRATA_LIFECYCLE_CONCURRENCY` (Phase 1) and `STRATA_GC_SHARDS` (Phase 2), plus the dual-write cutover playbook. |
| [Backup + restore]({{< ref "/best-practices/backup-restore" >}}) | Snapshot strategy across the metadata, data, and replication tiers. |
| [Capacity planning]({{< ref "/best-practices/capacity-planning" >}}) | Chunk fan-out math, when to scale shards / replicas, dedup roadmap. |
| [Quotas + billing]({{< ref "/best-practices/quotas-billing" >}}) | `BucketQuota` / `UserQuota` shape, `QuotaExceeded` 403, the `bucket_stats` counter, the reconcile + usage-rollup workers. |
| [S3 multi-cluster routing]({{< ref "/best-practices/s3-multi-cluster" >}}) | `STRATA_S3_CLUSTERS` / `STRATA_S3_CLASSES` env shape, `credentials_ref` discriminator, per-class `(cluster, bucket)` routing, rolling-restart workflow. |
| [Web UI (Strata Console)]({{< ref "/best-practices/web-ui" >}}) | Embedded operator console: pages, env vars, end-to-end tests. |

## See also

- [Architecture deep dive]({{< ref "/architecture" >}}) for the
  implementation rationale behind each knob.
- [Deploy]({{< ref "/deploy" >}}) for end-to-end deployment guides
  (single-node, Docker Compose, multi-replica, Kubernetes).
- [S3 Compatibility]({{< ref "/s3-compatibility" >}}) for the
  supported / unsupported S3 surface.
