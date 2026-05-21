# PRD: P1 fixes — bucket_stats RMW saturation + RGW lab bootstrap

## Introduction

`ralph/rgw-benchmarks` cycle (closed in commit `d8e6ab3`) surfaced two
P1 entries + one P3 follow-up:

- **P1 (line 106)**: `bucket_stats` RMW saturates the per-bucket TiKV
  key under concurrent PUT/DELETE — pessimistic-txn retry tornado at
  c≥8.
- **P1 (line 117)**: RGW `bench-rgw` lab bootstrap fragile post-
  restart — `period update --commit` EOVERFLOW + zone missing.
- **P3 (line 282)**: rgw-benchmarks follow-up — measured numbers +
  bucket-index claim verdict. Previous bench couldn't complete due to
  the two P1s.

This cycle bundles all three into one pass. After this cycle the
README "drop-in RGW replacement" claim has complete bench numbers +
the bucket-index claim is verdict'd with data.

**Pre-launch product** per [Pre-launch no deploys] memory — hard
cutover on `bucket_stats` key shape, no migration backfill.

Branch: `ralph/p1-fixes`. Starts from `main`. **9 stories.**

## Goals

- Eliminate `bucket_stats` per-bucket TiKV-key RMW saturation. Default
  fix shape: **per-shard fan-out** (option b from ROADMAP listing) —
  hard-coded 8 shards, no env knob, no per-bucket override.
- Best-effort BatchGet across shards for read; quotareconcile worker
  fixes any cross-shard drift (existing reconcile path already
  validates bucket_stats totals).
- Hard cutover from old `s/B/<bid>/bs` key shape to new
  `s/B/<bid>/bs/<shard>` — pre-launch, no fixture migration.
- Fix RGW lab bootstrap post-restart fragility: entrypoint reconciles
  zonegroup → zone membership inside the period BEFORE
  `period update --commit`. Logs zone-count-per-period for
  diagnostics.
- Rerun the full ralph/rgw-benchmarks sweep (8 workloads ×
  concurrency × 3 runs) with both P1s fixed. ~120 min full sweep.
- Update `docs/site/content/architecture/benchmarks/rgw-comparison.md`
  with complete numbers + headline verdict on bucket-index claim.
- README features-vs-RGW backfill with updated numbers (≤120 lines
  guard preserved from `ralph/readme-docs-rewrite` US-001).

## User Journey

Three personas covered:

- **Operator running a high-concurrency bucket.** Today: PUT/DELETE
  at c=8+ hits write-conflict tornado on `bucket_stats`. After cycle:
  per-shard fan-out absorbs concurrent writes; no retry storm.
- **Maintainer rerunning bench harness.** Today: second
  `make up-bench-rgw` after `make down` fails with EOVERFLOW; must
  `docker volume rm strata-bench-creds`. After cycle: idempotent
  period reconcile makes restarts safe.
- **Maintainer defending README RGW claim.** Today: bench numbers
  incomplete (c=128 + delete + iam-auth aborted). After cycle:
  complete numbers in rgw-comparison.md + bucket-index verdict.

## User Stories

### US-001: bucket_stats fix scoping spike — Cassandra probe + decision tree

**Description:** As an implementer, I want a brief design spike that
includes a Cassandra concurrency probe — the PRD review found that
Cassandra's `BumpBucketStats` is **also LWT RMW with bounded retry
(maxAttempts=32)**, NOT a counter table. Spike outcome decides
whether fan-out applies to TiKV only or to BOTH backends.

**Acceptance Criteria:**
- [ ] Read existing `bucket_stats` impls (verified paths):
      `internal/meta/tikv/bucket_stats.go` (BumpBucketStats line 88
      / GetBucketStats line 63 — single per-bucket key `s/B/<bid>/bs`
      with pessimistic txn).
      `internal/meta/cassandra/store.go` (BumpBucketStats line 3447
      — **LWT CAS loop, maxAttempts=32**, NOT a counter table per
      schema.go:226-231 bigint columns).
      `internal/meta/memory/store.go` (sync.RWMutex on in-struct
      counter — no RMW contention beyond mutex; in-process serial).
- [ ] **Cassandra concurrency probe**: run 100 concurrent
      `BumpBucketStats(+1)` against Cassandra testcontainer.
      Capture: final count (= 100 if no lost updates), number of
      CAS retries observed, any CAS-exhausted errors (caller hit
      maxAttempts=32 ceiling). Sample via `gocql` query observer or
      attempt counter at the impl layer.
- [ ] **Decision tree** based on Cassandra probe outcome:
      (a) Cassandra final=100 + zero CAS-exhausted at c=100 →
          Cassandra OK with bounded retry. Fan-out **TiKV only**.
      (b) Cassandra final<100 OR CAS-exhausted observed at c=100 →
          Cassandra **also needs** fan-out. PRD scope grows: US-003
          becomes Cassandra-side fan-out (new sibling table
          `bucket_stats_shard` OR row-key extension `(bucket_id,
          shard)` PK). Cycle becomes 10 stories (US-001 + new
          US-002b for Cassandra + renumber).
      (c) Cassandra fails differently (deadlock, timeout, not
          retry-exhausted) → escalate; spike doesn't ship — return
          to design.
- [ ] Document the chosen branch (a/b/c) + the Cassandra probe
      numbers in progress.txt as the contract for US-002+ stories.
- [ ] **Shard count**: hard-coded **8** (no env knob, no per-bucket
      override) per scoping decision 1A. Document the choice.
- [ ] **Shard selection**: `fnv32a(uuid.NewString()) % 8` per op —
      uniform distribution + unbiased across PUT/DELETE.
      Alternative `fnv32a(bucketID + op_seq)` rejected (would bias
      hot keys to one shard).
- [ ] **Read shape**: single TiKV `BatchGet` across 8 shard keys +
      sum in-memory. Best-effort consistency — quotareconcile worker
      fixes any drift (existing reconcile path already validates
      bucket_stats totals against manifest scan).
- [ ] **Hard cutover**: pre-launch product — old `s/B/<bid>/bs` key
      shape dropped. Existing CI fixture rows from prior cycles will
      be invalid; expected.
- [ ] No prod code changes in this story — probe code OK as throwaway
      test if needed. Design notes go in progress.txt as contract.
- [ ] **Shard count**: hard-coded **8** (no env knob, no per-bucket
      override) per scoping decision 1A. Document the choice in the
      impl + in the architecture page (US-008 covers doc update).
- [ ] **Shard selection**: `fnv32a(uuid.NewString()) % 8` per op —
      uniform distribution + unbiased across PUT/DELETE.
      Alternative `fnv32a(bucketID + op_seq)` rejected (would bias
      hot keys to one shard).
- [ ] **Read shape**: single TiKV `BatchGet` across 8 shard keys +
      sum in-memory. Best-effort consistency — quotareconcile worker
      fixes any drift (existing reconcile path already validates
      bucket_stats totals against manifest scan).
- [ ] **Hard cutover**: pre-launch product — old `s/B/<bid>/bs` key
      shape dropped. Existing CI fixture rows from prior cycles will
      be invalid; expected.
- [ ] No code changes in this story — design notes go in
      progress.txt as the contract for US-002.
- [ ] Typecheck passes (no code touched — vacuous).
- [ ] Tests pass.

### US-002: TiKV per-shard fan-out impl

**Description:** As an operator running concurrent PUT/DELETE
workloads, I want bucket_stats writes to land on one of 8 sibling
keys (not a single per-bucket key) so the pessimistic-txn retry
tornado disappears.

**Acceptance Criteria:**
- [ ] New constant in `internal/meta/tikv/keys.go` (path verified):
      `bucketStatsShardCount = 8`.
- [ ] Key shape change: `s/B/<bid>/bs` → `s/B/<bid>/bs/<shard>` where
      `<shard>` ∈ `[0..7]` (one-byte raw uint8, NO byte-stuffing
      since fixed-width). Existing `subBucketStats = "bs"` constant
      retained; new `subBucketStatsShard` follows the existing
      sub-key naming convention.
- [ ] **Write path** (`BumpBucketStats` in
      `internal/meta/tikv/bucket_stats.go`, line 88 — verified path,
      NOT `store.go`):
      Pick shard = `fnv32a(uuid.NewString()) % 8` (import
      `hash/fnv` already used in `internal/meta/tikv/list.go` per
      precedent).
      Pessimistic txn per shard key (RMW: read shard counter, add
      delta, write back, commit). Explicit `txn.Rollback()` on every
      non-error early return per CLAUDE.md TiKV gotcha.
- [ ] **Read path** (`GetBucketStats` in
      `internal/meta/tikv/bucket_stats.go`, line 63 — verified path):
      Single `txn.BatchGet(ctx, [s/B/<bid>/bs/0..7])` → decode each
      counter → sum → return `meta.BucketStats`.
      Snapshot read — NO pessimistic txn (read-only).
- [ ] **Hard cutover**: drop the old single-key write path entirely.
      Drop the old single-key read fallback entirely. Compile-time
      delete the old code, don't dead-code-comment it.
- [ ] **Concurrency safety**: 100-goroutine race test against TiKV
      testcontainer — 100 concurrent BumpBucketStats by `+1` each →
      final GetBucketStats reports **exactly 100** (no lost updates)
      AND **zero CAS/txn-exhausted errors** at the impl layer
      (assert via injected error counter / observer hook).
      Both assertions required — `sum=100 with hidden lost updates`
      is correctness regression too.
- [ ] **Metric**: add `strata_bucket_stats_shard_writes_total{shard}`
      counter — operator can see shard hot-spot via Prom (uniform
      distribution = healthy; one shard dominating = hash collision
      pattern).
- [ ] `go vet -tags ceph ./...` + `go vet ./...` both pass.
- [ ] `go test -race ./internal/meta/tikv/...` passes.
- [ ] Typecheck passes; tests pass.

### US-003: Memory + Cassandra strategy per US-001 spike outcome

**Description:** As a maintainer, I want the Cassandra + memory
strategy decided by the US-001 spike outcome — NOT pre-assumed.
Cassandra's `BumpBucketStats` is LWT CAS loop with maxAttempts=32,
NOT a counter table; whether 32-cap absorbs c=128 concurrent
bumps cleanly OR also needs fan-out depends on the spike probe.

**Acceptance Criteria:**
- [ ] **Branch on US-001 spike outcome** (recorded in progress.txt):
      **(a) Cassandra OK** — final=100 + zero CAS-exhausted at
      c=100: this story is no-change confirmation for both backends.
      Memory backend (`internal/meta/memory/store.go`) — sync.RWMutex
      absorbs concurrent bumps in-process; no change.
      Cassandra backend (`internal/meta/cassandra/store.go:3447`
      `BumpBucketStats`) — LWT CAS loop with maxAttempts=32 is
      sufficient at lab c=100; no change. Document the probe
      numbers + 32-cap headroom estimate (e.g. "Cassandra observed
      4 retries at c=100; 8× headroom before 32-cap").
      **(b) Cassandra ALSO needs fan-out** — final<100 OR
      CAS-exhausted observed: this story becomes Cassandra-side
      fan-out impl. Add new table `bucket_stats_shard
      (bucket_id uuid, shard tinyint, used_bytes bigint,
      used_objects bigint, updated_at timestamp, PRIMARY KEY
      ((bucket_id, shard)))` via `alterStatements`. Write path
      picks shard same as TiKV: `fnv32a(uuid.NewString()) % 8`.
      Read path sums via `SELECT * FROM bucket_stats_shard WHERE
      bucket_id = ?`. Old `bucket_stats` table dropped (pre-launch
      hard cutover). PRD cycle grows to 10 stories: US-002b
      (Cassandra fan-out) inserts between US-002 and current
      US-003 — renumber.
      **(c) Cassandra fails differently** — escalate, halt cycle.
- [ ] Contract test in `internal/meta/storetest/contract.go` already
      exercises `BumpBucketStats` + `GetBucketStats` across 3
      backends. Run against TiKV's new fan-out shape (and
      Cassandra's if branch b fired) — must pass without
      modifications.
- [ ] If contract test exposes a behavioral difference between
      backends (e.g. read-after-write consistency expectations), the
      test is the source of truth — adjust impl, not the test.
- [ ] Add new contract case `BucketStatsConcurrentBumps`: 50
      concurrent `Bump(+1)` → assert final `Get` reports 50 + zero
      CAS-exhausted errors. Runs against all 3 backends.
- [ ] Contract test in `internal/meta/storetest/contract.go` already
      exercises `BumpBucketStats` + `GetBucketStats` across 3
      backends. Run against TiKV's new fan-out shape — must pass
      without modifications.
- [ ] If contract test exposes a behavioral difference between
      backends (e.g. read-after-write consistency expectations), the
      test is the source of truth — adjust TiKV impl, not the test.
- [ ] Add new contract case `BucketStatsConcurrentBumps`: 50
      concurrent `Bump(+1)` → assert final `Get` reports 50. Runs
      against all 3 backends.
- [ ] `go vet ./...` passes.
- [ ] `go test -race ./internal/meta/...` passes.
- [ ] Typecheck passes; tests pass.

### US-004: Bench-validate `bucket_stats` saturation gone

**Description:** As a maintainer, I want empirical confirmation that
the fix actually removes the pessimistic-txn retry tornado — by
rerunning the failing legs from `ralph/rgw-benchmarks` US-007 +
US-008.

**Acceptance Criteria:**
- [ ] **Failing leg #1**: `ralph/rgw-benchmarks` US-007 list 100k
      seed phase @ c=8 — previously aborted with write-conflict on
      Strata. Rerun via:
      ```
      bash scripts/bench-rgw-comparison.sh list strata
      ```
      Expected: seed phase completes without abort; the 100 paginated
      ListObjectsV2 calls return clean numbers.
- [ ] **Failing leg #2**: `ralph/rgw-benchmarks` US-008 delete + iam-
      auth — previously HEALTH_WARN slow ops on the OSD. Rerun via:
      ```
      bash scripts/bench-rgw-comparison.sh delete strata
      bash scripts/bench-rgw-comparison.sh iam-auth strata
      ```
      Expected: both complete; ceph cluster stays HEALTH_OK
      throughout.
- [ ] **Saturation metric check** during rerun: grep prom for
      `strata_bucket_stats_shard_writes_total` — uniform distribution
      across 8 shards (each shard within ±20% of mean).
- [ ] **HEALTH_OK assertion**: `docker exec strata-ceph ceph health
      detail` returns `HEALTH_OK` (or only ignorable warnings — not
      slow ops) at the end of the bench.
- [ ] Capture jsonl + brief markdown table in progress.txt — compare
      pre-fix vs post-fix p99 numbers for these two workloads.
- [ ] Typecheck passes; tests pass.

### US-005: RGW lab bootstrap idempotent period reconcile

**Description:** As a maintainer, I want
`make down && make up-all && make up-bench-rgw` to succeed
repeatedly without manual `docker volume rm` between cycles.

**Acceptance Criteria:**
- [ ] **Investigate first**: capture `radosgw-admin period get` JSON
      after the first successful `make up-bench-rgw`, then after the
      failing second `make up-bench-rgw`. Diff the two JSON
      structures — identify which field is drift. Document the
      finding inline in the entrypoint script header comment.
- [ ] **Fix in `deploy/docker/rgw-bootstrap/rgw-entrypoint.sh`**:
      Add a new step BEFORE `period update --commit`:
      `radosgw-admin period get` → parse → assert each
      zonegroup ID in the period contains the expected zone
      ID. If a zonegroup is missing the zone, call
      `radosgw-admin zonegroup modify --rgw-zonegroup=<id>
      --add-zone=<zoneid>` (or equivalent — research the exact
      reconcile command).
- [ ] **Log line**: `strata-rgw-bootstrap: period reconcile —
      zonegroups=<N>, zones-per-zonegroup=[<id1>:<n1>, <id2>:<n2>]`
      so the operator can see the period state at startup.
- [ ] **Test**: `make down && make up-all && make up-bench-rgw`
      sequence × 3 cycles (no `docker volume rm` between) — all 3
      cycles succeed; RGW container reaches healthy state.
- [ ] Capture the 3-cycle log output in progress.txt — the
      "period reconcile" line should appear on cycle 2 + cycle 3
      reporting consistent state.
- [ ] `make up-bench-rgw` on a fresh box (no prior state) still
      works (idempotent reconcile must not assume prior state).
- [ ] `make smoke-tikv-default-lab` still passes — RGW changes must
      not affect bare default.
- [ ] Typecheck passes; tests pass.

### US-006: RGW lab restart smoke harness

**Description:** As a maintainer, I want an explicit smoke harness
that asserts the 3× restart cycle works — so future regressions are
caught by `make smoke-*` not by ops-time discovery.

**Acceptance Criteria:**
- [ ] New script `scripts/smoke-rgw-lab-restart.sh`:
      Loop 3 iterations: `make up-bench-rgw` → `make wait-rgw` →
      `aws s3 ls` (returns empty) → `make down`. Asserts no aborts
      across any iteration.
- [ ] Each iteration logs: timestamp + iteration number + state
      (`rgw-up`, `rgw-wait-ok`, `rgw-down`).
- [ ] `make smoke-rgw-lab-restart` Makefile target.
- [ ] Smoke duration ~5-8 min total (RGW container boot ~30s each
      cycle + teardown ~10s).
- [ ] Typecheck passes; tests pass.

### US-007: rgw-benchmarks full sweep rerun

**Description:** As a maintainer, I want the full 8-workload sweep
from `ralph/rgw-benchmarks` rerun with both P1s fixed — capturing
the complete numbers that previously couldn't land.

**Acceptance Criteria:**
- [ ] Pre-flight: verify both P1 fixes are in place via
      `grep -nE 'bucket_stats_shard_writes_total\|period reconcile'
      internal/meta/tikv/store.go deploy/docker/rgw-bootstrap/rgw-entrypoint.sh`
      → 2+ matches.
- [ ] Run `make up-all && make up-bench-rgw && make
      bench-rgw-comparison` end-to-end → all 8 workloads complete
      including:
      - US-004 (PUT/GET 1KB+1MB concurrency sweep 1/8/32/128)
      - US-005 (PUT/GET 100MB)
      - US-006 (multipart 5GB)
      - US-007 (list 100k — the bucket-index claim leg)
      - US-008 (range GET + delete + iam-auth)
- [ ] Capture the full jsonl run in
      `scripts/bench-results/rgw-comparison-<date>.jsonl` — 144 rows
      expected (8 workloads × 2 targets × 3 runs +
      concurrency-sweep extras).
- [ ] Compare pre-fix vs post-fix numbers for US-007 list + US-008
      delete/iam-auth — should now have complete numbers (not
      partial / abort).
- [ ] Document actual sweep duration vs ~120 min estimate in
      progress.txt.
- [ ] Typecheck passes; tests pass.

### US-008: rgw-comparison.md complete numbers + bucket-index verdict

**Description:** As an operator evaluating Strata vs RGW, I want the
complete bench report with all 8 workloads + the headline verdict
on the bucket-index claim.

**Acceptance Criteria:**
- [ ] Update
      `docs/site/content/architecture/benchmarks/rgw-comparison.md`:
      - Replace partial-number placeholders with the full
        post-fix jsonl results.
      - **Headline conclusion** updated with bucket-index
        verdict: e.g. "Strata X× faster than RGW on list 100k"
        (claim verified) OR "RGW Y× faster — bucket-index claim
        does NOT hold" (claim refuted).
      - **Saturation note**: brief paragraph documenting the
        bucket_stats fan-out fix that unblocked the bench.
- [ ] **README backfill refresh** — re-grep the README's
      `Features vs RGW` table (from `ralph/readme-docs-rewrite`
      US-001) for the cells US-011 of rgw-benchmarks backfilled.
      Update each cell with the new ratio. **Hard guard**:
      `wc -l README.md` ≤ 120 lines preserved.
- [ ] If bucket-index claim is REFUTED (Strata slower on list),
      US-009 MUST add a new **P1** ROADMAP entry per CLAUDE.md
      "Discovering a new gap" rule (regression of foundational
      claim).
- [ ] `make docs-build` green; the bench mermaid chart still
      renders + reflects updated numbers.
- [ ] Typecheck passes; tests pass.

### US-009: Smoke + ROADMAP close-flip × 3 + new P3 entries + PRD removal

**Description:** As a future-maintainer, I want the 3-entry cycle
verified end-to-end + close-flip + any new gaps captured + PRD
removed.

**Acceptance Criteria:**
- [ ] Run `make smoke-tikv-default-lab` → all 4 scenarios pass.
- [ ] Run `make smoke-rgw-lab-restart` → 3 restart cycles green.
- [ ] Run `make smoke` → green.
- [ ] Run `make smoke-signed` → green.
- [ ] Run full `go test -race ./...` → green; capture duration.
- [ ] Run `make test-integration` (Cassandra testcontainers) →
      green (parity preserved).
- [ ] Run `make vet` + `make docs-build` → green.
- [ ] **ROADMAP close-flip × 3 in same commit**:
      (a) **P1** line 106 (`bucket_stats RMW saturates per-bucket
          TiKV key`) → Done. Summary references US-001..US-004 +
          the 8-shard fan-out shape + bench saturation removal.
      (b) **P1** line 117 (`RGW bench-rgw lab bootstrap fragile
          post-restart`) → Done. Summary references US-005..US-006
          + the period reconcile fix + 3× restart smoke.
      (c) **P3** line 282 (`ralph/rgw-benchmarks follow-up:
          measured numbers + verdict`) → Done. Summary references
          US-007..US-008 + the complete sweep numbers + the
          headline verdict.
- [ ] **NEW ROADMAP entries** (per CLAUDE.md "Discovering a new
      gap" rule):
      (a) If list workload (US-007) shows Strata SLOWER than RGW
          → new **P1** entry under `## Correctness & consistency`
          for the bucket-index claim regression.
      (b) If any other workload > 1.5× slower → new **P3** entry
          under `## Scalability & performance`.
- [ ] Each close-flip + new entry carries `(commit pending)`
      placeholder; SHA backfill on `main` as fast-follow commit.
- [ ] `tasks/prd-p1-fixes.md` REMOVED via `git rm`.
- [ ] `scripts/ralph/progress.txt` carries one US-009 block:
      pre/post-fix bench numbers summary + new ROADMAP entries
      (if any) + 3-cycle RGW restart confirmation.
- [ ] Typecheck passes; tests pass.

## Functional Requirements

- FR-1: TiKV `bucket_stats` MUST write to one of 8 sibling keys
  (`s/B/<bid>/bs/0..7`) per op, selected via
  `fnv32a(uuid.NewString()) % 8`.
- FR-2: TiKV `bucket_stats` read MUST `BatchGet` all 8 shards +
  sum in-memory (best-effort consistency; quotareconcile worker
  closes drift).
- FR-3: Old `s/B/<bid>/bs` single-key shape MUST be removed
  entirely (no fallback; pre-launch hard cutover).
- FR-4: Memory backend MUST be unchanged (sync.RWMutex absorbs
  concurrent bumps in-process).
- FR-4b: Cassandra backend strategy MUST be decided by US-001
  spike outcome — either no-change (branch a) OR sibling fan-out
  table (branch b) OR halt (branch c).
- FR-5: New contract test case `BucketStatsConcurrentBumps`
  MUST exercise 50-goroutine concurrent bumps + assert final
  sum on all 3 backends.
- FR-6: New counter
  `strata_bucket_stats_shard_writes_total{shard}` MUST be
  registered + bumped per op.
- FR-7: RGW lab bootstrap entrypoint MUST reconcile zonegroup →
  zone membership inside the period BEFORE `period update
  --commit`.
- FR-8: Entrypoint MUST log
  `period reconcile — zonegroups=<N>, zones-per-zonegroup=[...]`
  at startup.
- FR-9: `make smoke-rgw-lab-restart` MUST run 3 restart cycles
  green without manual `docker volume rm` between iterations.
- FR-10: rgw-benchmarks full sweep MUST run end-to-end with
  bucket_stats fan-out + RGW reconcile in place.
- FR-11: `rgw-comparison.md` MUST carry the headline verdict on
  bucket-index claim with updated post-fix numbers.
- FR-12: README features-vs-RGW backfill MUST stay ≤ 120 lines
  (preserve `ralph/readme-docs-rewrite` US-001 constraint).
- FR-13: If bucket-index claim refuted (Strata slower on list)
  → new **P1** ROADMAP entry under `## Correctness & consistency`
  in US-009.

## Non-Goals

- No per-bucket `stats_shards` override (parked — option B from
  scoping rejected; hard-coded 8 is the choice).
- No env knob `STRATA_BUCKET_STATS_SHARDS` (parked).
- No CRDT-additive option (a from ROADMAP listing — rejected;
  TiKV has no native counter primitive, would still be RMW).
- No async aggregation worker (c from ROADMAP listing —
  rejected; eventual consistency unacceptable for quota check).
- No migration backfill for old `s/B/<bid>/bs` rows (pre-launch
  hard cutover).
- No Cassandra `bucket_stats` table change IF US-001 spike fires
  branch (a). Branch (b) DOES change the table — cycle scope grows
  in flight, not parked.
- No memory backend change.
- No bench rerun against external RGW (lab only).
- No CI integration for bench (still operator-run-only; ~120
  min).

## Design Considerations

- **Shard count = 8** chosen for default lab workload. Matches
  Cassandra's `STRATA_BUCKET_SHARDS=64` magnitude but tuned
  smaller for the per-bucket counter case (one bucket × 8
  counters is much less skew-prone than full 64).
- **`fnv32a(uuid.NewString())` selection** — distributes
  uniformly across shards regardless of key / bucket identity.
  Bias-free.
- **Best-effort BatchGet read** — single TiKV round-trip, not
  atomic across shards but quotareconcile worker closes any
  drift. Acceptable since quota check operates on GB scale; a
  ±N-row drift is irrelevant.
- **Period reconcile** — RGW period state is the
  zonegroups + zones JSON blob committed to the realm.
  Idempotency requires reading the live period + comparing
  against expected zones, not just re-creating the zone row.
- **README backfill** — relies on the table format from
  `ralph/readme-docs-rewrite` US-001 staying stable. Grep-verify
  cells before edit (same discipline as US-011 of rgw-benchmarks).

## Technical Considerations

- **Concurrency safety for fan-out**: 100-goroutine race test
  is the canonical assertion. Past TiKV bugs have hidden in
  per-shard lock leaks (pessimistic txn forgot to Rollback on
  early return) — test must verify final count exactly, not
  approximately.
- **`quotareconcile` worker** — already exists per CLAUDE.md
  `cmd/strata server --workers=quota-reconcile`. Closes any
  drift between bucket_stats and real manifest scan; runs on
  default 1h cadence. Best-effort BatchGet read is safe
  precisely because of this worker.
- **`strata_bucket_stats_shard_writes_total{shard}` metric** —
  uniform distribution = healthy. One shard dominating = sign
  of hash collision or biased op-id generator. Operator can
  spot in 30s via Grafana.
- **RGW period drift root cause** — `period update --commit`
  EOVERFLOW (errno 34) suggests period contains more
  zonegroup-zone tuples than RGW expects. Likely cause:
  duplicate zone entry from stale state. Period reconcile fix
  removes duplicates + re-adds canonical zone.
- **rgw-benchmarks sweep duration** — ~120 min for full sweep
  per prior cycle measurement. Operator-side resource budget:
  300GB free disk (pre-flight check from rgw-benchmarks US-003
  already gates this).

## Success Metrics

- TiKV bucket_stats write throughput at c=128 increases by
  ≥ 4× vs pre-fix (or no longer hits write-conflict tornado
  at all — exact ratio depends on lab).
- `strata_bucket_stats_shard_writes_total{shard}` uniform
  distribution across 8 shards (each within ±20% of mean).
- ceph cluster stays HEALTH_OK throughout rgw-benchmarks
  rerun (no slow-ops HEALTH_WARN).
- `make smoke-rgw-lab-restart` 3 cycles green without manual
  volume cleanup.
- rgw-benchmarks full sweep produces 144+ jsonl rows (8
  workloads × 2 targets × 3 runs + sweep extras).
- 3 ROADMAP entries close in one cycle; 0-1 new P1 entries
  open (if bucket-index claim refuted).
- Cycle ships in 9 stories.

## Open Questions

- RGW period reconcile command — exact `radosgw-admin
  zonegroup modify` flag shape needs verification (resolved
  inside US-005 research step).
- Hash collision pattern detection — should US-002's metric
  carry an alert threshold ("if shard count[i] > 1.5× mean
  for 5 minutes, page operator")? Default: no alert this
  cycle; manual Grafana inspection sufficient at lab scale.
