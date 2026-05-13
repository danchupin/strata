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

## Drain lifecycle

Drain is the operator hook that retires a cluster gracefully. The
`ralph/drain-lifecycle` cycle ships the strict mode, the progress
endpoint, the UI bar, the completion signal, and the bucket-impact
preview drawer described below. Use this section as the runbook before
running `POST /admin/v1/clusters/<id>/drain` in production.

### When drain is useful (and when it is not)

Drain is built around the assumption that the cluster being drained
has **somewhere to migrate chunks to**:

- **Multi-cluster placement deployments** — buckets carry a
  `Placement` policy that names ≥2 clusters with non-zero weight. The
  rebalance worker reads `PickClusterExcluding` over the policy with
  `draining` excluded and moves every chunk from the drained cluster
  to a peer named in the policy. Drain is genuinely retiring storage
  here.
- **Single-cluster-per-class config** — every storage class points at
  exactly one cluster via `STRATA_RADOS_CLASSES` /
  `STRATA_S3_CLASSES`, and buckets either do not carry a policy or
  their policy names only the one cluster. Drain still flips the
  cluster to `state=draining` and `PickClusterExcluding` still skips
  it, but chunks have nowhere to go — the rebalance worker emits no
  moves, `chunks_on_cluster` never reaches zero, and
  `deregister_ready` never flips true. Drain in this shape is a pure
  **stop-write** flag; the operator needs to (a) add new buckets with
  alternate-cluster policies before the drained cluster fills up, or
  (b) accept that the cluster stays in `draining` until traffic
  naturally ages out via lifecycle expiration.

If the goal is "stop writes immediately" without migration, drain is
the right primitive — strict mode (`STRATA_DRAIN_STRICT=on`) closes
the fail-open fallback so PUTs targeting a single-cluster policy on
the drained cluster get a clean 503 DrainRefused. If the goal is
"actually move bytes off this hardware", drain is the right primitive
**only when** the bucket Placement policies name a peer cluster — the
pre-drain checklist below catches the gap before it surprises you.

### Pre-drain operator checklist

Walk this list before clicking Drain on the cluster card. Each item
has a single console surface to drive it.

1. **Open the Pools matrix.** `/console/storage` → Data tab → Pools
   table. The matrix renders `#clusters × #distinct-pools` rows (see
   "Pools matrix" in the [Web UI capability
   matrix]({{< ref "/best-practices/web-ui" >}})). Confirm the cluster
   you intend to drain actually holds bytes — a `0 B` row means the
   class is routed elsewhere and drain is a no-op.
2. **Open the bucket-references drawer.** Click "Show affected
   buckets" on the cluster card. The drawer lists every bucket whose
   `Placement` policy carries a non-zero weight on the cluster, sorted
   desc by chunk_count. Hot buckets at the top need a policy update
   FIRST — either widen them to name a peer cluster (so rebalance can
   migrate) or accept the chunks will be unreachable for new PUTs once
   drain starts.
3. **Verify pool names exist on target clusters.** The rebalance
   worker copies chunks from the source pool to the *same-named* pool
   on the target cluster. If a class maps `STANDARD → strata-hot` and
   the target cluster doesn't have a `strata-hot` pool, every move
   fails. The Pools matrix surfaces target pools — confirm visually
   before draining.
4. **Decide on strict mode.** Set `STRATA_DRAIN_STRICT=on` if you want
   PUTs that *would have fallen back to the drained cluster* (empty
   policy / all-policy-clusters-draining) to refuse with 503
   DrainRefused instead. The "strict" badge on every cluster card
   confirms the global flag is on. The flag is a process-boot env —
   flipping it requires a rolling restart. Without strict mode, PUTs
   silently fall back to the class default's `spec.Cluster` even when
   it's draining.
5. **Skim the BucketDetail Placement tab for policy-drain warnings.**
   When every cluster with non-zero weight in a saved bucket policy is
   `state=draining`, the Placement tab renders an amber
   "All clusters in this policy are draining" chip. Update those
   policies before traffic resumes; otherwise new PUTs land in the
   fail-open / refuse trap depending on strict mode.

### Drain procedure

The console flow per cluster:

1. **Show affected buckets** → review the drawer; close it.
2. **Drain** → `<ConfirmDrainModal>` opens. The modal's info row
   surfaces `<N> buckets reference this cluster in their Placement
   policy` — click "view list" if you want to re-check the drawer
   without closing the modal.
3. **Typed-confirm.** Type the cluster id verbatim to arm the
   destructive submit. Mistypes keep Drain disabled.
4. **Wait for the progress bar to drain.** The cluster card flips to
   `draining` state and renders `<DrainProgressBar>` — the amber bar
   advances as the rebalance worker moves chunks. Text shows
   `<N> chunks remaining · <M> at start · ~<ETA>`; ETA is derived from
   `rate(strata_rebalance_chunks_moved_total{from=<id>}[5m])`.
5. **Green chip = deregister-ready.** When `chunks_on_cluster`
   transitions `>0 → 0` the bar is replaced with an emerald
   `✓ Ready to deregister (env edit + restart)` chip — and the
   rebalance worker logs INFO `drain complete`, writes a
   `drain.complete` audit row, bumps `strata_drain_complete_total`,
   and best-effort fans an `s3:Drain:Complete` event through every
   sink in `STRATA_NOTIFY_TARGETS`.
6. **Env edit + rolling restart.** Drop the cluster id from
   `STRATA_RADOS_CLUSTERS` / `STRATA_S3_CLUSTERS` and rolling-restart
   the gateway replicas. There is no admin endpoint that performs the
   deregister — the env shape is the source of truth.

### Abort / recovery

`POST /admin/v1/clusters/<id>/undrain` clears the `cluster_state` row
(absence == live) and invalidates the in-process drain cache. The
rebalance worker's per-tick scan reaps the cached progress snapshot on
the next iteration so a future re-drain re-fires the completion event
when chunks reach zero again. Undrain is idempotent and safe to issue
at any point in the drain — chunks moved before the undrain stay
moved, the rest stop migrating.

### Drain progress + completion endpoints

| Path | What it does | Caller |
| ---- | ----------- | ------ |
| `GET /admin/v1/clusters` | Lists every configured cluster + its state + the global `drain_strict` flag. | `<ClustersSubsection>` (10 s poll) |
| `GET /admin/v1/clusters/{id}/drain-progress` | Per-cluster `{state, chunks_on_cluster, bytes_on_cluster, base_chunks_at_start, last_scan_at, eta_seconds, deregister_ready, warnings}`. Reads from the rebalance worker's in-process `ProgressTracker` — never scans manifests synchronously. | `<DrainProgressBar>` (30 s poll) |
| `GET /admin/v1/clusters/{id}/bucket-references` | Pre-drain bucket-impact preview. Lists buckets whose `Placement[<id>] > 0` joined with `bucket_stats` for chunk_count + bytes_used. Paginated via `?limit=N&offset=M` (default 100). | `<BucketReferencesDrawer>` |
| `POST /admin/v1/clusters/{id}/drain` / `.../undrain` | Flip `cluster_state` row to `draining` / `live`. Audit-stamped `admin:DrainCluster` / `admin:UndrainCluster`. | `<ConfirmDrainModal>` / Undrain button |

`drain-progress` numeric fields are nullable: when state=live every
numeric field is `null` (only meaningful while draining). When
state=draining but no scan has committed yet, the response carries
`warnings: ["progress scan pending; rebalance worker has not yet
committed a tick"]` and the UI renders "scan pending" instead of
zero counts.

### `STRATA_DRAIN_STRICT` env

| Variable                  | Default | Range          | Purpose |
| ------------------------- | ------- | -------------- | ------- |
| `STRATA_DRAIN_STRICT`     | `off`   | `on` / `off` / boolean strings | When `on`, RADOS + S3 `PutChunks` refuse to fall back to a `draining` cluster — returns `data.ErrDrainRefused`, the gateway maps it to `503 ServiceUnavailable` with `<Code>DrainRefused</Code>` and `Retry-After: 300`. **Only PUT chunks** are affected — GET / HEAD / DELETE / multipart Complete / Abort / List on the drained cluster all keep working (drain semantic is stop-write, not stop-read). |

Parsed at gateway boot; unknown values fail-fast with a clear error.
Counter `strata_putchunks_refused_total{reason="drain_strict",cluster}`
increments per refusal — wire to alerting if you expect zero refusals
in steady state.

### User journey walkthrough

The smoke harness `scripts/smoke-drain-lifecycle.sh` drives every
step below against the running `multi-cluster` compose profile;
exit-non-zero on any assertion miss. Run via
`make smoke-drain-lifecycle` once the lab is up
(`docker compose --profile multi-cluster up -d`).

| # | Operator action | Surface | Backing story |
|---|-----------------|---------|---------------|
| 1 | Open `/console/storage`, see both cluster cards live | `<ClustersSubsection>` | (placement-ui, existing) |
| 2 | See per-cluster per-pool bytes/chunks in Pools table | Pools matrix (`#clusters × #distinct-pools` rows) | US-001 |
| 3 | Click "Show affected buckets" link on draining-candidate card | `<BucketReferencesDrawer>` | US-006 |
| 4 | Identify hot buckets that need policy update before drain | drawer lists buckets desc by `chunk_count` | US-006 |
| 5 | Optionally flip `STRATA_DRAIN_STRICT=on` in env + restart | docs + cluster-card "strict" chip | US-002 + US-004 |
| 6 | Click "Drain" on the cluster card | `<ConfirmDrainModal>` | (placement-ui, existing) |
| 7 | Modal surfaces "<N> buckets reference this cluster" + typed-confirm | enhanced modal | US-006 |
| 8 | Type cluster id → submit enabled → click Drain | typed confirm | (placement-ui, existing) |
| 9 | Banner appears in AppShell, card flips to `draining` state | `<PlacementDrainBanner>` | (placement-ui, existing) |
| 10 | Card now shows `<DrainProgressBar>` "<N> chunks · <M> at start · ~<ETA>" | progress bar | US-003 + US-004 |
| 11 | Watch progress bar shrink as rebalance worker ticks every `STRATA_REBALANCE_INTERVAL` | TanStack 30 s poll | US-004 |
| 12 | Receive operator notification (notify worker) when drain reaches 0 chunks | `s3:Drain:Complete` event | US-005 |
| 13 | Card shows green chip "✓ Ready to deregister" | `deregister_ready=true` | US-004 |
| 14 | Edit `STRATA_RADOS_CLUSTERS` env, remove cluster entry, rolling restart | docs (out-of-band) | docs |
| 15 | Verify cluster gone from console + every former chunk readable on peers | Storage page rerender | US-007 smoke |

Negative paths covered by the smoke (`scripts/smoke-drain-lifecycle.sh`):

- **(a)** Strict mode on + bucket policy `{drained:1}` only + PUT →
  503 DrainRefused with `Retry-After: 300`.
- **(b)** Strict mode off (default) + same bucket policy → fail-open
  to the class default with WARN log; no client-visible refusal.
- **(c)** Operator clicks Undrain mid-drain → state flips back to
  `live`, AppShell banner disappears, drain-progress cache reaped on
  the next tick.
- **(d)** Single-cluster-per-class config + drain → no chunks
  migrate; `deregister_ready` never flips true. Documented above (see
  "When drain is useful (and when it is not)").

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
