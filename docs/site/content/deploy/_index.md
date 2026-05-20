---
title: 'Deploy'
weight: 20
bookFlatSection: true
description: 'Production deployment guides — single-node, Docker Compose, multi-replica, Kubernetes.'
---

# Deploy

Strata ships four deployment shapes. Each sub-page follows the same
template — **Prerequisites → Install → Configure → Verify → Monitor →
Troubleshoot** — so you can scan them side by side.

| Shape | When to pick it | Guide |
|---|---|---|
| **Single-node** | Lab, dev, single-tenant pilot. One gateway against memory or Cassandra metadata + memory or RADOS data. | [Single-node]({{< ref "/deploy/single-node" >}}) |
| **Docker Compose** | The bundled reference stack — TiKV-default 2-replica lab, profile-gated Cassandra regression lab, profile-gated tracing collector. | [Docker Compose]({{< ref "/deploy/docker-compose" >}}) |
| **Multi-replica** | ≥2 gateway replicas behind a load balancer against shared TiKV / RADOS storage. | [Multi-replica]({{< ref "/deploy/multi-replica" >}}) |
| **Kubernetes** | 3-replica `Deployment` + `Service` + `Ingress` against external TiKV + RADOS. Raw manifests under `deploy/k8s/` plus a Helm chart under `deploy/helm/strata/`. | [Kubernetes]({{< ref "/deploy/kubernetes" >}}) |

## Cross-references

- [Get Started]({{< ref "/get-started" >}}) — 5-minute first run.
- [Concepts]({{< ref "/concepts" >}}) — what Strata is, S3 surface, multi-cluster, drain.
- [Operate](/operate/) — day-2 workflows (drain, scale, back up).
- [Reference — environment variables]({{< ref "/reference/env-vars" >}}) — full env knob table.
- [Architecture]({{< ref "/architecture" >}}) — the runtime layers each shape sits on top of.
