---
title: 'Deploy'
weight: 20
bookFlatSection: true
description: 'Production deployment guides — single-node, Docker Compose, multi-replica, Kubernetes.'
---

# Deploy

Strata supports four deployment shapes:

- **Single-node** — one gateway against memory or Cassandra metadata + memory or RADOS data. Lab / dev / single-tenant.
- **Docker Compose** — the bundled `deploy/docker/docker-compose.yml` ships every supported profile (default, `tikv`, `lab-tikv`, `tracing`, `s3-backend`).
- **Multi-replica** — N gateways behind a load balancer with shared metadata + data. See [`multi-replica`]({{< ref "/deploy/multi-replica" >}}).
- **Kubernetes** — apply-tested example manifests under `deploy/k8s/`.

Single-node, Docker Compose, and Kubernetes pages land in US-005 / US-006.
