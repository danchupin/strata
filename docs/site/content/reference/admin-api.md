---
title: 'Admin API surface'
weight: 20
---

<!--
Source of truth: `internal/adminapi/openapi.yaml`. This page is a derived flat
index — rebuild it when paths are added/removed from the OpenAPI document.
Audit-verb column mirrors the `s3api.SetAuditOverride(ctx, action, ...)` stamp
in the handler. GET/HEAD/OPTIONS are skipped by the audit middleware regardless
of any defensive stamp, so read-only rows show "—".
-->

# Admin API surface

_This page is the operator index. Authoritative contract lives in
[`internal/adminapi/openapi.yaml`](https://github.com/danchupin/strata/blob/main/internal/adminapi/openapi.yaml);
rendered viewer at `/reference/admin-api-viewer/` (authored in US-005)._

All paths are relative to the `/admin/v1` prefix on the gateway port. Auth: SigV4
or the `strata_session` cookie issued by `POST /auth/login`. The `Full schema`
column links to the interactive viewer with a Redoc operation anchor
(`#operation/<operationId>`) — the anchor is auto-generated from the
`operationId` field on each `paths.*.<method>` entry.

The `Audit verb` column reflects the
`s3api.SetAuditOverride(ctx, action, ...)` stamp at the handler. GET/HEAD/OPTIONS
requests skip the [`AuditMiddleware`]({{< ref "/architecture/auth" >}}) — those
rows show `—`. Write operations (POST/PUT/DELETE) always emit one audit row.

## Session

| Method | Path | Audit verb | Summary | Full schema |
|---|---|---|---|---|
| `POST` | `/auth/login` | — | Issue a 24h session cookie from IAM credentials. | [authLogin](/reference/admin-api-viewer/#operation/authLogin) |
| `POST` | `/auth/logout` | — | Clear the session cookie. | [authLogout](/reference/admin-api-viewer/#operation/authLogout) |
| `GET` | `/auth/whoami` | — | Probe current session; 401 when expired. | [authWhoami](/reference/admin-api-viewer/#operation/authWhoami) |

## Cluster status

| Method | Path | Audit verb | Summary | Full schema |
|---|---|---|---|---|
| `GET` | `/cluster/status` | — | Cluster-wide status hero (version, uptime, node counts, meta + data backend). | [getClusterStatus](/reference/admin-api-viewer/#operation/getClusterStatus) |
| `GET` | `/cluster/nodes` | — | Heartbeat table — one row per replica with workers + leader-for chips. | [getClusterNodes](/reference/admin-api-viewer/#operation/getClusterNodes) |

## Cluster lifecycle

| Method | Path | Audit verb | Summary | Full schema |
|---|---|---|---|---|
| `GET` | `/clusters` | — | List data-backend clusters with persisted drain / weight state (rows missing → `live`). | [listClusters](/reference/admin-api-viewer/#operation/listClusters) |
| `POST` | `/clusters/{id}/drain` | `admin:DrainCluster` | Flip cluster_state to `draining_readonly` or `evacuating`. Body `{mode}` required. | [drainCluster](/reference/admin-api-viewer/#operation/drainCluster) |
| `POST` | `/clusters/{id}/undrain` | `admin:UndrainCluster` | Clear draining state — drops the row. Works from `draining_readonly` or `evacuating`. | [undrainCluster](/reference/admin-api-viewer/#operation/undrainCluster) |
| `POST` | `/clusters/{id}/activate` | `admin:ActivateCluster` | Promote a `pending` cluster to `live` with the supplied default-routing weight. | [activateCluster](/reference/admin-api-viewer/#operation/activateCluster) |
| `PUT` | `/clusters/{id}/weight` | `admin:UpdateClusterWeight` | Adjust default-routing weight `[0, 100]` on a live cluster. Cache invalidates synchronously. | [updateClusterWeight](/reference/admin-api-viewer/#operation/updateClusterWeight) |
| `GET` | `/clusters/{id}/bucket-references` | — | Buckets whose `Placement[<id>] > 0`, joined with `bucket_stats` (paginated). | [getClusterBucketReferences](/reference/admin-api-viewer/#operation/getClusterBucketReferences) |

## Drain & rebalance

| Method | Path | Audit verb | Summary | Full schema |
|---|---|---|---|---|
| `GET` | `/clusters/{id}/rebalance-progress` | — | Per-destination cluster rebalance counters + 1h sparkline. Degrades when Prom is unset. | [getClusterRebalanceProgress](/reference/admin-api-viewer/#operation/getClusterRebalanceProgress) |
| `GET` | `/clusters/{id}/drain-progress` | — | In-process drain-progress cache (chunks remaining, ETA, `deregister_ready`). 503 when no rebalance worker on this replica. | [getClusterDrainProgress](/reference/admin-api-viewer/#operation/getClusterDrainProgress) |
| `GET` | `/clusters/{id}/drain-impact` | — | Synchronous bucket scan — `migratable / stuck_single_policy / stuck_no_policy` categorisation. 5-min cache. | [getClusterDrainImpact](/reference/admin-api-viewer/#operation/getClusterDrainImpact) |
| `GET` | `/gc-config` | — | Resolved `STRATA_GC_*` tunables snapshot (grace, interval, batch_size, concurrency, shards). | [getGCConfig](/reference/admin-api-viewer/#operation/getGCConfig) |
| `GET` | `/rebalance-config` | — | Resolved `STRATA_REBALANCE_*` tunables + live replica count. | [getRebalanceConfig](/reference/admin-api-viewer/#operation/getRebalanceConfig) |
| `GET` | `/rebalance-bandwidth` | — | Cluster-wide 1m rebalance bandwidth + chunks/sec roll-up. | [getRebalanceBandwidth](/reference/admin-api-viewer/#operation/getRebalanceBandwidth) |

## Placement

| Method | Path | Audit verb | Summary | Full schema |
|---|---|---|---|---|
| `GET` | `/buckets/{bucket}/placement` | — | Get per-bucket placement policy + mode (`weighted` / `strict`). | [getBucketPlacement](/reference/admin-api-viewer/#operation/getBucketPlacement) |
| `PUT` | `/buckets/{bucket}/placement` | `admin:PutBucketPlacement` (or `admin:UpdateBucketPlacementMode` when body carries `mode`) | Replace placement policy. Weights `[0, 100]`, sum > 0. Optional `mode` flips weighted ↔ strict. | [putBucketPlacement](/reference/admin-api-viewer/#operation/putBucketPlacement) |
| `DELETE` | `/buckets/{bucket}/placement` | `admin:DeleteBucketPlacement` | Idempotent — 204 even when no policy was configured. | [deleteBucketPlacement](/reference/admin-api-viewer/#operation/deleteBucketPlacement) |

## Buckets

| Method | Path | Audit verb | Summary | Full schema |
|---|---|---|---|---|
| `GET` | `/buckets` | — | List buckets (search, sort, paginate). `total` is unpaginated row count. | [listBuckets](/reference/admin-api-viewer/#operation/listBuckets) |
| `GET` | `/buckets/top` | — | Top buckets by size or 24h request count. | [getBucketsTop](/reference/admin-api-viewer/#operation/getBucketsTop) |
| `GET` | `/buckets/{bucket}` | — | Bucket detail — metadata + size + object count (bounded ListObjects walk). | [getBucket](/reference/admin-api-viewer/#operation/getBucket) |
| `GET` | `/buckets/{bucket}/distribution` | — | Per-shard byte / object distribution + skew banner trigger. | [getBucketDistribution](/reference/admin-api-viewer/#operation/getBucketDistribution) |
| `GET` | `/buckets/{bucket}/replication-lag` | — | Per-bucket `replication_queue_age_seconds` time-series (recharts shape). | [getBucketReplicationLag](/reference/admin-api-viewer/#operation/getBucketReplicationLag) |
| `GET` | `/buckets/{bucket}/objects` | — | Read-only object browser. `marker`-paginated, `delimiter` default `/`. | [listObjects](/reference/admin-api-viewer/#operation/listObjects) |
| `GET` | `/buckets/{bucket}/usage` | — | Per-bucket daily usage history from `usage_aggregates` (default 30-day window). | [getBucketUsage](/reference/admin-api-viewer/#operation/getBucketUsage) |
| `GET` | `/buckets/{bucket}/quota` | — | Get bucket quota. | [getBucketQuota](/reference/admin-api-viewer/#operation/getBucketQuota) |
| `PUT` | `/buckets/{bucket}/quota` | `admin:PutBucketQuota` | Replace bucket quota. Zero on any field means unlimited. | [putBucketQuota](/reference/admin-api-viewer/#operation/putBucketQuota) |
| `DELETE` | `/buckets/{bucket}/quota` | `admin:DeleteBucketQuota` | Idempotent quota removal. | [deleteBucketQuota](/reference/admin-api-viewer/#operation/deleteBucketQuota) |

## IAM

| Method | Path | Audit verb | Summary | Full schema |
|---|---|---|---|---|
| `GET` | `/iam/users/{userName}/quota` | — | Get user quota. | [getUserQuota](/reference/admin-api-viewer/#operation/getUserQuota) |
| `PUT` | `/iam/users/{userName}/quota` | `admin:PutUserQuota` | Replace user quota. | [putUserQuota](/reference/admin-api-viewer/#operation/putUserQuota) |
| `DELETE` | `/iam/users/{userName}/quota` | `admin:DeleteUserQuota` | Idempotent quota removal. | [deleteUserQuota](/reference/admin-api-viewer/#operation/deleteUserQuota) |
| `GET` | `/iam/users/{userName}/usage` | — | Per-user daily usage rolled across owned buckets + cross-row totals. | [getUserUsage](/reference/admin-api-viewer/#operation/getUserUsage) |

## Consumers & metrics

| Method | Path | Audit verb | Summary | Full schema |
|---|---|---|---|---|
| `GET` | `/consumers/top` | — | Top consumers by 24h requests or bytes. | [getConsumersTop](/reference/admin-api-viewer/#operation/getConsumersTop) |
| `GET` | `/metrics/timeseries` | — | Cluster-wide Prometheus-backed time-series (request rate, latency p50/p95/p99, error rate, bytes in/out). | [getMetricsTimeseries](/reference/admin-api-viewer/#operation/getMetricsTimeseries) |

## Diagnostics

| Method | Path | Audit verb | Summary | Full schema |
|---|---|---|---|---|
| `GET` | `/diagnostics/traces` | — | List recently-captured traces from the in-process OTel ring buffer (LRU; filter by method / status / path / duration). 503 when ring buffer is disabled. | [getDiagnosticsTraces](/reference/admin-api-viewer/#operation/getDiagnosticsTraces) |

## Scope note

`internal/adminapi/server.go` registers additional `/admin/v1/*` routes that are
not yet documented in `openapi.yaml` — bucket lifecycle / CORS / policy /
inventory / logging / ACL / object actions / multipart admin / IAM users + access
keys + managed policies / audit log / diagnostics extras (hot buckets, hot
shards, slow queries, node detail, trace by request id) / storage probes /
settings. Stamping those endpoints into the YAML is tracked as a separate doc
follow-up and not in scope for this reference page.
