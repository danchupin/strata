# PRD: Binary consolidation — single `strata` server binary (CockroachDB-shape)

## Introduction

Strata currently ships eleven `cmd/*` binaries: one HTTP gateway, eight background
workers (gc, lifecycle, notify, replicator, access-log, inventory, audit-export,
manifest-rewriter), one one-shot SSE key-rotation worker (rewrap), and one operator
CLI (admin). Every worker is independently leader-elected through `internal/leader`,
so the split was never about correctness — it grew organically as Ralph stories landed.

The split has real operational cost: ten Docker entrypoints, ten Kubernetes
Deployments, ten dashboards / alert sets, and ten Cassandra connection pools per
cluster. CockroachDB ships a single `cockroach` binary; every node runs the full
stack and an internal scheduler picks which node owns which job through leases.
Strata's `internal/leader` already provides the lease primitive — the binary split
is the only thing keeping us from the same shape.

This PRD collapses the eight worker binaries plus the gateway into a single
`strata` binary with subcommands (CockroachDB-style: `strata server`, `strata
version`, `strata migrate`, ...). The operator CLI stays separate as `strata-admin`
to preserve a small auditable attack surface for privileged operations. The
one-shot rewrap moves into `strata-admin rewrap`. End state: **two binaries**.

## Goals

- One `strata` binary that can run the gateway plus any subset of background workers
  in a single process.
- One Docker image, one Kubernetes Deployment object, one Cassandra connection pool
  per replica.
- `strata-admin` stays as the separate operator CLI; one-shot SSE key rotation moves
  into it as `strata-admin rewrap`.
- Hard cut on old binary names — no thin wrappers, no symlinks. CI, Dockerfile,
  docker-compose, Makefile, and docs all migrate in one pass.
- Default `strata server` runs the three workers without which long-running S3
  traffic breaks (`gc`, `lifecycle`) — others are opt-in via `--workers` /
  `STRATA_WORKERS` so a default install does not create unused leader-lease churn.
- Each worker keeps its own `internal/leader` lease keyed by the worker's name, so
  N replicas of a single binary still elect exactly one writer per worker.
- Establish a durable rule (in project root `CLAUDE.md`) that every feature commit
  keeps `ROADMAP.md` current — closed items flip to `Done` with the commit SHA,
  newly discovered gaps and latent bugs land as new entries — so the roadmap is
  the honest state-of-project list at every SHA, not a drifting TODO file.

## User Stories

### US-001: Roadmap maintenance rule in project CLAUDE.md

**Description:** As a developer or autonomous agent doing feature work on Strata,
I need an explicit, durable rule in the project root `CLAUDE.md` that any commit
which closes or discovers a `ROADMAP.md` item must update the roadmap in the
same commit, so the roadmap stays an honest reflection of project state at every
SHA — not a stale TODO list that drifts behind the code. Lands first so all
subsequent stories in this cycle (and every future cycle) follow the rule.

**Acceptance Criteria:**
- [ ] New top-level section `## Roadmap maintenance` appended to project root
      `CLAUDE.md` (the file at the repo root, NOT the Ralph harness one at
      `scripts/ralph/CLAUDE.md`)
- [ ] Rule states: every commit that **closes** a `ROADMAP.md` item MUST flip
      the bullet to the format
      `~~**P<n> — <title>.**~~ — **Done.** <one-line summary>. (commit `<sha>`)`
      in the same commit. Acceptable to land the bullet edit in the immediate
      follow-up commit when the closing commit's SHA is needed inline (e.g.
      committing the feature, then a one-line roadmap edit referencing the
      previous SHA)
- [ ] Rule states: every commit that **discovers** a new gap, latent bug, or
      regression MUST add a new entry under the appropriate `ROADMAP.md` section:
      P1/P2/P3 by severity, or `Known latent bugs` for bugs. New entries follow
      the existing one-paragraph shape used in the file
- [ ] Rule applies to all work — Ralph autorun and human-driven commits alike.
      Includes one explicit example of each (close + new-discovery) so a junior
      reader knows the exact diff shape
- [ ] Rule notes the ROADMAP file is the canonical project state list; PRDs in
      `tasks/` are scoped to specific cycles and do NOT need to mirror the
      roadmap
- [ ] No code or test changes (docs-only story)
- [ ] `git diff CLAUDE.md` shows only the new section added; no unrelated edits
- [ ] Typecheck passes (no-op for docs; included for consistency)

### US-002: Cobra skeleton for the unified `strata` command

**Description:** As a developer, I need a root `cmd/strata/main.go` that uses
[`spf13/cobra`](https://github.com/spf13/cobra) (or stdlib `flag` with manual
dispatch — see Technical Considerations) to dispatch subcommands, so each worker
becomes a subcommand of one binary.

**Acceptance Criteria:**
- [ ] New `cmd/strata/main.go` with a root command that prints help and exits 0
- [ ] `strata version` subcommand prints the build's git SHA + Go runtime
- [ ] `strata server --help` prints flags and the `--workers` list. Flag set is
      cross-cutting only: `--listen`, `--workers`, `--auth-mode`, `--vhost-pattern`,
      `--log-level`. Per-worker tunables remain env-only.
- [ ] `go build -o bin/strata ./cmd/strata` succeeds with and without `-tags ceph`
- [ ] `go vet ./cmd/strata/...` passes
- [ ] `go test ./cmd/strata/...` covers the help and version paths

### US-003: `strata server` subcommand wires the existing gateway entrypoint

**Description:** As a developer, I want `strata server` to run the same code path
that `cmd/strata-gateway/main.go` runs today, so the migration is a refactor — not
a rewrite — and the smoke pass still passes.

**Acceptance Criteria:**
- [ ] `cmd/strata-gateway/main.go` content moves to `cmd/strata/server.go` as
      `func runServer(ctx, cfg) error` — no behavioural changes, just relocation
- [ ] `strata server` flags accept the same `STRATA_*` env vars the gateway reads
      today; explicit flags (`--listen`, `--auth-mode`, `--vhost-pattern`) override
      env, env overrides defaults
- [ ] `make smoke` passes pointing at `bin/strata server` instead of
      `bin/strata-gateway`
- [ ] `make smoke-signed` passes with the new binary
- [ ] No new top-level package; helper functions in `cmd/strata-gateway` move to a
      shared internal package only if they are genuinely reused by another worker

### US-004: Worker registry + leader-elected goroutine pattern + panic recovery

**Description:** As a developer, I need a `cmd/strata/workers/` package (or
equivalent) that registers each worker by name, owns its `internal/leader` session,
and runs its `Run(ctx)` loop in a goroutine, so `strata server --workers=gc,lifecycle`
spins up exactly those workers in one process.

**Acceptance Criteria:**
- [ ] New `cmd/strata/workers/registry.go` with `type Worker struct { Name string;
      Build func(deps Dependencies) (Runner, error) }` and a `Register(w Worker)`
      function
- [ ] `Dependencies` carries `*slog.Logger`, `meta.Store`, `data.Backend`,
      `*otel.Provider`, `prometheus.Registerer`, plus any per-worker extras the
      worker reads from env
- [ ] Each registered worker runs in its own goroutine with its own
      `leader.Session` keyed on the worker name (`gc-leader`, `lifecycle-leader`,
      ...) so the existing Cassandra `worker_locks` rows keep working unchanged
- [ ] Loss-of-lease cancels only that worker's context, never the gateway's or
      another worker's
- [ ] **Panic recovery:** each worker goroutine wraps its `Run(ctx)` in `defer
      recover()`. A caught panic logs a structured ERROR with the stack trace,
      increments a `strata_worker_panic_total{worker=<name>}` counter, releases
      the leader lease, waits a backoff, then re-acquires and restarts the
      worker. The process must NOT exit; the gateway must NOT be affected.
- [ ] Backoff between restarts is exponential with caps (1s, 5s, 30s, 2m max)
      and resets to 1s after the worker stays up for ≥5 minutes; backoff is
      injectable for tests
- [ ] `--workers=` parses `gc,lifecycle,notify` into a deduplicated set; unknown
      names cause an immediate startup error
- [ ] `STRATA_WORKERS` env var sets the same list (flag overrides env)
- [ ] Unit tests cover: registry round-trip, unknown name rejection, dependency
      injection, lease loss isolation, **panic-then-restart cycle (inject panic,
      verify worker restarts within backoff, verify gateway and sibling workers
      untouched, verify panic counter incremented)**
- [ ] `go test ./cmd/strata/workers/...` passes

### US-005: Migrate `gc` worker into the registry

**Description:** As a developer, I want `gc` registered as a worker so
`strata server --workers=gc` runs what `cmd/strata-gc` runs today.

**Acceptance Criteria:**
- [ ] `cmd/strata/workers/gc.go` registers a worker named `gc` whose `Build` mirrors
      `cmd/strata-gc/main.go` — same env-var parsing, same `internal/gc.Worker`
      construction
- [ ] Per-worker env vars (`STRATA_GC_INTERVAL`, `STRATA_GC_BATCH_SIZE`, ...) are
      documented in the `strata server --help` output
- [ ] `cmd/strata-gc/` directory is deleted (no thin wrapper)
- [ ] Existing `internal/gc` unit tests are unchanged
- [ ] `make smoke` still cleans up tombstoned chunks under `strata server --workers=gc`

### US-006: Migrate `lifecycle` worker into the registry

**Description:** As a developer, I want `lifecycle` registered as a worker.

**Acceptance Criteria:**
- [ ] `cmd/strata/workers/lifecycle.go` registers `lifecycle`
- [ ] Per-worker env vars documented in `--help`
- [ ] `cmd/strata-lifecycle/` deleted
- [ ] Existing `internal/lifecycle` unit + integration tests pass
- [ ] Lifecycle smoke (run a transition + an expiration + an MP-abort under the
      new binary) succeeds

### US-007: Migrate `notify` worker into the registry

**Description:** As a developer, I want the `notify` worker registered so
`strata server --workers=notify` runs what `cmd/strata-notify` runs today.

**Acceptance Criteria:**
- [ ] `cmd/strata/workers/notify.go` registers `notify`
- [ ] Per-worker env vars (`STRATA_NOTIFY_*`) documented in `--help`
- [ ] `cmd/strata-notify/` deleted
- [ ] Existing `internal/notify` tests unchanged and still pass
- [ ] Webhook + SQS sink dispatch verified by an integration smoke against the
      unified binary

### US-008: Migrate `replicator` worker into the registry

**Description:** As a developer, I want the replicator registered so
`strata server --workers=replicator` runs what `cmd/strata-replicator` runs today.

**Acceptance Criteria:**
- [ ] `cmd/strata/workers/replicator.go` registers `replicator`
- [ ] Per-worker env vars (`STRATA_REPLICATOR_*`) documented in `--help`
- [ ] `cmd/strata-replicator/` deleted
- [ ] Existing `internal/replicator` tests unchanged and still pass
- [ ] Strata-to-Strata replication round-trip verified under the unified binary

### US-009: Migrate `access-log` worker into the registry

**Description:** As a developer, I want the access-log worker registered so
`strata server --workers=access-log` runs what `cmd/strata-access-log` runs today.

**Acceptance Criteria:**
- [ ] `cmd/strata/workers/access_log.go` registers `access-log`
- [ ] Per-worker env vars (`STRATA_ACCESS_LOG_*`) documented in `--help`
- [ ] `cmd/strata-access-log/` deleted
- [ ] Existing `internal/accesslog` tests unchanged and still pass
- [ ] 5-minute flush of the `access_log_buffer` table into AWS-format log objects
      verified by an integration test

### US-010: Migrate `inventory` worker into the registry

**Description:** As a developer, I want the inventory worker registered so
`strata server --workers=inventory` runs what `cmd/strata-inventory` runs today.

**Acceptance Criteria:**
- [ ] `cmd/strata/workers/inventory.go` registers `inventory`
- [ ] Per-worker env vars (`STRATA_INVENTORY_*`) documented in `--help`
- [ ] `cmd/strata-inventory/` deleted
- [ ] Existing `internal/inventory` tests unchanged and still pass
- [ ] `manifest.json` + CSV.gz report round-trip verified under the unified binary

### US-011: Migrate `audit-export` worker into the registry

**Description:** As a developer, I want the audit-export worker registered so
`strata server --workers=audit-export` runs what `cmd/strata-audit-export` runs today.

**Acceptance Criteria:**
- [ ] `cmd/strata/workers/audit_export.go` registers `audit-export`
- [ ] Per-worker env vars (`STRATA_AUDIT_EXPORT_*`) documented in `--help`
- [ ] `cmd/strata-audit-export/` deleted
- [ ] Existing `internal/auditexport` tests unchanged and still pass
- [ ] Daily JSON-lines drain + source-partition delete round-trip verified under
      the unified binary

### US-012: Migrate `manifest-rewriter` worker into the registry

**Description:** As a developer, I want `manifest-rewriter` (US-049 of the prior
cycle) as a registered worker so the JSON → protobuf migration runs inside the
unified binary.

**Acceptance Criteria:**
- [ ] `cmd/strata/workers/manifest_rewriter.go` registers `manifest-rewriter`
- [ ] Per-worker env vars documented in `--help`
- [ ] `cmd/strata-manifest-rewriter/` deleted
- [ ] Existing `internal/manifestrewriter` tests pass
- [ ] Idempotent re-run (skip already-proto rows) verified

### US-013: Move `rewrap` into `strata-admin` as a subcommand

**Description:** As an operator, I want `strata-admin rewrap` to run the SSE master
key rotation worker, since it is a one-shot operation invoked by a human, not a
long-running daemon — keeping it in the admin CLI matches its actual use shape.

**Acceptance Criteria:**
- [ ] `cmd/strata-admin/rewrap.go` adds a `rewrap` subcommand that wires up
      `internal/rewrap` exactly as `cmd/strata-rewrap/main.go` does today
- [ ] `--target-key-id <id>` flag selects the destination wrap key; defaults match
      the env-driven flow
- [ ] Logs progress to stderr; exits 0 when every bucket completes; exits non-zero
      with a structured error if a bucket fails
- [ ] `cmd/strata-rewrap/` deleted
- [ ] Idempotency guarantee preserved (re-running skips already-rewrapped rows) —
      verified by an existing or new integration test
- [ ] No `--continue` / `--from-bucket` flags — resumption is automatic via the
      persisted per-bucket progress rows

### US-014: Delete `cmd/strata-gateway/`

**Description:** As a developer, I want the old gateway binary directory removed
once `strata server` is the supported entrypoint, so there is one source of truth.

**Acceptance Criteria:**
- [ ] `cmd/strata-gateway/` deleted
- [ ] Any helper packages it owned that are not reused are removed; reusable ones
      moved to `internal/<area>` with the rename in the same commit
- [ ] No file outside `cmd/strata/` references `cmd/strata-gateway`
- [ ] `go build ./...` and `go build -tags ceph ./...` both pass

### US-015: Dockerfile + entrypoint update

**Description:** As an operator, I want one Docker image whose entrypoint is the
unified `strata` binary, so `docker run strata server` is the canonical run shape.

**Acceptance Criteria:**
- [ ] `deploy/docker/Dockerfile` builds only `strata` and `strata-admin`
- [ ] Image `ENTRYPOINT ["/usr/local/bin/strata"]`, default `CMD ["server"]`
- [ ] Final image size does not regress more than 5% vs the previous gateway-only
      image (single binary should be smaller; this is the upper bound)
- [ ] `make docker-build` succeeds; image labels include the git SHA

### US-016: docker-compose update — single service with profiles

**Description:** As a developer running `make up-all`, I want one `strata` service
in compose with workers selected by environment and optional profile-gated extras,
so the default dev stack is the small "gateway + gc + lifecycle" shape.

**Acceptance Criteria:**
- [ ] `deploy/docker/docker-compose.yml` collapses
      `strata-gateway`/`strata-gc`/`strata-lifecycle`/...  services into a single
      `strata` service running `strata server`
- [ ] Default `STRATA_WORKERS=gc,lifecycle`
- [ ] `--profile features` enables additional services (still single binary, just
      additional workers): `STRATA_WORKERS` extended via compose override or a
      second `strata` replica with the larger worker list
- [ ] `make up`, `make up-all`, `make wait-cassandra`, `make wait-ceph` all work
      against the new compose shape
- [ ] `make smoke` and `make smoke-signed` pass against the new compose stack

### US-017: Makefile + scripts update

**Description:** As a developer, I want `make run-memory` / `make run-cassandra` to
launch the unified binary, so day-to-day development uses the same surface as prod.

**Acceptance Criteria:**
- [ ] `make build` builds `bin/strata` and `bin/strata-admin` (no other binaries)
- [ ] `make run-memory` runs `./bin/strata server` against in-memory backends
- [ ] `make run-cassandra` runs `./bin/strata server` against Cassandra metadata +
      memory data with `STRATA_WORKERS=gc,lifecycle`
- [ ] `scripts/s3-tests/run.sh` and `scripts/ralph/ralph.sh` invoke the new binary
- [ ] `scripts/smoke.sh`, `scripts/smoke-signed.sh` work unchanged or with minimal
      tweaks (env-var renames only)

### US-018: CI matrix update

**Description:** As a developer, I want CI to build, vet, and test only the two
binaries we ship, so the matrix shrinks and reflects the real deploy artifacts.

**Acceptance Criteria:**
- [ ] `.github/workflows/ci.yml` has two build jobs: `strata` and `strata-admin`
- [ ] `lint-build`, `unit`, `integration-cassandra`, `docker-build`, and `e2e`
      jobs all pass
- [ ] `e2e` runs against the unified binary (gateway + gc + lifecycle workers
      enabled by default)
- [ ] One additional `e2e-full` job (or matrix axis) runs with all S3-feature
      workers enabled (`notify,replicator,access-log,inventory,audit-export`) and
      verifies `/readyz` plus a notification round-trip

### US-019: Documentation update

**Description:** As a user reading the repo, I want every doc, example, and
architecture diagram to reflect the new two-binary shape — no stale references to
`cmd/strata-gateway` or `cmd/strata-gc` and friends.

**Acceptance Criteria:**
- [ ] `CLAUDE.md` updated: architecture diagram shows one `strata` binary running
      gateway + workers; `cmd/<binary>` references replaced with `strata server`
      and `strata-admin <subcommand>`
- [ ] `README.md` (if present) mirrors the change
- [ ] `docs/backends/scylla.md` and any `docs/` guides reference the new binary
- [ ] `examples/` directory commands point at the unified binary
- [ ] `ROADMAP.md` Consolidation section flips its "P1 — Single-binary `strata`"
      bullet from `TODO` to `Done` with a link to the merge commit

### US-020: Release notes + migration guide

**Description:** As an operator upgrading an existing deploy, I need a one-page
guide explaining which env vars moved, which old binary maps to which new
subcommand, and which dashboards / alerts to retire.

**Acceptance Criteria:**
- [ ] `docs/migrations/binary-consolidation.md` lists:
      old-binary → new-subcommand mapping table; env-var renames (none expected,
      but documented if any); Kubernetes manifest delta (single Deployment +
      `STRATA_WORKERS`); dashboard / alert renames (`strata_gateway_*` →
      `strata_*` if metric labels change)
- [ ] Tagged release notes link to this doc

## Functional Requirements

- FR-1: A single `strata` binary exists at `cmd/strata/main.go`. Subcommands:
  `server`, `version`. Future operator subcommands (`migrate`, `config check`)
  are out of scope for this PRD.
- FR-2: `strata server` runs the HTTP gateway on `STRATA_LISTEN`, reads the same
  env vars `cmd/strata-gateway/main.go` reads today, and starts every worker
  named in the `--workers` flag (or `STRATA_WORKERS` env). Per-worker tunables
  stay env-only (`STRATA_GC_INTERVAL`, `STRATA_LIFECYCLE_TICK`,
  `STRATA_NOTIFY_WORKER_COUNT`, ...) — no flag mirrors. Cross-cutting flags only:
  `--listen`, `--workers`, `--auth-mode`, `--vhost-pattern`, `--log-level`.
- FR-3: Default `--workers` value is `gc,lifecycle`. An empty value (`--workers=`)
  starts the gateway alone with no workers.
- FR-4: Each worker holds its own `internal/leader` lease keyed on the worker name.
  Lease loss cancels only that worker's context. Multiple replicas of `strata
  server` with the same `--workers` set elect exactly one active worker per name
  across the cluster.
- FR-4a: **Worker panics never kill the process.** Each worker goroutine has a
  `defer recover()` wrapper. Caught panics log ERROR with stack trace, increment
  `strata_worker_panic_total{worker=<name>}`, release the leader lease, and
  schedule a restart with exponential backoff (1s → 5s → 30s → 2m cap; reset to
  1s after ≥5 min uptime). The gateway and sibling workers continue serving
  uninterrupted.
- FR-5: Worker registration is centralised in `cmd/strata/workers/`. A worker is
  added by registering a `Worker{Name, Build}` struct; no edits to the dispatcher
  required.
- FR-6: `cmd/strata-gateway`, `cmd/strata-gc`, `cmd/strata-lifecycle`,
  `cmd/strata-notify`, `cmd/strata-replicator`, `cmd/strata-access-log`,
  `cmd/strata-inventory`, `cmd/strata-audit-export`, `cmd/strata-manifest-rewriter`,
  `cmd/strata-rewrap` are all deleted. No symlinks. No thin wrappers.
- FR-7: `strata-admin` keeps its current surface plus a new `rewrap` subcommand
  that runs the SSE key rotation worker to completion. No `--continue` /
  `--from-bucket` flags — `internal/rewrap` already persists per-bucket progress
  via `meta.Store.SetRewrapProgress`, so re-invoking the subcommand resumes
  automatically.
- FR-8: Docker image, docker-compose, Makefile, CI, examples, and CLAUDE.md all
  reference only `strata` and `strata-admin`.
- FR-9: `make smoke`, `make smoke-signed`, `make test`, `make test-integration`,
  `make test-rados`, `scripts/s3-tests/run.sh` all pass against the unified shape.
- FR-10: Every commit landed during this cycle (and every commit after it,
  per the project root `CLAUDE.md` rule established in US-001) keeps
  `ROADMAP.md` aligned with code state. Closing a roadmap item flips its bullet
  to `~~**P<n> — <title>.**~~ — **Done.** <summary>. (commit `<sha>`)` in the
  same commit (or the next one if SHA was needed). Discoveries (gaps, latent
  bugs) become new entries under the appropriate ROADMAP section in the same
  commit.

## Non-Goals

- No change to `internal/leader`, `internal/gc`, `internal/lifecycle`,
  `internal/notify`, `internal/replicator`, `internal/accesslog`,
  `internal/inventory`, `internal/auditexport`, `internal/manifestrewriter`,
  `internal/rewrap` — only their `cmd/<binary>/main.go` wiring moves.
- No change to Cassandra schema, including the `worker_locks` table — lock names
  stay (`gc`, `lifecycle`, `notify`, ...).
- No change to S3 surface, Prometheus metric names, or OTel span names.
- No deprecation period for the old binary names. Hard cut.
- No external "supervisor" pattern (systemd-style daemon manager) inside the
  binary. The unified binary's worker dispatcher is itself a lightweight
  in-process supervisor: panic recovery + exponential-backoff restart per worker
  (see FR-4a). It is intentionally narrow — no health probing of worker
  internals, no resource quotas, no restart-cap-then-give-up. If a worker
  panic-loops, that surfaces as a high `strata_worker_panic_total` rate; the
  operator alerts on it. Repeated unrecoverable panics do not bring the gateway
  down.
- No collapse of `strata-admin` into `strata`. The admin surface stays as a
  separate auditable binary.
- No keystone-style "control plane" rewrite. Workers stay leader-elected
  individually; this PRD is a packaging change.

## Design Considerations

- CockroachDB references: `cockroach start` runs the full node; `cockroach sql`,
  `cockroach init`, `cockroach node ls` are operator subcommands. Strata follows
  the same pattern: `strata server` runs the full node; future operator
  subcommands will be added as they earn their keep.
- Worker isolation: each worker owns its leader session, its prometheus
  collectors, and its goroutine tree. A worker panic is recovered at the
  dispatcher boundary — the panicking goroutine dies, the lease is released, a
  new goroutine is spawned after backoff, and other workers + gateway continue
  serving. Recovery is scoped narrowly to the worker `Run(ctx)` call; nested
  goroutines a worker spawns must catch their own panics or accept that they
  bubble up to that worker's recovery boundary and trigger its restart.
- Help output: `strata server --help` lists every registered worker name and a
  one-line description; `strata-admin rewrap --help` documents the rotation flow.
- Configuration precedence: explicit flag > `STRATA_*` env > built-in default.
  This matches the existing per-binary behaviour.

## Technical Considerations

- **CLI library choice.** `spf13/cobra` is the natural fit and brings standard
  help output, but it is a non-trivial dependency. Stdlib `flag` plus a small
  manual subcommand dispatcher is sufficient for the surface this PRD covers
  (`server`, `version`, future `migrate`). Pick one in US-002 and document the
  choice in the commit message; do not mix.
- **Single Cassandra connection pool.** `meta.Store` and `data.Backend` are
  built once at process start and shared across the gateway + every worker. This
  reduces total connection count from `N_workers × pool_size` to `pool_size`.
  Watch `STRATA_CASSANDRA_NUM_CONNS` defaults — we may want to raise the per-host
  count slightly to compensate for shared use.
- **Single Prometheus registry.** All workers register collectors against the
  process-global default registry. Metric names already include component labels
  (`strata_gc_*`, `strata_lifecycle_*`, ...); no rename required. Confirm no
  collector double-registers when multiple workers start.
- **Logging.** `logging.Setup()` is called once in `cmd/strata/main.go`; every
  worker takes a child logger via `logger.With("component", "<worker>")`. This
  preserves the JSON shape and request-id correlation.
- **OTel.** `strataotel.Init` is called once. The same `*otel.Provider` is
  threaded into the gateway, every Cassandra session observer, and every RADOS
  observer (US-033 wiring already supports a single tracer per process).
- **Build tags.** `-tags ceph` continues to gate the RADOS data backend. The
  single binary builds clean with and without the tag; workers that depend on
  RADOS (e.g. inventory's CSV upload, audit-export's gzip upload) already work
  through `data.Backend` and inherit whichever backend the binary was built with.
- **Migration ordering.** Land US-001 (roadmap maintenance rule) first so every
  subsequent commit in this cycle keeps `ROADMAP.md` current as items close.
  Then US-002..US-004 (skeleton + worker registry + panic recovery), then
  workers one PR at a time (US-005..US-012 — gc, lifecycle, notify, replicator,
  access-log, inventory, audit-export, manifest-rewriter). US-013 (rewrap →
  admin) and US-014 (delete gateway) land after worker migrations are stable.
  US-015..US-020 batch into a single "deploy migration" PR (Dockerfile, compose,
  Makefile, CI, docs, migration guide).

## Success Metrics

- `cmd/` directory contains exactly two binaries (`strata`, `strata-admin`).
- `make build` produces two artifacts.
- Default Docker image runs `strata server` and serves S3 with `gc` + `lifecycle`
  workers without further configuration.
- Total Cassandra connections (`SELECT COUNT(*) FROM system.clients`) from a
  Strata cluster of N replicas drops from `~10 × N × pool_size` to `~N ×
  pool_size`.
- `make smoke`, `make smoke-signed`, `scripts/s3-tests/run.sh` pass rates remain
  flat or improve vs the pre-consolidation baseline.
- `ROADMAP.md` Consolidation P1 bullet for "Single-binary `strata`" flips to Done.

## Resolved decisions (was: Open Questions)

**Q1 — Per-worker flags vs env-only?**
**Decision: env-only.** `strata server` exposes only cross-cutting flags
(`--listen`, `--workers`, `--auth-mode`, `--vhost-pattern`, `--log-level`).
Per-worker tunables (`STRATA_GC_INTERVAL`, `STRATA_LIFECYCLE_TICK`,
`STRATA_NOTIFY_WORKER_COUNT`, ...) stay as env vars — they are already documented
and stable across the existing per-binary deploys, so reusing them removes a
source of cutover friction. Adding flag mirrors would duplicate the surface and
keep two sources of truth in sync.

**Q2 — Coordinate with internal users running per-worker binaries?**
**Decision: no coordination required, hard cut.** This is a single-developer
project; no external operator runs per-worker binaries today. Migration lands in
one tagged release. Existing `make smoke` / `make smoke-signed` / s3-tests CI
gate the cutover.

**Q3 — `strata-admin rewrap` resumption flags?**
**Decision: always run to completion, no `--continue` / `--from-bucket` flags.**
`internal/rewrap` persists per-bucket progress through `meta.Store.SetRewrapProgress`
(US-007 of the SSE cycle), so re-running the subcommand automatically skips
buckets already rewrapped to the active key id. Operator workflow stays simple:
invoke `strata-admin rewrap`, wait for exit 0, done. Surface fewer flags to
reduce mistakes on a security-sensitive operation.
