# PRD: Consolidate `strata-admin` binary into `strata admin` subcommand

## Introduction

`cmd/strata-admin/` is a separate binary (`bin/strata-admin`) with 1909 LOC across 10 .go files. Existing subcommands: `iam`, `lifecycle`, `gc`, `sse`, `replicate`, `bucket`, `rewrap`, `bench-gc`, `bench-lifecycle`. Most subcommands proxy through the admin HTTP API via `client.go`; `rewrap` is a heavyweight standalone job; `bench-*` run benchmarks against the meta store directly.

Per CLAUDE.md single-binary invariant: ALL functionality must live in one `strata` binary. New CLI features as subcommands `strata admin <name>`. Existing `cmd/strata-admin` migration tracked as P2 — closed by this cycle.

Closes ROADMAP P2 *Consolidate `strata-admin` binary into `strata` as `admin` subcommand* (added under Correctness & consistency).

## Goals

- Single `strata` binary contains both `server` and `admin` subcommands
- `strata server [flags]` runs gateway as today (zero behavior change)
- `strata admin <subcommand> [flags]` exposes the full strata-admin CLI surface
- `bin/strata-admin` no longer produced (hard cut, no symlink, no migration shim)
- `make build` produces one binary; Docker image ships one binary
- All docs / scripts / compose / CI workflows reference `strata admin <name>` instead of `strata-admin <name>`
- 1 ROADMAP entry close-flipped + CLAUDE.md "Single-binary invariant" marked consolidated

## User Journey Walkthrough

Pre-cycle walkthrough per `feedback_cycle_end_to_end.md`. Operator end-to-end scenario after consolidation:

| # | Action | Surface | Story |
|---|--------|---------|-------|
| 1 | Operator runs `strata --help` | top-level usage lists `server`, `admin` | **US-001** |
| 2 | `strata server --help` prints existing gateway flags | unchanged | **US-001** (back-compat) |
| 3 | `strata admin --help` lists all subcommands (iam, lifecycle, gc, sse, replicate, bucket, rewrap, bench-gc, bench-lifecycle) | new sub-help | **US-001** |
| 4 | `strata admin rewrap --target-key-id X --dry-run` succeeds with same output as legacy `strata-admin rewrap --target-key-id X --dry-run` | preserved subcommand | **US-001** |
| 5 | `strata admin iam create-user alice` proxies to admin API as before | preserved subcommand | **US-001** |
| 6 | `strata admin bench-gc --entries 1000` runs the bench harness against meta store as before | preserved subcommand | **US-001** |
| 7 | `bin/strata-admin <anything>` → file does not exist (hard-cut removal) | filesystem | **US-001** |
| 8 | `make build` outputs ONE binary `bin/strata` | Makefile | **US-001** |
| 9 | Docker image final stage has single `/usr/local/bin/strata` | Dockerfile | **US-001** |
| 10 | `docs/site/content/architecture/migrations/binary-consolidation.md` flipped: K8s Job example uses `command: ["/usr/local/bin/strata", "admin", "rewrap"]` | docs | **US-002** |
| 11 | `grep -rn "strata-admin" docs/ scripts/ deploy/ .github/ Makefile` returns zero (except archived progress files) | docs sweep | **US-002** |
| 12 | `make smoke-single-binary` runs and asserts walkthrough steps 1-11 | smoke script | **US-002** |

Negative paths:
- Legacy invocation `strata-admin rewrap` fails at shell level (binary not found) — operator sees clear error from their own shell, no migration shim. Documented in CLAUDE.md migration note.
- `strata` with no args prints top-level help + exits 0 (matches the convention of most CLIs); `strata bogus` exits 2 with usage hint.
- `strata admin` with no subcommand prints admin sub-help + exits 2.
- `strata admin rewrap --bogus-flag` rejects with the same flag-parse error as legacy `strata-admin rewrap --bogus-flag`.

## State Truth Tables

### Top-level subcommand dispatch (US-001)

| `os.Args[1]` | Action | Exit code |
|--------------|--------|-----------|
| `""` (absent) | Print top-level help to stdout | 0 |
| `"--help"` / `"-h"` | Print top-level help to stdout | 0 |
| `"server"` | Call `server.Run(os.Args[2:])` | (forwarded) |
| `"admin"` | Call `admin.Run(os.Args[2:])` | (forwarded) |
| anything else | Print "unknown subcommand X" + usage to stderr | 2 |

### Admin sub-dispatch (US-001)

| `args[0]` (after `admin`) | Action | Exit code |
|--------------------------|--------|-----------|
| `""` / `"--help"` | Print admin sub-help listing all subcommands | 0 (for --help) / 2 (for empty) |
| `"iam"`, `"lifecycle"`, `"gc"`, `"sse"`, `"replicate"`, `"bucket"`, `"rewrap"`, `"bench-gc"`, `"bench-lifecycle"` | Dispatch to existing handler (preserved from current main.go) | (forwarded) |
| anything else | Print "unknown admin subcommand X" + usage to stderr | 2 |

## Cache Invalidation Ledger

No caches introduced this cycle (refactor only). Existing caches unaffected.

## Safety Claims Preconditions

| Claim | Preconditions | Verified by |
|-------|---------------|-------------|
| "`strata admin rewrap` behaves identically to legacy `strata-admin rewrap`" | All flag parsing + business logic moved verbatim; no semantic change | US-001 unit tests + US-002 smoke walkthrough |
| "Single binary build" | Makefile + Dockerfile drop the second build step | US-002 smoke step 8 (`make build` produces one binary) |
| "Zero `strata-admin` residue in repo" | grep returns nothing outside archived progress logs | US-002 smoke step 11 |

## User Stories

### US-001: Move `cmd/strata-admin/` to `cmd/strata/admin/` subcommand package + top-level dispatcher
**Description:** As a developer, I need the strata-admin source moved into a subcommand package under cmd/strata so the single binary can dispatch both server and admin subcommands without changing the operator-visible CLI surface.

**Acceptance Criteria:**
- [ ] New package `cmd/strata/admin/` containing the migrated code; exports single `Run(args []string) error` entry point
- [ ] Move all 8 .go source files from `cmd/strata-admin/` → `cmd/strata/admin/`:
  - `main.go` → `run.go` (rename to avoid `package admin` having a `func main()`); convert `func main()` to `func Run(args []string) error`; flag parsing receives `args` instead of reading `os.Args` directly
  - `client.go` → `client.go` (unchanged, just relocated)
  - `rewrap.go` + `rewrap_test.go` → relocated
  - `bench_common.go` + `bench_gc.go` + `bench_gc_test.go` → relocated
  - `bench_lifecycle.go` + `bench_lifecycle_test.go` → relocated
  - `main_test.go` → `run_test.go` (renamed; tests against `Run()` instead of `main()`)
- [ ] Package declaration in all moved files updated from `package main` to `package admin`
- [ ] All exported symbols + unexported references re-resolved; imports adjusted (no `os.Exit` calls inside `Run` — instead return error so the parent main can decide exit code)
- [ ] `cmd/strata/main.go` extended with top-level dispatcher:
  - `func main()` reads `os.Args[1:]`; checks first arg; routes to `server.Run` or `admin.Run`
  - Existing `cmd/strata/server.go` (or equivalent — verify path) refactored similarly: exports `Run(args []string) error`; current main-style entry becomes the server.Run body
  - Top-level help when args empty or `--help` lists `server` + `admin` with one-line descriptions
  - Unknown subcommand prints error + usage to stderr + exit 2
- [ ] Remove `cmd/strata-admin/` directory entirely (no symlink, no migration shim — hard cut per CLAUDE.md single-binary invariant)
- [ ] Makefile `build` target produces only `bin/strata` (drop the `strata-admin` build line and any `BIN_ADMIN` variable)
- [ ] `deploy/docker/Dockerfile`:
  - drops `go build -tags ceph -trimpath -o /out/strata-admin ./cmd/strata-admin`
  - final stage copies single binary `/usr/local/bin/strata`
  - ENTRYPOINT / CMD unchanged (still uses `strata server` default)
- [ ] `cmd/strata/main_test.go` (or new test file) covers dispatcher shapes: `strata server --help` succeeds; `strata admin --help` succeeds; `strata admin rewrap --dry-run` succeeds; `strata unknown` exits 2; `strata` (no args) prints help
- [ ] Existing tests in `cmd/strata/admin/*_test.go` continue to pass after move + package rename
- [ ] No regression: build a binary, run `bin/strata admin rewrap --help` — output equivalent to legacy `bin/strata-admin rewrap --help` (verified by smoke in US-002)
- [ ] `go vet ./...` passes
- [ ] `go test ./cmd/strata/...` passes
- [ ] `make build` succeeds and produces exactly one binary
- [ ] Typecheck passes; tests pass

### US-002: Find/replace docs + scripts + compose + CI + smoke + ROADMAP close-flip + PRD removal
**Description:** As a maintainer, I need every reference to `strata-admin` in docs, scripts, compose, and CI updated to the new `strata admin` subcommand syntax so operators following the runbooks land on the right command.

**Acceptance Criteria:**
- [ ] `grep -rn "strata-admin" docs/ scripts/ deploy/ .github/ Makefile CLAUDE.md` enumerated; every match (outside `scripts/ralph/archive/` snapshots) replaced with `strata admin`
- [ ] `docs/site/content/architecture/migrations/binary-consolidation.md` flipped: K8s Job example uses `command: ["/usr/local/bin/strata", "admin", "rewrap"]`; intro paragraph updated to "Consolidation complete: single `strata` binary with `server` + `admin` subcommands"
- [ ] `docs/site/content/architecture/observability.md` line referencing `cmd/strata-admin` updated to `cmd/strata/admin`
- [ ] `docs/site/content/architecture/migrations/gc-lifecycle-phase-2.md` lines mentioning `strata-admin` probe → `strata admin` probe
- [ ] CLAUDE.md "Single-binary invariant" section: bullet "Migration of the existing `cmd/strata-admin` into `cmd/strata admin` is tracked as a P2 ROADMAP entry" updated to "Consolidation complete in commit `<SHA>` — single `strata` binary holds both `server` + `admin` subcommands". Rest of section preserved (forward-looking discipline).
- [ ] CLAUDE.md "Common commands" table: any reference to `strata-admin <cmd>` → `strata admin <cmd>`
- [ ] `deploy/docker/docker-compose.yml`: no compose service references `strata-admin` binary (verified by grep)
- [ ] `.github/workflows/*.yml`: any CI job invoking `strata-admin` → `strata admin`
- [ ] Smoke + race + integration test scripts updated
- [ ] `scripts/smoke-single-binary.sh` (new) verifies:
  - `bin/strata --help` output contains both `server` and `admin` subcommand listings
  - `bin/strata admin --help` lists all 9 expected subcommands (iam, lifecycle, gc, sse, replicate, bucket, rewrap, bench-gc, bench-lifecycle)
  - `bin/strata admin rewrap --help` prints rewrap usage
  - `bin/strata-admin` does NOT exist after `make build`
  - `strata unknown` exits 2 with helpful error
- [ ] `make smoke-single-binary` Makefile target wraps the script (or fold into existing `make smoke` if simpler)
- [ ] `ROADMAP.md` close-flip the P2 entry → `~~**P2 — Consolidate strata-admin binary into strata as admin subcommand.**~~ — **Done.** <one-line summary>. (commit \`<pending>\`)`; closing SHA backfilled in follow-up commit on main
- [ ] `tasks/prd-single-binary.md` REMOVED in the same commit (PRD lifecycle rule)
- [ ] `make docs-build` succeeds (no broken cross-references after the rename)
- [ ] `make vet` succeeds
- [ ] `make test` succeeds
- [ ] `make smoke-single-binary` succeeds
- [ ] Typecheck passes

## Functional Requirements

- **FR-1:** Single `strata` binary contains `server` + `admin` subcommands
- **FR-2:** `strata --help` / `strata -h` / `strata` (no args) prints top-level help listing both subcommands
- **FR-3:** `strata server [flags]` runs gateway identically to today (zero behavior change in server)
- **FR-4:** `strata admin <subcommand> [flags]` exposes the full strata-admin CLI surface (iam, lifecycle, gc, sse, replicate, bucket, rewrap, bench-gc, bench-lifecycle)
- **FR-5:** `strata <unknown>` exits 2 with usage hint
- **FR-6:** `strata admin <unknown>` exits 2 with admin-level usage hint
- **FR-7:** `cmd/strata-admin/` directory removed entirely
- **FR-8:** Makefile produces exactly one binary `bin/strata`
- **FR-9:** Dockerfile final stage ships single `/usr/local/bin/strata`
- **FR-10:** All docs / scripts / compose / CI updated to `strata admin <name>` syntax
- **FR-11:** ROADMAP P2 entry close-flipped
- **FR-12:** Smoke script verifies the consolidation

## Non-Goals

- **No symlink / migration shim** for legacy `strata-admin` invocation. Operators must update scripts. Per CLAUDE.md "no prod deploys" — hard cut acceptable.
- **No subcommand renames or flag changes.** Every flag + subcommand name preserved exactly.
- **No new admin subcommands** introduced. Pure refactor + consolidation.
- **No CLI framework migration.** Keep existing `flag` package usage; do not introduce cobra / urfave/cli.
- **No deprecation period.** Ship the consolidation; legacy binary removed in the same commit.
- **No nested-package refactor of admin.** All 9 subcommands stay in one package `cmd/strata/admin` (matches the current flat `cmd/strata-admin` layout).

## Design Considerations

- **`Run(args []string) error` signature** for both server + admin entrypoints — caller (main) decides exit code based on error. Allows clean error propagation + testability.
- **Top-level help text** lists subcommands with one-line description:
  ```
  Usage: strata <subcommand> [flags]
  
  Subcommands:
    server   Run the S3 gateway + opt-in background workers
    admin    Operator CLI: iam, lifecycle, gc, sse, replicate, bucket, rewrap, bench-*
  
  Run "strata <subcommand> --help" for subcommand-specific help.
  ```
- **Admin sub-help** mirrors the existing `strata-admin` usage block verbatim.
- **Test placement**: `cmd/strata/main_test.go` covers top-level dispatch; `cmd/strata/admin/run_test.go` covers admin subcommand routing; existing per-command tests (`rewrap_test.go`, `bench_gc_test.go`, etc) move with the source files.
- **CLAUDE.md "Single-binary invariant" section** — update the migration note line; preserve the forward-looking discipline (any future CLI feature must be a subcommand).

## Technical Considerations

- **`func main()` extraction**: simplest path is to have `cmd/strata-admin/main.go` body become `cmd/strata/admin/run.go::Run(args)` — replace `flag.Parse()` with `fs.Parse(args)` on a `*flag.FlagSet` we construct from `args`. The existing `root := flag.NewFlagSet("strata-admin", flag.ContinueOnError)` already follows this pattern, so the refactor is mostly mechanical.
- **Server entrypoint refactor parallel**: `cmd/strata/main.go` currently has `func main()` that calls server bootstrap. Extract to `server.Run(args)`. May require a touch-up to surface errors instead of `log.Fatal`.
- **Test fixtures**: any `rewrap_test.go` setup that uses `cmd/strata-admin` import paths or builds the binary externally needs updating to the new path. Verify with `grep -rn "cmd/strata-admin" --include="*.go"` before merging US-001.
- **Docker layer caching**: removing the second `go build` shaves ~30s off the Docker build on a cold cache. Side benefit, not a goal.
- **CI matrix unaffected**: existing CI builds `./cmd/strata/...` which picks up the new admin subpackage automatically.
- **ralph progress files**: `scripts/ralph/archive/**` contain historical references to `strata-admin` — DO NOT rewrite these (they're frozen snapshots). Smoke grep step in US-002 must exclude `scripts/ralph/archive/` and `tasks/archive/` if either exists.

## Success Metrics

- `make build` produces exactly one binary
- `strata --help` lists `server` + `admin`
- `strata admin rewrap --help` output byte-identical to today's `strata-admin rewrap --help` (modulo the "Usage:" prefix)
- Zero `strata-admin` residue in docs/scripts/compose/CI (excluding archived ralph snapshots)
- Smoke green; vet green; tests green; docs-build green

## Open Questions

- Should `strata` (no args) exit 0 with help or exit 2 with hint? Recommendation: exit 0 — matches most CLI conventions (kubectl, docker, git all print help on bare invocation with exit 0).
- Should we add a `version` top-level subcommand in this cycle? Defer — separate concern.
- Should `strata admin --json` global flag for machine-readable output be added? Defer — existing `strata-admin` doesn't have it, scope creep here.
