---
title: 'Benchmarks'
weight: 50
bookFlatSection: true
description: 'Operator-runnable benchmarks for hot-path operations and worker scaling curves.'
---

# Benchmarks

- [GC + lifecycle scaling]({{< ref "/architecture/benchmarks/gc-lifecycle" >}}) — Phase 1 single-leader concurrency cap + Phase 2 multi-leader scaling.
- [Rebalance scaling]({{< ref "/architecture/benchmarks/rebalance" >}}) — Phase 2 multi-leader fan-out for the rebalance worker.
- [Meta-backend comparison]({{< ref "/architecture/benchmarks/meta-backend-comparison" >}}) — TiKV vs Cassandra vs memory on the headline operations.
- [RGW comparison]({{< ref "/architecture/benchmarks/rgw-comparison" >}}) — Strata (TiKV-default lab) vs Ceph RGW v19 on the same RADOS cluster; validates the README "drop-in RGW replacement" claim.
