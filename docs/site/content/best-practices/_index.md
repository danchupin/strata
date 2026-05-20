---
title: 'Best Practices'
weight: 40
bookFlatSection: true
description: 'Tuning guides for placement, tracing, GC, lifecycle, quotas, multi-cluster, and the operator console.'
---

# Best Practices

Tuning guides for running Strata in production. Each page covers one
operational concern end-to-end with the env knobs, metrics, and
runbook shape an on-call operator needs. Day-2 workflows (drain a
cluster, monitor, scale, back up, plan capacity) live under
[Operate]({{< ref "/operate" >}}); the pages below cover the knobs
the workflows reference.

| Page | When to read it |
|---|---|
| [Placement + rebalance]({{< ref "/best-practices/placement-rebalance" >}}) | Per-bucket `Placement` policy, `STRATA_REBALANCE_*` knobs, drain sentinel + safety rails, register → drain → deregister runbook. |
| [Tracing]({{< ref "/best-practices/tracing" >}}) | OpenTelemetry coverage matrix, span name conventions, `strata.component=gateway\|worker` filters, sampling. |
| [GC + lifecycle tuning]({{< ref "/best-practices/gc-lifecycle-tuning" >}}) | Tuning `STRATA_GC_CONCURRENCY` / `STRATA_LIFECYCLE_CONCURRENCY` (Phase 1) and `STRATA_GC_SHARDS` (Phase 2), plus the dual-write cutover playbook. |
| [Quotas + billing]({{< ref "/best-practices/quotas-billing" >}}) | `BucketQuota` / `UserQuota` shape, `QuotaExceeded` 403, the bucket-stats counter, the reconcile + usage-rollup workers. |
| [S3 multi-cluster routing]({{< ref "/best-practices/s3-multi-cluster" >}}) | `STRATA_S3_CLUSTERS` / `STRATA_S3_CLASSES` env shape, `credentials_ref` discriminator, per-class `(cluster, bucket)` routing, rolling-restart workflow. |
| [Web UI (Strata Console)]({{< ref "/best-practices/web-ui" >}}) | Embedded operator console: pages, env vars, end-to-end tests. |

## See also

- [Operate]({{< ref "/operate" >}}) for day-2 ops workflows — drain a
  cluster, monitor, scale, back up, plan capacity.
- [Architecture deep dive]({{< ref "/architecture" >}}) for the
  implementation rationale behind each knob.
- [Deploy]({{< ref "/deploy" >}}) for end-to-end deployment guides
  (single-node, Docker Compose, multi-replica, Kubernetes).
- [S3 Compatibility]({{< ref "/s3-compatibility" >}}) for the
  supported / unsupported S3 surface.
