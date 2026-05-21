---
title: 'Strata Documentation'
type: 'docs'
weight: 1
bookFlatSection: true
bookToc: false
description: 'S3-compatible object gateway. Cassandra/TiKV metadata, RADOS data. Drop-in replacement for Ceph RGW.'
---

# Strata

S3-compatible object gateway, written in Go. Metadata in Cassandra,
ScyllaDB, or TiKV. Data as 4 MiB chunks in RADOS or any S3 bucket. Drop-in
replacement for Ceph RGW — without the bucket-index ceiling.

> ⚠️ **Alpha software.** Pre-launch; no production deploys yet. APIs and
> schemas may change without notice.

## Where to start

{{% columns %}}

- {{< card href="/get-started/" >}}**Get Started** — Install Strata, verify it is healthy, and put your first object in under five minutes.{{< /card >}}

- {{< card href="/concepts/" >}}**Concepts** — What Strata is, the S3 surface it speaks, how multi-cluster routing and drain work, what the workers do.{{< /card >}}

- {{< card href="/deploy/" >}}**Deploy** — Production deployment guides — single-node, Docker Compose, multi-replica, Kubernetes.{{< /card >}}

{{% /columns %}}

{{% columns %}}

- {{< card href="/operate/" >}}**Operate** — Day-2 workflows — drain a cluster, monitor, scale, back up, plan capacity.{{< /card >}}

- {{< card href="/best-practices/" >}}**Best Practices** — Tuning guides for placement, tracing, GC, lifecycle, quotas, billing, compliance, multi-cluster.{{< /card >}}

- {{< card href="/reference/" >}}**Reference** — Env vars, Admin API surface, S3 API surface, interactive OpenAPI viewer.{{< /card >}}

{{% /columns %}}

{{% columns %}}

- {{< card href="/s3-compatibility/" >}}**S3 Compatibility** — Supported / unsupported S3 surface — read this first if you are evaluating Strata as a Ceph RGW replacement.{{< /card >}}

- {{< card href="/architecture/" >}}**Architecture** — Layer-by-layer architecture — auth, router, meta-store, data-backend, workers, plus deep dives for the critical flows.{{< /card >}}

{{% /columns %}}

## Project

- [GitHub](https://github.com/danchupin/strata) — source, issues, releases.
- [ROADMAP](https://github.com/danchupin/strata/blob/main/ROADMAP.md) — what is shipped vs in flight vs parked.
- [Developers]({{< relref "/developers/" >}}) — editing the docs.
- [Architecture Decision Records]({{< relref "/adr/" >}}) — captured design decisions.
