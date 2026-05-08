---
title: 'Storage status'
weight: 30
description: 'Operator guide to the meta + data backend health surfacing in the Strata Console.'
---

# Storage status — operator guide

The Storage page (`/console/storage`) and the Cluster Overview Storage hero
card surface the live health of the meta + data backends and the
per-storage-class object distribution. This page covers the env vars the UI
reads through, what each warning means, and how to interpret the
RADOS / TiKV / Cassandra-specific signals.

The page is fed by three small admin endpoints:

| Endpoint | Returns | Polled by |
|---|---|---|
| `GET /admin/v1/storage/meta` | `MetaHealthReport` (Cassandra peers / TiKV PD stores / memory) | `/storage` Meta tab @ 30 s |
| `GET /admin/v1/storage/data` | `DataHealthReport` (RADOS pool stats / S3 reachability / memory) | `/storage` Data tab @ 30 s |
| `GET /admin/v1/storage/classes` | `{classes:[…], pools_by_class:{…}}` | `/storage` Data tab @ 30 s, Cluster Overview hero @ 60 s |
| `GET /admin/v1/storage/health` | aggregate `{ok, warnings, source}` | `<StorageDegradedBanner>` @ 30 s on every authed page |

Schema details live in `internal/adminapi/openapi.yaml`.

## Backend selection

| Variable | Default | Purpose |
|---|---|---|
| `STRATA_META_BACKEND` | `memory` | One of `memory`, `cassandra`, `tikv`. Drives which `meta.HealthProbe` the Meta tab reports. |
| `STRATA_DATA_BACKEND` | `memory` | One of `memory`, `rados`, `s3`. Drives which `data.HealthProbe` the Data tab reports. |

Memory backends always report a single in-process row — there is no network
topology to surface. The page renders an explainer card instead of an empty
table.

## Cassandra (Meta tab)

The Cassandra probe issues two `LOCAL_ONE` queries (CQL has no `UNION ALL`):

```cql
SELECT peer, data_center, rack, release_version, schema_version
  FROM system.peers;
SELECT broadcast_address, data_center, rack, release_version, schema_version
  FROM system.local;
```

Results are merged Go-side into one `[]NodeStatus`. Replication factor reads
from `system_schema.keyspaces`. Results are cached in-process for 10 s so
adminapi pollers absorb bursts without re-querying.

| Warning | Meaning |
|---|---|
| `schema versions disagree: …` | Two or more nodes report different `system.schema_version`. Ride it out during a rolling upgrade; investigate if persistent. |
| `keyspace not found: <ks>` | The configured `STRATA_CASSANDRA_KEYSPACE` is missing — bootstrap likely never ran. |
| `replication factor unknown` | `system_schema.keyspaces` returned no rows for the keyspace; same root cause. |

Relevant env vars (full list in
[ScyllaDB backend]({{< ref "/architecture/backends/scylla" >}})): `STRATA_CASSANDRA_HOSTS`,
`STRATA_CASSANDRA_KEYSPACE`, `STRATA_CASSANDRA_LOCAL_DC`,
`STRATA_CASSANDRA_SLOW_MS`.

## TiKV (Meta tab)

A thin bootstrap-only HTTP client lives at
`internal/meta/tikv/pdclient.go`. It tries each PD endpoint in order with a
2 s per-endpoint timeout and returns the first non-error response from
`GET /pd/api/v1/stores`. Cross-PD failover for the data path is owned by
`tikv/client-go`, so the probe deliberately does not refresh
`/pd/api/v1/members` — bootstrap-only.

| Warning | Meaning |
|---|---|
| `raft leader imbalance: store <id> has 0 leaders` | Some live TiKV store carries no Raft leaders while peers do. Usually transient during a rolling restart; persistent imbalance suggests a region-scheduler stall. |
| `pd endpoints unreachable: …` | Every configured PD endpoint timed out / returned non-200. The data path is likely down too. |

Relevant env vars (full list in
[TiKV backend]({{< ref "/architecture/backends/tikv" >}})): `STRATA_TIKV_PD_ENDPOINTS`.

## RADOS (Data tab)

The RADOS probe iterates the configured `[rados] classes` map (from
`STRATA_CONFIG`), groups unique `(cluster, pool, ns)` tuples, and reuses the
same `*goceph.IOContext` the gateway data path opens via
`internal/data/rados/backend.go::ioctx`. Per-pool stats come from
`(*IOContext).GetPoolStats()` (`Num_kb*1024` → `BytesUsed`, `Num_objects` →
`ObjectCount`). Cluster-wide health uses
`(*Conn).MonCommand({"prefix":"status","format":"json"})` per unique cluster.

`HEALTH_OK` from `ceph status` suppresses warnings entirely. Anything else
surfaces the headline status string plus up to 5 sorted check summaries:

| Warning | Meaning |
|---|---|
| `HEALTH_WARN: Reduced data availability: 2 pgs inactive` | Common during recovery; see [Ceph health checks](https://docs.ceph.com/en/latest/rados/operations/health-checks/) for the full table. |
| `HEALTH_ERR: 1 stale pgs` | Operator action required — typically OSD outage. |
| `pool stats failed for <pool>: …` | Per-pool stat call errored; the row degrades to `state=error` and the report continues for sibling pools. |

Per-pool degraded-object counts (parsed from `ceph osd df` JSON) are a
deliberate future P3 — the current cycle ships headline-only.

## S3-over-S3 (Data tab)

The S3 backend probe runs `HeadBucket` on the configured backend bucket and
reports `state=reachable` or `state=error`. `BytesUsed` and `ObjectCount` are
left at `0` because the upstream S3 API does not expose them in O(1); the
per-class breakdown (`/admin/v1/storage/classes`) covers the Strata-side
dimensions.

## Per-storage-class breakdown

Per-class `bytes` and `objects` are produced by the `bucketstats` sampler
(`internal/bucketstats/sampler.go`). The sampler walks every bucket on
`Interval` (default 1 h, override via `STRATA_BUCKETSTATS_INTERVAL`) and
publishes the cluster-wide totals to a shared `*Snapshot` consumed by
`/admin/v1/storage/classes`. Per-(bucket, class) gauges
(`strata_storage_class_bytes`, `strata_storage_class_objects`) are also
emitted, capped at top-N buckets (`STRATA_BUCKETSTATS_TOPN`, default 100).

`pools_by_class` in the response carries the static class → backend pool
mapping configured under `[rados] classes`. It is empty for `memory` and
`s3` backends; the UI hides the `→ pool` chip suffix in those cases.

| Variable | Default | Purpose |
|---|---|---|
| `STRATA_BUCKETSTATS_INTERVAL` | `1h` | Sampler tick cadence (Go duration). E2E sets `500ms`; production defaults are fine for the storage page. |
| `STRATA_BUCKETSTATS_TOPN` | `100` | Per-(bucket, class) gauge cardinality cap. The cluster-wide totals are unaffected. |

A fresh gateway emits one initial sample pass during `Sampler.Run` so the
hero card and `/storage` page populate without waiting an hour for the first
tick.

## Aggregate health + degraded banner

`/admin/v1/storage/health` folds the meta + data probes into a single
`{ok, warnings, source}` payload. `ok=false` if either probe returns
warnings, or if any node / pool is in a non-OK state. The banner component
(`web/src/components/StorageDegradedBanner.tsx`) polls this endpoint every
30 s on every authed page and renders above the AppShell when `ok=false`.
Dismissal is keyed on a stable signature `<source>|<sorted-warnings>` so a
fresh distinct degraded condition re-alerts even if the previous one was
dismissed for the session.

`STRATA_STORAGE_HEALTH_OVERRIDE` is the e2e knob: set to a JSON-shaped
override and the handler returns it verbatim. Invalid JSON is logged at
WARN and falls through to the live probe so a misconfigured env-var does
not blank the banner permanently. Don't ship this in production.
