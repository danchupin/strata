---
title: 'Deploy'
weight: 20
bookFlatSection: true
description: 'Production deployment guides — single-node, Docker Compose, multi-replica, Kubernetes.'
---

# Deploy

Strata supports four deployment shapes.

| Shape | When to pick it | Guide |
|---|---|---|
| **Single-node** | Lab, dev, single-tenant pilot. One gateway against memory/Cassandra metadata + memory/RADOS data. | [`single-node`]({{< ref "/deploy/single-node" >}}) |
| **Docker Compose** | Anything driven by the bundled `deploy/docker/docker-compose.yml`. Service map, ports, env, volumes, profiles (`tikv`, `lab-tikv`, `tracing`, `features`). | [`docker-compose`]({{< ref "/deploy/docker-compose" >}}) |
| **Multi-replica** | HA HTTP traffic (≥2 gateway replicas) behind an LB with shared TiKV / RADOS storage. STRATA_GC_SHARDS-aware. | [`multi-replica`]({{< ref "/deploy/multi-replica" >}}) |
| **Kubernetes** | 3-replica `Deployment` + `Service` + `ConfigMap` + `Secret` + `Ingress` against external TiKV + RADOS. Apply-tested manifests under `deploy/k8s/`. | [`kubernetes`]({{< ref "/deploy/kubernetes" >}}) |

For the 5-minute first run, see [Get Started]({{< ref "/get-started" >}}).
For the runtime layers each shape sits on top of, see
[Architecture]({{< ref "/architecture" >}}).
