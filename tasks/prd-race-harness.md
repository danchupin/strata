# PRD: Race Harness as a Real Test, Not a Gate

## Introduction

The race harness (`internal/s3api/race_test.go`, `race_integration_test.go`)
landed in US-035 of the prior cycle but is currently used only as a
sanity-check unit test against the in-memory backend, plus a build-tag-gated
integration test against a testcontainers Cassandra. It has never been run at
scale against the full `make up-all` stack (gateway + Cassandra + RADOS) for a
sustained duration.

This PRD covers turning the harness into a load-bearing nightly verification:
extend it to drive the gateway over HTTP against the real stack, run for ≥1
hour, capture inconsistency reports, and integrate the result into CI as a
nightly job on a free `ubuntu-latest` GitHub-hosted runner (not a per-PR gate
— failures investigate, not block merge).

This is the second open P1 item in the "Consolidation & validation" section of
`ROADMAP.md`. It does not introduce new gateway functionality; it proves the
existing functionality holds up under concurrent abuse against the production
backends within the resource budget of a free GitHub-hosted runner.

**Resource budget (target environment):**
- `ubuntu-latest` GitHub-hosted runner
- 2 vCPU, 7 GB RAM, 14 GB SSD
- 6 h job timeout (we use 90 min)
- Public repo → unlimited minutes
- No paid larger runners, no self-hosted

The full `make up-all` stack (Cassandra + Ceph MON/MGR/OSD + strata) must fit
into this budget. Ceph is the dominant memory consumer (default
`osd_memory_target=4Gi` would already exceed the runner's RAM); compose
config gets a stripped-down "ci" profile that caps OSD/MON/Cassandra heap
sizes.

## Goals

- Run the race harness for ≥1 hour against `cassandra+RADOS` via `make up-all`
- Fit the entire stack + harness into a free `ubuntu-latest` GitHub-hosted
  runner (2 vCPU / 7 GB RAM / 14 GB SSD)
- Detect and surface any consistency violations (zero is the expected baseline)
- Wire the run into a nightly GitHub Actions workflow with artefact upload +
  GitHub Step Summary; no email / Slack / auto-issue plumbing — manual triage
- Produce a structured report (JSON-lines + human summary) per run
- Per-commit ROADMAP.md sync — flip the P1 entry to Done when verified

## User Stories

### US-001: Carve out `internal/racetest` library package
**Description:** As a developer, I want the race harness logic in a reusable
package so it can be driven by both `go test` and a standalone binary.

**Acceptance Criteria:**
- [ ] Move shared workload helpers (`raceFixture`, `newRaceClient`,
      `raceWorkload`) from `internal/s3api/race_test.go` into a new
      `internal/racetest/` package
- [ ] Existing `TestRace*` tests in `internal/s3api/` import from
      `internal/racetest/` and still pass under `go test -race`
- [ ] No semantic change to the workload mix; only relocation + minimal
      exported-API surface
- [ ] `internal/racetest/` exposes `Run(ctx, cfg) (*Report, error)` accepting
      `Config{HTTPEndpoint, Duration, Concurrency, BucketCount, ObjectKeys,
      VerifyEvery}` and returning a structured report
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: `cmd/strata-racecheck` standalone binary
**Description:** As an operator, I want to drive the race harness against an
already-running gateway without building the full Go test binary, so CI can
target a containerised stack.

**Acceptance Criteria:**
- [ ] Add `cmd/strata-racecheck/main.go` that calls `racetest.Run()`
- [ ] Flags: `--endpoint`, `--duration`, `--concurrency`, `--buckets`,
      `--keys-per-bucket`, `--report=<path>`, `--access-key`, `--secret-key`,
      `--region`
- [ ] Process exit code: 0 on no inconsistencies, 1 on inconsistencies, 2 on
      transport / setup error
- [ ] Writes a JSON-lines report file (one event per line: `op_started`,
      `op_done`, `inconsistency`, `summary`)
- [ ] Prints a human-readable summary to stdout at end
- [ ] Builds without `-tags ceph` — talks pure HTTP, no librados linkage
- [ ] Add `cmd/strata-racecheck` to the `make build` target alongside
      `strata` and `strata-admin`
- [ ] Update `tasks/prd-binary-consolidation.md` PRD goal note: the
      consolidation cycle landed two binaries; `strata-racecheck` is a
      developer/CI tool, not a production binary, so it does NOT count as a
      regression on the two-binary goal. Document this exception in the same
      file's "Non-Goals" section.
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: Workload extension — multipart + versioning + delete
**Description:** As a developer, I want the race harness to exercise the full
high-risk surface (multipart, versioning flips, conditional delete) so the
≥1h run covers the operations historically prone to LWT bugs.

**Acceptance Criteria:**
- [ ] Existing workload (PUT/GET/DELETE/list interleave) preserved
- [ ] New op classes: `MultipartCreate+Upload+Complete`, `EnableVersioning`
      flip mid-load, `ConditionalPut(IfNoneMatch)`, `DeleteObjects` batch
- [ ] Workload weights configurable via `Config.Mix map[string]float64`
- [ ] Default mix exercises every op class at least 5% of the time so a
      ≥1h run hits each at least 50× per worker
- [ ] Race detector (`go test -race`) clean against the in-memory backend
      with the new ops
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: Inconsistency oracle — read-after-write + listing convergence
**Description:** As a developer, I want the harness to assert specific
invariants beyond "no go race detector hit" so silent data divergence shows
up in the report.

**Acceptance Criteria:**
- [ ] After every successful PUT, the harness records the expected
      `(bucket, key, version_id, etag, size)` tuple
- [ ] A separate "verifier" goroutine periodically GETs the latest version
      and compares ETag + size; mismatch = `inconsistency` report event
- [ ] After every `EnableVersioning` flip, subsequent PUTs produce
      monotonically increasing `version_id` per key (verifier asserts via
      ListObjectVersions)
- [ ] After every `DeleteObjects` batch, the deleted keys do NOT reappear in
      a follow-up `ListObjects` for the same prefix within a configurable
      grace window (default 5s)
- [ ] Report JSON-lines includes `inconsistency` events with full diagnostic
      payload (expected vs observed, request_id, timestamp)
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: Compose `ci` profile — stack tuned for 7 GB / 2 vCPU
**Description:** As a CI maintainer, I want a `ci`-profile compose
configuration that fits the full stack into a `ubuntu-latest` runner without
triggering OOM, so the nightly soak runs on free GitHub minutes.

**Acceptance Criteria:**
- [ ] Add `deploy/docker/docker-compose.ci.yml` (compose override file) that:
  - Sets `osd_memory_target=1073741824` (1 GiB) on the Ceph OSD
  - Caps Cassandra heap via `MAX_HEAP_SIZE=2G`, `HEAP_NEWSIZE=400M` env
  - Disables Ceph MGR dashboard module (lower memory, faster boot)
  - Single replica everywhere (no pool replication, no Cassandra RF>1)
  - Pins Cassandra `cluster_replication: SimpleStrategy/RF=1`
  - Removes Prometheus + Grafana from the default `ci`-profile up-set
    (not needed for the soak; saves ~400 MB RAM)
- [ ] `make up-all-ci` brings up the trimmed stack via
      `docker compose -f deploy/docker/docker-compose.yml -f
      deploy/docker/docker-compose.ci.yml --profile ci up -d`
- [ ] Stack reaches `/readyz` 200 within 8 min on `ubuntu-latest` (verified
      by a CI smoke step that times out at 10 min)
- [ ] Idle stack RSS (post-readyz, pre-load) ≤ 5 GB total — leaving ≥2 GB for
      strata-racecheck workload + OS overhead
- [ ] Existing `make up-all` (full-fat) is unchanged; `ci` is purely additive
- [ ] Typecheck passes (compose validation via `docker compose config -q`)
- [ ] Tests pass

### US-006: `make race-soak` target + script
**Description:** As an operator, I want one command to bring up the full
stack and run the race harness for ≥1 hour against it.

**Acceptance Criteria:**
- [ ] New script `scripts/racecheck/run.sh` that:
  - Brings up the stack via `make up-all-ci` if `CI=true`, else `make up-all`
  - Waits for `/readyz` on the gateway (ceiling 10 min)
  - Runs `bin/strata-racecheck --duration=${RACE_DURATION:-1h} --concurrency=${RACE_CONCURRENCY:-32}
        --report=report/race.jsonl --endpoint=http://localhost:9999`
  - Captures `docker logs` for `strata`, `strata-cassandra`, `strata-ceph`
    into `report/`
  - Emits `df -h /` + `free -m` snapshots before/after into `report/host.txt`
    (helps post-mortem OOM diagnosis on the runner)
  - Exits with the harness's exit code
- [ ] New `make race-soak` target invoking the script
- [ ] Default duration (1 h) and concurrency (32) overridable via
      `RACE_DURATION=` / `RACE_CONCURRENCY=` env vars; CI workflow fixes them
      via `workflow_dispatch` inputs
- [ ] Concurrency cap: harness refuses to start with `--concurrency > 64` —
      higher values OOM the 7 GB runner empirically; raise the ceiling only
      after measurement
- [ ] Script is idempotent — re-running clears prior `report/` directory
- [ ] Typecheck passes (script is shell, no compile)
- [ ] Tests pass

### US-007: Nightly CI workflow on `ubuntu-latest`
**Description:** As a maintainer, I want the race-soak run to execute on a
nightly schedule and surface failures via GitHub Actions, without blocking
PRs and without paid runners.

**Acceptance Criteria:**
- [ ] New `.github/workflows/race-nightly.yml` declares
      `runs-on: ubuntu-latest` (explicit; no `runs-on: ubuntu-latest-large`
      or self-hosted)
- [ ] `schedule: cron: '0 3 * * *'` (03:00 UTC daily) and
      `workflow_dispatch:` for manual runs with `duration` + `concurrency`
      inputs
- [ ] Job builds the strata image via `make docker-build`, brings up the
      `ci`-profile compose stack via `make up-all-ci`, runs `make race-soak`
      with `RACE_DURATION=1h`
- [ ] Uploads `report/` as the `race-nightly-<run-id>` artefact (retain 30
      days)
- [ ] On every run (success OR fail), writes the `report/SUMMARY.md` content
      into `$GITHUB_STEP_SUMMARY` so the run's UI page shows pass/fail
      headline + throughput + inconsistency count without downloading the
      artefact
- [ ] On failure, workflow exits non-zero so the workflow badge flips red.
      No email, no Slack, no auto-issue — triage is manual via the GitHub
      Actions UI
- [ ] Workflow does NOT run on `pull_request` — only `schedule` and
      `workflow_dispatch`
- [ ] Workflow timeout = 90 min (under 6 h GH ceiling; covers ~8 min stack
      bring-up + 60 min soak + ~10 min log capture + buffer)
- [ ] `concurrency: race-nightly` group + `cancel-in-progress: true` so a
      manually-triggered run preempts an in-flight nightly (single execution
      at a time; consistent baseline)
- [ ] Existing `ci.yml` jobs unchanged — race nightly is a separate file
- [ ] Typecheck passes
- [ ] Tests pass

### US-008: Report parsing + summary
**Description:** As a maintainer reviewing nightly artefacts, I want a
one-page summary at the top of the report folder so I can triage in 30
seconds.

**Acceptance Criteria:**
- [ ] New `scripts/racecheck/summarize.sh` (POSIX sh + jq) that consumes
      `report/race.jsonl` and prints:
  - Total ops by class
  - Op throughput (ops/sec)
  - p50 / p95 / p99 latency per class
  - Inconsistency count + first-3 examples
  - Host snapshot delta (RSS pre/post, disk pre/post — pulled from
    `report/host.txt`)
- [ ] Summary embedded into `report/SUMMARY.md` (Markdown) and printed in
      the workflow log via `cat report/SUMMARY.md >> "$GITHUB_STEP_SUMMARY"`
- [ ] Script handles empty / partial reports gracefully (process killed
      mid-run): returns sensible "incomplete run" output without crashing
- [ ] Script runs in <5 s on a 20 MB JSON-lines report (the expected size for
      a 1 h run)
- [ ] Typecheck passes
- [ ] Tests pass

### US-009: Baseline run + ROADMAP close-flip
**Description:** As a developer, I want a recorded zero-inconsistency
baseline run committed to the repo so future regressions are detectable
against a known-good shape.

**Acceptance Criteria:**
- [ ] Trigger the nightly workflow manually via `workflow_dispatch` (or wait
      for the first scheduled run); commit `docs/racecheck/baseline-2026-XX.md`
      with the SUMMARY.md content + commit SHA + workflow link
- [ ] Commit ROADMAP.md close-flip in the same commit:
      `~~**P1 — Race harness as a real test, not a gate.**~~ — **Done.** ...`
      with the closing SHA (or `(commit pending)` placeholder per the
      "follow-up SHA edit" rule)
- [ ] If any inconsistencies are detected, file them as a P1 entry under
      "Known latent bugs" or "Correctness & consistency" before flipping;
      do NOT flip the headline if any are open
- [ ] If the workflow OOMs or stack fails to come up within 10 min: file a
      P2 entry under "Consolidation & validation" and tune US-005 limits
      tighter; do not flip the headline
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- FR-1: `internal/racetest` is the canonical home for the race harness logic;
  `internal/s3api/race_test.go` becomes a thin shim that exercises
  `racetest.Run` against an in-memory backend
- FR-2: `cmd/strata-racecheck` is a build-target binary (under `make build`)
  that drives the harness against any S3 endpoint via HTTP
- FR-3: The harness exercises PUT, GET, DELETE, ListObjects, multipart upload,
  versioning flip, conditional PUT, and DeleteObjects within one workload
- FR-4: A verifier goroutine asserts read-after-write + listing-convergence
  invariants; violations are reported as `inconsistency` events in JSON-lines
- FR-5: A `deploy/docker/docker-compose.ci.yml` override caps memory on Ceph,
  Cassandra, and disables non-essential services, so the full stack fits a
  7 GB / 2 vCPU runner
- FR-6: `make race-soak` runs the harness for `RACE_DURATION` (default 1h)
  against the up-all stack (or up-all-ci on CI)
- FR-7: A nightly GitHub Actions workflow on `ubuntu-latest` runs
  `make race-soak` and uploads the report directory; failure does not block
  PRs and does not send notifications — triage is via Actions UI
- FR-8: A summary script produces `report/SUMMARY.md` with throughput,
  latency, inconsistency counts, and host RSS/disk deltas; the same content
  is mirrored to `$GITHUB_STEP_SUMMARY`
- FR-9: ROADMAP.md is updated in the same commit as the cycle's final
  baseline-run story per the project root CLAUDE.md "Roadmap maintenance"
  rule

## Non-Goals

- **No replacement of the per-PR `go test -race ./...` gate.** Unit-level
  race detection stays where it is; this PRD adds a separate, longer,
  HTTP-driven check
- **No paid runners.** Larger GitHub-hosted runners (4-core / 8-core) and
  self-hosted runners are explicitly out of scope. Stack must fit
  `ubuntu-latest`
- **No notification plumbing.** No email / Slack / auto-issue / PagerDuty
  integration. Workflow failure surfaces only as a red badge + workflow run
  in the Actions UI; maintainers triage by reviewing the artefact and
  `$GITHUB_STEP_SUMMARY`
- **No chaos / fault injection.** The workload is concurrent abuse against a
  healthy stack, not network partitions or backend kills. Chaos belongs in a
  future P2 item
- **No replication / multi-region race scenarios.** Single-region only;
  replicator workers do not run during the soak
- **No automatic regression pinning.** The baseline file is a human-readable
  record, not a test fixture; nightly runs do not assert against it
- **No expansion of `make up-all` defaults.** The `ci` profile is purely
  additive; developer-laptop bring-up keeps the full-fat stack with
  Prometheus/Grafana

## Technical Considerations

### GitHub-hosted runner budget (`ubuntu-latest`)
- Spec: 2 vCPU, 7 GB RAM, 14 GB SSD, 6 h job timeout
- Public repo → unlimited monthly minutes; nightly cron is sustainable
- Stack must idle at ≤5 GB RSS post-readyz so harness has ≥2 GB headroom
- Single OSD with `osd_memory_target=1Gi` (default 4Gi would OOM)
- Cassandra heap capped at 2 GB; default would auto-size to ~50% RAM and
  collide with Ceph
- No Prometheus / Grafana in CI profile — they consume ~400 MB and the
  nightly does not query them

### Concurrency budget
- Harness default = 32 workers. Higher concurrency on 2 vCPU produces
  context-switch overhead, not throughput
- Empirical OOM ceiling = 64 workers on this stack shape; harness refuses to
  start above it (US-006)
- Plausible nightly throughput: 200-500 ops/sec total. Acceptable — goal is
  consistency-violation detection, not benchmarking

### Job timeout sizing (90 min)
- ~8 min for stack bring-up (image pull + Ceph init + Cassandra schema)
- 60 min harness duration
- ~10 min for log capture + summary + artefact upload
- ~12 min buffer for first-time pulls / runner cold starts

### General
- The harness drives the gateway over HTTP, so it does not need `-tags ceph`
  to compile
- Cassandra LWT under sustained concurrent versioning flips is the highest-
  risk hot path; the workload should weight versioning flips at ≥10% of all
  ops
- The harness must NOT use `STRATA_AUTH_MODE=optional` — sign requests with
  static credentials so SigV4 path is exercised
- Report file rotates at ~100 MB to avoid filling the 14 GB SSD on long
  soaks; default 1 h run produces ~5-20 MB of JSON-lines
- The workflow should mount `/var/lib/docker` on the runner's SSD (default);
  no need to attach external volume

## Success Metrics

- Nightly workflow green for 7 consecutive days = baseline established
- Zero inconsistencies in baseline run = ROADMAP P1 flips to Done
- Any inconsistency surfaces a fresh P1 entry in ROADMAP.md within the same
  cycle (per the maintenance rule)
- Stack idle RSS measured ≤5 GB on `ubuntu-latest` (verified in workflow)
- Job completes within 90 min wall-clock for 7 consecutive nights

## Open Questions

- Should the harness also exercise the streaming SigV4 path
  (`x-amz-content-sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD`) or only the
  pre-computed-SHA path? Streaming has its own gotchas (chain HMAC validation
  is a known P2 gap) — recommend including both at 50/50 weight in US-003
- If the `ci`-profile stack still OOMs on `ubuntu-latest` after US-005, do we
  drop to Cassandra+memory-data-backend (no RADOS) for the CI variant? Would
  cover meta-LWT gones — the highest-risk surface — at the cost of skipping
  RADOS-stress. Decision deferred to baseline-run results in US-009
- 30-day artefact retention on a public repo with daily cron: each artefact
  is ~20 MB, so 30 × 20 = 600 MB/30d/repo. Within free-tier 500 MB cap?
  Public repo storage is also unlimited per docs as of 2025; verify before
  US-007 lands. If capped, drop to 14-day retention
