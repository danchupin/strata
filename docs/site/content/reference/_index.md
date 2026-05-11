---
title: 'Reference'
weight: 60
bookFlatSection: true
description: 'Reference tables — env vars, Admin API, S3 surface. Expansion deferred to a P3 follow-up.'
---

# Reference

The full reference (env vars table, Admin API surface, S3 operations table)
is queued as a P3 follow-up after the cycle closes. Until then:

- README has the canonical env-var table.
- Admin API schema lives in `internal/adminapi/openapi.yaml`.
- S3 surface coverage: see [S3 Compatibility]({{< ref "/s3-compatibility" >}}).

## Worker env vars (selected)

The full set lives in README; the entries that recently shipped and are
not yet folded into the planned env-vars subpage:

| Env | Default | Worker | Doc |
|---|---|---|---|
| `STRATA_QUOTA_RECONCILE_INTERVAL` | `6h` | `--workers=quota-reconcile` | [Quotas + billing]({{< ref "/best-practices/quotas-billing" >}}#drift-reconcile-workersquota-reconcile) |
| `STRATA_USAGE_ROLLUP_AT` | `00:00` (UTC) | `--workers=usage-rollup` | [Quotas + billing]({{< ref "/best-practices/quotas-billing" >}}#usage-rollup-workersusage-rollup) |
| `STRATA_USAGE_ROLLUP_INTERVAL` | `24h` | `--workers=usage-rollup` | [Quotas + billing]({{< ref "/best-practices/quotas-billing" >}}#usage-rollup-workersusage-rollup) |
| `STRATA_RADOS_PUT_CONCURRENCY` | `32` (range `[1, 256]`) | gateway PUT path | [Parallel chunk PUT + GET]({{< ref "/architecture/benchmarks/parallel-chunks" >}}#tuning-knobs) |
| `STRATA_RADOS_GET_PREFETCH` | `4` (range `[1, 64]`) | gateway GET path | [Parallel chunk PUT + GET]({{< ref "/architecture/benchmarks/parallel-chunks" >}}#tuning-knobs) |
| `STRATA_CLUSTER_REGISTRY_INTERVAL` | `30s` (range `[5s, 5m]`) | rados backend in-process watcher | Cluster registry watcher poll cadence. Every gateway replica polls `meta.ListClusters` at this interval; added clusters lazy-dial on next traffic, removed clusters safely drain cached conn + ioctxes. Increments `strata_cluster_registry_changes_total{op=add|remove|update}` per reconciliation. |
