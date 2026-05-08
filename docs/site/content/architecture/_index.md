---
title: 'Architecture'
weight: 30
bookFlatSection: true
description: 'Layer-by-layer architecture: auth, router, meta-store, data-backend, workers, sharding, observability.'
---

# Architecture

The full diagram + per-layer pages land in US-007. Section index for now:

- [Backends]({{< ref "/architecture/backends" >}}) — TiKV, ScyllaDB, S3-over-S3.
- [Benchmarks]({{< ref "/architecture/benchmarks" >}}) — GC + lifecycle scaling, meta-backend comparison.
- [Migrations]({{< ref "/architecture/migrations" >}}) — binary consolidation, GC + lifecycle Phase 2.
- [Storage]({{< ref "/architecture/storage" >}}) — meta + data backend health surfacing.
