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

## Cluster lifecycle (register → activate → ramp)

`ralph/cluster-weights` (US-001..US-005) folded a 5th state — `pending` —
into the cluster machine and added a per-cluster `weight int (0..100)`
field. Together they enable a safe gradual-activation flow for new
clusters: register the new cluster via env, validate it without taking
client traffic, ramp the routing share 10% → 25% → 50% → 100% under
observation.

### Two weight layers — never combined

Strata has **two** weight knobs that look alike. They serve different
scopes and are **never multiplied together**:

| Layer | Source | Scope | When consulted |
| ----- | ------ | ----- | -------------- |
| Bucket `Placement` policy | `meta.Bucket.Placement` (PUT `/admin/v1/buckets/<name>/placement`) | Per-bucket override | Always wins. Picker consults it first; if non-nil, cluster weights are ignored for this bucket. |
| Cluster `weight` | `cluster_state.weight` (POST `/clusters/<id>/activate` or PUT `/clusters/<id>/weight`) | Cross-bucket default | Only when `bucket.Placement == nil` AND the class env carries no `@cluster` pin. Picker synthesises `{<live-cluster>: <its-weight>}` for that PUT. |

The picker order inside `placement.PickCluster` is:

1. `bucket.Placement != nil` → use bucket policy (short-circuit before
   weights).
2. Class env spec carries `@cluster` pin (e.g. `STANDARD=hot@cephb`) →
   use that cluster.
3. Synthesise default policy from live cluster weights (skipping
   pending / draining_readonly / evacuating / removed). All-zero-live →
   policy is empty → caller falls back to class spec.Cluster.

### Walkthrough table — adding `cephc` to a live deploy

| # | Operator action | Surface | What changes |
|---|-----------------|---------|--------------|
| 1 | Edit `STRATA_RADOS_CLUSTERS`: add `cephc:/etc/ceph-c/ceph.conf` | env file edit | New cluster id appears at next restart. |
| 2 | Rolling-restart strata replicas | `docker compose restart` | Out-of-band — orchestrator picks the cadence. |
| 3 | Gateway boot reconcile (`internal/serverapp/cluster_reconcile.go`) | log INFO `"cluster auto-init"` | Compares env vs `cluster_state` rows. New id with no chunks → state=`pending` weight=0. Existing id whose `bucket_stats` already reference it → state=`live` weight=100 (backwards-compat). Idempotent on re-run. |
| 4 | Open `/console/storage` | Storage page | `<ClustersSubsection>` renders the new card. |
| 5 | See cephc card with gray badge "Pending — not receiving writes" | `<ClusterCard>` (pending variant) | No Drain button. "Activate" CTA replaces it. |
| 6 | Click Activate | `<ActivateClusterModal>` opens | Modal mirrors the typed-confirm precedent from `<ConfirmDrainModal>` — Submit stays disabled until you type the exact cluster id. |
| 7 | Drag slider / fill numeric input to 10 (default), type `cephc`, Submit | `POST /admin/v1/clusters/cephc/activate {weight:10}` | Cluster flips to `state=live weight=10`. Drain cache invalidated synchronously so the next PUT sees the new state without waiting out the 30 s TTL. Audit row stamped `admin:ActivateCluster`. |
| 8 | New PUTs on nil-policy buckets start landing on cephc ~10% of the time | rebalance worker is **not** involved on the PUT path — `placement.PickCluster` consults the synthesised policy directly | Monitor on Grafana over a tier (hour → day) to confirm the cluster behaves under real load. |
| 9 | Drag the inline slider on the cephc card to 25 | `<LiveClusterWeightSlider>` | Debounces 500 ms then `PUT /admin/v1/clusters/cephc/weight {weight:25}`. Rapid drags coalesce to one PUT. Optimistic UI — slider position updates immediately; revert on 4xx. Audit row stamped `admin:UpdateClusterWeight`. |
| 10 | Repeat ramp 25 → 50 → 100 | (each step a slider drag) | Cluster fully integrated. Default routing among live clusters is proportional to `weight`. |

### Choosing the initial weight

| Scenario | Suggested initial weight | Why |
| -------- | ----------------------- | --- |
| Brand-new production cluster (untrusted hardware, fresh OSDs) | `10` | One-tenth of new traffic flows there. Diagnose under low load before ramping. |
| Trusted clone of an existing cluster (drop-in identical hardware) | `100` | No reason to throttle — symmetric capacity, well-known config. |
| Reactivating a recently-drained cluster | `0`, then ramp manually | Lets the operator validate state=live separately from routing share. `weight=0 + state=live` is legal — reads + explicit policies still route there. |

### Negative paths

- **Activate on a `live` cluster** → `409 InvalidTransition`. Use
  `PUT /weight` for in-place adjustments.
- **PUT `/weight` on a `pending` cluster** → `409 InvalidTransition`.
  Must POST `/activate` first.
- **All live clusters have `weight=0`** → synthesised default policy is
  empty → `PickCluster` returns `""` → caller falls back to class
  `spec.Cluster` (or 503 `<Code>DrainRefused</Code>` if the fallback
  cluster is draining).
- **Boot reconcile sees an existing-live cluster (chunks via
  `bucket_stats`)** → auto-creates `state=live weight=100`, not
  `pending`. Zero operator action required during upgrade from older
  strata versions.

### `cluster_state` schema reminder

`cluster_state` rows carry `state` + `mode` + `weight` (added in
ralph/cluster-weights US-001). Absence still means "live" — the boot
reconcile materialises the row when needed. Memory + Cassandra + TiKV
backends carry the same three fields; the TiKV value is a 3-segment
byte string `state\x00mode\x00<decimal-weight>` so older 1- and
2-segment values decode as `weight=0` on read (forward-compat with
mid-upgrade clusters).

## Drain lifecycle

Drain is the operator hook that takes a cluster out of the write hot path —
either temporarily for maintenance, or permanently to deregister. The
4-state machine + mode picker shipped in `ralph/drain-transparency`
(US-001..US-008) separates the two intents; the old single-mode drain
from `ralph/drain-lifecycle` is now a special case (`mode=evacuate`).

### Stop-writes vs. evacuate — pick the right mode

`POST /admin/v1/clusters/<id>/drain` requires a body `{"mode":"…"}`.
There is no default — the operator must pick:

| Mode | State | Scan + migration | Reversible | When to use |
| ---- | ----- | ---------------- | ---------- | ----------- |
| `readonly` | `draining_readonly` | No (worker skips the per-cluster scan to save cost) | Yes — `POST /undrain` returns to `live` with no side effects | Maintenance window: short-lived pause where you want new writes refused but the cluster stays in service for reads, deletes, list, and in-flight multipart. No bytes move. |
| `evacuate` | `evacuating` | Yes — rebalance worker categorizes chunks (migratable / stuck single-policy / stuck no-policy) and migrates the migratable subset to peers | Yes — `POST /undrain` returns to `live` BUT migrated chunks stay on their new target (no reverse migration) | Decommission: you want the cluster's bytes off the hardware so it can be deregistered. Reads / deletes / in-flight multipart continue throughout. |

Upgrade path: `draining_readonly → evacuating` via a second
`POST /drain {"mode":"evacuate"}` — the modal renders the readonly
radio hidden and the title flips to "Upgrade to evacuate". There is no
downgrade (evacuate → readonly); use undrain → re-drain readonly if
you really need it.

Drain is **unconditionally strict in both modes** (US-007). RADOS + S3
`PutChunks` refuse to fall back to a `draining_readonly` or
`evacuating` cluster — `data.ErrDrainRefused` → HTTP 503
`<Code>DrainRefused</Code>` with `Retry-After: 300`.

### Pre-drain operator checklist

Walk this list before submitting the ConfirmDrainModal. Each item has a
console surface to drive it.

1. **Open the Pools matrix.** `/console/storage` → Data tab → Pools
   table. The matrix renders `#clusters × #distinct-pools` rows. A
   `0 B` row means the class is routed elsewhere and drain is a no-op.
2. **Click "Show affected buckets" on the cluster card.** The
   `<BucketReferencesDrawer>` lists every bucket whose `Placement`
   policy carries a non-zero weight on the cluster, sorted desc by
   chunk_count.
3. **Run the impact analysis.** Inside `<ConfirmDrainModal>` pick the
   `Full evacuate (decommission)` radio — the modal fetches
   `GET /admin/v1/clusters/<id>/drain-impact` and renders three
   categorized counters:
   - **Migratable** — bucket has a `Placement` policy AND
     `PickClusterExcluding` finds a peer. Will move on the next
     rebalance tick.
   - **Stuck (single policy)** — bucket has a `Placement` policy but
     every cluster named in it is draining. Will NOT migrate; new PUTs
     get 503 DrainRefused.
   - **Stuck (no policy)** — bucket has no `Placement` (relies on
     class-env routing) and has chunks routed to the draining cluster.
     Same outcome: won't migrate.
4. **Fix the stuck buckets.** If `stuck>0` the modal renders
   `<BulkPlacementFixDialog>` behind a "Fix N buckets" CTA. Multi-
   select rows, pick a suggested policy per bucket (or one uniform
   policy across all selected via the dialog-level toggle), Apply →
   the dialog issues `PUT /admin/v1/buckets/<name>/placement` per
   bucket and refetches `/drain-impact` on close.
5. **Submit.** When stuck=0 the typed-confirm input arms the
   destructive submit; click Drain. Stuck>0 keeps the submit disabled
   with the explainer "Drain blocked — fix N stuck buckets".

For `readonly` drain the modal skips the impact analysis (no migration
will happen, so the categorization is moot). Submit is armed by the
typed-confirm input alone.

### Drain procedure

The console flow per cluster, mode-specific:

**Stop-writes (readonly):**
1. Click `Drain` → ConfirmDrainModal opens, readonly radio selected.
2. Type the cluster id to arm submit; click `Drain (stop-writes)`.
3. Cluster card flips to `draining_readonly` state. `<DrainProgressBar>`
   renders a single orange stop-writes chip plus an "Upgrade to
   evacuate" button and a small "Undrain" button.
4. Run your maintenance; click `Undrain` when done. State flips back
   to `live` with no migration cost.

**Full evacuate:**
1. Click `Drain` → ConfirmDrainModal opens; flip to the evacuate radio.
2. Run the impact analysis + fix stuck buckets via the
   BulkPlacementFixDialog (above).
3. Type the cluster id to arm submit; click `Drain (X chunks will
   migrate)`.
4. Cluster card flips to `evacuating` state. `<DrainProgressBar>`
   renders a red "Evacuating" label + progress bar + three categorized
   counters. ETA is derived from
   `rate(strata_rebalance_chunks_moved_total{from=<id>}[5m])` and
   appears only when migratable>0.
5. When `chunks_on_cluster` transitions `>0 → 0` the bar surfaces an
   emerald `✓ Ready to deregister (env edit + restart)` chip; the
   rebalance worker logs INFO `drain complete`, writes a
   `drain.complete` audit row, bumps `strata_drain_complete_total`,
   and best-effort fans an `s3:Drain:Complete` event through every
   sink in `STRATA_NOTIFY_TARGETS`.
6. **Env edit + rolling restart.** Drop the cluster id from
   `STRATA_RADOS_CLUSTERS` / `STRATA_S3_CLUSTERS` and rolling-restart
   the gateway replicas. There is no admin endpoint that performs the
   deregister — the env shape is the source of truth.

### Abort / recovery

`POST /admin/v1/clusters/<id>/undrain` works from both
`draining_readonly` and `evacuating`. It deletes the `cluster_state`
row (absence == live) and invalidates the in-process drain cache.
Migrated chunks stay on their new target; only future moves halt.
Undrain is idempotent and refused (409 InvalidTransition) from `live`
or `removed`.

### Drain endpoints

| Path | What it does | Caller |
| ---- | ----------- | ------ |
| `GET /admin/v1/clusters` | Lists every configured cluster id, its `state` ∈ {live, draining_readonly, evacuating, removed} + `mode` ∈ {"", readonly, evacuate}. Drain is unconditionally strict — the legacy `drain_strict` field is gone (US-007). | `<ClustersSubsection>` (10 s poll) |
| `POST /admin/v1/clusters/{id}/drain` | Body `{"mode":"readonly"\|"evacuate"}` required. 4-state machine enforced server-side: invalid transitions → 409 `InvalidTransition` with `current_state` + `requested_mode`. Audit-stamped `admin:DrainCluster`. | `<ConfirmDrainModal>` |
| `POST /admin/v1/clusters/{id}/undrain` | Drops cluster_state row. Refuses from live/removed (409 InvalidTransition). Audit-stamped `admin:UndrainCluster`. | Undrain buttons |
| `GET /admin/v1/clusters/{id}/drain-progress` | Per-cluster `{state, mode, chunks_on_cluster, bytes_on_cluster, base_chunks_at_start, migratable_chunks, stuck_single_policy_chunks, stuck_no_policy_chunks, by_bucket, last_scan_at, eta_seconds, deregister_ready, warnings}`. Reads from the rebalance worker's in-process `ProgressTracker` — never scans manifests synchronously. Readonly state returns null counts + a `"stop-writes mode — migration scan skipped"` warning. | `<DrainProgressBar>` (30 s poll) |
| `GET /admin/v1/clusters/{id}/drain-impact` | Pre-evacuate analysis: `{cluster_id, current_state, migratable_chunks, stuck_single_policy_chunks, stuck_no_policy_chunks, total_chunks, by_bucket[], total_buckets, next_offset, last_scan_at}`. Each `by_bucket` entry carries category + chunk_count + `suggested_policies[]`. State ∈ {live, draining_readonly} → 200 (synchronous one-off scan, 5-min in-process cache); state ∈ {evacuating, removed} → 409 InvalidTransition (use /drain-progress instead). Paginated via `?limit=N&offset=M` (default 100, max 1000). Audit-stamped `admin:GetClusterDrainImpact`. | `<ConfirmDrainModal>` evacuate mode + `<BulkPlacementFixDialog>` |
| `GET /admin/v1/clusters/{id}/bucket-references` | Coarser pre-drain preview: buckets whose `Placement[<id>] > 0` joined with `bucket_stats` for chunk_count + bytes_used. Drawer-shape — no suggested policies. | `<BucketReferencesDrawer>` |

`drain-progress` numeric fields are nullable: when state=live every
numeric field is `null`. When state=evacuating but no scan has
committed yet, the response carries
`warnings: ["progress scan pending; rebalance worker has not yet
committed a tick"]` and the UI renders "scan pending" instead of
zero counts.

### Drain refusal semantics (always strict)

Drain is **unconditionally strict** in both modes (US-007
drain-transparency — the former opt-in `STRATA_DRAIN_STRICT` env was
retired). RADOS + S3 `PutChunks` always refuse to fall back to a
draining cluster — they return `data.ErrDrainRefused`, the gateway
maps it to `503 ServiceUnavailable` with `<Code>DrainRefused</Code>`
body and `Retry-After: 300` header. **PUT chunks only** — GET / HEAD /
DELETE / multipart UploadPart / Complete / Abort / List against
draining clusters keep working (drain semantic is stop-write, not
stop-read). In-flight multipart sessions persist their initial cluster
id in the upload handle (`cluster\x00bucket\x00key\x00uploadID`) and
never re-consult the picker, so UploadPart / Complete / Abort on an
already-open multipart finish gracefully on the drained cluster.

**Breaking change for Prometheus dashboards.** Counter label flipped
from `reason="drain_strict"` to `reason="drain_refused"` —
`strata_putchunks_refused_total{reason="drain_refused",cluster}`
increments per refusal. Wire to alerting if you expect zero refusals
in steady state. Legacy `STRATA_DRAIN_STRICT` env in the environment
is ignored at boot with a single WARN log line (remove from your
deploy descriptors).

### Three-scenario walkthrough

The smoke harness `scripts/smoke-drain-transparency.sh` drives every
step below against the running `multi-cluster` compose profile;
exit-non-zero on any assertion miss. Run via
`make smoke-drain-transparency` once the lab is up
(`docker compose --profile multi-cluster up -d`). The legacy
`scripts/smoke-drain-lifecycle.sh` still validates the basic flip-to-
draining contract; the new harness covers mode-picker + impact
analysis + multipart graceful contract end-to-end.

**Scenario A — Stop-writes drain (maintenance):**

| # | Operator action | Surface | Backing story |
|---|-----------------|---------|---------------|
| 1 | Open `/console/storage` → Data tab; pick cluster card | `<ClustersSubsection>` | existing |
| 2 | Click `Drain` → modal opens, readonly radio default | `<ConfirmDrainModal>` | US-004 |
| 3 | Type cluster id → submit armed → click `Drain (stop-writes)` | typed confirm | US-004 |
| 4 | Card flips to `draining_readonly`; `<DrainProgressBar>` shows orange stop-writes chip + Upgrade / Undrain buttons | progress bar | US-006 |
| 5 | New PUT to a cephb-only bucket → 503 DrainRefused | gateway | US-007 |
| 6 | GET on existing object → 200; in-flight multipart Init+UploadPart+Complete on cephb mid-drain → 200 | gateway | US-007 |
| 7 | Click `Undrain` → state=live → new PUT succeeds | progress bar | US-001 |

**Scenario B — Full evacuate (decommission):**

| # | Operator action | Surface | Backing story |
|---|-----------------|---------|---------------|
| 1 | Pre-seed 3 buckets: `tx-split {cephb:1,default:1}`, `tx-stuck {cephb:1}`, `tx-residual` (no policy) | aws-cli | walkthrough |
| 2 | Click `Drain` → flip mode picker to `Full evacuate (decommission)` | `<ConfirmDrainModal>` | US-004 |
| 3 | Impact analysis fires → counters render: migratable>0, stuck_single_policy>0 | `/drain-impact` | US-003 + US-004 |
| 4 | "Fix N buckets" amber CTA → opens `<BulkPlacementFixDialog>` | bulk fix | US-005 |
| 5 | Pick suggested policy per bucket (or "Apply uniform to all") → Apply → PUTs issued | bulk fix | US-005 |
| 6 | Bulk dialog closes; modal refetches /drain-impact; stuck=0; submit enables | invalidation | US-005 |
| 7 | Type cluster id → click `Drain (X chunks will migrate)` | typed confirm | US-004 |
| 8 | Card flips to `evacuating`; `<DrainProgressBar>` renders red label + three categorized counters + ETA | progress bar | US-006 |
| 9 | Wait for rebalance worker → `chunks_on_cluster` reaches 0 → emerald deregister-ready chip | completion | US-002 / US-005 / US-006 |
| 10 | Env edit + rolling restart removes cluster id | docs | out-of-band |

**Scenario C — Upgrade readonly → evacuate:**

| # | Operator action | Surface | Backing story |
|---|-----------------|---------|---------------|
| 1 | Start from Scenario A's state=draining_readonly | precondition | US-001 |
| 2 | `<DrainProgressBar>` renders "Upgrade to evacuate" button | bar | US-006 |
| 3 | Click upgrade → `<ConfirmDrainModal>` opens with title "Upgrade to evacuate" + readonly radio HIDDEN | modal | US-004 |
| 4 | Impact analysis fires → counters render | `/drain-impact` | US-003 |
| 5 | If stuck>0 → bulk fix flow; else type cluster id → submit | shared with Scenario B | US-004 / US-005 |
| 6 | Card flips draining_readonly → evacuating; migration begins | server | US-001 / US-002 |
| 7 | Wait → deregister-ready chip | completion | US-005 |

Negative paths covered:

- **Modal blocks submit when stuck>0** — clicking Drain on the
  evacuate radio with stuck>0 keeps the submit disabled even when the
  typed-confirm matches; the explainer text reads "Drain blocked — fix
  N stuck buckets". Validated in `web/e2e/drain-transparency.spec.ts`
  Scenario B.
- **In-flight multipart finishes gracefully** — Init+UploadPart+
  Complete on a draining cluster all return 200; bytes land on the
  drained cluster's pool and are immediately readable. Validated in
  `scripts/smoke-drain-transparency.sh` Scenario A step 6.
- **Undrain from live or removed → 409 InvalidTransition** — the 4-
  state machine refuses no-op transitions. Validated in
  `internal/adminapi/clusters_drain_test.go`.

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
