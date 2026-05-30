# PRD: QA Production-Readiness Hardening

## Introduction

Strata is an S3-compatible object gateway (TiKV/Cassandra metadata, RADOS/S3 data).
It already carries broad test coverage — 301 `_test.go` files, a 92.7 % `s3-tests`
compatibility baseline (165/178, the 13 misses are deliberate SigV2/boto3 gaps),
and solid unit coverage on the core packages (auth 84 %, placement 91 %, memory
74 %). The open question for a release decision is **not "are there tests"** but
**"are the production-critical failure modes — adversarial auth, concurrency
races, and storage-fault recovery — actually exercised, and do they hold?"**

This cycle is a senior-QA critical pass. It audits the existing suite, closes the
real gaps with focused tests (no duplication of what is already covered), fixes
every real bug those tests expose, and produces a go/no-go production-readiness
report. Functional S3 correctness is already near-ceiling, so the value lands in
the three under-exercised dimensions: **adversarial security/auth**,
**concurrency/race**, and **durability/fault-injection**.

Backends in scope: **TiKV + memory (canonical CI default), RADOS/ceph, and
S3-compatible (MinIO)**. Cassandra stays first-class in code but remains gated off
the per-PR CI gate (known latent debt — JVM-heavy Paxos starves the runner).

## Goals

- Produce a per-package, per-dimension coverage map and a written go/no-go
  production-readiness report with an explicit residual-risk list.
- Close the **real** functional gaps (notably RFC 7232 conditional requests, which
  have zero direct tests) without duplicating already-covered surface.
- Build an **adversarial auth matrix** (SigV4, presigned, streaming chunk-HMAC,
  anonymous/auth-mode enforcement) that proves the secure default denies and the
  permissive modes behave as documented.
- Extend `internal/racetest` to cover the concurrency hot-paths that have only
  indirect coverage today: multipart complete/abort races, versioning put/delete
  races, LWT/CAS contention, GC + `bucket_stats` fan-out consistency.
- Add **fault-injection / durability** tests: chunk loss / corruption on GET,
  partial-write rollback, drain/rebalance under concurrent load.
- Every story is verified by a **CI run on the cycle branch** — many of these
  failure modes do not reproduce cleanly on a local macOS/lima box.
- Every real bug found is fixed in the same story; bugs that cannot be fixed in
  one iteration get a `ROADMAP.md` P-item and a documented residual risk.

## User Stories

### US-001: Coverage audit + readiness-report skeleton
**Description:** As a QA engineer, I need a baseline coverage map and a living
report document so the release decision has evidence, not vibes.

**Acceptance Criteria:**
- [ ] Add a `make test-cover` target (or `scripts/qa/coverage.sh`) that runs
      `go test -cover ./...` on the **default tag** (TiKV + memory, no ceph) and
      emits a per-package coverage table to `tasks/qa-readiness-report.md`.
- [ ] CGO note: `internal/s3api` (and any cgo-touching package) builds its tests
      with cgo; on a macOS box without the Xcode license accepted this fails with
      `build failed: Xcode license` — that is an env issue, not a code bug. The
      coverage numbers for those packages are taken from the **CI (Linux) run**, the
      source of truth (FR-4); the local target may `CGO_ENABLED=0`-skip them with a
      logged note rather than failing.
- [ ] The measured per-package numbers are **recorded verbatim** in the report as
      the named baseline — these exact numbers become the ratchet-floor for the
      US-013 coverage gate (no aspirational targets invented later).
- [ ] Report skeleton committed at `tasks/qa-readiness-report.md` with sections:
      *Coverage by package*, *Dimension matrix* (functional / auth / concurrency /
      durability), *Residual risks*, *Go/No-Go verdict* (verdict left `PENDING`
      until US-013).
- [ ] Handler→test matrix for `internal/s3api/` enumerated in the report; every
      handler `.go` is marked covered / thin / uncovered with the test file that
      covers it (by function reference, not filename).
- [ ] No production code changed in this story (audit only).
- [ ] `make build` + `make vet` pass.
- [ ] Verify via a CI run on the cycle branch (the coverage job is green).

### US-002: RFC 7232 conditional-request test matrix
**Description:** As a QA engineer, I need conditional requests covered, because
`internal/s3api/conditional.go` currently has **zero** direct tests and these
control read-after-write and lost-update safety for clients.

**Acceptance Criteria:**
- [ ] New `internal/s3api/conditional_test.go` exercises GET + HEAD with
      `If-Match`, `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since`:
      strong-ETag match/mismatch, `*` wildcard, multi-value lists, and the
      precedence rules (If-Match over If-Modified-Since per RFC 7232 §6).
- [ ] PUT-side `If-Match` / `If-None-Match` (lost-update + create-if-absent) paths
      covered, including the `If-None-Match: *` overwrite-rejection (412).
- [ ] Assert exact status codes: 200 / 206 / 304 / 412 for each branch.
- [ ] Any divergence from AWS behaviour found is fixed in `conditional.go` (or
      `putObject`) in this story, or filed as a `ROADMAP.md` P-item if it cannot be
      fixed in one iteration.
- [ ] Does NOT duplicate `range_test.go`; reuses `newHarness(t)`.
- [ ] `make test` passes locally; verify via a CI run on the cycle branch.

### US-003: SigV4 adversarial auth matrix
**Description:** As a QA engineer, I need to prove SigV4 rejects every tampering
class, because auth is the security boundary of the gateway.

**Acceptance Criteria:**
- [ ] Table-driven test in `internal/auth/` covering: bad signature, expired
      request (`X-Amz-Date` skew beyond tolerance), wrong region, wrong service,
      tampered canonical request (mutated header/path/query after signing),
      missing `Host`, missing `x-amz-content-sha256`, and unsigned-payload vs
      signed-payload mismatch — each asserting `ErrSignatureInvalid` (or the exact
      sentinel) and the resulting S3 `APIError` code.
- [ ] Positive control: a correctly-signed request for each case passes (proves the
      negative is the tamper, not a setup error).
- [ ] Extends the existing `internal/auth/sigv4_test.go` surface; does not
      duplicate cases already asserted there.
- [ ] Any accepted-when-should-reject finding fixed in `internal/auth/` this story.
- [ ] Verify via a CI run on the cycle branch.

### US-004: Presigned + streaming chunk-signature adversarial
**Description:** As a QA engineer, I need presigned-URL and streaming-upload auth
hardened against expiry, replay, and chain-HMAC tampering.

**Acceptance Criteria:**
- [ ] Presigned tests: expired `X-Amz-Expires`, future-dated, tampered signed
      query param, method mismatch (signed GET used for PUT), and a valid replay
      within the window (documents whether replay is permitted — AWS allows it).
- [ ] Streaming tests (`internal/auth/streaming.go`, US-022 surface): broken
      chain-HMAC (mutated `prevSig`), out-of-order chunk, truncated final chunk,
      and `STREAMING-...-TRAILER` checksum mismatch — each rejected with the exact
      sentinel.
- [ ] Reuses existing trailer fixtures (`STRATA_REGEN_TRAILER_FIXTURES` path); does
      not regenerate or duplicate them.
- [ ] Any bug fixed in `internal/auth/` this story.
- [ ] Verify via a CI run on the cycle branch.

### US-005: Anonymous + auth-mode enforcement matrix
**Description:** As a QA engineer, I need the auth-mode × public-access matrix
proven so the secure default cannot silently regress to open access.

**Acceptance Criteria:**
- [ ] Matrix test over auth mode `{off (full-access), optional, required}` ×
      request `{anonymous, signed}` × `{public-access-block on/off, bucket-policy
      grants anon, bucket/object ACL public-read}` asserting allow/deny per cell.
- [ ] Explicitly asserts: `required` (default) denies all anonymous; `off` grants
      full access (dev-only, documented); `optional` honours ACL/policy for anon.
- [ ] Asserts `public-access-block` overrides a permissive bucket policy (block
      wins).
- [ ] Lives in `internal/s3api/` (reuses `newHarness`); does not duplicate
      `access.go` unit assertions already present elsewhere.
- [ ] Any enforcement bug fixed this story.
- [ ] Verify via a CI run on the cycle branch.

### US-006: SSE negative + key-mismatch + rotation hardening
**Description:** As a QA engineer, I need encryption failure paths covered, not just
happy-path round-trips, so a misconfigured key never silently returns plaintext or
the wrong object.

**Acceptance Criteria:**
- [ ] Negative tests added (extending `sse_c_test.go` / `sse_object_test.go`, no
      duplication of happy-path round-trips): SSE-C GET with wrong customer key →
      403; SSE-C with missing/short key → 400; SSE-KMS unwrap with wrong key id →
      maps to `ErrKeyIDMismatch` (the `IncorrectKeyException` path).
- [ ] Multipart SSE: complete a multipart upload under SSE-KMS and assert each part
      decrypts via the per-part locator (`Manifest.PartChunkCounts`).
- [ ] Rotation: a `strata admin rewrap` to a new key id followed by a GET still
      decrypts (reuses the rotation integration test harness, MinIO KMS +
      `internal/crypto/kms` fake).
- [ ] Verify via a CI run on the cycle branch (integration-tagged, MinIO + KMS
      containers).

### US-007: Multipart concurrency race tests
**Description:** As a QA engineer, I need multipart races covered under `-race`,
because concurrent complete/abort is where LWT correctness is easiest to break.

**Acceptance Criteria:**
- [ ] New scenario in `internal/racetest/` (extends `workload.go`; does not fork a
      parallel harness): N goroutines racing `CompleteMultipartUpload` +
      `AbortMultipartUpload` on the same upload id — exactly one Complete wins, the
      rest get `ErrMultipartInProgress`/`NoSuchUpload`, no double object row, no
      orphaned chunks left un-GC'd.
- [ ] Concurrent uploads of the same key by different upload ids resolve to a single
      latest object with a consistent manifest.
- [ ] Runs green under `go test -race` on TiKV + memory backends.
- [ ] Any race/correctness bug fixed in the handler or `meta.Store` this story.
- [ ] Verify via a CI run on the cycle branch (the `-race` job covers it).

### US-008: Versioning + LWT/CAS contention races
**Description:** As a QA engineer, I need versioning and CAS contention covered so
concurrent writers cannot corrupt the version chain or lose updates.

**Acceptance Criteria:**
- [ ] Race scenario: concurrent `PutObject` + `DeleteObject` (delete-marker) on a
      versioned key — the version chain stays monotonic, `IsLatest` is held by
      exactly one row, and `?versionId=null` resolves deterministically.
- [ ] CAS contention: concurrent `SetObjectStorage` (lifecycle-transition vs client
      overwrite) — the loser's chunks land in the GC queue, the winner's manifest is
      the one read back (the documented lifecycle-CAS invariant).
- [ ] Suspended-versioning replace-null path covered under contention
      (`VersioningSuspendedReplaceNull` semantics) on TiKV + memory.
- [ ] Runs green under `-race`; any bug fixed this story.
- [ ] Verify via a CI run on the cycle branch.

### US-009: Drain stop-write invariant under concurrent load
**Description:** As a QA engineer, I need the drain stop-write invariant proven under
live traffic, because evacuation is a production-critical, hard-to-reverse operation.
(Split from the original combined drain+rebalance story — too large for one
iteration; the mover is US-010.)

**Acceptance Criteria:**
- [ ] Integration test: mark a cluster `evacuating`, fire concurrent PUTs at it →
      every PUT gets `503 DrainRefused` + `Retry-After: 300`; reads / deletes / HEAD
      keep working (the always-strict drain invariant).
- [ ] An in-flight multipart session bound to the draining cluster survives to
      completion (routing recovered from `BackendUploadID`, never re-picks).
- [ ] `deregister_ready` does NOT flip to true while a multipart session is still
      bound to the cluster (the `ListMultipartUploadsByCluster` safety gate).
- [ ] Verify via a CI run on the cycle branch (integration + e2e jobs).

### US-010: Rebalance mover correctness under load
**Description:** As a QA engineer, I need the rebalance mover proven to move chunks
correctly while traffic flows, so evacuation never loses or duplicates data. (Split
from US-009.)

**Acceptance Criteria:**
- [ ] Extends `internal/rebalance/s3_mover_integration_test.go` (and the RADOS mover
      path) — no duplication of the existing single-pass move test.
- [ ] Under concurrent client writes, the mover moves chunks with correct manifest
      CAS: the winner's locator is read back, CAS losers' chunks land in the GC
      queue (the documented mover invariant).
- [ ] Old source keys are queued in the `_by_cluster` GC table after the move; the
      object body is readable on the destination cluster afterwards.
- [ ] Mover refuses moves into a draining or >90 %-full cluster (the safety rails).
- [ ] Verify via a CI run on the cycle branch (integration + e2e jobs).

### US-011: Chunk-loss / corruption / partial-write recovery
**Description:** As a QA engineer, I need to prove a damaged data plane fails loud,
never silently returning truncated or wrong bytes.

**Acceptance Criteria:**
- [ ] Corruption test: a corrupted/missing chunk on GET returns an error (5xx/IO
      error), never a silently-truncated 200 body — for both RADOS
      (`CorruptFirstChunk` hook) and the S3/MinIO backend.
- [ ] Partial-write rollback: a `PutChunks` that fails mid-stream does not leave a
      readable half-object (manifest is only committed after all chunks land).
- [ ] ETag/size mismatch on read is detected (the racecheck verifier path) and
      surfaced, not swallowed.
- [ ] NOTE: the memory backend **already exposes** `CorruptFirstChunk()`
      (`internal/data/memory/backend.go:148`). The skip at
      `internal/s3api/sse_object_test.go:207` is **stale** — it fires when the call
      returns `false` (no chunk stored), not because the method is absent. Fix the
      stale guard so the corruption assertion runs on the **memory backend on the
      default CI tag** (no container needed) in addition to RADOS + MinIO.
- [ ] Corruption assertion runs on memory (default tag) + RADOS + MinIO
      (integration tag). No new corruption hook is added — memory's existing one is
      reused.
- [ ] Verify via a CI run on the cycle branch (unit + integration + e2e jobs).

### US-012: GC + bucket_stats fan-out concurrency consistency
**Description:** As a QA engineer, I need the fan-out counters and GC queue proven
consistent under concurrency, because these drive billing and storage reclaim.

**Acceptance Criteria:**
- [ ] Concurrent `BumpBucketStats` from N goroutines: `GetBucketStats` sums to the
      exact expected total (no lost increments), and the
      `strata_bucket_stats_shard_writes_total{shard}` distribution is non-degenerate
      (picker uses a fresh uuid per call, not a stable key).
- [ ] Concurrent `EnqueueChunkDeletion` + `AckGCEntry` keep `gc_entries_v2` and
      `gc_entries_by_cluster` in lockstep (the dual-write invariant); no entry is
      acked twice, none leaked.
- [ ] GC under per-shard fan-out deletes each chunk exactly once (no double-delete,
      no skip).
- [ ] Runs on TiKV + memory; any bug fixed this story.
- [ ] Verify via a CI run on the cycle branch.

### US-013: Production-readiness report + close-flip + final green CI
**Description:** As a QA engineer, I need to deliver the go/no-go verdict backed by a
fully green CI run so the team can make the release call.

**Acceptance Criteria:**
- [ ] `tasks/qa-readiness-report.md` filled in: final coverage table (before/after),
      the dimension matrix all populated, every bug found+fixed listed with its
      commit, residual risks enumerated with severity, and an explicit
      **GO / NO-GO** verdict with justification.
- [ ] `scripts/s3-tests/README.md` re-baselined if any functional story moved the
      pass rate; the 13 deliberate gaps re-confirmed as deliberate. NOTE: `s3-tests`
      is **not a CI job** — this re-baseline is run **manually/locally** against the
      compose stack (`scripts/s3-tests/run.sh`), not verified by the cycle CI run.
- [ ] `ROADMAP.md` close-flipped for this cycle; any unfixable finding added as a
      P1/P2/P3 entry in the same commit.
- [ ] A full CI run on the cycle branch is green end-to-end (all required checks).
- [ ] The coverage table is **promoted to a CI gate**: a coverage job whose
      per-package floor is set to the **US-001 baseline numbers** (ratchet-floor —
      fail only on regression *below* the measured baseline, never an aspirational
      target) for the core packages (`internal/auth`, `internal/s3api`,
      `internal/meta/{memory,tikv}`, `internal/data/{memory,placement}`,
      `internal/crypto/kms`). The job runs green on the cycle branch before the gate
      is wired.
- [ ] Branch-protection update is prepared as a documented **manual post-merge
      step** (the new check context + the `gh api` command), NOT executed by the
      autonomous cycle — headless Ralph lacks admin-API auth (same posture as the
      ci-green cycle, where protection was applied by hand after merge). The story's
      job is to make the check green and write the exact apply command into the
      report.
- [ ] Verify via the final green CI run on the cycle branch.

## Functional Requirements

- FR-1: All new tests run on the **default build tag** (TiKV + memory, no cgo)
  unless they genuinely require ceph (`integration`+`ceph` tag) or a container
  (`integration` tag, testcontainers MinIO/PD/TiKV).
- FR-2: No test is duplicated — every story extends the nearest existing test file
  / harness (`newHarness`, `storetest` contract, `racetest/workload.go`,
  `*_integration_test.go`) rather than forking a parallel one. The PRD names the
  target file for each story.
- FR-3: Every story that finds a real bug fixes it in the same commit, or files a
  `ROADMAP.md` P-item with a documented residual risk if it cannot fit one
  iteration.
- FR-3a: Small testability refactors are permitted (extract an interface, add a
  seam/hook for mocking) when the code under test is not exercisable as-is. Keep
  the refactor minimal and behaviour-preserving — no opportunistic "while I'm here"
  rewrites; the diff must stay scoped to enabling the test.
- FR-4: Every story is verified by a CI run on the cycle branch; "passes locally"
  is necessary but not sufficient (prior cycle: failures that only repro in CI).
- FR-5: The cycle produces exactly one new tracked artifact —
  `tasks/qa-readiness-report.md` — updated incrementally and finalized in US-013.
- FR-8: If a story's acceptance criteria cannot land in one Ralph iteration (one
  context window), the agent SPLITS it into `US-00N` + a new trailing story rather
  than half-finishing — and records the split in `progress.txt`. US-009/US-010
  (drain vs mover) are already split for this reason; US-005 (auth matrix) and
  US-011 (multi-backend corruption) are the next most likely split candidates.
- FR-6: Cassandra is NOT added to the per-PR CI gate; if a story needs the
  Cassandra contract, it runs the existing gated/integration path, not the PR gate.
- FR-7: Secure-default invariant is asserted, never weakened: auth mode `required`
  is the default and denies anonymous; `off` is dev-only full-access and must be
  documented as such in any test that uses it.

## Non-Goals (Out of Scope)

- Re-enabling Cassandra integration tests on per-PR CI (tracked separately as the
  existing latent-debt P2).
- Implementing new product features (content-addressed dedup, Intelligent-Tiering,
  Select Object Content remain ROADMAP items, not QA scope).
- Chasing the 13 deliberate `s3-tests` gaps (SigV2, boto3 prefix-decode, anonymous
  list) — these stay documented deliberate gaps.
- Performance/throughput benchmarking and SLO definition (separate effort; this
  cycle covers correctness-under-load, not latency targets).
- Fuzzing harnesses (out of scope for this cycle; may be a follow-on P3).

## Technical Considerations

- Test harness entry points to reuse: `internal/s3api/testutil_test.go`
  (`newHarness(t)`, `h.doString`, `h.mustStatus`); `internal/meta/storetest`
  (`Run(t, factory)` contract, exercised on TiKV + memory); `internal/racetest`
  (`workload.go`, the verifier + tracker); the `*_integration_test.go` MinIO
  pattern (`startMinio`, `MINIO_KMS_SECRET_KEY` for SSE).
- Local macOS/lima caveat: `internal/s3api` test build needs cgo (Xcode license) —
  treat local `build failed: Xcode license` as an env issue, not a code bug; CI
  (Linux) is the source of truth, matching FR-4.
- Drain/rebalance, GC fan-out, and SSE-KMS paths are integration-tagged and need
  containers (PD/TiKV, MinIO, optionally ceph) — these stories run in the
  `integration` / `e2e` CI jobs, not the unit job.
- The corruption hook `CorruptFirstChunk()` exists on **both** RADOS and the memory
  backend (`internal/data/memory/backend.go:148`); the
  `sse_object_test.go:207` skip is a stale guard (fires on a no-op return, not a
  missing method). US-011 fixes the guard so corruption runs on memory (default tag)
  + RADOS + MinIO — no new hook is added.

## Success Metrics

- Every `internal/s3api` handler is marked covered (not "thin"/"uncovered") in the
  US-001 matrix by US-013, or its gap is a documented residual risk.
- `conditional.go` goes from 0 direct tests to a full RFC 7232 matrix.
- The adversarial auth matrix proves rejection on every tamper class with a passing
  positive control.
- `internal/racetest` gains multipart, versioning/CAS, and GC/stats concurrency
  scenarios, all green under `-race` in CI.
- A full CI run on the cycle branch is green end-to-end at US-013.
- The report carries an explicit, justified GO/NO-GO verdict with a residual-risk
  list — the deliverable that answers "is Strata ready for users".

## Resolved Decisions

- **Coverage gate:** informational in US-001; **promoted to a required CI gate in
  US-013** (ratchet-floor = US-001 baseline; branch-protection apply is a manual
  post-merge step, not run by the autonomous cycle).
- **US-011 corruption:** memory backend already has `CorruptFirstChunk()`, so the
  corruption assertion runs on **memory (default tag) + RADOS + MinIO**; US-011 fixes
  the stale `sse_object_test.go:207` skip. No new corruption hook is added.
- **Testability refactors:** **allowed but minimal** (FR-3a) — interface/seam
  extraction only when needed to make code testable; no opportunistic rewrites.
- **Deliberate `s3-tests` gaps:** the 13 stay deliberate; a story may reclassify one
  as a real bug only if it proves a genuine divergence (then it gets fixed +
  re-baselined per US-013).
