---
title: 'Multi-cluster routing'
weight: 20
description: 'Per-bucket placement, weighted default routing, storage classes.'
---

# Multi-cluster routing

A single Strata gateway can fan writes across multiple data clusters. The
clusters can be Ceph RADOS pools, S3-compatible buckets in different regions
or providers, or a mix. The routing decision happens on every `PutObject`
and works the same for chunked RADOS writes and S3-over-S3 pass-through.

## The two layers

Routing is decided by two independent inputs, evaluated in order:

1. **Bucket placement policy.** A bucket can carry an explicit
   `Placement` map — `{cluster-a: 70, cluster-b: 30}` — set via
   `PUT /admin/v1/buckets/{name}/placement`. When present, this policy wins.
   The gateway weighted-picks among the live clusters in the policy and
   never routes the bucket elsewhere.
2. **Cluster weights.** When the bucket has no explicit policy, the gateway
   synthesizes a default policy from per-cluster weights set via
   `PUT /admin/v1/clusters/{id}/weight {weight: N}`. Weights are integers
   in `[0, 100]`; clusters with weight `0` accept reads and explicit-policy
   writes but receive no new default-routed writes.

The two layers do not combine. If a bucket has an explicit placement, the
cluster-weight wheel is ignored for that bucket.

## Cluster states

Each data cluster has a state in the metadata store:

| State | Picker behavior |
|---|---|
| `pending` | Excluded from default routing. Explicit policy still routes there. Operator activates via the admin API. |
| `live` | Included in the default-routing weight wheel proportional to its weight. |
| `draining_readonly` | Excluded from the picker. PUTs that would have landed here return `503 DrainRefused`. Reads, deletes, in-flight multipart sessions continue. |
| `evacuating` | Same picker exclusion as `draining_readonly`, plus the rebalance worker actively moves chunks off. |
| `removed` | Tombstone. Excluded from every code path. |

Bucket policy entries that name a draining cluster are filtered out at PUT
time. A bucket pinned to a single cluster that is now draining will get
`503 DrainRefused` instead of silently falling back to another cluster —
this is intentional for compliance / data-sovereignty pins.

## Strict vs weighted placement

Each bucket has a `PlacementMode`:

- **`weighted`** (default) — if every cluster in the bucket's placement
  policy is draining, the gateway falls back to the cluster-weight wheel so
  writes keep working.
- **`strict`** — if every cluster in the policy is draining, PUTs refuse.
  No fallback. Use this for buckets whose location is a compliance or
  replication-design requirement.

`PlacementMode` is set alongside the placement map on
`PUT /admin/v1/buckets/{name}/placement`.

## Storage classes

Storage classes (`STANDARD`, `INTELLIGENT_TIERING`, custom names) bind to a
specific cluster via the `STRATA_S3_CLASSES` environment variable on S3
backends, or to a RADOS pool on RADOS backends. Lifecycle transitions move
objects between classes; replication can target a different class on the
destination bucket.

## Where to read next

- [Drain & rebalance]({{< relref "/concepts/drain-rebalance" >}}) — the cluster lifecycle from `live` through `removed`.
- [Placement + rebalance best practices]({{< relref "/best-practices/placement-rebalance" >}}) — operator runbook.
- [Architecture: multi-cluster routing]({{< relref "/architecture/" >}}) — implementation details.
