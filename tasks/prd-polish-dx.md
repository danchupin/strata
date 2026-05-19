# PRD: Polish — DX + latent-bug fixes

## Introduction

Polish cycle bundling 3 small fixes plus a smoke-validation story.
Scope is deliberately narrow: one stuck-worker latent bug, one flaky test,
one worker-retry gap. No new feature surface, no new admin endpoint, no UI
work. Goal is to clear small operator/contributor papercuts and prove the
result with a single smoke pass.

The originally-scoped fourth story (module-tags cleanup pushing go-ceph out
of the default-tag dependency graph) was dropped during PRD review: only
`internal/data/rados/{backend,rebalance}.go` import go-ceph and both files
are already `//go:build ceph`; `internal/data/rados/stub.go` covers the
default tag. `go build ./...` is already hermetic on a box without
librados. Removing the direct `require github.com/ceph/go-ceph` line from
`go.mod` would require splitting the module — out of scope for this polish
cycle.

Branch: `ralph/polish-dx`. Starts from `main` per the cycle-branch policy.

## Goals

- Stop `gc.Worker` from accumulating failed-but-still-listed entries in the
  GC backlog when the underlying RADOS / S3 object is already gone — lift
  the ENOENT classifier into the data backend layer so the worker's
  ack-vs-loop decision is backend-agnostic.
- Make `TestThreeReplicaDistribution` deterministic so
  `make test -race -count=N` is reliable signal for unrelated changes.
- Give the lifecycle worker a bounded transient-error retry so a transient
  network hiccup mid-batch doesn't push the whole batch to the next tick.
- Prove all of the above with one smoke pass and close the ROADMAP entries
  in the same cycle.

## User Journey

Three personas, all touched by one of the three fixes:

- **Operator running a drain in a degraded cluster.** Sibling GC leader had
  already swept a chunk; the local `gc.Worker` hits ENOENT on the
  per-chunk `Data.Delete` call, today logs warn + returns nil from the
  inner goroutine without ack'ing the entry — next tick re-issues the
  same batch from `ListGCEntriesShard` and gets the same entries back
  forever. After the fix the backend's `Delete` impl lifts native ENOENT
  into `data.ErrChunkNotFound`, the worker recognises it as terminal,
  acks the entry, and the GC queue makes forward progress.
- **Operator running a lifecycle pass through a transient S3 / network
  blip.** Today a single 503 mid-batch leaves the rest of the batch
  un-actioned until the next tick (default 5 m); after the fix the
  worker retries the operation with 1s→3s→10s backoff (ctx-aware) before
  giving up and deferring to the next tick.
- **Developer running `make test -race -count=20` on `main`.** Today
  `TestThreeReplicaDistribution` flakes ~30 % of the time (random UUID
  + FNV-32a-mod-3 distribution skew — empirically 4/20 fail). After the
  fix the test seeds from a deterministic SHA1 UUID derivation by bucket
  index and is reproducible run-to-run.

## User Stories

### US-001: gc.Worker ack's terminal ENOENT instead of looping

**Description:** As an operator, I want `gc.Worker` to recognise that a
chunk already deleted by a sibling leader is the terminal state and ack
the entry so the queue keeps moving, instead of accumulating
failed-but-still-listed entries in the GC backlog forever.

**Acceptance Criteria:**
- [ ] New backend-agnostic sentinel `data.ErrChunkNotFound` declared in
      `internal/data/errors.go` (or `internal/data/data.go` — verify):
      `var ErrChunkNotFound = errors.New("chunk not found")`. Goes in a
      build-tag-free file so callers compile regardless of `-tags ceph`.
- [ ] RADOS backend lifts native `goceph.ErrNotFound` at the boundary —
      in `internal/data/rados/backend.go` `Delete` method, when the
      underlying `ioctx.Delete(oid)` returns an error matching
      `errors.Is(err, goceph.ErrNotFound)`, wrap and return
      `fmt.Errorf("chunk %s: %w", oid, data.ErrChunkNotFound)` so callers
      match via `errors.Is(err, data.ErrChunkNotFound)`.
- [ ] S3 backend lifts similarly — in `internal/data/s3/backend.go`
      `Delete` method, when the AWS SDK returns `NoSuchKey`, wrap and
      return `data.ErrChunkNotFound`.
- [ ] Memory backend behaviour preserved — returns nil on missing chunk
      today (no-op delete); no change needed.
- [ ] gc.Worker fix point: `internal/gc/worker.go` inner `eg.Go` body
      inside `drainCount` (~lines 222-228 — the
      `if err := w.Data.Delete(delCtx, manifest); err != nil` branch).
      On `errors.Is(err, data.ErrChunkNotFound)`, treat as terminal:
      bump `strata_gc_terminal_ack_total{reason="enoent"}`, skip the
      `recordErr` + `subErr` lines, continue straight to
      `w.Meta.AckGCEntry(delCtx, w.Region, e)` (the same ack call used
      on the success path).
- [ ] Non-ENOENT errors keep current behaviour (log warn, set
      `subErr`, call `recordErr`, return nil WITHOUT ack — next tick
      retries). Only `data.ErrChunkNotFound` is reclassified as terminal
      in this story; pool-not-found, mis-routed-cluster, transport
      errors stay loop-fail-with-warn per scope decision.
- [ ] No new env knob — `STRATA_GC_ACK_ON_ERROR` discussed in scoping
      and rejected.
- [ ] Counter `strata_gc_terminal_ack_total` registered in
      `internal/metrics/gc.go` (or wherever existing `strata_gc_*`
      counters live — verify via grep) with `reason` label.
- [ ] Unit test in `internal/gc/worker_test.go`: fake `Data` returns
      `data.ErrChunkNotFound` (wrapped) for OID X, generic transport
      error for OID Y, success for OID Z. After one `RunOnce`: X is
      ack'd + counter incremented; Y is NOT ack'd + counter NOT bumped +
      stickyErr surfaces Y's error; Z is ack'd. Two more `RunOnce` calls:
      X never re-appears (already ack'd); Y still re-appears (no ack);
      Z is gone.
- [ ] Backend `Delete` unit tests confirm the lift: rados backend
      (`-tags ceph` required, skip otherwise) — `Delete` on a
      non-existent OID returns an error satisfying
      `errors.Is(err, data.ErrChunkNotFound)`. S3 backend — same shape
      with mocked `NoSuchKey` AWS response.
- [ ] `go vet ./...` and `go vet -tags ceph ./...` both pass
- [ ] `go test -race ./internal/gc/... ./internal/data/...` passes

### US-002: Deterministic seed for TestThreeReplicaDistribution

**Description:** As a developer, I want `make test -race -count=N` to be
reliable signal for the changes I'm working on, not flake ~30 % of the
time on an unrelated distribution test.

**Acceptance Criteria:**
- [ ] Root cause is `internal/lifecycle/distribute_test.go:54` —
      `seedExpiringBucket` generates the bucket UUID via
      `uuid.New().String()`. With 9 random UUIDs +
      `bucketReplicaIndex` (FNV-32a mod 3), the per-replica counts
      legitimately drift to 6/3/0 or 7/2/0 — the test's `[1, 5]`
      per-replica guard fails ~30 % of runs.
- [ ] Fix shape: add `seedExpiringBucketWithID(t, ctx, store, be, name, id uuid.UUID)`
      variant that accepts a caller-supplied UUID.
      `seedExpiringBucket` becomes a thin wrapper calling the new
      variant with `uuid.New()` so the other 4 call sites (lines 76,
      204, 243, and the existing line 120 if not migrated) keep their
      current shape.
- [ ] `TestThreeReplicaDistribution` (the test function starting near
      line 111) switches to the new variant: derive each bucket UUID
      via `uuid.NewSHA1(uuid.Nil, []byte(fmt.Sprintf("bucket-%d", i)))`
      so the FNV-32a-mod-3 hash is reproducible run-to-run.
- [ ] Hard-coded expected split: compute the deterministic per-replica
      counts once (run the test, observe the triple) and replace the
      `[1, 5]` guard assertion with the exact triple via
      `require.Equal(t, [3]int{a, b, c}, counts)`.
- [ ] Invariant preserved — the `totalDeletes == buckets` assertion
      (the real correctness signal: every bucket processed exactly once
      across replicas) stays exactly as it is today.
- [ ] Run `go test -race -count=20 -run TestThreeReplicaDistribution
      ./internal/lifecycle/...` → 20/20 pass (today: ~14/20).
- [ ] Touch only `TestThreeReplicaDistribution` + the new helper. Do
      NOT modify the other 4 callers of `seedExpiringBucket` — keeping
      them on random UUIDs preserves their existing coverage.
- [ ] `go vet ./...` passes

### US-003: Lifecycle worker bounded transient-error retry

**Description:** As an operator, I want the lifecycle worker to retry a
transient failure (network blip, 503, ctx-deadline) before abandoning the
remainder of the batch to the next tick.

**Acceptance Criteria:**
- [ ] Call sites: `internal/lifecycle/worker.go` per-action calls —
      `expireNoncurrent` line 393 (`w.Meta.DeleteObject(ctx, b.ID, v.Key, v.VersionID, true)`),
      plus any other per-object `Data.Delete` / `Meta.DeleteObject`
      site inside `applyRule` / `applyNoncurrentActions` (enumerate via
      `grep 'w\.Meta\.DeleteObject\|w\.Data\.Delete'` in the file).
- [ ] Wrap each call site in `retryAction(ctx, func() error { ... })`:
      single retry entry point so call sites stay compact.
      `retryAction`: 3 attempts total, backoff `1s → 3s → 10s` (sleep
      BEFORE the 2nd + 3rd attempt).
- [ ] Hard-coded retry budget; no `STRATA_LIFECYCLE_RETRY_*` env knob.
- [ ] Transient classifier `internal/lifecycle/retry.go` (or fold into
      worker.go if < 30 LoC):
      `isTransient(err) bool` returns true for
      `errors.Is(err, context.DeadlineExceeded)`,
      `errors.Is(err, syscall.ECONNRESET)`,
      `errors.Is(err, syscall.ETIMEDOUT)`, and any error whose chain
      contains an `interface{ Temporary() bool }` that returns true.
      Returns false for `errors.Is(err, meta.ErrNotFound)`,
      `errors.Is(err, data.ErrChunkNotFound)`,
      `errors.Is(err, data.ErrNoSuchKey)`,
      `errors.Is(err, data.ErrNoSuchBucket)`, and any error not
      satisfying transient.
- [ ] Terminal errors short-circuit the retry loop (no sleep, no
      second attempt) — caller continues to the next action.
- [ ] Ctx-aware sleep: between attempts, race `time.After(d)` against
      `ctx.Done()` so a cancelled lifecycle worker exits its retry
      loop immediately on shutdown — return `ctx.Err()` if cancelled.
- [ ] Counter `strata_lifecycle_retry_total{outcome="ok|exhausted|terminal"}`
      registered in `internal/metrics/lifecycle.go`. Outcome
      meanings: `ok` = succeeded on attempt > 1 (no bump on first-try
      success), `exhausted` = 3 attempts all transient-failed,
      `terminal` = first attempt hit a terminal error.
- [ ] Unit test `internal/lifecycle/retry_test.go`: table-driven —
      (a) success on first attempt → no retry, no counter bump;
      (b) transient × 2 then success → 2 sleeps total (use injectable
      `sleep func(time.Duration)` for test determinism), counter
      `ok=1`;
      (c) transient × 3 → counter `exhausted=1`, returns last error;
      (d) terminal on first attempt → no retry, counter `terminal=1`;
      (e) ctx-cancel mid-backoff → retry loop returns immediately with
      `ctx.Err()`.
- [ ] NEW ROADMAP entry added in US-004 (per CLAUDE.md "Discovering a
      new gap" rule — lifecycle no-retry is not pre-roadmapped).
- [ ] `go vet ./...` passes
- [ ] `go test -race ./internal/lifecycle/...` passes

### US-004: Smoke validation + ROADMAP close-flip + PRD removal

**Description:** As a future-maintainer, I want one explicit verification
pass that proves all three fixes landed correctly, plus the ROADMAP
entries flipped + the PRD markdown removed per the cycle-end-to-end
discipline.

**Acceptance Criteria:**
- [ ] Run `go test -race -count=5 -run TestThreeReplicaDistribution
      ./internal/lifecycle/...` → 5/5 pass (proves US-002 flake fix is
      stable across multiple runs).
- [ ] Run full `go test -race ./...` (default tag) → green; capture
      duration in `progress.txt`.
- [ ] Run `go test -tags ceph ./internal/data/rados/...` if Docker
      available (ENOENT-lift backend unit tests); else document the
      expected pass in `progress.txt`.
- [ ] Run `make vet` → green.
- [ ] Run `go build ./...` + `go build -tags ceph ./...` both succeed.
- [ ] `ROADMAP.md` close-flip in the same commit:
      - **`gc.Worker.drainCount` infinite-loops** (latent-bug entry) →
        flipped to Done, summary references US-001, the
        `data.ErrChunkNotFound` sentinel lift in backend `Delete`
        impls, and `strata_gc_terminal_ack_total{reason="enoent"}`
        counter.
      - **`TestThreeReplicaDistribution` is flaky** (latent-bug entry)
        → flipped to Done, summary references US-002
        deterministic-PRNG-via-`uuid.NewSHA1` approach + hard-coded
        expected split.
      - **NEW P3 entry** for `Lifecycle worker no-retry on transient
        failures` added under `Correctness & consistency` AND closed
        in same commit (per CLAUDE.md "Discovering a new gap" rule).
        Summary references the 3-attempt 1s→3s→10s retry, ctx-aware
        sleep racing `time.After` against `ctx.Done`,
        `strata_lifecycle_retry_total` counter, and the
        transient/terminal classifier.
- [ ] Each close-flip carries `(commit <pending>)` placeholder per the
      established cycle convention; SHA backfill lands on `main`
      post-merge as fast-follow commit.
- [ ] `tasks/prd-polish-dx.md` REMOVED via `git rm` per CLAUDE.md PRD
      lifecycle rule.
- [ ] `scripts/ralph/progress.txt` carries one US-004 block summarising
      smoke results + any learnings.
- [ ] `go vet ./...` passes
- [ ] All tests pass

## Functional Requirements

- FR-1: Data backend `Delete` impls MUST lift native ENOENT into the
  backend-agnostic sentinel `data.ErrChunkNotFound`.
- FR-2: `gc.Worker` MUST ack a GC entry whose backend `Delete` returns
  `data.ErrChunkNotFound` and MUST NOT re-emit it on the next tick.
- FR-3: `gc.Worker` MUST NOT ack on any other error class — current
  warn-and-loop behaviour preserved for pool-not-found,
  mis-routed-cluster, and transport errors.
- FR-4: `TestThreeReplicaDistribution` MUST be deterministic —
  repeated runs always produce identical per-replica counts.
- FR-5: Lifecycle worker MUST retry transient errors up to 3 attempts
  with `1s → 3s → 10s` backoff before deferring to the next tick.
- FR-6: Lifecycle worker MUST NOT retry terminal errors (NoSuchBucket,
  NoSuchKey, `meta.ErrNotFound`, `data.ErrChunkNotFound`,
  `data.ErrNoSuchKey`, `data.ErrNoSuchBucket`).
- FR-7: Lifecycle worker retry sleep MUST be ctx-aware (cancellable
  mid-backoff via `ctx.Done()`).
- FR-8: Operator-visible counters
  `strata_gc_terminal_ack_total{reason}` and
  `strata_lifecycle_retry_total{outcome}` MUST be registered and unit-
  tested.
- FR-9: ROADMAP MUST carry close-flip × 2 (existing entries) + 1 new-
  and-closed entry (lifecycle retry) in the US-004 commit.

## Non-Goals

- No new admin endpoint.
- No UI work — none of the 3 fixes surfaces in the operator console.
- No env knob for retry budget (`STRATA_LIFECYCLE_RETRY_MAX` rejected
  in scoping).
- No env knob for GC ack policy (`STRATA_GC_ACK_ON_ERROR` rejected in
  scoping — ENOENT-only).
- No expansion of ENOENT-ack to pool-not-found or
  mis-routed-cluster — those stay warn-and-loop.
- No retry on non-lifecycle workers (GC, notify, replicator) — only
  the lifecycle worker gets the bounded retry in this cycle.
- No ADR seed (parked).
- No module-tags cleanup (parked — see Introduction).

## Technical Considerations

- **Sentinel placement** — `data.ErrChunkNotFound` lives in a build-
  tag-free file under `internal/data/` so the gc worker compiles
  regardless of `-tags ceph`. The lift happens inside each backend's
  `Delete` impl (RADOS, S3) so the gc worker stays
  backend-agnostic.
- **Counter cardinality** — `strata_gc_terminal_ack_total{reason}`
  has only `reason="enoent"` today; future ENOENT-adjacent terminal
  classes can add labels without metric churn.
  `strata_lifecycle_retry_total{outcome}` has 3 label values
  (`ok|exhausted|terminal`) — acceptable; resisting an `action` label
  (transition/expiration/abort) keeps the metric tight.
- **`seedExpiringBucket` blast radius** — function is shared with 4
  test functions; adding a `WithID` variant rather than modifying the
  caller-side UUID gen preserves coverage on the other 4 sites that
  may rely on random UUID behaviour.
- **Lifecycle retry helper** — `retryAction(ctx, fn func() error) error`
  is the single retry entry point; call sites stay one-liner:
  `if err := retryAction(ctx, func() error { return w.Meta.DeleteObject(...) }); err != nil { ... }`.

## Success Metrics

- `make test -race -count=20 -run TestThreeReplicaDistribution
  ./internal/lifecycle/...` → 20/20 pass (today: ~14/20).
- `gc.Worker` no longer accumulates ENOENT-failed entries —
  counter `strata_gc_terminal_ack_total{reason="enoent"}` visible in
  Prom post-deploy on the next sibling-leader-race operator report.
- 3 ROADMAP entries close in one cycle (2 existing + 1 new-and-
  closed).
- Cycle ships in ≤ 4 stories; no UI, no admin endpoint, no env knob.

## Open Questions

- Should the RADOS / S3 backend `Delete` impls also lift other
  "obviously terminal" errors (e.g. pool-not-found, bucket-not-found)
  into a shared sentinel for the gc worker to ack? Default NO this
  cycle — narrow scope to ENOENT only; revisit if operators report
  the same wedging pattern on those classes.
- `retryAction` placement — `internal/lifecycle/retry.go` keeps the
  helper lifecycle-scoped; if a second worker (e.g. notify) later
  wants the same retry shape, lift to `internal/workers/retry.go`.
  Default to lifecycle-scoped now.
