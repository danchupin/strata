---
title: 'Placement + rebalance'
weight: 75
description: 'Per-bucket placement policy (`meta.Bucket.Placement`) + cross-cluster rebalance worker — operator workflow (register → set Placement → drain → rebalance → deregister), STRATA_REBALANCE_* env tuning table, drain sentinel cache, safety rails, troubleshooting CAS-conflict storms and target-full refusals.'
---

# Placement + rebalance

Once you run more than one RADOS / S3 cluster behind a single Strata
deployment, you also need to control **which** cluster a bucket's chunks
land on and how to migrate old chunks when a new cluster joins or an
old cluster is being retired. Strata ships both pieces:

- A per-bucket **placement policy** (`meta.Bucket.Placement` — a
  `{cluster: weight}` map) that the chunk PUT path consults via a
  stable hash-mod router.
- A leader-elected **rebalance worker** (`strata server --workers=rebalance`)
  that walks every bucket with a non-nil policy, compares the actual
  per-cluster chunk distribution to the policy's target, and copies
  chunks A → B until the two match.

A bucket without a policy (`Placement == nil`) behaves exactly as
before — chunks land on the storage class's default cluster. No
migration, no schema bump, no behavior change. The policy + worker are
both opt-in.

For the conceptual S3 multi-cluster overview see
[S3 multi-cluster routing]({{< ref "/best-practices/s3-multi-cluster" >}}).
This page is the operator runbook.

## Operator workflow

The end-to-end "add a new cluster and drain an old one" workflow is
four steps:

1. **Register the new cluster** in `STRATA_RADOS_CLUSTERS` /
   `STRATA_S3_CLUSTERS` (env-driven — rolling-restart each replica).
2. **Set placement** on the buckets that should start using the new
   cluster:
   ```bash
   curl -X PUT http://strata-admin/admin/v1/buckets/<name>/placement \
        -H 'content-type: application/json' \
        -d '{"placement":{"oldc":1,"newc":3}}'
   ```
   New PUTs split per the weights immediately. Existing chunks stay on
   `oldc` until the rebalance worker moves them.
3. **Drain the cluster you want to retire** (when you also want the
   leftover chunks moved off):
   ```bash
   curl -X POST http://strata-admin/admin/v1/clusters/oldc/drain
   ```
   This marks `oldc` as `draining` in the `cluster_state` table.
   `placement.PickClusterExcluding` will skip `oldc` on the PUT hot
   path (drain is treated as weight=0 even when the policy still lists
   it). The rebalance worker treats `draining` as the **source** of
   moves but refuses to ever pick a `draining` cluster as a target. The
   admin handler invalidates the drain cache so the flip takes effect
   on the next PUT without waiting out the 30 s TTL.
4. **Let the rebalance worker converge.** It runs on the
   `STRATA_REBALANCE_INTERVAL` cadence (default `1h`) and is leader-
   elected on the `rebalance-leader` lease — exactly one replica
   moves chunks at any given moment. Watch
   `strata_rebalance_planned_moves_total` go to zero and the
   `_chunks_moved_total{from=oldc}` counter plateau, then deregister
   the cluster by dropping it from `STRATA_RADOS_CLUSTERS` /
   `STRATA_S3_CLUSTERS` and rolling-restarting.

Undrain (if you change your mind mid-workflow):

```bash
curl -X POST http://strata-admin/admin/v1/clusters/oldc/undrain
```

This deletes the `cluster_state` row (absence == live) and invalidates
the drain cache.

## Placement policy shape

The policy is a `{cluster: weight}` JSON object:

```json
{"placement": {"hot": 1, "warm": 3, "cold": 0}}
```

Validation runs in `meta.ValidatePlacement`:

| Rule                                                        | Sentinel                  |
| ----------------------------------------------------------- | ------------------------- |
| `len(placement) > 0`                                        | `ErrInvalidPlacement`     |
| `weight ∈ [0, 100]` for every entry                         | `ErrInvalidPlacement`     |
| `sum(weights) > 0` (at least one non-zero weight)           | `ErrInvalidPlacement`     |
| Cluster id resolves in `STRATA_RADOS_CLUSTERS` / `STRATA_S3_CLUSTERS` | `ErrUnknownCluster` (admin layer; meta stays backend-agnostic) |

A zero-weight entry is legal — it pins the cluster in the policy
without letting new chunks land there. Useful for "decommissioning
soon, drained but not yet deregistered".

## Routing — stable hash-mod

`internal/data/placement.PickCluster` is the chunk PUT router:

1. Empty / nil policy → return `""` so the caller falls back to the
   class's `$defaultCluster`.
2. Sort cluster ids lex (`sort.Strings(slices.Collect(maps.Keys(policy)))`)
   so the walk is deterministic regardless of Go's random map order.
3. Compute `fnv32a("<bucketID>/<key>/<chunkIdx>") % sum(weights)` and
   walk the weight wheel.

Determinism guarantee: the same `(bucketID, key, chunkIdx)` always
maps to the same cluster across retries, gateway restarts, and policy
edits that don't change the weight wheel. Adding a fourth cluster
to a `{a:1, b:1, c:1}` policy moves only ~1/4 of chunks, not all of
them — the wheel grows but the spokes are stable.

The drain-aware variant is `PickClusterExcluding`:

```go
PickClusterExcluding(bucketID, key, chunkIdx, policy, draining map[string]bool) string
```

Entries in `draining` are treated as weight=0; if every cluster in the
policy is draining, the function returns `""` and the caller falls
back to `$defaultCluster`.

## STRATA_REBALANCE_* env knobs

| Variable                       | Default | Range          | Purpose                                                                                              |
| ------------------------------ | ------- | -------------- | ---------------------------------------------------------------------------------------------------- |
| `STRATA_REBALANCE_INTERVAL`    | `1h`    | `[1m, 24h]`    | Tick cadence. Out-of-range clamped + WARN-logged at worker build time.                               |
| `STRATA_REBALANCE_RATE_MB_S`   | `100`   | `[1, 10000]`   | Bandwidth ceiling. Both read + write debit the same token bucket — `chunkSize × 2` tokens per move. |
| `STRATA_REBALANCE_INFLIGHT`    | `4`     | `[1, 64]`      | Per-`Move(plan)` errgroup bound. Shared between copy + CAS phases.                                   |

All three are env-only, read at worker `Build` time — no flags. Restart
the replica that owns the `rebalance-leader` lease to pick up new
values (or rolling-restart, the lease re-acquires on the next replica).

## Safety rails

The rebalance worker won't dispatch a move when:

- **Target is `draining`.** The `PickClusterExcluding` policy filter
  rejects this at plan time; the post-filter in
  `Worker.applySafetyRails` is defense-in-depth for the race between
  scan emission and a drain flip. Bumps
  `strata_rebalance_refused_total{reason="target_draining",target}`.
- **Target is > 90 % full** (RADOS only). The worker type-asserts
  `data.ClusterStatsProbe` against the data backend (RADOS implements;
  S3 + memory don't). Per-tick fill probe is cached so a fan-out of N
  moves into K targets costs K probes per iteration, not N. S3-only
  deployments treat `data.ErrClusterStatsNotSupported` as "OK to
  proceed" with one WARN per iteration. Bumps
  `strata_rebalance_refused_total{reason="target_full",target}`.

Both rails are post-filter — the plan is built first, refused moves
get logged + metricked + skipped, and the rest of the plan still
executes.

## Movers

The worker dispatches the plan through a `MoverChain` that partitions
by target-cluster ownership:

| Backend  | Mover                              | Same-endpoint shortcut        | Cross-endpoint fallback                       |
| -------- | ---------------------------------- | ----------------------------- | --------------------------------------------- |
| RADOS    | `internal/rebalance/rados_mover.go` — `Read(srcIoctx, oid) → Write(tgtIoctx, newOID)` (fresh OID avoids cross-pool name collisions) | n/a (one cluster per pool) | n/a |
| S3-over-S3 | `internal/rebalance/s3_mover.go` — server-side `awss3.CopyObject` when endpoint+region match | yes — no bytes through gateway | streaming `GetObject` → `manager.Uploader.PutObject` |

After every move the mover issues a **per-object manifest CAS** via
`meta.Store.SetObjectStorage(... expectedClass=currentClass)`. A pre-
CAS sanity check inside `buildUpdatedManifest` (RADOS) /
`buildUpdatedBackendManifest` (S3) verifies the live chunk locator
still matches the planned `SrcRef` — a concurrent client write that
rewrote the chunk between scan and Move is caught BEFORE the LWT so
the rebalance doesn't clobber a newer locator. If the live row
diverges, the new target chunks go to the GC queue and
`strata_rebalance_cas_conflicts_total{bucket}` bumps.

On CAS success the OLD chunks are enqueued via
`meta.Store.EnqueueChunkDeletion` and the existing `gc` worker
collects them per `STRATA_GC_GRACE`.

## Metric family

| Metric                                                      | Labels                  | Meaning                                                                |
| ----------------------------------------------------------- | ----------------------- | ---------------------------------------------------------------------- |
| `strata_rebalance_planned_moves_total`                      | `bucket`                | One increment per chunk whose current cluster ≠ `PickCluster` verdict. |
| `strata_rebalance_bytes_moved_total`                        | `from`, `to`            | Bytes copied on the target write — retried reads don't double-count.  |
| `strata_rebalance_chunks_moved_total`                       | `from`, `to`, `bucket`  | Chunks successfully copied (post target write).                        |
| `strata_rebalance_cas_conflicts_total`                      | `bucket`                | LWT lost the race — target chunks routed to GC, live manifest intact. |
| `strata_rebalance_refused_total`                            | `reason`, `target`      | `reason ∈ {target_full, target_draining}` — safety rail refusals.     |

`from` / `to` carry the cluster id from `STRATA_RADOS_CLUSTERS` /
`STRATA_S3_CLUSTERS`.

## Trace shapes

Iteration parent: `worker.rebalance.tick` with
`strata.component=worker` + `strata.worker=rebalance` +
`strata.iteration_id=<atomic.uint64>`. Sub-ops:

- `rebalance.scan_bucket` — one per bucket scanned per tick. Attrs:
  `strata.rebalance.bucket`, `bucket_id`, planned-moves count.
- `rebalance.move_chunk` — one per chunk move. Attrs:
  `strata.rebalance.{bucket,key,from,to,chunk_idx}`. Spans get
  `RecordError` + `SetStatus(Error)` on failure; the iteration
  parent's sticky-err accumulator flips it to Error so the tail-
  sampler exports the full iteration regardless of
  `STRATA_OTEL_SAMPLE_RATIO`.

Filter recipe — "what did the last rebalance tick move across all
clusters":

```
strata.component=worker
strata.worker=rebalance
```

## Troubleshooting

### `_planned_moves_total` stays high, `_chunks_moved_total` stays flat

The plan keeps being built but no moves complete. Likely causes:

- **Safety rails refusing.** Check
  `strata_rebalance_refused_total{reason}` — if `target_full` dominates,
  the target cluster crossed 90 % fill; either grow the cluster or
  re-route to a different cluster id via the policy. If `target_draining`
  dominates, an operator drained the cluster you were trying to fill —
  un-drain or change the policy.
- **Token bucket starved.** Raise `STRATA_REBALANCE_RATE_MB_S`. Both
  read + write debit the same bucket so the wall-clock throughput is
  `RATE_MB_S` MiB/s on the busier leg.
- **CAS conflict storm.** Watch
  `strata_rebalance_cas_conflicts_total{bucket}`. Steady conflicts on
  one bucket means concurrent client traffic keeps winning the LWT —
  this is correct behavior (client always wins) and the chunks will
  re-plan next tick. If conflicts grow unboundedly, you have hot keys
  being rewritten faster than the rebalance loop can converge; pause
  the rebalance via `STRATA_REBALANCE_INTERVAL=24h` until traffic
  settles.

### Target-full refusals

The 90 % fill ceiling is hard-coded in `internal/rebalance/Worker.FillCeiling`.
Operators who want a different threshold should grow the cluster
(easier) or open a tracking issue. S3-side has no fill probe — the
worker proceeds. If your S3 backend has a quota, monitor it externally.

### Drain cache TTL surprises

The drain sentinel is cached in-process for 30 s
(`DefaultDrainCacheTTL` in `internal/data/placement/draincache.go`).
The drain / undrain admin handlers `Invalidate()` the cache so the
flip takes effect on the next PUT — operators never wait the TTL.
Multi-replica deployments need to invalidate on every replica; the
admin handler runs locally so an external load balancer must hit each
replica's drain endpoint, or you can rely on the 30 s TTL for the
replicas you didn't hit. For zero-downtime drains the safer path is a
rolling drain — drain via one admin endpoint and wait 30 s before
expecting cluster-wide quiescence.

### Rebalance worker never picks up the lease

The lease is `rebalance-leader` on whatever `meta.Locker` you
configured (`cassandra` LWT lease or in-process memory locker for
dev). Check `strata server` logs for the `leader_for=rebalance`
heartbeat chip; if absent, the worker isn't running. Common cause:
`STRATA_WORKERS=...` doesn't include `rebalance`. Set
`STRATA_WORKERS=rebalance` (or include it in your comma-separated
list) on the replica you expect to lead.

## What's NOT supported

- **Per-version rebalance.** The scan walks only current versions via
  `meta.Store.ListObjects` — non-current versions keep their original
  cluster. The version-DESC clustering means walking every version
  would dominate the scan budget for almost no operator value. If you
  need to rebalance a tombstoned version, restore it first.
- **Cross-backend moves.** RADOS chunks can't be moved into an S3
  cluster and vice-versa — the mover chain partitions by target-
  cluster owner, and a RADOS source / S3 target pair has no owner.
  Use a class re-route + lifecycle transition instead.
- **Per-cluster pool overrides on RADOS.** Placement routes the
  cluster id; the pool + namespace come from the class spec. If you
  need a per-cluster pool override, register a second class on the
  target cluster (same pattern as the S3 backend's `bucketOnCluster`).

## Web UI

The operator console ships UI surfaces for every endpoint described above
so day-to-day placement / drain operations no longer need `curl`. All
four surfaces share the `clusters` TanStack Query key (10 s poll from
`<ClustersSubsection>`, 15 s from `<PlacementDrainBanner>`) so the
gateway is hit once per cache window regardless of how many components
read the topology.

| Surface | Where | What it does |
| ------- | ----- | ------------ |
| Clusters subsection | `/storage` → Data tab | One `<Card>` per registered cluster — id, state badge (live/draining), backend chip (rados/s3), aggregated used bytes (RADOS only). Per-card `Drain` opens a typed-confirmation modal (mistype keeps submit disabled); `Undrain` is one-click. Skipped when `backend=memory`. |
| Placement tab | `/buckets/<name>` → Placement | One row per registered cluster — `<input type="range">` 0–100 paired with a numeric `<Input>` two-way bound to the same row state. Save calls `PUT /admin/v1/buckets/<name>/placement`; Reset to default opens a confirmation Dialog and calls `DELETE …`. Draining clusters carry a `(draining)` chip but remain editable. |
| Drain banner | AppShell (every authed page) | Orange palette mirroring `<StorageDegradedBanner>`. Renders only when ≥1 cluster sits in `state=draining`. Dismiss writes `drain_banner_dismissed=<JSON.stringify(sortedDrainingIds)>` to `localStorage`; the banner returns when the draining set changes (new cluster entering draining → stamp differs). |
| Rebalance progress chip | Inside each cluster card | "`<N>` chunks moved · `<M>` refused" plus an inline 1h/1m sparkline of `rate(strata_rebalance_chunks_moved_total{to="<id>"}[5m])`. Served by `GET /admin/v1/clusters/<id>/rebalance-progress`, which `fmt.Sprintf`s the per-cluster PromQL and degrades to `metrics_available=false` when `STRATA_PROMETHEUS_URL` is unset or Prom is unreachable — the chip renders `(metrics unavailable)` instead of erroring. Skipped when `backend=memory`. |

Bundle delta of the four surfaces combined: ≤ ~10 KiB gzipped on the
Storage / BucketDetail chunks. No new chart libraries — the sparkline
reuses the same Recharts wrapper as the Metrics page + Cluster Overview.

E2E coverage lives in `web/e2e/placement.spec.ts`; see
[Web UI — End-to-end tests]({{< ref "/best-practices/web-ui#end-to-end-tests" >}}).

## See also

- [S3 multi-cluster routing]({{< ref "/best-practices/s3-multi-cluster" >}}) —
  env shape for `STRATA_S3_CLUSTERS` / `STRATA_S3_CLASSES`, credentials
  envelope, rolling-restart workflow.
- [Workers]({{< ref "/architecture/workers" >}}) — registration shape,
  supervisor lifecycle, leader-election semantics.
- [Observability]({{< ref "/architecture/observability" >}}) — full
  metric family + span shapes shipped by Strata.
