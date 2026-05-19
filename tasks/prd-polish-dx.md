# PRD: Polish — DX + latent-bug fixes

## Introduction

Polish cycle bundling 4 small fixes plus a smoke-validation story.
Scope is deliberately narrow: one stuck-worker latent bug, one flaky test, one
worker-retry gap, one dependency-graph cleanup. No new feature surface, no new
admin endpoint, no UI work. Goal is to clear small operator/contributor
papercuts that have accumulated and prove the result with a single smoke pass.

Branch: `ralph/polish-dx`. Starts from `main` per the cycle-branch policy.

## Goals

- Stop `gc.Worker` from wedging on a single batch when the underlying RADOS
  object is already gone (ENOENT) — operator never has to manually flush the
  GC queue to unstick a healthy chunk-delete loop.
- Make `TestThreeReplicaDistribution` deterministic so `make test -race -count=N`
  is reliable signal for unrelated changes.
- Give the lifecycle worker a bounded transient-error retry so a transient
  network hiccup mid-batch doesn't push the whole batch to the next tick.
- Remove `github.com/ceph/go-ceph` from the default-tag dependency graph so a
  fresh contributor on a machine without librados can `go build ./...` /
  `go mod tidy` without warnings or surprise transitive bloat.
- Prove all of the above with one smoke pass and close the ROADMAP entries in
  the same cycle.

## User Journey

Three personas, all touched by one of the four fixes:

- **Operator running a drain in a degraded cluster.** Sibling GC leader had
  already swept a chunk; the local `gc.Worker` hits ENOENT, today wedges its
  batch and stops making forward progress on every chunk after it. After the
  fix the worker ack's ENOENT (chunk gone is the terminal state) and the
  drain finishes on the documented ETA.
- **Operator running a lifecycle pass through a transient S3 / network blip.**
  Today a single 503 mid-batch leaves the rest of the batch un-actioned until
  the next tick (default 5 m); after the fix the worker retries the operation
  with 1s→3s→10s backoff before giving up and deferring to the next tick.
- **Developer running `make test -race -count=20` on `main`.** Today
  `TestThreeReplicaDistribution` flakes ~30 % of the time (random UUID +
  FNV-32a distribution skew). After the fix the test seeds from a fixed PRNG
  and is deterministic.
- **Contributor cloning the repo on a macOS lima box without librados.**
  Today `go build ./...` works but `go mod tidy` complains about
  unreachable cgo packages and `go.sum` carries go-ceph entries the build
  never uses. After the fix the default-tag build graph is hermetic; the
  `-tags ceph` build remains byte-for-byte equivalent.

## User Stories

### US-001: gc.Worker ack's terminal ENOENT instead of looping

**Description:** As an operator, I want `gc.Worker` to recognise that a chunk
already deleted by a sibling leader is the terminal state and ack the entry
so the queue keeps moving, instead of wedging the worker on the same batch
forever.

**Acceptance Criteria:**
- [ ] `internal/gc/worker.go` (around `:123-126`): on `errors.Is(err, rados.ErrNotFound)`
      from `Data.Delete(ctx, entry.Cluster, entry.Pool, entry.OID)`, call
      `Meta.AckGCEntry(ctx, entry)` and continue to the next entry — do NOT
      return early from the per-entry goroutine without ack.
- [ ] Non-ENOENT errors keep current behaviour (log warn, no ack, next tick
      retries) — only ENOENT is reclassified as terminal in this story; pool-
      not-found and mis-routed-cluster stay loop-fail with warn per scope
      decision (operator should still investigate those).
- [ ] New `gc.Worker` counter
      `strata_gc_terminal_ack_total{reason="enoent"}` increments per ack-on-
      ENOENT path so operators can see how often the sibling-leader race
      fires.
- [ ] Unit test in `internal/gc/worker_test.go`: fake `Data` returns
      `rados.ErrNotFound` for OID `X`, real error for OID `Y`, success for
      OID `Z`. After one tick: X is ack'd + counter incremented; Y is NOT
      ack'd + counter NOT bumped; Z is ack'd. Two more ticks: X never
      re-appears (already ack'd); Y still re-appears (no ack); Z is gone.
- [ ] No new env knob — `STRATA_GC_ACK_ON_ERROR` discussed in scoping and
      rejected (keep behaviour matrix narrow).
- [ ] `go vet ./...` passes
- [ ] `go test -race ./internal/gc/...` passes

### US-002: Deterministic seed for TestThreeReplicaDistribution

**Description:** As a developer, I want `make test -race -count=N` to be
reliable signal for the changes I'm working on, not flake ~30 % of the time
on an unrelated distribution test.

**Acceptance Criteria:**
- [ ] `internal/lifecycle/distribute_test.go:111` (`TestThreeReplicaDistribution`)
      replaces `uuid.New()` random-UUID seeding with a fixed-PRNG seeded shim:
      `rng := rand.New(rand.NewSource(0xCAFE))`, derive bucket UUIDs via
      `uuid.NewSHA1(uuid.Nil, []byte(fmt.Sprintf("bucket-%d", i)))` (or
      equivalent deterministic generator).
- [ ] With the deterministic seed, asserting the per-replica split lands on
      the known triple (compute once, hard-code expected counts in the test
      — e.g. `assert.Equal(t, [3]int{4, 3, 2}, counts)`).
- [ ] The `totalDeletes == buckets` invariant assertion (the real
      correctness signal) stays.
- [ ] Run `go test -race -count=20 -run TestThreeReplicaDistribution
      ./internal/lifecycle/...` → 20/20 pass.
- [ ] `go vet ./...` passes

### US-003: Lifecycle worker bounded transient-error retry

**Description:** As an operator, I want the lifecycle worker to retry a
transient failure (network blip, 503, ctx-deadline) before abandoning the
remainder of the batch to the next tick, so a 200 ms network hiccup doesn't
delay 999 successful expirations by 5 minutes.

**Acceptance Criteria:**
- [ ] `internal/lifecycle/worker.go` per-action call site (the inner loop
      that invokes `Data.Delete` / `Meta.DeleteObject` per lifecycle action)
      wraps the call in a bounded retry: 3 attempts total, backoff
      `1s → 3s → 10s` (sleep BEFORE the second + third attempt).
- [ ] Hard-coded retry budget; no `STRATA_LIFECYCLE_RETRY_*` env knob (B
      option from scoping rejected — narrow this cycle).
- [ ] Transient classifier `internal/lifecycle/retry.go` (or fold into
      worker.go if < 30 LoC):
      `isTransient(err) bool` returns true for `errors.Is(err, context.DeadlineExceeded)`,
      `errors.Is(err, syscall.ECONNRESET)`, `errors.Is(err, syscall.ETIMEDOUT)`,
      and any error whose chain contains an `interface{ Temporary() bool }`
      that returns true. Returns false for `errors.Is(err, meta.ErrNotFound)`,
      `errors.Is(err, data.ErrNoSuchKey)`, `errors.Is(err, data.ErrNoSuchBucket)`,
      and any error that does not satisfy the transient predicate.
- [ ] Terminal errors short-circuit the retry loop (no sleep, no second
      attempt) — caller continues to the next action.
- [ ] Counter `strata_lifecycle_retry_total{outcome="ok|exhausted|terminal"}`
      stamps each call site: `ok` = succeeded on attempt > 1, `exhausted` =
      3 attempts all transient-failed, `terminal` = first attempt hit a
      terminal error.
- [ ] Unit test `internal/lifecycle/retry_test.go`: table-driven —
      (a) success on first attempt → no retry, no counter bump;
      (b) transient × 2 then success → 2 sleeps total (use injectable
      `sleep func(time.Duration)`), counter `ok=1`;
      (c) transient × 3 → counter `exhausted=1`, returns last error;
      (d) terminal on first attempt → no retry, counter `terminal=1`;
      (e) ctx-cancel mid-backoff → retry loop returns immediately with
      ctx.Err().
- [ ] **NEW ROADMAP entry** added in US-005 (per CLAUDE.md "Discovering a
      new gap" rule — lifecycle no-retry is not currently on ROADMAP).
- [ ] `go vet ./...` passes
- [ ] `go test -race ./internal/lifecycle/...` passes

### US-004: Module tags cleanup — go-ceph out of default-tag dep graph

**Description:** As a contributor, I want `go build ./...` and `go mod tidy`
on a machine without librados to be hermetic — no go-ceph reachable through
the default-tag import graph, no warnings, no transitive bloat in
`go.mod`/`go.sum`.

**Acceptance Criteria:**
- [ ] `internal/data/rados/` gets a stub mirror under `//go:build !ceph`:
      every exported type (`Backend`, `Config`, helper structs) has a stub
      symbol returning a sentinel error `data.ErrRadosNotCompiled`
      (`errors.New("rados backend not compiled — rebuild with -tags ceph")`).
      Real implementations stay under `//go:build ceph`.
- [ ] Audit every Strata package for direct `import "github.com/ceph/go-ceph/..."`.
      Move each such import into a `//go:build ceph` file; the
      build-tag-free file uses only the stub-mirror types from
      `internal/data/rados`.
- [ ] `internal/serverapp/serverapp.go` backend selector keeps both branches
      compilable under both tags — the `case "rados":` branch returns a
      typed error containing `ErrRadosNotCompiled` when default-tag stub
      is hit (operator runs the wrong binary against the rados config).
- [ ] `cmd/strata/server.go` and any other cmd entry has zero go-ceph imports
      in default-tag files.
- [ ] Run `go mod tidy` on a clean checkout (no `-tags`) — `go.mod` no
      longer lists `github.com/ceph/go-ceph` in the top `require` block;
      either it disappears entirely OR it moves to `// indirect` (acceptable
      if a transitive dep still references it — verify via `go mod why`).
- [ ] Run `go mod tidy -tags ceph` — `go.mod`/`go.sum` regains the entry as
      direct dep; resulting state is what gets committed (the tidied-with-
      tag shape).
- [ ] Default `go build ./...` succeeds on a box without librados (verify
      in a clean container if local box has librados installed: run
      `docker run --rm -v $PWD:/src -w /src golang:1.23 go build ./...` —
      no librados in the golang base image, must succeed).
- [ ] `go build -tags ceph ./...` still succeeds against librados (run
      inside the existing ceph-base image, e.g.
      `docker build -f deploy/docker/Dockerfile .` succeeds end-to-end).
- [ ] `go test ./...` (default tag) passes — stub returns sentinel error
      where rados is touched, callers handle it (or test skips on
      sentinel).
- [ ] `go test -tags ceph ./...` passes against the ceph-tagged build
      (smoke via `make test-rados` if Docker is available, else document
      the expected pass).
- [ ] `go vet ./...` and `go vet -tags ceph ./...` both pass.

### US-005: Smoke validation + ROADMAP close-flip + PRD removal

**Description:** As a future-maintainer, I want one explicit verification
pass that proves all four fixes landed correctly, plus the ROADMAP entries
flipped + the PRD markdown removed per the cycle-end-to-end discipline.

**Acceptance Criteria:**
- [ ] Run `go test -race -count=5 -run TestThreeReplicaDistribution
      ./internal/lifecycle/...` → 5/5 pass (proves US-002 flake fix is
      stable across multiple runs).
- [ ] Run default `go build ./...` from a `make clean`-equivalent state →
      succeeds without referencing go-ceph in any compilation step
      (`-x -v` logs grep-clean for `github.com/ceph/go-ceph`).
- [ ] Run `go build -tags ceph ./...` → succeeds against librados as today.
- [ ] Run full `go test -race ./...` (default tag) → green; capture
      duration in `progress.txt`.
- [ ] Run `make vet` → green.
- [ ] `ROADMAP.md` close-flip in the same commit:
      - **`gc.Worker.drainCount` infinite-loops** (latent-bug entry) →
        flipped to Done, summary references US-001 + `strata_gc_terminal_ack_total`.
      - **`TestThreeReplicaDistribution` is flaky** (latent-bug entry) →
        flipped to Done, summary references US-002 fixed-PRNG seed.
      - **Module tags cleanup** (P3 DX entry) → flipped to Done, summary
        references US-004 stub-mirror approach.
      - **NEW P3 entry** for "Lifecycle worker no-retry on transient
        failures" added AND closed in same commit (per CLAUDE.md
        "Discovering a new gap" rule — US-003 is not pre-roadmapped).
- [ ] Each close-flip carries `(commit <pending>)` placeholder per the
      established cycle convention; the SHA backfill lands on `main`
      post-merge as the fast-follow commit.
- [ ] `tasks/prd-polish-dx.md` REMOVED via `git rm` per CLAUDE.md PRD
      lifecycle rule (markdown PRD is disposable; canonical record is the
      Ralph auto-archive pair under `scripts/ralph/archive/<date>-<branch>/`).
- [ ] `scripts/ralph/progress.txt` carries one US-005 block summarising
      smoke results + any learnings.
- [ ] `go vet ./...` passes
- [ ] All tests pass

## Functional Requirements

- FR-1: `gc.Worker` MUST ack a GC entry whose backend delete returns
  `rados.ErrNotFound` and MUST NOT re-emit it on the next tick.
- FR-2: `gc.Worker` MUST NOT ack on any other error class — current
  warn-and-loop behaviour preserved for pool-not-found, mis-routed-cluster,
  and transport errors.
- FR-3: `TestThreeReplicaDistribution` MUST be deterministic — repeated
  runs always produce identical per-replica counts.
- FR-4: Lifecycle worker MUST retry transient errors up to 3 attempts with
  1s → 3s → 10s backoff before deferring to the next tick.
- FR-5: Lifecycle worker MUST NOT retry terminal errors (NoSuchBucket,
  NoSuchKey, meta.ErrNotFound, data.ErrNoSuchKey, data.ErrNoSuchBucket).
- FR-6: Default-tag (no `-tags ceph`) build of every Strata package MUST
  succeed without librados present on the build host.
- FR-7: Default-tag `go.mod` MUST NOT list `github.com/ceph/go-ceph` in the
  top `require` block (either removed entirely or moved to `// indirect`
  via stub-mirror).
- FR-8: `-tags ceph` build MUST remain byte-for-byte equivalent to today's
  ceph-tag binary — production RADOS path is untouched.
- FR-9: All four operator-visible counters / behaviour changes
  (`strata_gc_terminal_ack_total`, `strata_lifecycle_retry_total`,
  worker-retry sleep budget, ENOENT-ack classifier) MUST have unit-test
  coverage.
- FR-10: ROADMAP MUST carry close-flip × 3 (existing entries) + 1 new-and-
  closed entry (lifecycle retry) in the US-005 commit.

## Non-Goals

- No new admin endpoint.
- No UI work — none of the 4 fixes surfaces in the operator console.
- No env knob for retry budget (`STRATA_LIFECYCLE_RETRY_MAX` rejected in
  scoping — narrow this cycle).
- No env knob for GC ack policy (`STRATA_GC_ACK_ON_ERROR` rejected in
  scoping — ENOENT-only, no operator-tunable escalation).
- No ADR seed (parked — separate cycle once the docs/adr/ directory and
  template land).
- No expansion of ENOENT-ack to pool-not-found or mis-routed-cluster (4A
  scoping — those stay warn-and-loop; operator must investigate).
- No retry on non-lifecycle workers (GC, notify, replicator, etc.) — only
  the lifecycle worker gets the bounded retry in this cycle; expand later
  if the pattern proves out.
- No new make targets — smoke validation runs the existing `go test` +
  `go build` commands directly; `make smoke-polish` rejected as YAGNI.

## Technical Considerations

- **gc.Worker ENOENT classifier** lives in `internal/gc/worker.go`;
  `rados.ErrNotFound` is exported by `internal/data/rados/errors.go` and
  is build-tag-aware (the stub mirror from US-004 must re-export it).
  Coordinate US-001 ↔ US-004: US-004 lands first, US-001 imports the
  shared sentinel.
- **Lifecycle retry classifier** does NOT import the RADOS error
  sentinels — lifecycle worker should be backend-agnostic. Classify on
  `interface{ Temporary() bool }` + `context.DeadlineExceeded` + the
  generic meta/data sentinels.
- **Stub-mirror pattern** (US-004) — mirror every exported symbol from
  `internal/data/rados/` with the build-tag-flipped file convention:
  `backend.go` (real) → `backend_stub.go` (stub). Stub functions return
  `data.ErrRadosNotCompiled`. Internal types stay private; only exported
  surface gets mirrored.
- **`go mod tidy` outcome** depends on whether any transitive (e.g.
  `go-ceph` is a dep of zerolog or gocql — it isn't, but verify with
  `go mod why github.com/ceph/go-ceph`). If no transitive ref exists,
  default-tag `go.mod` drops the line entirely; if a transitive ref
  exists, it moves to `// indirect`. Both outcomes acceptable.
- **CI impact** — verify `.github/workflows/ci.yml` `lint+build` step
  still passes the default-tag build path after US-004. The Cassandra
  integration job already runs default-tag tests. The Docker build job
  exercises `-tags ceph`. No CI rewiring expected.

## Success Metrics

- `make test -race -count=20 -run TestThreeReplicaDistribution
  ./internal/lifecycle/...` → 20/20 pass (today: ~14/20).
- `gc.Worker` no longer wedges on operator-reported sibling-leader-ENOENT
  race — counter `strata_gc_terminal_ack_total` visible in Prom
  post-deploy.
- Default-tag `go.mod` has zero direct `go-ceph` reference.
- 4 ROADMAP entries close in one cycle (3 existing + 1 new-and-closed).
- Cycle ships in ≤ 5 stories; no UI, no admin endpoint, no env knob.

## Open Questions

- US-004 stub-mirror: should `data.ErrRadosNotCompiled` live in
  `internal/data/errors.go` (shared) or `internal/data/rados/stub_errors.go`
  (rados-scoped)? Default to the latter to keep `internal/data` free of
  backend-specific sentinels.
- Retry counter cardinality: `strata_lifecycle_retry_total{outcome}` has
  3 label values — acceptable. Should we add `action` label
  (transition/expiration/abort)? Default NO — keeps the metric tight.
