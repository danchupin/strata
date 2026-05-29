# PRD: CI green + speedup (ci-green cycle)

## Introduction

`main`'s CI (`.github/workflows/ci.yml`) has been red for a long time across several jobs — for independent
**infrastructure/flake** reasons, NOT code-quality defects. Earlier work already fixed the lint-build YAML
syntax error, the release/e2e Docker Go-pin (`1.25.1`→`1.25.10`), and the cert-reloader test flake, and shipped
secure-by-default auth (PR #11) which fixed the e2e-ui `admin.spec` failures. This cycle finishes the job:
drive every remaining job green so the `main` branch-protection required-checks set can be expanded from the
current 7 reliably-green checks to the full critical set, and cut CI wall-clock with GitHub-native layer/asset
caching.

Several of these failures do **not** reproduce cleanly on a local macOS/dev box (hermetic Ubuntu runner,
network fetches, runner load). Stories must therefore be **verifiable via a CI run on the cycle branch**, not
only locally.

## Goals

- Turn every `ci.yml` job green on a PR from the cycle branch: `Build + vet`, `gosec`, `Unit tests (-race)`,
  `End-to-end UI (Playwright)`, `End-to-end (compose + smoke)`, `End-to-end (all workers + notify round-trip)`.
- Cut CI wall-clock by caching Docker image layers (GitHub `type=gha` backend) and Playwright browsers.
- Expand `main` branch-protection `required_status_checks` to include the newly-stabilised contexts.
- Keep `ROADMAP.md` honest (close-flip + any newly-surfaced gaps).

## User Stories

### US-001: Pin promtool version in the Build+vet job
**Description:** As a CI maintainer, I want the `Build + vet` job's "Install promtool" step to stop fetching
`@latest` so a flaky/failed `prometheus@v0.312.0` fetch no longer fails the build.

**Acceptance Criteria:**
- [ ] The `Install promtool` step in `.github/workflows/ci.yml` pins an explicit, known-good promtool version
      (e.g. `go install github.com/prometheus/prometheus/cmd/promtool@v<X.Y.Z>`), no `@latest`.
- [ ] The pinned version resolves and installs in CI (verified by a green `Build + vet` job on the cycle branch).
- [ ] If the install remains network-fragile, the step is wrapped with a bounded retry (e.g. 3 attempts) so a
      single transient registry hiccup does not fail the job.
- [ ] `Build + vet` job is green on a cycle-branch CI run.

### US-002: Fix the gosec gate failing with zero findings
**Description:** As a CI maintainer, I want the `gosec (MEDIUM+ gate, SARIF emit)` step to pass on a clean tree,
because `gosec -severity=medium` with the CI args finds 0 issues locally yet the job fails.

**Acceptance Criteria:**
- [ ] Root cause identified (one of: securego/gosec docker-action exit semantics; SARIF/CodeQL `upload-sarif`
      step; gosec failing to compile ceph build-tagged files in `internal/data/rados/cephimpl`).
- [ ] Fix applied at the real root cause (no masking): the gosec gate passes when there are 0 MEDIUM+ findings,
      and still **fails** when a genuine MEDIUM+ finding exists (prove the gate still bites with a temporary
      throwaway finding, then remove it).
- [ ] If gosec cannot compile ceph-tagged code without librados, the gosec scan scope is corrected (e.g. it
      already scopes to the main module; document why cephimpl is or isn't scanned) rather than blanket-excluding
      real code.
- [ ] `.gosec.yml` + `scripts/check-gosec.sh` + the workflow `-exclude=` list stay in lockstep (per the existing
      repo invariant).
- [ ] `gosec` job is green on a cycle-branch CI run.

### US-003: Make TestRunSmokeBinary deterministic
**Description:** As a CI maintainer, I want `cmd/strata-racecheck`'s `TestRunSmokeBinary` to stop intermittently
exiting non-zero under CI load (it passes 3/3 locally in isolation), because it flakes the `Unit tests (-race)`
job.

**Acceptance Criteria:**
- [ ] The flake mechanism is identified (timing/duration budget vs concurrency vs exit-code mapping in
      `cmd/strata-racecheck/main.go`).
- [ ] The test is made robust under load (e.g. generous duration ceiling that still returns early on success,
      and/or a deterministic completion gate) WITHOUT weakening what it asserts.
- [ ] `go test ./cmd/strata-racecheck/ -run TestRunSmokeBinary -count=20` passes locally.
- [ ] `Unit tests (-race)` job is green on a cycle-branch CI run.

### US-004: Fix the live audit-tail SSE in the e2e debug console
**Description:** As an operator using the debug console, I want the live audit-tail SSE stream to connect, so the
`debug.spec.ts` "audit-tail" e2e test (which times out waiting for "connected" at `web/e2e/debug.spec.ts:52`)
passes against the memory-backend e2e harness.

**Acceptance Criteria:**
- [ ] Root cause of the SSE `EventSource` never flipping to "connected" identified (server endpoint, auth/cookie
      flow on the SSE route, memory-backend audit source, or proxy/buffering).
- [ ] Fix applied so the audit-tail stream connects and a freshly PUT object's audit row appears in the live tail.
- [ ] `web/e2e/debug.spec.ts` "audit-tail" test passes locally (`playwright test debug.spec.ts -g audit-tail`).
- [ ] `End-to-end UI (Playwright)` job is green on a cycle-branch CI run (all of critical-path + admin + debug +
      storage specs pass).
- [ ] Verify in browser using the dev-browser/run skill that the live tail shows a "connected" state and a new
      audit row.

### US-005: Docker layer caching for the docker-build job (type=gha)
**Description:** As a CI maintainer, I want `docker-build` to cache image layers via the GitHub Actions cache
backend so the huge ceph base + `go mod download` are not rebuilt from scratch every run.

**Acceptance Criteria:**
- [ ] `docker-build` job switched from plain `docker build` to `docker/build-push-action@v6` with
      `cache-from: type=gha` + `cache-to: type=gha,mode=max` (so BuildKit `--mount=type=cache` mounts export too),
      with a distinct `scope=` per image (`strata-ceph`, `strata-ceph-bootstrap`).
- [ ] `load: true` so the built image stays available for downstream local use; tags unchanged (`strata:ceph`,
      `strata-ceph:local`).
- [ ] Second consecutive CI run on an unchanged Dockerfile shows a materially faster `docker-build` (cache hit
      logged); record before/after wall-clock in the story's progress note.
- [ ] `docker-build` job still green on a cycle-branch CI run.

### US-006: Extend gha cache to compose + ceph-bootstrap builds
**Description:** As a CI maintainer, I want the `e2e` and `e2e-full` compose builds (which rebuild the same ceph
image) and the `ceph-bootstrap` image to reuse the `type=gha` cache, so the image is effectively built once.

**Acceptance Criteria:**
- [ ] Compose service `build:` sections (or the CI build step that precedes `docker compose up`) consume the
      `type=gha` cache for the strata/ceph images (e.g. `build.cache_from` in the compose file, or pre-build with
      build-push-action + `--load` so compose reuses the local image instead of rebuilding).
- [ ] `ceph-bootstrap` image build participates in the gha cache (its own `scope=`).
- [ ] No duplicate full ceph rebuild across `docker-build` + `e2e` + `e2e-full` in one CI run (verified from logs).
- [ ] `e2e` and `e2e-full` jobs still green on a cycle-branch CI run.

### US-007: Playwright browser cache
**Description:** As a CI maintainer, I want chromium cached across runs so the e2e-ui job stops re-downloading it.

**Acceptance Criteria:**
- [ ] `actions/cache@v4` added to the `e2e-ui` job for `~/.cache/ms-playwright`, keyed on
      `${{ runner.os }}-playwright-${{ hashFiles('web/pnpm-lock.yaml') }}` with a restore-key prefix.
- [ ] `playwright install` becomes a cache-hit no-op on the second run (verified in logs: no chromium download).
- [ ] `End-to-end UI (Playwright)` job still green on a cycle-branch CI run.

### US-008: Verify e2e + e2e-full (compose + smoke) green
**Description:** As a CI maintainer, I want the compose-based e2e jobs confirmed green on the cycle branch (they
were Go-pin-fixed but never observed green), fixing any residual breakage.

**Acceptance Criteria:**
- [ ] `End-to-end (compose + smoke)` and `End-to-end (all workers + notify round-trip)` jobs are green on a
      cycle-branch CI run.
- [ ] Any residual failure (compose env, smoke script auth-mode, signed-smoke, RADOS integration) is root-caused
      and fixed; smoke scripts that start/expect a gateway set `STRATA_AUTH_MODE` explicitly (post secure-by-default
      flip).
- [ ] Progress note records the green run id + per-job conclusions.

### US-009: Full green CI + expand branch-protection required checks
**Description:** As a repo owner, I want the now-stable check contexts added to `main` branch-protection so only
fully-passing PRs can merge.

**Acceptance Criteria:**
- [ ] A full CI run on the cycle-branch PR is green end-to-end (every non-skipped job success).
- [ ] `main` branch-protection `required_status_checks.checks` is updated via `gh api -X PUT
      repos/danchupin/strata/branches/main/protection` to ADD: `Build + vet`, `Unit tests (-race)`,
      `gosec (Go static-analysis scan)`, `End-to-end UI (Playwright)`, `End-to-end (compose + smoke)`,
      `End-to-end (all workers + notify round-trip)` — on top of the existing 7.
- [ ] The applied protection is read back (`gh api ... /protection`) and the required list is asserted to contain
      the full set (verify it actually landed, do not trust the PUT response alone).
- [ ] If the Ralph runtime token lacks `admin:repo` scope to set protection, the story writes the exact `gh api`
      command + target required-set into `ROADMAP.md`/docs and flags it for the operator instead of silently
      passing.
- [ ] `ROADMAP.md` CI/quality entry flipped to Done with the cycle summary + closing SHA.

## Functional Requirements

- FR-1: `Install promtool` pins an explicit promtool version (no `@latest`) and tolerates a transient fetch via
  bounded retry.
- FR-2: The gosec gate passes at 0 MEDIUM+ findings and fails at ≥1 genuine MEDIUM+ finding; root cause fixed, not
  masked; `.gosec.yml` + `scripts/check-gosec.sh` + workflow exclude-list kept in lockstep.
- FR-3: `TestRunSmokeBinary` is deterministic under load without weakening assertions.
- FR-4: The debug-console live audit-tail SSE connects in the memory-backend e2e harness; the audit-tail e2e test
  passes.
- FR-5: `docker-build` uses `docker/build-push-action@v6` with `type=gha` cache (mode=max, per-image scope, load).
- FR-6: Compose e2e builds + the ceph-bootstrap image reuse the `type=gha` cache; the ceph image is built once per
  CI run.
- FR-7: The e2e-ui job caches `~/.cache/ms-playwright` via `actions/cache`.
- FR-8: All `ci.yml` jobs are green on a cycle-branch PR.
- FR-9: `main` branch-protection required-checks expanded to the full critical set and read-back verified.

## Non-Goals (Out of Scope)

- No new product features, S3 surface, or auth behavior changes (auth secure-by-default already shipped in #11).
- No migration to self-hosted runners or a different CI provider.
- No Dependabot PR triage (the open major-bump PRs #5–#10 are handled separately).
- No rewrite of the smoke/e2e test logic beyond what's needed to make existing tests pass deterministically.
- No bumping the ceph base image version (that is a Dependabot PR, out of scope here).

## Technical Considerations

- **Local-irreproducibility:** US-001/002/003/008 root causes surface mainly on the Ubuntu runner — each story's
  definitive verification is a CI run on the cycle branch, not a local pass alone.
- **gha cache limits:** GitHub Actions Cache is ~10 GB/repo with LRU eviction; use distinct `scope=` per image so
  the large ceph layers don't evict the bootstrap/Go caches.
- **Branch protection already active:** `main` currently requires 7 checks + PR + `enforce_admins`. The cycle
  branch's own PR must pass those 7 to merge; US-009 expands the set only after the new checks are observed green.
- **Secure-by-default interaction:** the default auth mode is now `required`; any smoke/e2e path that starts a
  gateway must set `STRATA_AUTH_MODE` explicitly (US-008 guards this).
- **Single-binary / module split invariants** (cephimpl separate module, hermetic default-tag build) must be
  preserved by the gosec scope decision in US-002.

## Success Metrics

- 100% of non-skipped `ci.yml` jobs green on the cycle PR.
- `docker-build` second-run wall-clock reduced by a clear margin (cache hit) vs the from-scratch baseline; ceph
  image built once per run instead of 3×.
- e2e-ui no longer re-downloads chromium (cache hit).
- `main` branch-protection required set expanded from 7 → 13 contexts, read-back verified.

## Open Questions

- Exact promtool version to pin — pick the latest tag that installs cleanly on the runner (story decides).
- Whether the gosec failure is the docker-action vs `upload-sarif` step — US-002 determines empirically via CI.
- Whether the audit-tail SSE failure is auth/cookie-related on the SSE route or a memory-backend audit-source gap
  — US-004 determines.
