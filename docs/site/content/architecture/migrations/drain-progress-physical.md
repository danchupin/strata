---
title: 'Drain progress physical chunks'
weight: 40
description: 'Drain progress UI surfaces physical RADOS chunk count as primary; manifest count moves to collapsible detail. Additive `/drain-progress` fields, no schema migration.'
---

# Migrating to the physical-chunk drain progress

`GET /admin/v1/clusters/<id>/drain-progress` and the operator console
`<DrainProgressBar>` used to surface `chunks_on_cluster` — the
manifest-derived chunk count — as the sole headline metric. After the
`ralph/drain-progress-physical` cycle (US-001..US-003) the response
gains three additive fields and the UI renders a 3-state machine that
distinguishes manifest progress from physical pool state.

This page documents the wire shape change, the back-compat fallback for
backends without the new probe, and the operator-visible state machine.
There is no schema migration; no data migration; the change is purely
additive on the `/drain-progress` JSON shape.

## What changed

| | Pre-cycle | Post-cycle |
| - | --------- | ---------- |
| Primary headline | `chunks_on_cluster` (manifest count) | `physical_chunks_on_cluster ?? chunks_on_cluster` |
| New response fields | — | `physical_chunks_on_cluster *int64`, `physical_bytes_on_cluster *int64`, `gc_queue_pending int` |
| UI state machine | 2-state: Migrating + Ready to deregister (with `Not ready` chip via `not_ready_reasons` mid-grace) | 3-state: Migrating + Awaiting GC cleanup + Ready to deregister |
| Operator confusion | Manifest CAS rewrite drops `chunks_on_cluster` to 0 mid-grace → "0 Migrating" surfaces; operator thinks drain stalled | Awaiting GC chip explicitly explains: physical chunks linger until `STRATA_GC_GRACE` elapses + next gc tick |

The wire shape is **strictly additive** — every pre-cycle field is
preserved verbatim and old consumers ignore unknown fields. The
existing `not_ready_reasons` array and `deregister_ready` boolean are
untouched; the new `gc_queue_pending` integer is a magnitude on top of
the existing `gc_queue_pending` token in `not_ready_reasons`.

## Why physical-not-manifest is the operator metric

The drain pipeline has two distinct phases per chunk:

1. **Manifest rewrite** — rebalance worker reads
   `placement.PickClusterExcluding(<drained>)`, copies the chunk to
   the target cluster, CASes `objects.manifest` via
   `meta.Store.SetObjectStorage`. Manifest count `chunks_on_cluster`
   drops by 1.
2. **Physical delete** — the CAS-loser chunk on the drained cluster
   lands in the `gc_entries_v2` queue. The gc worker pops it after
   `STRATA_GC_GRACE` elapses (default 5 min) and on the next gc tick
   issues a RADOS `rados_remove` against the pool.

Between (1) and (2) the manifest count is 0 but the physical pool still
holds the chunk. The pre-cycle UI showed "0 Migrating" during this
window — operators read it as "drain didn't start" or "drain stalled"
and the `deregister_ready=false` chip felt arbitrary. The
`ralph/drain-cleanup` cycle had already added `not_ready_reasons` to
explain why dereg is gated, but the headline-count discrepancy
remained the dominant operator-perceived confusion.

Post-cycle the headline reads from the physical pool whenever the
backend can probe it; the manifest count moves to the collapsible
detail row.

## Response shape — additive fields

Three new fields:

```json
{
  "cluster_id": "cephb",
  "state": "evacuating",
  "mode": "evacuate",

  // pre-cycle fields, preserved verbatim:
  "chunks_on_cluster": 0,
  "bytes_on_cluster": 0,
  "not_ready_reasons": ["gc_queue_pending"],
  "deregister_ready": false,

  // new in ralph/drain-progress-physical:
  "physical_chunks_on_cluster": 300,
  "physical_bytes_on_cluster": 1258291200,
  "gc_queue_pending": 300
}
```

| Field | Semantics | Source | Null when |
| ----- | --------- | ------ | --------- |
| `physical_chunks_on_cluster *int64` | RADOS object count summed across `(pool, ns)` tuples filtered by cluster id | `data.ClusterObjectCountProbe.ClusterObjectCount` — RADOS loops `b.classes`, opens an `ioctx` per tuple, calls `GetPoolStats().Num_objects`. Reuses the existing `DataHealth` ioctx path. | Backend does not implement `ClusterObjectCountProbe` (memory, S3 pass-through) **OR** the probe errored and the gateway-side `ClusterStatsCache` is cold. |
| `physical_bytes_on_cluster *int64` | RADOS used bytes via the same probe path | Existing `data.ClusterStatsProbe.ClusterStats(usedBytes)` — already wired pre-cycle but never surfaced on `/drain-progress`. | Same as above. |
| `gc_queue_pending int` | Count of orphan chunks on the cluster awaiting physical delete | `meta.Store.ListChunkDeletionsByCluster(cluster)` length — denormalized on Cassandra via `gc_entries_by_cluster` (`ralph/drain-followup` US-005). | Never null — explicit `0` means queue clear. |

## `chunks_on_cluster` semantic shift (on null-physical backends)

When `physical_chunks_on_cluster` is null, the UI falls back to
`chunks_on_cluster` as the primary headline. The pre-cycle
operator-facing semantic is preserved on backends that cannot probe
physical state (memory + S3 pass-through). Operators on RADOS gateways
shipping this version see the physical-first behaviour; operators on
S3-backed gateways or test harnesses see the manifest-first fallback.

Both code paths render the same green "Ready to deregister" chip when
`deregister_ready=true` — the safety gate is unchanged from
`ralph/drain-cleanup` US-006 and never depends on the physical probe.

## Backend capability matrix

| Backend | `ClusterStatsProbe` (bytes) | `ClusterObjectCountProbe` (chunks) | UI primary |
| ------- | --------------------------- | ---------------------------------- | ---------- |
| `internal/data/rados` (ceph build tag) | Yes — `MonCommand("df")` payload + cached | Yes — `GetPoolStats().Num_objects` summed per cluster | `physical_chunks_on_cluster` |
| `internal/data/s3` (S3 pass-through) | No | No | `chunks_on_cluster` (manifest) + italic tooltip |
| `internal/data/memory` (tests / smoke) | No | No | `chunks_on_cluster` (manifest) + italic tooltip |

Adding a new backend (e.g. a future Azure/Blob backend) is opt-in: do
not implement `ClusterObjectCountProbe` and the UI falls through to the
manifest-only path with no code change in the admin handler.

## Operator workflow — what's new

`<DrainProgressBar>` renders one of three chips during `evacuating`:

```
state=evacuating, mode=evacuate

  ┌──────────────────────────────────────────────────────────────┐
  │  Migrating: N chunks remaining                                │  ← primary>0 && chunks>0
  │  ETA: ~3m  (from rebalance throughput)                        │
  │  ▾ Detail                                                     │
  └──────────────────────────────────────────────────────────────┘

  ┌──────────────────────────────────────────────────────────────┐
  │  Awaiting GC cleanup: N chunks awaiting physical delete       │  ← primary>0 && chunks==0
  │  ⓘ Physical delete completes after STRATA_GC_GRACE elapses    │
  │     (~5m default) plus the next gc worker tick.               │
  │  ▾ Detail                                                     │
  └──────────────────────────────────────────────────────────────┘

  ┌──────────────────────────────────────────────────────────────┐
  │  ✓ Ready to deregister                                         │  ← physical==0 && chunks==0
  │  ⓘ Edit STRATA_RADOS_CLUSTERS env to remove this cluster,     │     && gc==0 && dereg_ready
  │     then rolling restart.                                     │
  └──────────────────────────────────────────────────────────────┘
```

The collapsible `Detail` row shows `Manifest chunks: X`,
`GC queue: Y`, `Physical bytes: Z B`. Hidden by default; expand for
audit / debugging.

## Caching + per-poll cost

The gateway-side `ClusterStatsCache` (in
`internal/data/placement/clusterstats_cache.go`) memoises the
`(bytes, objects)` tuple per cluster id for 10 seconds. Every
`/drain-progress` call goes through `Get` first; cache hit → no RADOS
call. A miss issues one `ClusterStats` + one `ClusterObjectCount` and
caches the merged result.

Cache is per-replica; there is no shared cross-gateway cache. UI
polling at 5 s amortises to roughly one RADOS probe per cluster per
10 s regardless of how many operators have the console open. No admin
endpoint or event invalidates the cache — TTL only. Drain / undrain
do not invalidate it because the underlying `ceph df` numbers do not
change at those events; the next 10 s tick refreshes naturally.

## New observability

Counter `strata_drain_progress_probe_errors_total{cluster, probe}`
where `probe ∈ {stats, object_count}`. Increments once per probe
error on the `/drain-progress` builder path. The response still
succeeds with `null` physical fields (or stale cache values when
available). Suggested alert: `rate(...[5m]) > 0` for more than 15
minutes — chronic probe failure means operators are seeing the
back-compat fallback on a backend that should support the probe.

## Smoke validation

`scripts/smoke-drain-progress-ui.sh` (`make smoke-drain-progress-ui`)
drives all three states end-to-end against the bare
`docker compose up -d` lab. Smoke-only env:

| Env | Smoke value | Prod default | Reason |
| --- | ----------- | ------------ | ------ |
| `STRATA_REBALANCE_RATE_MB_S` | `1` | `100` | Widen Migrating phase past one 3-second poll interval so the smoke can actually sample it. |
| `STRATA_GC_GRACE` | `60s` | `5m` | Shorten the Awaiting GC phase into the script's 5-min total budget. |

**Do not run prod gateways with these throttled values.** Operating at
`1 MB/s + 60s` stalls migration on real workloads and risks
deleting chunks before in-flight references settle. The smoke
harness restores prod defaults on exit (recreates the strata
container without the overrides).

## Related cycles + references

- `internal/data/backend.go` — new `ClusterObjectCountProbe` interface
  defined next to existing `ClusterStatsProbe`. Optional capability
  type-asserted by `internal/adminapi/clusters_drain_progress.go`.
- `internal/data/rados/health.go` — `Backend.ClusterObjectCount`
  implementation (build-tag `ceph`).
- `internal/data/placement/clusterstats_cache.go` — 10 s TTL cache,
  per-cluster, sync.RWMutex.
- `internal/adminapi/clusters_drain_progress.go` — builder gained the
  three new fields + cache-first probe path.
- `web/src/components/storage/DrainProgressBar.tsx` — 3-state machine
  + collapsible detail + null-physical fallback.
- ROADMAP — closed P3 entry "Drain progress UI shows manifest counts
  instead of physical chunks"; parked P3 follow-up "Precise
  drain-progress ETA from gateway GC tunables".
- Best-practices runbook — see
  [Placement & rebalance → Drain progress states]({{< ref
  "/best-practices/placement-rebalance#drain-progress-states--physical-vs-manifest-us-001us-003-drain-progress-physical"
  >}}) for the full operator narrative.
