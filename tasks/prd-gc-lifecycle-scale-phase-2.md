# PRD: gc / lifecycle scale — Phase 2 (sharded leader-election)

## Introduction

Phase 1 (commit `6561845`) lifted the per-leader concurrency cap via bounded errgroups
(`STRATA_GC_CONCURRENCY` / `STRATA_LIFECYCLE_CONCURRENCY`). Lab numbers: gc 11108→100275
chunks/s (c=1→c=256, knee at c=64); lifecycle 485→9150 obj/s (c=1→c=256, no knee in swept
range). Beyond that, the leader itself is single-replica — every other gateway in a
multi-replica deploy idles on the gc / lifecycle workers.

Phase 2 shards the leader-election space so multiple replicas process disjoint slices
in parallel. Target prod scale: 10k object PUTs/s × ~4 chunks/object = 40k chunks/s
sustained churn, which saturates the single-leader cap even at c=256.

## Goals

- Multi-leader gc: replicas grab one of `gc-leader-0..N-1` leases (`STRATA_GC_SHARDS=N`,
  default 1) and process disjoint shards of the GC entry queue.
- Multi-leader lifecycle: replicas process disjoint subsets of buckets via
  `hash(bucketID) % replicaCount` filter; per-bucket lease (`lifecycle-leader-<bucketID>`)
  guards individual bucket walks.
- New `Meta.ListGCEntriesShard(ctx, region, shardID, shardCount, batch)` API with
  server-side shard filtering — readers fetch only their disjoint partition, no
  over-fetch.
- Backwards-compat: existing single-replica operators see no change at default
  `STRATA_GC_SHARDS=1`. Code path is always shard-aware with `shardCount=1` so there is
  one code branch.
- Quantify the multi-leader curve: rerun `strata-admin bench-gc` / `bench-lifecycle` on
  a 3-replica stack and capture the throughput multiplier vs Phase 1 baseline.

## User Stories

### US-001: Add `shard_id` to GC entry schema + `ListGCEntriesShard` on memory backend
**Description:** As a developer, I need the GC entry queue keyed on shard so disjoint
readers fetch their partition directly without over-fetch.

**Acceptance Criteria:**
- [ ] `gc_entries` schema gains `shard_id int` column; partition key includes
  `(region, shard_id)` so each shard is its own partition. `shard_id` derived as
  `fnv32a(oid) % 1024` at write time (1024 logical shards, mapped to N runtime shards
  via `shard_id % shardCount` at read time so operators can change `STRATA_GC_SHARDS`
  without re-keying).
- [ ] New `Meta.ListGCEntriesShard(ctx, region, shardID, shardCount, batch) ([]GCEntry, error)`
  method on the `meta.Store` interface.
- [ ] Memory backend implements: walks in-memory entries, returns only those with
  `entry.ShardID % shardCount == shardID`. Existing `ListGCEntries` rewires to call
  `ListGCEntriesShard(ctx, region, 0, 1, batch)` so the legacy entry point stays.
- [ ] `EnqueueGCEntry` computes shard_id at write time on memory backend.
- [ ] Contract test in `internal/meta/storetest/contract.go` for `ListGCEntriesShard`:
  insert 1000 entries, query with shardCount=4 across shardID=0..3, assert union
  equals full set + each subset is disjoint + each subset size is roughly 250 ± 25%.
- [ ] Contract test runs against memory backend.
- [ ] Typecheck passes (`go vet ./...`); unit tests pass (`go test ./...`).

### US-002: `ListGCEntriesShard` on Cassandra backend
**Description:** As a developer, I need the same shard-filter API on the production
Cassandra backend with no destructive migration.

**Acceptance Criteria:**
- [ ] Cassandra schema migration in `internal/meta/cassandra/schema.go`:
  `alterStatements` adds `shard_id int` to `gc_entries`. Existing rows get
  `shard_id=0`; lazy-rehash on write is acceptable (long-running clusters expire
  pre-migration rows naturally via grace window).
- [ ] **Partition key change requires new table.** Create `gc_entries_v2` with
  `PRIMARY KEY ((region, shard_id), enqueued_at, oid)` and dual-write window:
  writers write to both `gc_entries` and `gc_entries_v2` until `STRATA_GC_DUAL_WRITE=off`
  is flipped; readers prefer v2 with v1 fallback. Default `STRATA_GC_DUAL_WRITE=on`
  during this cycle; close-flip story documents the operator cutover.
- [ ] Cassandra `ListGCEntriesShard` queries the single partition `(region, shard_id)`
  and returns rows with `shard_id % shardCount == shardID` (server-side equality on
  shard_id when `shardCount=1024`; app-side modulo when `shardCount<1024`).
- [ ] `EnqueueGCEntry` computes `shard_id = fnv32a(oid) % 1024` at write time.
- [ ] Integration test (build tag `integration`): contract suite runs green on
  Cassandra container.
- [ ] `make test-integration` passes.
- [ ] Typecheck + unit tests pass.

### US-003: `ListGCEntriesShard` on TiKV backend
**Description:** As a developer, I need the same shard-filter API on the TiKV backend
using key-prefix filtering for native ordered range scans.

**Acceptance Criteria:**
- [ ] TiKV key shape changes from `gc/<region>/<oid>` to `gc/<region>/<shardID2BE>/<oid>`
  where `shardID2BE` is a fixed 2-byte BE encoding of `fnv32a(oid) % 1024`. Forward-compat
  via dual-key-write window mirroring the Cassandra story (default
  `STRATA_GC_DUAL_WRITE=on`).
- [ ] `ListGCEntriesShard` does range-scan on prefix `gc/<region>/<shardID2BE>/` for each
  logical shard `shard_id` where `shard_id % shardCount == shardID`. With `shardCount=1`
  scan all 1024 prefixes; with `shardCount=4` scan 256 prefixes; with `shardCount=1024`
  scan exactly 1.
- [ ] `EnqueueGCEntry` writes both legacy + new key during dual-write window.
- [ ] Integration test against PD+TiKV containers (CI workflow `ci-tikv.yml`) passes.
- [ ] Typecheck + unit tests pass.

### US-004: Multi-leader gc worker (STRATA_GC_SHARDS)
**Description:** As an operator running multiple gateway replicas, I want each replica
to grab its own gc shard lease so they process the queue in parallel.

**Acceptance Criteria:**
- [ ] `cmd/strata/workers/gc.go` reads `STRATA_GC_SHARDS` (int, default 1, range 1..1024).
- [ ] On supervisor start, gc spawns N goroutines (one per shard 0..N-1); each runs the
  existing leader-election loop against `gc-leader-<shardID>` instead of the legacy
  `gc-leader` key. A replica may hold zero, one, or multiple shards depending on
  contention.
- [ ] `gc.Worker.drainCount` invocation calls
  `meta.ListGCEntriesShard(ctx, region, shardID, shardCount, batch)` (always — even at
  shardCount=1; the legacy `gc-leader` key is retired).
- [ ] Single-replica deploy at `STRATA_GC_SHARDS=1` is functionally identical to Phase 1
  (one shard, one leader, one drainer). Smoke run via `make smoke` passes.
- [ ] Three-replica deploy at `STRATA_GC_SHARDS=3` distributes shards across replicas
  (verified by integration test — three supervisors race for `gc-leader-0..2`, asserts
  each lease is held by exactly one supervisor at any time).
- [ ] Panic in shard-N goroutine releases shard-N lease only; sibling shards keep
  draining. Existing `strata_worker_panic_total{worker=gc}` metric gains `shard` label.
- [ ] Typecheck + unit tests + integration tests pass.

### US-005: Per-bucket lifecycle lease (`lifecycle-leader-<bucketID>`)
**Description:** As an operator running multiple gateway replicas, I want lifecycle
work to fan out per bucket so each replica owns a strict subset.

**Acceptance Criteria:**
- [ ] `lifecycle.Worker` walks all buckets, then for each bucket attempts
  `locker.Acquire(ctx, "lifecycle-leader-<bucketID>", ...)` non-blocking. Buckets where
  acquisition fails are skipped (another replica owns it). The legacy `lifecycle-leader`
  global lease is retired.
- [ ] Distribution gate: replica only attempts `Acquire` if
  `fnv32a(bucketID) % replicaCount == myReplicaID`. `replicaCount` and `myReplicaID`
  come from the same membership pool used by gc shards (i.e. replica's smallest gc shard
  index it holds is `myReplicaID`; `replicaCount = STRATA_GC_SHARDS`). When
  `STRATA_GC_SHARDS=1`, every replica passes the filter and the bucket-level lease alone
  serializes work — identical behaviour to Phase 1.
- [ ] On lease release between cycles, sibling replica picks up the bucket on its next
  iteration — no permanent ownership.
- [ ] Three-replica integration test: 9 buckets, each lifecycle-eligible; assert each
  bucket processed exactly once per cycle, distributed roughly 3-3-3 across replicas.
- [ ] Typecheck + unit tests + integration tests pass.

### US-006: Bench rerun on 3-replica stack — multi-leader curve
**Description:** As an operator evaluating Phase 2, I need the bench harness numbers to
quantify the throughput multiplier vs Phase 1.

**Acceptance Criteria:**
- [ ] `docs/benchmarks/gc-lifecycle.md` gains a "Phase 2 — multi-leader" section.
- [ ] Bench harness rerun on lab-tikv stack with 3 strata replicas at
  `STRATA_GC_SHARDS=3`, `STRATA_GC_CONCURRENCY=64`. Capture throughput at
  `c=1, 4, 16, 64, 256` for both gc and lifecycle. Per-replica numbers + aggregate.
- [ ] Comparison table: Phase 1 (single-replica, c=64 default) vs Phase 2 (3-replica,
  shards=3, c=64 per replica). Expected multiplier: 2.5–3× on gc, similar on lifecycle.
- [ ] Document the cap shape — at what point does the gc queue become the bottleneck
  vs the data backend.
- [ ] Operator-facing recommendation: "scale STRATA_GC_SHARDS with replica count up to
  bucket-shard cardinality limit" + concrete defaults (e.g. `STRATA_GC_SHARDS=3` for
  3-replica deploy).
- [ ] Bench JSON artifact committed under `docs/benchmarks/data/gc-lifecycle-phase-2/`.

### US-007: Docs + ROADMAP close-flip
**Description:** As a maintainer, I need the ROADMAP entry flipped Done with the
measured multipliers and a pointer to the bench doc, plus the Phase 1 → Phase 2
operator migration steps documented.

**Acceptance Criteria:**
- [ ] `ROADMAP.md` line 176 (current P1 — gc/lifecycle Phase 2) flipped to
  `~~**P1 — gc / lifecycle Phase 2 — sharded leader-election.**~~ — **Done.** <one-line
  summary citing measured multiplier from US-006>. (commit \`<pending>\`)`.
- [ ] `docs/migrations/binary-consolidation.md` gains `STRATA_GC_SHARDS` env entry
  alongside the existing `STRATA_GC_CONCURRENCY` / `STRATA_LIFECYCLE_CONCURRENCY` block.
- [ ] New operator doc `docs/migrations/gc-lifecycle-phase-2.md` covers: dual-write
  window for gc_entries_v2 (Cassandra) + new TiKV key shape, monitoring
  `strata_gc_dual_write_lag_seconds` (or equivalent), the operator cutover step
  (`STRATA_GC_DUAL_WRITE=off` after queue drains), rollback shape if Phase 2 needs to
  be reverted.
- [ ] No regressions: `go vet ./...`, `go test ./...`, `make smoke` all clean.
- [ ] `tasks/prd-gc-lifecycle-scale-phase-2.md` REMOVED in this commit per CLAUDE.md
  PRD lifecycle rule.
- [ ] Closing-SHA backfill follow-up commit on main (mirrors Phase 1 / multi-replica
  shape).

## Functional Requirements

- FR-1: `meta.Store` interface gains `ListGCEntriesShard(ctx, region, shardID, shardCount, batch)`.
- FR-2: Memory, Cassandra, TiKV backends all implement the new method with shard-filter
  semantics that match (assertion: contract test runs on all three).
- FR-3: GC entry write path computes `shard_id = fnv32a(oid) % 1024` at enqueue time.
- FR-4: `STRATA_GC_SHARDS=N` (int, default 1, range 1..1024) controls how many
  `gc-leader-<i>` lease keys exist; gc supervisor spawns N drain goroutines.
- FR-5: Lifecycle worker uses per-bucket lease `lifecycle-leader-<bucketID>` and
  pre-filters via `fnv32a(bucketID) % replicaCount == myReplicaID`.
- FR-6: `myReplicaID` derived as the smallest gc shard index the replica holds;
  `replicaCount = STRATA_GC_SHARDS`. (Lifecycle and gc share the same shard-membership
  signal — no second config knob.)
- FR-7: `STRATA_GC_DUAL_WRITE` env (default `on`) enables dual-write of gc entries
  during Cassandra/TiKV schema migration. `off` after operator-driven cutover.
- FR-8: Cassandra: `gc_entries_v2` table with `((region, shard_id), enqueued_at, oid)`
  primary key; TiKV: `gc/<region>/<shardID2BE>/<oid>` key shape. Both additive.
- FR-9: Bench harness (`strata-admin bench-gc` / `bench-lifecycle` from Phase 1)
  unchanged — Phase 2 reuses the same harness from a 3-replica stack.

## Non-Goals

- No automatic shard-count tuning; `STRATA_GC_SHARDS` stays operator-set.
- No reshard-while-running primitive (the 1024 logical shard fan-out covers the
  practical operator range; resharding the *physical* shard count requires a queue
  drain + restart).
- No notification / replication / access-log / inventory worker sharding (Phase 2 is
  scoped to gc + lifecycle only — those were the cap discovered in Phase 1 bench).
- No bench harness changes beyond the new Phase 2 section in
  `docs/benchmarks/gc-lifecycle.md`.
- No removal of legacy `gc-leader` lease key in this cycle — drop in a follow-up
  cleanup once all production deploys have flipped to multi-leader. (Documented in
  the migration doc as a future cleanup.)

## Technical Considerations

- **`shard_id` is a 1024-wide logical fan-out, not the runtime shard count.** Operators
  set `STRATA_GC_SHARDS` to the runtime count (typically replica count, 1..16); the
  app maps logical → runtime via `shard_id % runtime`. This decouples the on-disk
  partitioning from the operator's deployment shape so changing replica count does
  not require a queue drain.
- **`fnv32a` is the shard hash for both gc entries and lifecycle bucket distribution.**
  Stable, fast, non-cryptographic. Same hash on both sides keeps the
  shard-membership signal coherent.
- **Cassandra partition rebuild is irreversible.** Ship `gc_entries_v2` as a fresh
  table + dual-write window per CLAUDE.md "schema migrations are additive" rule.
  `gc_entries` (v1) stays read-fallback during the dual-write window.
- **TiKV key shape change is a write-time switch** — readers do range-scan on the new
  prefix; old keys live until lifecycle expiration grace window. Bridging via
  dual-write mirrors the Cassandra story for operator parity.
- **Membership coordination piggybacks on gc lease holdings.** No new
  `replica_set` lease pool — adding one would be a second coordination primitive to
  maintain. The smallest gc-shard a replica holds is its `myReplicaID`; replicas
  holding zero gc shards (transient, between leases) skip lifecycle work that cycle —
  acceptable since lifecycle iterates per-cycle anyway.
- **Per-bucket lifecycle lease is *cheap* on the underlying lock primitives.**
  Cassandra LWT lease + TiKV LWT lease both scale linearly with bucket count. At
  ~10k buckets per deploy with a 5min lifecycle interval, lease churn is ~33/s,
  inside both backends' load capacity.
- **Single-replica is the default.** `STRATA_GC_SHARDS=1` means one shard, one
  leader, one drainer — same wall-clock behavior as Phase 1. Code path is unified
  (always shard-aware, just with `shardCount=1`); no two diverging code branches.

## Success Metrics

- **3-replica multi-leader gc throughput ≥ 2.5× Phase 1 single-replica baseline** at
  matched concurrency (c=64 per replica). Current baseline 100275 chunks/s; target
  ≥250000 chunks/s aggregate.
- **3-replica lifecycle throughput ≥ 2.5× Phase 1 baseline.** Current baseline 9150
  obj/s at c=256; target ≥22000 obj/s aggregate at the same per-replica concurrency.
- **Zero regressions on single-replica deploys** (`STRATA_GC_SHARDS=1`): smoke + bench
  numbers within 5% of Phase 1.
- **Operator migration is zero-downtime:** dual-write window allows Phase 1 → Phase 2
  flip without queue drain or restart-stop.

## Open Questions

- At what `STRATA_GC_SHARDS` setting does Cassandra LWT contention on
  `gc-leader-<shardID>` start dominating? Bench it.
- Should the dual-write window have an automatic close trigger (e.g. "all v1 rows
  expired via grace") or stay operator-driven? Default to operator-driven for safety.
- Do we want a fast-fail check on bucket count vs replica count? If replicaCount=10
  and bucketCount=8, two replicas idle every lifecycle cycle. Probably fine — log a
  WARN once on startup, no hard error.
