# PRD: Pre-production architecture hardening

## Introduction

A senior-architect critical review of Strata (2026-05-31) — against the known
Ceph RGW scaling pain points the project claims to solve by design — found the
codebase **mostly production-ready** (s3-tests 92.7%, QA-cycle GO verdict,
coverage ratchet gate, adversarial auth matrices, controlled drain instead of
CRUSH rebalance storms). The TiKV-default backend genuinely defeats RGW's
bucket-index ceiling by being an ordered-range-scan store with **no index
object and no resharding at all** — verified by the RGW comparison bench
(`put-small c=8` Strata p99 ≈211 ms vs RGW 11–22 s; `list-100k` 208k ops/s
where RGW saturates the OSD on the seed phase).

But the review surfaced one **load-bearing claim that the code does not back**,
plus a cluster of accepted-but-open P2 risks from the QA cycle. This PRD is the
remediation plan to close them before launch, with **TiKV + Cassandra parity**
as the bar (both backends production-ready).

### The headline finding (verified in code)

Strata's docs (`docs/site/content/architecture/sharding.md`, root `CLAUDE.md`)
sell **per-bucket object sharding + online reshard** as the Cassandra-side
answer to RGW's bucket-index ceiling. ROADMAP marks US-045 "online shard
resize" as **shipped**. The code disagrees:

1. **`internal/reshard/reshard.go:121 runJob` never rewrites rows.** It iterates
   `ListObjectVersions`, increments a `copied` counter, advances a watermark,
   then `CompleteReshard` flips `buckets.shard_count`. No row is moved to a new
   partition. The package doc-comment admits it: *"Cassandra's per-row partition
   rewrite lands on top of this skeleton in a follow-up story."* The follow-up
   never landed.
2. **`internal/meta/cassandra/store.go` ignores `bucket.ShardCount` on the hot
   path.** `PutObject` (`:463`), `GetObject`/scalar reads (`:587`), delete
   (`:752`) all compute the shard as `shardOf(key, s.defaultShard)` — the
   **process-global** `STRATA_BUCKET_SHARDS` constant (default 64) — and
   `ListObjects`/`ListObjectVersions` (`:820`, `:919`) set
   `shardCount := s.defaultShard`. The per-bucket `shard_count` column is
   written by `CreateBucket`, read into `Bucket.ShardCount`, and then **never
   consulted** when addressing a row.

Combined effect on Cassandra/Scylla: per-bucket shard count is a dead column,
and "online reshard" is an inert no-op (the `shard_count` flip changes nothing
because the data plane keys on the global constant). It is *not* active data
loss today only because the global-constant short-circuit makes the flip inert —
but the moment anyone wires `bucket.ShardCount` into the hot path naïvely
(without the reshard machinery), every point-GET for a key whose new shard ≠ old
shard returns 404. TiKV is unaffected (no shards; range scan).

This PRD makes the claim true: per-bucket shard count works on Cassandra, and
online reshard genuinely rewrites rows with a correct transitional read/write
model — with red/green proof.

## Goals

- Make per-bucket object sharding **functional** on the Cassandra backend (hot
  path resolves `bucket.ShardCount`, not the process-global constant).
- Make online reshard **actually move rows** to the new partition layout, with a
  correct transitional model (writers + readers behave correctly while a job is
  in flight) and crash-resumability — closing US-045 honestly.
- Reach **TiKV + Cassandra parity**: both backends production-ready, Cassandra
  integration tests back on a CI gate.
- Close the QA-cycle P2 residual risks: `DeleteObjects` direct test (R1),
  object-access policy∪ACL union semantics (R6), plaintext per-chunk checksum on
  the read path (R9).
- Every behavioural change carries red→green proof; no claim ships ahead of its
  code.

## User Stories

### US-001: Cassandra hot path resolves per-bucket shard count
**Description:** As an operator who created a bucket with a non-default
`STRATA_BUCKET_SHARDS`, I want reads and writes to that bucket to use **its**
shard count, so the per-bucket sharding the docs promise actually takes effect.

**Acceptance Criteria:**
- [ ] **ALL nine `shardOf(key, s.defaultShard)` call sites** in
      `internal/meta/cassandra/store.go` resolve the bucket's `ShardCount`, not
      the global — verified by grep, they are: `PutObject:463`, `GetObject:587`,
      `DeleteObject:752`, **`SetObjectReplicationStatus:1887`**,
      **`resolveVersionID:2408`** (CRITICAL — this resolves `?versionId`; missing
      it 404s every versioned point-read on a non-default bucket),
      **`UpdateObjectSSEWrap:4118`**, plus the two further object-mutation sites at
      `:4167` and `:4198`. The three `shardCount := s.defaultShard` list/scan
      sites (`ListObjects:820`, `ListObjectVersions:919`, and the
      `bucketIsEmpty:357` / loop `:1042` / `SampleBucketShardStats:1142`
      helpers) likewise. Missing **any** one reintroduces the 404 bug — the AC is
      "no remaining `s.defaultShard` on an object-addressing path", proven by a
      grep assertion in review.
- [ ] `ListObjects` / `ListObjectVersions` fan out over the bucket's
      `ShardCount` partitions, not `s.defaultShard`.
- [ ] A shard-count resolver cache (keyed on bucketID, short TTL) avoids a
      `buckets` round-trip on every object op; cache miss falls back to a read;
      `s.defaultShard` is the value only for the pre-existing default-N buckets.
      **The cache MUST carry both `ShardCount` (active) and `TargetShardCount`**
      so US-002's in-flight writers can route to the target layout without a
      second lookup; `CompleteReshard` invalidates the entry synchronously.
- [ ] **Steady-state only** in this story (no in-flight reshard transition yet):
      a bucket created with `shard_count=128` round-trips PUT→GET→LIST→DELETE
      correctly; a bucket with `shard_count=16` likewise; both differ from the
      process default.
- [ ] TiKV + memory backends unchanged (TiKV is range-scan, shard-agnostic).
- [ ] New contract case in `internal/meta/storetest` proving non-default
      per-bucket shard count round-trips (Cassandra integration + memory).
- [ ] `make vet` + unit tests pass; Cassandra leg under `-tags integration`.

### US-002: Reshard transitional read/write model (meta contract)
**Description:** As the system, while a reshard job is in flight I must serve a
**stable key set** — every key visible before the job started stays visible
throughout — so clients see no gaps or duplicates during a resize.

**Acceptance Criteria:**
- [ ] Define the transitional contract: while `shard_count_target != 0`, writes
      land in the **target** layout (`fnv%target`) and reads return the **union**
      of source (`fnv%active`) and target partitions, deduplicated by
      `(key, version_id)` (target row wins on collision).
- [ ] `GetObject` point lookups during a job probe both the source-shard and
      target-shard partition for the key and return the newest by version.
- [ ] `ListObjects`/`ListObjectVersions` during a job merge source∪target with
      dedup so a half-migrated key is never double-emitted and never missing.
- [ ] The union-read path is exercised by a contract case that seeds a bucket,
      flips it into the in-flight state with rows split across both layouts, and
      asserts the listed/point-read key set equals the pre-flip set exactly.
- [ ] Power-of-two doubling invariant documented in code: `target = 2·active`,
      each old shard `s` maps to `s` or `s+active`; no three-way splits.
- [ ] Memory backend (shard-agnostic flat map) returns the same key set — its
      transitional model is a no-op but the contract test still passes.
- [ ] `make vet` + contract tests pass (memory + Cassandra integration).

### US-003: Reshard worker actually rewrites rows + cleanup
**Description:** As an operator resizing a hot bucket's shards, I want the
reshard worker to physically move each row to its new partition and then flip the
active shard count, so post-flip point-GET addresses the right partition.

**Acceptance Criteria:**
- [ ] `internal/reshard/reshard.go::runJob` is rewritten: for each source
      partition, scan rows; for every row whose `fnv%target != fnv%active`,
      **copy** it to the target partition (INSERT under the new layout); persist
      the `LastKey` watermark per batch for crash-resume.
- [ ] **Ordering is cleanup-BEFORE-flip** (critical-review CR-1): while the job
      is in-flight (`target != 0`) union-read is active, so the worker first
      deletes each moved source-partition orphan (the row now also living in the
      target partition), THEN `CompleteReshard` LWT-flips `active = target` and
      clears `shard_count_target`. Flipping first would open a window where
      post-flip listing (union-read off) reads partitions `0..2N-1`, still sees
      the un-deleted source orphan at `j-N` AND the moved copy at `j`, and
      double-emits the key. Cleanup-before-flip closes that window.
- [ ] Cleanup is idempotent and crash-safe (re-running the job after a partial
      cleanup converges; no row deleted that the target does not already hold).
- [ ] The flip itself MUST be LWT (`IF EXISTS`) — it is an UPDATE on a row read
      at quorum, so the codebase's LWT-on-LWT rule applies, or a reader can
      observe a stale `shard_count`. (Note: the move+flip is NOT one atomic op;
      only the flip is atomic — don't overclaim atomicity in code/docs, CR-6.)
- [ ] **Red/green proof:** seed a bucket with N keys at `shard_count=64`, run a
      `64→128` reshard to completion, then assert **every** key is reachable via
      point-GET (the bug today: keys whose new shard ≥ 64 would 404), and
      `ListObjectVersions` emits each key exactly once (no duplicate from the old
      partition). Capture the test failing against the skeleton, passing after.
- [ ] Versioned keys: every version of a key moves together; latest-version
      ordering preserved post-flip.
- [ ] `make vet` + tests pass; Cassandra leg under `-tags integration`.

### US-004: Reshard semantics on TiKV + memory (explicit, not accidental)
**Description:** As a developer, I want `StartReshard`/`CompleteReshard` to have
defined, tested behaviour on every backend so the admin surface is backend-safe.

**Acceptance Criteria:**
- [ ] TiKV: `internal/meta/tikv/reshard.go::StartReshard` **already runs a real
      pessimistic-txn reshard flow** (verified — it is NOT a no-op today; it
      locks the bucket key, validates `target > ShardCount`, queues a job). Per
      decision #4 this MUST be **reworked to an immediate-complete no-op**:
      `StartReshard` returns a job that `RunOnce` completes at once with zero rows
      moved (TiKV is range-scan, shard-agnostic — there is nothing to move). This
      is a behavioural change to existing code, not a fresh impl; the old
      job-queueing path is removed/short-circuited.
- [ ] Memory backend (`internal/meta/memory/store.go:2898 StartReshard` also
      already implemented): same immediate-complete no-op semantics; the
      shard-agnostic flat map means the key set is intact before/during/after.
- [ ] `meta.IsValidShardCount` (power-of-two, `n&(n-1)==0` — verified at
      `store.go:1429`) still gates the target on all backends so an invalid target
      is rejected even where the reshard is a no-op.
- [ ] Contract case asserts the chosen semantics on TiKV + memory: a reshard
      request leaves the full key set readable before, during, and after.
- [ ] `docs/site/content/architecture/sharding.md` updated: reshard applies to
      the Cassandra fan-out backend; TiKV needs no resharding (range scan).
- [ ] `make vet` + tests pass.

### US-005: Online-reshard admin endpoint + concurrent-write smoke
**Description:** As an operator, I want to trigger a reshard and have it complete
correctly **while clients keep writing**, proving the transitional model under
load.

**Acceptance Criteria:**
- [ ] **Existing endpoint reworked, not built fresh:** `adminBucketReshard`
      (`internal/s3api/admin.go:343`) today drives the reshard **synchronously**
      inline (`worker.RunOnce` in the request goroutine) — fine for the skeleton
      (no rows moved) but wrong once US-003 moves real data: a `64→128` reshard of
      a large bucket would block the HTTP request for minutes/hours. Split into a
      trigger (queue the job + return a job id immediately) + a separate progress
      read (job watermark / % complete / state).
- [ ] **Async drive — pick the model (open design point):** reshard is NOT a
      registered tick-worker today (verified: absent from `cmd/strata/workers/`;
      CLAUDE.md: "admin-driven one-shot, NOT a tick worker") — only the sync HTTP
      endpoint + `strata admin bucket reshard` CLI + `reshard.Worker.RunOnce`
      exist. There is **no `--workers=reshard` path** — do not assume one. To
      drive a long reshard off the request goroutine either (a) register a real
      leader-elected `reshard` background worker that picks up queued jobs (and
      update the CLAUDE.md note), or (b) run `RunOnce` in a server-managed
      goroutine keyed off the job row. (a) recommended for crash-resume +
      multi-replica safety.
- [ ] `admin:` audit stamp on the trigger (the endpoint currently has no audit
      override — add it per the CLAUDE.md "every new admin write" rule).
- [ ] `GetReshardJob` (already on `meta.Store`) backs the progress read on all
      backends.
- [ ] Smoke harness (`scripts/smoke-reshard.sh` + `make smoke-reshard`) against
      the Cassandra profile: seed a bucket, kick a `64→128` reshard, drive
      concurrent PUT/GET/DELETE throughout, assert (a) no client sees a 5xx or a
      spurious 404 during the job, (b) the key set is identical before and after,
      (c) post-flip point-GET hits the new layout.
- [ ] Crash-resume leg: kill the worker mid-job, restart, confirm it resumes
      from the watermark and still converges to a correct key set.
- [ ] `make vet` passes; smoke documented in the operator runbook.

### US-006: Reshard progress in the web console
**Description:** As an operator, I want to start and watch a reshard from the
console — mirroring the existing drain-progress UX — so the feature is not
server-only.

**Acceptance Criteria:**
- [ ] Bucket detail surfaces shard count + a "Reshard" action (target = next
      power-of-two, typed-confirm) when the backend supports it (hidden/disabled
      for TiKV with an explanatory tooltip "range-scan backend needs no
      resharding").
- [ ] A progress indicator reads the US-005 progress endpoint (state + %
      complete + ETA) reusing the `<DrainProgressBar>`-style component pattern.
- [ ] Playwright spec covers: trigger → in-progress → complete, plus the
      TiKV-disabled state.
- [ ] Typecheck/lint passes.
- [ ] Verify in browser using dev-browser skill.

### US-007: Direct test matrix for `DeleteObjects` (closes R1)
**Description:** As a maintainer, I want the multi-object delete handler directly
tested so its batch/partial-failure/versioned semantics are pinned, not just
exercised incidentally by the race workload.

**Acceptance Criteria:**
- [ ] Table-driven handler test over `newHarness` covering: quiet vs verbose
      mode, mixed success + `NoSuchKey` rows, the 1000-key cap (1001 → error),
      malformed `<Delete>` body, and the `<DeleteResult>`/per-key `<Error>` wire
      shape.
- [ ] Versioned-bucket leg: unversioned-style delete creates a delete marker;
      explicit `?versionId` removes a specific version; both reflected in the
      response rows.
- [ ] Tests fail against any deliberately-broken assertion (sanity), pass on
      `main`.
- [ ] ROADMAP R1 entry close-flipped with the commit sha.
- [ ] `make vet` + tests pass.

### US-008: Object-access policy ∪ ACL union semantics (closes R6)
**Description:** As an S3 client, I want a bucket policy granting me
`s3:GetObject` to actually grant access even when the object ACL is private —
matching AWS, where policy and ACL are a **union**, not an intersection.

**Acceptance Criteria:**
- [ ] `requireObjectAccess` is changed from policy **∩** ACL to policy **∪** ACL:
      an explicit policy `Allow` grants access regardless of the ACL gate (and
      vice-versa), while an explicit policy `Deny` still wins (AWS precedence:
      explicit deny > allow).
- [ ] PublicAccessBlock interaction preserved: `RestrictPublicBuckets` /
      `IgnorePublicAcls` still suppress the respective public grant (US-005 of the
      QA cycle must not regress — its tests stay green).
- [ ] **Red/green proof:** a test where an anonymous/non-owner GET is allowed by
      bucket policy but denied by a private ACL now returns 200 (was 403);
      `TestPAB_*` and the existing policy/ACL gate matrices stay green.
- [ ] **Fixture rework** (critical-review CR-4): the existing `TestPolicyGate_*`
      pass today only because the in-memory harness owner-matches the anonymous
      identity (per QA-cycle R6 note) — flipping ∩→∪ requires reworking those
      fixtures so a genuine non-owner anon principal exercises the union; the
      owner-full-control path and the explicit-Deny-wins precedence must each get
      a direct case (this is a security-sensitive widening — prove it doesn't
      grant beyond AWS).
- [ ] ROADMAP R6 entry close-flipped with the commit sha.
- [ ] `make vet` + tests pass.

### US-009: Per-chunk checksum on the read path, fail-loud (closes R9)
**Description:** As a data-integrity-conscious operator, I want a flipped byte in
a plaintext (non-SSE) chunk to fail the read loudly instead of returning a
corrupted 200, so silent at-rest corruption is detectable.

**Acceptance Criteria:**
- [ ] `data.ChunkRef` (`internal/data/manifest.go:102`, proto `message ChunkRef`
      `internal/data/manifest.proto:54`) carries a per-chunk **CRC32C** checksum
      (hardware-accelerated, integrity-only — byte-flip detection, no
      crypto-strength needed), set on `PutChunks`, schema-additive: Go field +
      **proto field 6** (fields 1–5 = cluster/pool/namespace/oid/size are taken;
      6 is the next free) + JSON `omitempty`. Legacy rows with no checksum decode
      to zero and skip verification (zero = "absent", not "CRC of zero bytes" —
      guard the sentinel).
- [ ] `GetChunks` verifies each chunk against its stored CRC32C and fails loud
      (`data.ErrChecksumMismatch`, surfaced as a 5xx / aborted stream — never a
      silent short or corrupted 200) on mismatch.
- [ ] **Red/green proof:** flip a byte in a stored plaintext chunk → read fails
      loud (today it returns a corrupted 200); a clean chunk reads normally;
      SSE objects unaffected (AEAD already covers them).
- [ ] **Range-GET boundary handling** (critical-review CR-3): the CRC covers a
      whole 4 MiB chunk, but a Range request reads the first/last chunk partially
      and cannot verify a partial chunk against a full-chunk CRC. Define the
      behaviour explicitly: for partially-read boundary chunks either (a) read the
      full chunk to verify then slice (extra IO, default), or (b) skip
      verification on the partial chunk and bump
      `strata_chunk_crc_unverified_total{reason="range_boundary"}`. Fully-covered
      interior chunks are always verified. Pick one, document it, test it.
- [ ] Verification is **on by default** with an env opt-out knob
      (`STRATA_CHUNK_CRC_VERIFY`, default on) for throughput-sensitive operators;
      hot-path cost measured/noted.
- [ ] ROADMAP R9 entry close-flipped with the commit sha.
- [ ] `make vet` + tests pass (memory always-on; RADOS leg under `ceph`/
      integration if a backend hook is needed).

### US-010: Cassandra integration tests back on a CI gate (parity)
**Description:** As a maintainer aiming for TiKV + Cassandra parity, I want the
Cassandra `meta.Store` LWT/Paxos contract running on CI so Cassandra regressions
are caught, not just found locally.

**Acceptance Criteria:**
- [ ] Cassandra contract round-trips (`TestCassandraStoreContract/*`,
      `GCQueueShardFanOut`, `VersioningSuspendedReplaceNull`, plus the new
      shard/reshard cases from US-001..US-005) run on a **green, required CI
      check** — delivered this cycle as a dedicated job (beefier runner) because
      the parity decision (A1b) does not allow Cassandra to stay untested.
- [ ] **Root-cause the per-PR-runner JVM/Paxos starvation** (heap cap / readiness
      / per-test resource limits) so Cassandra can rejoin the *per-PR* gate — not
      deferred to a vague follow-up, since A1b makes Cassandra co-equal. If the
      root-cause lands this cycle, run it per-PR; if not, the dedicated job is
      required and the root-cause is a tracked, owned ticket (not "someday").
- [ ] The job is green and its name is added to branch-protection required
      checks (documented as a manual post-merge step if admin token is needed,
      mirroring the QA-cycle coverage-gate step).
- [ ] ROADMAP "Cassandra integration tests gated off per-PR CI" entry
      close-flipped or updated with the new job reference.
- [ ] CI config validates locally (`make vet` unaffected).

### US-012: Cassandra ListObjects fan-out rebuilt for production
**Description:** As an operator running the Cassandra backend at scale, I want
the listing fan-out to be bounded and consistent so concurrent listings don't
exhaust goroutines/connections and reads have a defined consistency guarantee —
not the silent cluster default.

**Acceptance Criteria:**
- [ ] `ListObjects`/`ListObjectVersions` fan-out is **bounded**: today
      `internal/meta/cassandra/store.go::ListObjects` (line ~827) spawns
      `shardCount` goroutines per request with **no semaphore** — at high
      per-bucket shard counts × concurrent list requests this explodes
      goroutines and the gocql connection pool. Replace with a bounded worker
      pool (knob, sane default) so concurrency is capped regardless of shard
      count.
- [ ] **Read consistency is pinned explicitly** on the listing queries (e.g.
      `LOCAL_QUORUM`) rather than relying on the cluster/session default — the
      product's listing read-consistency guarantee must not depend on an operator
      not having changed a Cassandra default. (Pairs with A3: one documented
      contract.)
- [ ] Memory budget bounded: the cursor/heap-merge holds at most
      `boundedConcurrency × pageSize` rows in flight, not `shardCount × pageSize`;
      documented.
- [ ] Pagination cookies still resume per-shard cursors correctly under the
      bounded pool (no page gap/dup when concurrency < shardCount).
- [ ] **Red/green proof:** a test driving many concurrent ListObjects against a
      high-shard-count bucket asserts goroutine count stays bounded (no
      linear-in-shardCount blowup) and the listed key set is correct/stable; a
      consistency test asserts a write at `LOCAL_QUORUM` is visible to the very
      next list at `LOCAL_QUORUM`.
- [ ] TiKV unaffected (single ordered scan, no fan-out).
- [ ] `make vet` + tests pass; Cassandra leg under `-tags integration`.

### US-011: End-to-end pre-prod validation walkthrough
**Description:** As the release owner, I want one e2e pass that exercises the
whole hardened path on both backends so the GO/NO-GO decision rests on observed
behaviour, not story-by-story claims.

**Acceptance Criteria:**
- [ ] A single documented walkthrough (script + runbook section) brings up both
      the TiKV-default and the Cassandra-profile labs and exercises: per-bucket
      non-default shard count, an online `64→128` reshard under concurrent writes
      (Cassandra), `DeleteObjects` batch + versioned, a policy-only anonymous GET
      (union semantics), and a plaintext chunk-corruption fail-loud.
- [ ] The console reshard UI (US-006) is driven in the same pass.
- [ ] Outcome captured in a short readiness note (pass/fail per leg) committed
      under `tasks/`.
- [ ] `make vet` + the new smokes pass.

## Functional Requirements

- FR-1: The Cassandra data plane MUST address object rows using the bucket's
  `ShardCount`, not the process-global `s.defaultShard`, in all of
  Put/Get/Delete/List(Objects|ObjectVersions).
- FR-2: While `shard_count_target != 0`, writes MUST target the new layout and
  reads MUST return the deduplicated union of source and target partitions.
- FR-3: The reshard worker MUST physically copy each row whose partition changes
  under the new modulo, persist a resumable watermark, and on completion flip
  `active = target` and remove orphaned source rows idempotently.
- FR-4: `StartReshard`/`CompleteReshard` MUST have defined, tested semantics on
  TiKV (range-scan: effectively no-op) and memory; never silent corruption.
- FR-5: An admin endpoint MUST trigger a reshard and expose progress, audit-
  stamped; the web console MUST surface trigger + progress (disabled on TiKV).
- FR-6: `DeleteObjects` MUST have a direct table-driven handler test covering
  quiet/verbose, partial failure, the 1000-key cap, malformed body, and
  versioned semantics.
- FR-7: Object access MUST be the union of bucket-policy and ACL grants (explicit
  deny wins), preserving PublicAccessBlock suppression.
- FR-8: `GetChunks` MUST verify a per-chunk checksum and fail loud on mismatch
  for plaintext chunks; the manifest checksum field MUST be schema-additive.
- FR-9: The Cassandra contract suite (incl. new shard/reshard cases) MUST run on
  a green CI job and be a required check.
- FR-10: Every behavioural fix (US-003, US-007, US-008, US-009) MUST ship with a
  test observed failing before the fix and passing after.
- FR-11: Cassandra `ListObjects`/`ListObjectVersions` MUST use a bounded fan-out
  worker pool (not one goroutine per shard) and pin read consistency explicitly
  (`LOCAL_QUORUM`), with bounded in-flight memory.

## Non-Goals (Out of Scope)

- **Shard shrinking** (`N → N/2`). Power-of-two **doubling** only; halving is a
  separate future effort.
- **Storage-tier backup itself** (TiKV BR / Cassandra `nodetool snapshot`, RPO/
  RTO, retention) — **operator's responsibility**, not Strata's. We do not
  reinvent the database's own backup tooling. (The chunk back-reference +
  RADOS↔meta reconcile that makes a restore *usable* IS ours — see A2-own / G1,
  scoped as a structural recommendation, not a code story this cycle.)
- **Linux-box RGW head-to-head bench** — release gate G3, exercised outside the
  P1+P2 code scope.
- **Content-addressed dedup, Intelligent-Tiering, Select** — unrelated ROADMAP
  P2/P3 feature work.
- **ScyllaDB benchmark numbers** — separate P2.
- Changing the **TiKV** sharding model — it is range-scan by design and needs no
  resharding; this cycle only makes its reshard *API surface* explicit.
- Rolling-upgrade / migration-backfill shims — pre-launch, hard cutovers are fine
  (no users/data).

## Technical Considerations

- **Power-of-two doubling** (`meta.IsValidShardCount`) is the safety lever: each
  old shard `s` maps to exactly `{s, s+active}` under `2·active`, so the worker
  reads each source partition once and writes each moved row once.
- **Shard-count resolver cache** (US-001) must invalidate on `CompleteReshard`
  so the hot path sees the new active count immediately after a flip (same
  discipline as `placement.DrainCache` invalidation).
- **LWT discipline** (Cassandra): `CompleteReshard`'s flip is an UPDATE on a row
  that may be read at quorum — it MUST be LWT (`IF EXISTS`) per the codebase's
  LWT-on-LWT rule, or reads can observe a stale `shard_count`.
- **Dedup on union read**: merge by `(key, version_id)`; the target-layout row is
  authoritative when both layouts hold the same `(key, version_id)` mid-cleanup.
- **Checksum choice** (US-009): CRC32C (hardware-accelerated, `~GB/s`) is likely
  the right cost/coverage trade for the read hot path; sha256 only if a stronger
  guarantee is required and the throughput hit is acceptable.
- **Reuse harnesses**: `internal/meta/storetest` contract for the backend-
  agnostic cases; `newHarness` for the s3api handler tests; the existing
  smoke-harness shape (`scripts/smoke-*.sh` + `make smoke-*`).

## Success Metrics

- A bucket created with a non-default shard count round-trips on Cassandra
  (today: silently uses the global constant).
- An online `64→128` reshard under concurrent writes completes with a
  byte-identical key set before/after and **zero** spurious 404s — the skeleton
  cannot pass this.
- `DeleteObjects`, policy∪ACL, and plaintext-corruption each have a test that was
  observed red before the fix.
- Cassandra contract suite green on CI; both backends at parity.
- ROADMAP R1, R6, R9 and the US-045 "online reshard" claim all reflect code
  reality.

## Resolved decisions (2026-05-31, with the user)

1. **US-009 checksum algorithm** — **CRC32C** (hardware-accelerated,
   integrity-only; byte-flip detection without crypto-strength cost).
2. **US-009 default state** — **on by default** with an env opt-out knob
   (`STRATA_CHUNK_CRC_VERIFY`).
3. **US-010 CI shape** — **dedicated nightly Cassandra job** on a beefier runner;
   root-causing the per-PR runner starvation is a follow-up.
4. **US-004 TiKV reshard semantics** — **immediate-complete no-op job** at the
   API layer: `StartReshard` returns a job that `RunOnce` completes at once with
   zero rows moved (a direct API/CLI caller gets success, not an error). The
   **console** does not offer the action on TiKV — it disables the Reshard button
   with a "range-scan backend needs no resharding" tooltip (US-006), since
   offering a button that does nothing is worse UX than hiding it. API = no-op
   success; UI = disabled. Both consistent: nothing to reshard.
5. **Reshard write target during a job** — **target-only writes + union-read +
   post-flip cleanup** (fewer writes on the hot path; cleanup removes orphaned
   source rows).
6. **US-006 reshard console UI** — **in this cycle** (honours the
   "don't ship server-side without UI" rule).

## Open Questions

None outstanding — all six resolved above.

---

## Architecture recommendations (structural — beyond the story-level fixes)

The story work above makes the *current* architecture honest. These are the
senior-architect calls about whether the architecture itself should change.

### A1 — Full TiKV + Cassandra parity (DECIDED 2026-05-31: A1b)

The entire class of bugs this review found — dead per-bucket shard count, the
skeleton reshard, scatter-gather listing, hotshard risk, the 64-way fan-out
merge — exists **only on the Cassandra fan-out path**. TiKV (ordered range-scan)
has none of them. The two backends are *not* co-equal today: Cassandra is off the
per-PR CI gate, and the per-bucket-shard / reshard feature is inert on it,
despite the "first-class in lockstep" claim.

**Decision: A1b — keep Cassandra first-class and fix it fully.** Rationale
(user): the backend has been maintained the whole project lifetime; shipping it
half-working defeats that investment. Both backends are production-grade or
neither ships.

Consequences, now binding on this cycle:

- US-001..US-006 (real per-bucket sharding + genuine online reshard on Cassandra)
  are **in scope and required**, not optional — `internal/reshard` is implemented
  for real, not removed.
- US-010 is upgraded: Cassandra is not merely "back on a nightly job" — the
  parity bar means the Cassandra contract suite (incl. the new shard/reshard
  cases) is a **green, required CI check**. A nightly job is the *delivery*
  mechanism this cycle; the *bar* is parity, so the per-PR-runner starvation
  root-cause moves from "follow-up" to a tracked must-fix so Cassandra can rejoin
  the per-PR gate (see updated US-010).
- The "sharded objects table" framing in `CLAUDE.md` / `sharding.md` stays, but
  the docs must stop overclaiming until US-001..US-006 land — see US-002/US-004
  doc-update ACs.

### A2 — Metadata is the single point of data loss (the part that is OURS)

RADOS replicates the *bytes*, but the map from S3 key → RADOS chunks lives
**only** in the meta store. RADOS chunks are opaque random-OID blobs with **no
back-reference** (`data.ChunkRef` = `{Cluster, Pool, Namespace, OID, Size}` —
verified; the xattr write path exists but ships unused, "until xattrs are added
to the PUT/GET hot path"). Consequence: **a meta store that is restored to a
stale point, or partially corrupted, turns objects into unrecoverable garbage
even though the data is intact on RADOS.**

**Scope correction (user, 2026-05-31): the storage tier's own backup is NOT
ours.** Taking and restoring TiKV BR / Cassandra snapshots, RPO/RTO, retention —
that is the operator's responsibility for the database they run, and we should
not reinvent `br` / `nodetool snapshot`. Dropped from this PRD.

What remains genuinely **ours** — and is not solved by any storage-tier backup —
is the chunk **back-reference** (a product feature: stamp `{bucket_id, key,
version_id, chunk_idx}` as an xattr on every RADOS chunk at PUT, the
`writeChunkBatched` SetXattr seam already exists) plus a **reconcile / rebuild**
pass. Two things only this buys, that an operator's DB snapshot cannot:

1. **Restore-skew repair.** Data (RADOS) and metadata (TiKV/Cassandra) are backed
   up by *different* systems on *different* schedules — never a consistent cut.
   After any restore the tiers disagree at the edges (chunks with no manifest;
   manifests pointing at missing chunks). The back-reference lets a reconcile pass
   realign them; without it, skew is silent corruption.
2. **Last-resort rebuild** of the manifest index from a RADOS scan when the meta
   backup is itself lost/corrupt — the escape hatch RGW gets for free by
   colocating index with data (RGW exposes it via `rgw-orphan-list` /
   `bucket radoslist`), removed by Strata's separation unless re-added.

This is an **established pattern** (Lustre LFSCK, HDFS block reports, GFS chunk
reports), not a novel risk — but a self-contained, sizeable feature.

**Decision (user, 2026-05-31): variant (b) — separate PRD, next cycle.** Spec'd
in [`tasks/prd-metadata-data-reconcile.md`](prd-metadata-data-reconcile.md)
(back-reference at PUT, reconcile worker, `strata admin rebuild-index`,
shared-responsibility ops note, console surfacing). It is an **owned, tracked
go-live gate (G1)** — not "someday" — just kept out of this cycle's
reshard/sharding scope to avoid bloat.

### A3 — One consistency contract (DECIDED: rebuild Cassandra listing to prod-ready)

TiKV gives strongly-consistent ordered listing; Cassandra's fan-out listing today
is unbounded (one goroutine per shard, no semaphore) and reads at the silent
session/cluster consistency default. The product's listing guarantee thus depends
on the backend *and* on operator-set Cassandra defaults — not acceptable for prod.

**Decision (user, 2026-05-31): rebuild the Cassandra listing path to be
production-ready** rather than merely documenting the divergence — captured as
**US-012** (bounded fan-out worker pool + explicit `LOCAL_QUORUM` read
consistency + bounded in-flight memory). After US-012 both backends present a
defined listing read-consistency contract; the remaining ordered-scan-vs-merge
difference is documented as an explicit per-backend guarantee, not a silent one.

## Hard go-live gates beyond this cycle's P1+P2 scope

The chosen scope (P1 + P2) makes the code honest but does **not** by itself make
the product "definitely production-ready." A responsible go/no-go also requires:

- **G1 — Two-tier reconcile/rebuild (A2-own).** Mandatory. NOT a DB-backup
  runbook (that is the operator's job for TiKV/Cassandra) — but Strata MUST ship
  the chunk back-reference + a reconcile pass that realigns RADOS↔meta after a
  restore, because data and metadata are backed up by different systems on
  different schedules and can never be a consistent cut. No launch without a
  tested reconcile path.
- **G2 — Scale/soak proof.** The entire value proposition is scale, yet there is
  zero evidence at target scale — the RGW bench is "provisionally verified" on
  **2 of 12** workloads on a laptop. Required: a sustained soak on Linux + NVMe
  OSDs at target shape (≳10⁹ objects, ≳10⁴ buckets, multi-replica, days of
  runtime) measuring p99 stability, GC/lifecycle keep-up, rebalance under load,
  and meta-backend headroom.
- **G3 — Linux RGW head-to-head bench.** Close the `_pending_` cells so the
  "drop-in RGW replacement" claim rests on a full head-to-head, not 2 workloads.
- **G4 — Operational runbooks.** Capacity planning, failure recovery (meta + data
  + cluster loss), and rolling-upgrade procedures, exercised at least once.

These four are **release-blocking** for a real production claim even though they
sit outside the P1+P2 code scope. G1 and G2 are the two that would most embarrass
the product in its first incident.

**Decision (user, 2026-05-31): all four gates are committed work, not optional.**
G1 ships as the separate reconcile PRD; G2 (Linux + NVMe soak at target scale),
G3 (full Linux RGW head-to-head closing the `_pending_` cells), and G4
(capacity-planning / failure-recovery / rolling-upgrade runbooks, each exercised
at least once) are owned, tracked deliverables gating launch. Track them in
`ROADMAP.md` as explicit P1 go-live items so none silently slips.

## Next step

Adversarial critical review folded in (CR-1 ordering, CR-2 cache-carries-target,
CR-3 range-GET CRC, CR-4 union fixtures, CR-6 atomicity). Decide A1a vs A1b
first — it changes which stories survive — then convert to `prd.json` via
`ralph-skills:ralph`.
