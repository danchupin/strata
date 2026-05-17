# PRD: Rebalance worker Phase 2 — sharded leader election

## Introduction

Rebalance worker is leader-elected on a single `rebalance-leader` lease cluster-wide (SkipLease=false in `cmd/strata/workers/rebalance.go`). One goroutine on the elected replica scans ALL buckets sequentially via `meta.Store.ListBuckets` → plans moves per chunk via EffectivePolicy → executes through RADOS/S3 movers. For large deploys (10k+ buckets, M+ objects), full-sweep takes hours; multi-replica cluster has 2 idle replicas while 1 grinds.

gc + lifecycle workers got Phase 2 treatment via `ralph/gc-lifecycle-scale-phase-2` cycle: per-shard leases `gc-leader-0..N-1` driven by `STRATA_GC_SHARDS` (default 1, range [1, 1024]). Each replica races for one or more leases. Lifecycle uses `fnv32a(bucketID) % STRATA_GC_SHARDS == myReplicaShard` to distribute bucket ownership. Mirror this exact pattern for rebalance.

Closes ROADMAP P2 *Rebalance worker not sharded — single goroutine bottleneck on large deploys*.

## Goals

- `STRATA_REBALANCE_SHARDS` env (default 1, range [1, 1024])
- Per-shard leases `rebalance-leader-0..N-1` — each replica races to acquire 1+ leases
- Bucket distribution via `meta.Store.ListBucketsShard(ctx, shardID, totalShards)` filtering on `fnv32a(bucketID) % totalShards == shardID`
- Worker registered with `SkipLease: true` — owns its own leader election (canonical gc fan-out pattern)
- Per-shard panic recovery + exponential backoff (existing supervisor pattern)
- `leader_for=rebalance` chip emits once per replica regardless of multi-shard ownership (folded events)
- ProgressTracker per-cluster merges across shards on read — `/drain-progress` returns combined counts; operator UX unchanged
- Completion detection fires once when total across ALL shards transitions to 0
- Lab-tikv-3 bench measures ~3× throughput at SHARDS=3 vs SHARDS=1 baseline
- Single-replica SHARDS=1 reproduces Phase 1 byte-for-byte (back-compat)

## User Journey Walkthrough

Pre-cycle walkthrough per `feedback_cycle_end_to_end.md`. Operator scenario: large multi-cluster deployment, evacuate cephb across 3 replicas.

| # | Operator action | Surface | Story |
|---|-----------------|---------|-------|
| 1 | Set `STRATA_REBALANCE_SHARDS=3` on all 3 replicas in `lab-tikv-3` stack | env edit | (out-of-band) |
| 2 | Rolling restart | docker compose | (out-of-band) |
| 3 | Each replica acquires 1+ of `rebalance-leader-0..2` leases | leader-election logs | **US-002** |
| 4 | Heartbeat chip `leader_for=rebalance` propagates per replica (folded) | UI Cluster Overview | **US-002** |
| 5 | Operator drains cephb evacuate mode | existing ConfirmDrainModal | (existing) |
| 6 | All 3 replicas concurrently scan their bucket shards | worker logs `shard=0/1/2` | **US-001 + US-003** |
| 7 | ProgressTracker accumulates per-shard counters | progressCache.byShard | **US-003** |
| 8 | `/drain-progress` returns merged snapshot (sum of all shard contributions) | merged API | **US-003** |
| 9 | Migration throughput ~3× single-leader baseline | bench harness measures | **US-004** |
| 10 | When chunks_on_cluster=0 across all 3 shards → deregister_ready=true | completion detection at merged level | **US-003** |
| 11 | Identical operator UX (chip, button, modal text — unchanged) | existing UI | (existing) |
| 12 | Replica B dies → A acquires lease for shard 1 (fan-out within replica) | gc fan-out canonical case | **US-002** |
| 13 | Per-shard panic increments `strata_worker_panic_total{worker="rebalance",shard="<i>"}` | metric counter | **US-002** |

Negative paths:
- Single-replica deploy at SHARDS=1 → byte-for-byte Phase 1 behavior (back-compat verified by smoke)
- SHARDS=0 or SHARDS<0 → clamped to 1 + WARN log at boot
- SHARDS>1024 → clamped to 1024 + WARN log at boot
- Replica acquires multiple leases when peers die — fan-out within one replica (gc fan-out test fixture)

## State Truth Tables

### Per-replica shard ownership (US-002)

| Replicas alive | SHARDS env | Leases per replica | Effective distribution |
|----------------|-----------|--------------------|-----------------------|
| 1 | 1 | 1 (rebalance-leader-0) | 100% of buckets |
| 3 | 1 | 1 holder, 2 idle | 100% on holder (Phase 1 reproduction) |
| 3 | 3 | 1 each | ~33/33/33 bucket split |
| 1 | 3 | 3 on same replica (fan-out) | 100% on the one replica, split across 3 goroutines |
| 2 | 3 | 2/1 split (one replica owns 2 leases) | ~67/33 bucket split |

### Heartbeat chip emission (US-002)

| Replica state | LeaderEvents emits |
|---------------|-------------------|
| Acquired first shard lease (was 0 leases) | `(worker="rebalance", acquired=true)` once |
| Acquired additional shard lease (already had ≥1) | no event (folded) |
| Released a shard lease but still has ≥1 | no event (folded) |
| Released last shard lease (now 0 leases) | `(worker="rebalance", acquired=false)` once |

## Cache Invalidation Ledger

No new caches introduced this cycle. Existing `placement.DrainCache` (30s TTL) + `drainImpactCache` (5min) unaffected — sharding is a worker-side concern, doesn't touch admin endpoint caches.

## Safety Claims Preconditions

| Claim | Preconditions | Verified by |
|-------|---------------|-------------|
| "Single-replica SHARDS=1 reproduces Phase 1" | SkipLease=true + 1 shard = 1 lease = identical iteration to existing worker | US-005 smoke step 1 |
| "deregister_ready fires once when sum across shards = 0" | progressCache.byShard sums correctly; CompletionFiredAt stored at merged level | US-003 unit test |
| "Multi-leader bucket disjoint" | fnv32a deterministic; ListBucketsShard returns disjoint subsets per shard | US-001 unit test + integration test |

## User Stories

### US-001: `meta.Store.ListBucketsShard` + shard-scoped bucket distribution
**Description:** As a developer, I need a meta API that returns only buckets belonging to a specific shard so multiple worker replicas can scan disjoint subsets.

**Acceptance Criteria:**
- [ ] New method `meta.Store.ListBucketsShard(ctx context.Context, shardID int, totalShards int) ([]*meta.Bucket, error)` returns buckets where `fnv32a(bucket.ID) % totalShards == shardID`
- [ ] Validation: `shardID ∈ [0, totalShards)` AND `totalShards >= 1`; out-of-range returns `ErrInvalidShard` (new sentinel)
- [ ] Memory impl: iterate in-mem map, filter by hash
- [ ] Cassandra impl: stream via existing `ListBuckets` paging, filter in-process (no schema change — buckets table partition is already on `name`; secondary index by shard not worth it for the operator-driven scan rate)
- [ ] TiKV impl: scan key prefix `s/bk/`, filter by hash on each row (existing prefix scan shape)
- [ ] New contract test `caseListBucketsShard` in `internal/meta/storetest/contract.go` covers all three backends: seed 100 buckets with random UUIDs → list with 3 shards → assert union covers all 100, intersections empty, ~33/33/33 split (5% tolerance)
- [ ] Unit test: 1000 fake bucket IDs distributed across 3 shards → ~33/33/33 (5% tolerance); 10 shards → ~10% each (10% tolerance)
- [ ] Edge case: `totalShards=1` → returns all buckets (matches existing ListBuckets behavior with shardID=0)
- [ ] OpenAPI spec unchanged — this is an internal meta method, not exposed via admin API
- [ ] `go vet ./...` passes; typecheck passes; tests pass

### US-002: Rebalance worker fan-out — per-shard leases + SkipLease=true
**Description:** As a developer, I need the rebalance worker to fan out per-shard via the existing supervisor SkipLease pattern so multiple replicas can run shards concurrently.

**Acceptance Criteria:**
- [ ] `cmd/strata/workers/rebalance.go` register with `SkipLease: true` (worker owns its own leader-election); change line `workers.Register(workers.Worker{Name: "rebalance", Build: ..., SkipLease: false})` → `SkipLease: true`
- [ ] Build constructor reads `STRATA_REBALANCE_SHARDS` env at construct time (default 1, range [1, 1024]; out-of-range clamped + WARN-logged via deps.Logger)
- [ ] New `internal/rebalance.ShardedFanOut` struct mirrors `internal/gc/fanout.go` pattern (same package shape, similar interfaces): fields `name`, `shards`, `locker`, `holderID`, `runShard func(ctx, shardID, totalShards) error`, `emitLeader func(worker string, acquired bool)`
- [ ] `ShardedFanOut.Run(ctx)` spawns one goroutine per shard ID `0..shards-1`; each tries to acquire `rebalance-leader-<shardID>` lease via `leader.Session`; releases on context cancel or panic; on panic increments `strata_worker_panic_total{worker="rebalance",shard="<i>"}` + restarts with exponential backoff (1s → 5s → 30s → 2m, reset after 5min healthy)
- [ ] Per-shard goroutine acquires lease + invokes `runShard(ctx, shardID, totalShards)` (closure over the Worker)
- [ ] `emitLeader` folded: tracks shard count owned by this replica via atomic counter; emit `(worker="rebalance", acquired=true)` when counter goes 0→1; emit `(worker="rebalance", acquired=false)` when counter goes 1→0; no event on intermediate transitions
- [ ] Per-shard lease loss restarts immediately (no backoff — existing supervisor behavior); panic restart applies backoff
- [ ] Worker Build returns Runner that wraps `ShardedFanOut` (Runner.Run delegates to FanOut.Run)
- [ ] Unit test: fake locker + ShardedFanOut with 3 shards → assert 3 lease acquires (one per shard); cancel ctx → assert 3 releases; simulate panic in runShard for shard 1 → assert backoff + restart + metric incremented + sibling shards unaffected
- [ ] Unit test: leader-event folding — 3 shards acquired sequentially produce exactly 1 `acquired=true` event; release all 3 produces exactly 1 `acquired=false` event
- [ ] Integration test against memory backend: 3-shard fan-out + 9 buckets seeded → each shard handles its subset (verified via worker scan log inspection)
- [ ] `go vet ./...` passes; typecheck passes; tests pass

### US-003: Per-shard worker iteration + sharded ProgressTracker merge
**Description:** As a developer, I need the worker iteration to scan only its shard's bucket subset AND the ProgressTracker to aggregate per-shard contributions into a single per-cluster snapshot for the admin API.

**Acceptance Criteria:**
- [ ] `internal/rebalance.Worker.RunShard(ctx, shardID, totalShards) error` method — same iteration loop as today but iterates only `meta.Store.ListBucketsShard(ctx, shardID, totalShards)` instead of full `ListBuckets`
- [ ] Iteration span attributes carry `strata.rebalance.shard=<i>` (and existing `strata.worker="rebalance"`); the per-iteration tick span name unchanged (`worker.rebalance.tick`)
- [ ] `ProgressTracker.CommitScan(...)` signature extended to accept `shardID int`; per-cluster snapshot store extended from `progressCache map[clusterID]ProgressSnapshot` → `progressCache map[clusterID]map[shardID]ProgressSnapshot`
- [ ] `ProgressTracker.Snapshot(clusterID)` returns the merged snapshot — sums migratable/stuck_single_policy/stuck_no_policy/bytes across all shards; takes latest `LastScanAt` (max across shards); merges `byBucket` maps
- [ ] `/drain-progress` endpoint reads merged snapshot — operator-facing wire shape unchanged
- [ ] `/drain-impact` endpoint synchronous-scan path triggers ListBuckets (full, not sharded) because it's a one-shot admin scan, not a worker per-tick; documented in code comment
- [ ] Completion detection (from drain-cleanup US-005) fires once when sum across ALL shards transitions to 0 — track `CompletionFiredAt` at the merged level (in `progressCache[clusterID].mergedCompletionFiredAt` field)
- [ ] Unit test: 3 shards write per-shard snapshots `{Migratable:10, Migratable:20, Migratable:30}` → MergedSnapshot returns `Migratable:60`
- [ ] Unit test: 3 shards transition sum from >0 to 0 across multiple ticks → completion fires exactly once; new chunks land then drain again → completion fires twice (regression coverage)
- [ ] Unit test: byBucket maps from per-shard snapshots merge correctly (no duplicate keys since each bucket lives in exactly one shard)
- [ ] Integration test (memory backend, simulated multi-shard scan): seed buckets with chunks on a draining cluster, run scan from 3 shards, assert MergedSnapshot matches the full-scan result from Phase 1
- [ ] `go vet ./...` passes; typecheck passes; tests pass

### US-004: lab-tikv-3 multi-leader bench + published numbers
**Description:** As an operator, I need a measured bench against the lab-tikv-3 stack proving the shard fan-out gives roughly linear throughput improvement.

**Acceptance Criteria:**
- [ ] New bench harness `scripts/bench-rebalance-multi.sh` mirrors `scripts/multi-replica-smoke.sh` shape: spins up `lab-tikv-3` compose profile (existing 3-replica TiKV-backed stack via `--profile lab-tikv-3`); plants N=1000 buckets each with 10 chunks = 10k chunks on cluster `default` via `strata admin bench-gc` style harness
- [ ] Seeds buckets with `Placement={default:1, cephb:1}` (multi-cluster policy so all chunks are migratable on drain)
- [ ] Drains `default` evacuate mode; measures wall-clock from POST /drain submission until `chunks_on_cluster=0`
- [ ] Runs once with `STRATA_REBALANCE_SHARDS=1` (baseline) + once with `STRATA_REBALANCE_SHARDS=3`; prints two timings and the ratio
- [ ] Expected: SHARDS=3 wall-clock ≤ 40% of SHARDS=1 wall-clock (≥2.5× speedup); not strictly 3× because each replica also competes for the same target cluster's RADOS write bandwidth
- [ ] New `make bench-rebalance-multi` Makefile target wraps the script
- [ ] Bench docs published at `docs/site/content/architecture/benchmarks/rebalance.md` — table with SHARDS column vs wall-clock seconds + curve summary; reference back to gc/lifecycle Phase 2 benchmark for comparison context
- [ ] Numbers captured in ROADMAP close-flip narrative (US-005)
- [ ] Bench script EXITS NON-ZERO if SHARDS=3 wall-clock > 70% of SHARDS=1 (regression guard)
- [ ] `go vet ./...` passes (bench script is bash; the seeding harness is the existing `strata admin bench-gc` style program — no new Go binary)

### US-005: Smoke + docs + ROADMAP close-flip + PRD removal
**Description:** As an operator and as a future-maintainer, I need every change validated end-to-end against the lab + documented so I trust the cycle is actually done.

**Acceptance Criteria:**
- [ ] New `scripts/smoke-rebalance-scale.sh` covers:
  - **Scenario A (single-replica fan-out):** start single strata replica with `STRATA_REBALANCE_SHARDS=3`; verify worker boots 3 shard goroutines (grep log "rebalance: starting" with shard=0,1,2 across one process); seed 30 buckets; drain a cluster evacuate; assert migration completes; assert exactly 1 `leader_for=rebalance acquired=true` heartbeat event (folded)
  - **Scenario B (lab-tikv-3 multi-leader):** start lab-tikv-3 (3 replicas) with `STRATA_REBALANCE_SHARDS=3`; verify 3 leases distributed (one per replica via Cassandra `worker_locks` table inspection); seed 30 buckets; drain a cluster evacuate; assert each replica's worker log shows scans only for `shardID == myShardID`
  - **Scenario C (back-compat SHARDS=1):** start single replica with `STRATA_REBALANCE_SHARDS=1`; verify exactly 1 `rebalance-leader-0` lease; iteration log identical to legacy single-leader scan
  - **Scenario D (replica failover):** kill 1 replica of lab-tikv-3; surviving 2 replicas acquire the freed lease within `STRATA_GC_LEASE_TTL`; bench continues; on bring-up of killed replica, ownership rebalances
- [ ] `make smoke-rebalance-scale` Makefile target wraps the script
- [ ] Per-step `echo "==> Step N: ..."` lines; script EXITS NON-ZERO on any failure
- [ ] `docs/site/content/best-practices/placement-rebalance.md` gains new "Multi-leader scaling" subsection: STRATA_REBALANCE_SHARDS env doc + lab-tikv-3 bench reference + ownership distribution explanation
- [ ] `docs/site/content/architecture/benchmarks/rebalance.md` (created in US-004) referenced from operator runbook
- [ ] Project root `CLAUDE.md` "Background workers" section gets updated rebalance bullet: describes the new fan-out (`SkipLease: true` + per-shard leases + `STRATA_REBALANCE_SHARDS` env + folded heartbeat chip)
- [ ] `ROADMAP.md` close-flip the P2 entry → `~~**P2 — Rebalance worker not sharded — single goroutine bottleneck on large deploys.**~~ — **Done.** <one-line summary referencing the lab-tikv-3 bench result and STRATA_REBALANCE_SHARDS env>. (commit \`<pending>\`)`; closing SHA backfilled in follow-up commit on main
- [ ] `tasks/prd-rebalance-scale-phase-2.md` REMOVED in the same commit (PRD lifecycle rule)
- [ ] `make docs-build` succeeds; `make vet` succeeds; `make test` succeeds; `pnpm run build` succeeds; `make smoke-rebalance-scale` passes; `make bench-rebalance-multi` runs (may take >5min, OK)
- [ ] Typecheck passes

## Functional Requirements

- **FR-1:** `STRATA_REBALANCE_SHARDS` env (default 1, range [1, 1024], clamp + WARN on out-of-range)
- **FR-2:** Worker registered with `SkipLease: true`
- **FR-3:** Per-shard leases `rebalance-leader-0..N-1`
- **FR-4:** `meta.Store.ListBucketsShard(ctx, shardID, totalShards)` returns disjoint bucket subsets
- **FR-5:** Each replica races to acquire 1+ leases; multi-lease ownership = fan-out within replica
- **FR-6:** Worker iteration uses `ListBucketsShard` instead of `ListBuckets`
- **FR-7:** Per-shard panic recovery + exponential backoff; metric `strata_worker_panic_total{worker="rebalance",shard="<i>"}` incremented
- **FR-8:** `leader_for=rebalance` chip emits once per replica regardless of multi-shard ownership (folded)
- **FR-9:** ProgressTracker stores per-shard snapshots; Snapshot() merges sums on read
- **FR-10:** Completion fires once when sum across all shards transitions to 0
- **FR-11:** `/drain-progress` returns merged counts — operator UX unchanged
- **FR-12:** lab-tikv-3 bench publishes numbers showing ≥2.5× speedup at SHARDS=3
- **FR-13:** Single-replica SHARDS=1 reproduces Phase 1 byte-for-byte
- **FR-14:** ROADMAP close-flip + 4-scenario smoke + docs published

## Non-Goals

- **No dynamic shard count.** `STRATA_REBALANCE_SHARDS` read once at boot. Operator must restart all replicas to change. (Same as gc/lifecycle Phase 2 — established pattern.)
- **No cross-shard work stealing.** Each shard owns its bucket subset; if shard 0 finishes early it doesn't steal work from shard 1. Acceptable because all shards complete within similar time on a balanced bucket distribution.
- **No /drain-impact sharding.** That endpoint runs a synchronous one-off scan from the admin handler — already fast (operator-driven, not hot path). Keep full ListBuckets there.
- **No reconcile worker for missed shards.** If a replica dies mid-scan, the surviving replicas eventually pick up that shard's lease and scan it on next tick. No catch-up worker.
- **No UI changes for shard observability.** Operator runbook documents how to verify shard distribution via `worker_locks` table inspection; UI doesn't surface per-shard breakdown.
- **No bench-gc-style standalone CLI tool for rebalance bench.** Bench is a bash harness driving normal PUT/drain/observe paths; no new `strata admin bench-rebalance` subcommand (out of scope; could be P3 later if operators want self-serve bench).
- **No reordering across cycles.** drain-followup US-005 denormalize, drain-cleanup US-006 deregister_ready, effective-placement EffectivePolicy — all unchanged. This cycle composes with them, doesn't refactor.

## Design Considerations

- **`ShardedFanOut` shape mirrors `internal/gc/fanout.go`** — same field names, same goroutine spawn pattern, same panic recovery + emit-leader folding. Code reviewer should be able to diff the two impls side-by-side and see structural parity.
- **Worker register signature change** (`SkipLease: false` → `true`) requires the supervisor to NOT acquire an outer lease — verify by code-walking `cmd/strata/workers/supervisor.go` to confirm the SkipLease path delegates entirely to the Runner.
- **Per-shard span attributes** (`strata.rebalance.shard=<i>`) match existing observability conventions (gc worker emits `strata.gc.shard` per-shard spans).
- **MergedSnapshot called per `/drain-progress` request** — O(shards) cost per request, negligible (max 1024). Cache layer above this if profiling shows hotspot.
- **CompletionFiredAt at merged level**: prevents double-fire if 3 shards happen to all hit 0 at the same tick. Single tracker variable, single transition gate.

## Technical Considerations

- **`fnv32a(bucket.ID)` hash determinism**: bucket.ID is a uuid.UUID; serialise via `bucket.ID.String()` before hashing (existing pattern in `placement.PickCluster`). Verify consistency between ListBucketsShard hash and any future shard-aware caller.
- **Cassandra paging** for ListBucketsShard: paging size 1000 (existing default in ListBuckets); each page filtered in-process; no schema change. For very-large bucket counts (100k+), this is O(N) per shard scan but bounded by paging memory.
- **TiKV scan**: ListBucketsShard does a prefix scan on `s/bk/`, filters in-process by hash. Acceptable; TiKV's range scan throughput is high.
- **Memory backend** doesn't suffer scale issues; impl is straightforward filter.
- **Multi-replica race on `rebalance-leader-N` lease**: existing `leader.Session` (used by gc fan-out) handles this; lease TTL controlled by `STRATA_GC_LEASE_TTL` env (shared across all workers).
- **Bench environment**: lab-tikv-3 uses TiKV meta + RADOS data with 3 strata replicas. Existing `compose --profile lab-tikv-3` brings up the stack (verify in `deploy/docker/docker-compose.yml`). Bench should plant data via aws-cli with admin credentials.
- **Race-soak / nightly CI**: existing race-soak workflow may flake on the new multi-leader code path; verify smoke + race tests pass on first cycle merge.

## Success Metrics

- lab-tikv-3 bench at SHARDS=3 measures wall-clock ≤ 40% of SHARDS=1 baseline (≥2.5× speedup)
- Single-replica SHARDS=1 reproduces Phase 1 worker log + completion timing byte-for-byte
- 4-scenario smoke green
- Zero regression in existing drain UX (Playwright specs unchanged + still pass)
- 1 ROADMAP P2 entry closed in single commit

## Open Questions

- Should the lab-tikv-3 bench also measure SHARDS=10 to show diminishing returns? Recommendation: defer to P3 follow-up — first cycle establishes the curve at 1/3; extended sweep is nice-to-have.
- Should `STRATA_REBALANCE_SHARDS` and `STRATA_GC_SHARDS` be unified into a single env? Recommendation: no — keep per-worker for independent operator tuning (matches established gc/lifecycle separation in Phase 2 cycle).
- Should the worker emit a per-shard span (`worker.rebalance.shard<i>.tick`) instead of the existing per-tick span? Recommendation: keep per-tick span with `strata.rebalance.shard` attribute — existing observability conventions; querying by attribute is supported.
