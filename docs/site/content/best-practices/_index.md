---
title: 'Best Practices'
weight: 30
bookFlatSection: true
description: 'Tuning guides for placement, tracing, GC, lifecycle, quotas, billing, compliance, multi-cluster, and the operator console.'
---

# Best Practices

Tuning guides for running Strata in production. Each page covers one
operational concern end-to-end with the env knobs, metrics, and runbook
shape an on-call operator needs. Day-2 workflows (drain a cluster,
monitor, scale, back up, plan capacity) live under
[Operate]({{< ref "/operate" >}}); the pages below cover the knobs the
workflows reference.

{{% columns %}}

- {{< card href="/best-practices/placement-rebalance" >}}**Placement + rebalance** — Per-bucket `Placement` policy, `STRATA_REBALANCE_*` knobs, drain sentinel + safety rails, register → drain → deregister runbook.{{< /card >}}

- {{< card href="/best-practices/tracing" >}}**Tracing** — OpenTelemetry coverage matrix, span name conventions, `strata.component=gateway|worker` filters, sampling.{{< /card >}}

- {{< card href="/best-practices/web-ui" >}}**Web UI (Strata Console)** — Embedded operator console: pages, env vars, end-to-end tests.{{< /card >}}

{{% /columns %}}

{{% columns %}}

- {{< card href="/best-practices/gc-lifecycle-tuning" >}}**GC + lifecycle tuning** — Tuning `STRATA_GC_CONCURRENCY` / `STRATA_LIFECYCLE_CONCURRENCY` and `STRATA_GC_SHARDS`, plus the dual-write cutover playbook.{{< /card >}}

- {{< card href="/best-practices/quotas-billing" >}}**Quotas + billing** — `BucketQuota` / `UserQuota` shape, `QuotaExceeded` 403, the bucket-usage counter, the reconcile + usage-rollup workers.{{< /card >}}

- {{< card href="/best-practices/s3-multi-cluster" >}}**S3 multi-cluster routing** — `STRATA_S3_CLUSTERS` / `STRATA_S3_CLASSES` env shape, `credentials_ref` discriminator, per-class `(cluster, bucket)` routing, rolling-restart workflow.{{< /card >}}

{{% /columns %}}

{{% columns %}}

- {{< card href="/best-practices/compliance" >}}**Compliance** — S3 Object Lock COMPLIANCE workflow, retention modes, legal hold, and the three `objectlock:*` audit verbs.{{< /card >}}

- {{< card href="/best-practices/billing" >}}**Billing** — Byte-seconds trapezoid math, intra-day sampling, and the `usage_aggregates` feed external invoice generators consume.{{< /card >}}

- {{< card href="/best-practices/production-hardening" >}}**Production hardening** — 12-line checklist for prod readiness: HTTP timeouts, built-in TLS, backend mTLS, trusted proxies, split admin listener, ingress rate limit.{{< /card >}}

{{% /columns %}}

## See also

- [Operate]({{< ref "/operate" >}}) for day-2 ops workflows — drain a
  cluster, monitor, scale, back up, plan capacity.
- [Architecture deep dive]({{< ref "/architecture" >}}) for the
  implementation rationale behind each knob.
- [Deploy]({{< ref "/deploy" >}}) for end-to-end deployment guides
  (single-node, Docker Compose, multi-replica, Kubernetes).
- [S3 Compatibility]({{< ref "/s3-compatibility" >}}) for the
  supported / unsupported S3 surface.
