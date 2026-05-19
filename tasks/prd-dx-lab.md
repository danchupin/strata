# PRD: DX + lab bundle

## Introduction

DX/lab bundle cycle — 4 small improvements + final smoke story. No new
admin endpoint, no UI work. Cycle drains the remaining DX/lab P3 entries
from ROADMAP so post-cycle the roadmap holds only "global" items
(benchmarks vs RGW, content-addressed dedup, Intelligent-Tiering, Select
Object Content).

Branch: `ralph/dx-lab`. Starts from `main` per cycle-branch policy.

## Goals

- Land `make dev` as the single one-command bootstrap for contributors —
  TiKV-backed canonical lab (matches `## Compose shape` discipline in
  CLAUDE.md), with `make dev-down` companion.
- Restore the SHARDS=3 leg of `scripts/bench-rebalance-multi.sh` parked
  by the `ralph/tikv-default-lab` cycle — add `strata-c` (3rd replica,
  port 10003) under a new `bench-3replica` compose profile, un-skip the
  bench leg, capture fresh numbers.
- Ship a Helm chart `deploy/helm/strata/` for TiKV-backed Kubernetes
  deployment with `helm lint` wired into CI; raw manifests under
  `deploy/k8s/` remain canonical for non-Helm operators.
- Add a Go lint test that asserts `docs/site/content/reference/s3-api.md`
  stays in sync with `internal/s3api/server.go` switch arms (drift-proof
  the hand-curated table from the `ralph/docs-adr` cycle).
- Prove the bundle with `make dev` readiness + `make helm-lint` + the
  new s3-api drift test running green; close 4 ROADMAP P3 entries in
  one cycle.

## User Journey

Four DX touchpoints, one per fix:

- **Fresh contributor cloning the repo.** Today: `make up-all && make
  wait-tikv && make wait-ceph && make wait-strata-lab` then `docker
  compose logs -f strata-a strata-b` in another terminal. After cycle:
  `make dev`. One command, log stream foreground, `Ctrl-C` exits the
  tail without tearing down; `make dev-down` cleans up.
- **Benchmark author measuring rebalance speedup at 3 replicas.**
  Today: `scripts/bench-rebalance-multi.sh` SHARDS=3 leg `SKIP`-s with a
  one-line "parked under restore-3-replica-tikv-bench" message. After
  cycle: the leg runs against `--profile bench-3replica`, captures
  numbers, updates the benchmark doc.
- **Operator deploying Strata to a Kubernetes cluster with prom-operator.**
  Today: raw `kubectl apply -f deploy/k8s/`, then hand-write a
  ServiceMonitor. After cycle: `helm install strata deploy/helm/strata/
  -n strata --set monitoring.enabled=true` covers ServiceMonitor +
  resource limits + ingress in one chart values file.
- **Developer adding a new S3 handler in `internal/s3api/`.** Today:
  remembers to update `docs/site/content/reference/s3-api.md` manually,
  or doesn't — drift accumulates. After cycle: `go test ./internal/s3api/...`
  fails on missing row (or stale `file:line`), forcing the docs update
  in the same PR.

## User Stories

### US-001: `make dev` one-command developer cluster

**Description:** As a contributor, I want a single `make dev` command
that bootstraps the canonical TiKV-default lab and streams logs in the
foreground so I can get from clone to running stack in one command.

**Acceptance Criteria:**
- [ ] New Makefile target `dev` that runs (in order): `make up-all`
      → `make wait-tikv` → `make wait-ceph` → `make wait-strata-lab`
      → `docker compose -f deploy/docker/docker-compose.yml logs -f
      --tail=20 strata-a strata-b strata-lb-nginx`. Each step's stderr
      surfaces; failure of any step aborts the rest.
- [ ] New Makefile target `dev-down` that calls `make down` (mirrors
      the existing tear-down recipe). Doesn't touch volumes by default
      — operator runs `make down` directly for the data-wipe variant
      (or `docker compose down -v` for full wipe).
- [ ] Backend default: **TiKV** (matches CLAUDE.md `## Compose shape`
      canonical default — `pd + tikv + ceph + ceph-b + strata-a/b +
      strata-lb-nginx + prometheus + grafana`). Cassandra-backed lab
      stays under `make up-cassandra` per existing convention.
- [ ] Log stream uses `docker compose logs -f` (no `--no-log-prefix`
      so container names disambiguate strata-a vs strata-b output) and
      tails last 20 lines per container before streaming live.
- [ ] `Ctrl-C` on the foreground tail kills only the log stream; the
      compose stack stays up so operator can re-attach via `make
      dev-logs` (new target — `docker compose logs -f strata-a strata-b
      strata-lb-nginx`) or run smoke harnesses.
- [ ] Document the new targets via a Makefile header comment (the
      file uses comment blocks above target groups today — see lines
      around `up-all:` / `down:` for the pattern). The Makefile has no
      `help` target — don't introduce one in this story. Add a
      one-line description above each of `dev:`, `dev-down:`,
      `dev-logs:`.
- [ ] Document in `docs/site/content/get-started/_index.md` (verified
      to exist; `docs/site/content/developers/` has only `_index.md`
      — no `quickstart.md`) under a new "One-command dev" section:
      `make dev` to start, `make dev-down` to stop, `make dev-logs`
      to re-attach.
- [ ] `make dev` exits 0 once the log stream is attached (manual
      Ctrl-C to leave). For CI / scripted use, document the
      composable underlying targets (`make up-all && make wait-*`).
- [ ] `make vet` passes; no Go changes in this story.

### US-002: Restore 3-replica TiKV bench (SHARDS=3 rebalance-multi)

**Description:** As a benchmark author, I want the SHARDS=3 leg of
`scripts/bench-rebalance-multi.sh` re-enabled so I can measure the
3-replica speedup the rebalance fan-out promises.

**Acceptance Criteria:**
- [ ] New compose profile `bench-3replica` in
      `deploy/docker/docker-compose.yml` adds a third service
      `strata-c`: same image / env-shape / volumes as `strata-a` and
      `strata-b`; `STRATA_NODE_ID: strata-c` (matches the
      `strata-a` / `strata-b` naming pattern on lines ~167 / ~225 of
      the compose file); host port mapping `10003:9000`; mounts the
      shared `strata-jwt-shared` volume.
- [ ] `strata-lb-nginx` upstream block: today round-robins strata-a +
      strata-b; under the `bench-3replica` profile it ALSO includes
      strata-c. Use a profile-conditional approach (e.g. separate
      nginx config file copied in via the `bench-3replica` profile, OR
      runtime env var template parsed by an entrypoint script —
      whichever matches the existing pattern; verify via
      `grep -rn upstream deploy/docker/nginx*` to see how it's wired
      today).
- [ ] If nginx-on-profile is too invasive, fall back to running
      strata-c WITHOUT the LB hop — `bench-rebalance-multi.sh` SHARDS=3
      leg targets all 3 replicas directly via the host port numbers
      (10001 / 10002 / 10003). Document the chosen approach in
      progress.txt.
- [ ] Un-skip the SHARDS=3 leg in
      `scripts/bench-rebalance-multi.sh` — remove the `SKIP` short-
      circuit + the "parked under restore-3-replica-tikv-bench" log
      line. Wire the leg to bring up `--profile bench-3replica` before
      running, tear it down after.
- [ ] Run the SHARDS=3 leg one-shot on the local Docker. Capture the
      output (chunks/s, MB/s, completion time) and update
      `docs/site/content/architecture/benchmarks/rebalance.md` (the
      canonical rebalance benchmark page; verify by `ls
      docs/site/content/architecture/benchmarks/`) with a new
      "3-replica TiKV (post-restore)" subsection. Compare against any
      historical 3-replica numbers already in that file — if none,
      this is the first capture and the subsection becomes the
      baseline.
- [ ] If the local box can't run 3 strata replicas + 2 ceph clusters
      + pd + tikv concurrently (lima memory cap, etc.), document the
      blocker in progress.txt and leave the SHARDS=3 leg `SKIP`'d
      again — but at minimum the compose profile must exist + the
      bench script must reference it (the new SKIP message references
      the resource cap, not the absence of the lab).
- [ ] `make smoke-tikv-default-lab` (the existing 4-scenario harness
      from `ralph/tikv-default-lab`) still passes — the new
      `bench-3replica` profile must not affect the bare default
      (Scenario A `bare bring-up` must still succeed unchanged).

### US-003: Helm chart packaging for Kubernetes

**Description:** As an operator deploying Strata to a Kubernetes
cluster, I want a Helm chart with a configurable `values.yaml` so I can
deploy without hand-templating the raw manifests under `deploy/k8s/`.

**Acceptance Criteria:**
- [ ] New directory `deploy/helm/strata/` with:
      - `Chart.yaml` — `apiVersion: v2`, name `strata`, type
        `application`, version `0.1.0`, appVersion matching the latest
        release tag (verify via `git tag --sort=-v:refname | head -1`
        or default to `0.1.0` if no tags), description, sources.
      - `values.yaml` — see structure below.
      - `templates/deployment.yaml`, `service.yaml`, `configmap.yaml`,
        `secret.yaml`, `ingress.yaml`, `servicemonitor.yaml`,
        `_helpers.tpl`.
      - `.helmignore` excluding `*.swp`, `*.bak`, `.DS_Store`, `OWNERS`,
        etc.
- [ ] `values.yaml` structure:
      - `image.repository`, `image.tag`, `image.pullPolicy`.
      - `replicas` (default 2 — matches canonical lab shape).
      - `meta.backend: tikv` (TiKV-only this cycle per scoping);
        `meta.endpoints` list of PD addresses.
      - `data.clusters` mirroring `STRATA_RADOS_CLUSTERS` syntax.
      - `workers: [lifecycle, gc, rebalance, notify, ...]` mirroring
        `STRATA_WORKERS=`.
      - `service.type: ClusterIP`, `service.port: 9000`.
      - `ingress.enabled: false`, `ingress.className`, `ingress.hosts`,
        `ingress.tls`.
      - `resources.requests / resources.limits` (sensible defaults:
        500m CPU, 512Mi memory request; 2 CPU, 4Gi limit).
      - `monitoring.enabled: false` — gates the ServiceMonitor
        template via `{{ if .Values.monitoring.enabled }}`. When false,
        no ServiceMonitor manifest emits (prom-operator-free clusters
        stay clean).
      - `securityContext` + `podSecurityContext` placeholders (empty
        by default — operator overrides via values file).
- [ ] `templates/_helpers.tpl` defines `strata.fullname`,
      `strata.labels`, `strata.selectorLabels`, `strata.serviceAccountName`
      using standard chart conventions (study a recent Helm chart in the
      ecosystem if uncertain — bitnami/, prometheus/, etc.).
- [ ] `templates/servicemonitor.yaml` wrapped in `{{ if
      .Values.monitoring.enabled }}` — scrapes the `/metrics` endpoint
      on the strata service port; `interval: 30s`,
      `scrapeTimeout: 10s`.
- [ ] `helm lint deploy/helm/strata/` passes with zero errors and
      zero warnings.
- [ ] New Makefile target `helm-lint` runs `helm lint
      deploy/helm/strata/`. Wired into `make test` as an extra
      dependency (or as a separate target documented in `make help`
      — verify which fits the existing target-dependency graph).
- [ ] If `helm` binary is not installed locally, the `helm-lint`
      target prints a one-line "install helm to run this target" hint
      and exits 0 (don't gate `make test` on a missing toolchain).
      Use `command -v helm > /dev/null || (echo "helm not installed
      ..." && exit 0)` shape.
- [ ] Document the chart in
      `docs/site/content/deploy/kubernetes.md` (or wherever the k8s
      deploy doc lives — verify) under a new "Helm install" section.
      Cover: `helm install strata deploy/helm/strata/ -n strata
      --create-namespace`, `--set monitoring.enabled=true`,
      `--set replicas=3`. Note the chart is TiKV-only this cycle
      (Cassandra-backed users stick with raw manifests).
- [ ] `helm template deploy/helm/strata/ | kubectl apply --dry-run=client
      -f -` passes (templated manifests are syntactically valid
      Kubernetes resources).

### US-004: Drift-proof S3 ops reference table via Go lint test

**Description:** As a developer adding a new S3 handler in
`internal/s3api/`, I want a test that fails the build when I forget to
update `docs/site/content/reference/s3-api.md`, so the hand-curated
table never drifts.

**Acceptance Criteria:**
- [ ] New test file `internal/s3api/docs_reference_test.go` (default
      build tag — runs under standard `go test ./...`).
- [ ] Parse `internal/s3api/server.go` via `go/parser` + `go/ast`:
      walk every `case "<query>":` arm inside `handleBucket`,
      `handleObject`, and the bucket-less route switch in
      `ServeHTTP` (if present — verify by reading the file). For each
      arm extract `(case_string, handler_func_name, file_path,
      ast_line_number)`.
- [ ] Read `docs/site/content/reference/s3-api.md`. Parse the markdown
      tables (each row has `Operation | Handler file:line | Shipped in
      | AWS gotchas`). Build a map from `handler_func_name` to row.
- [ ] For each AST switch arm:
      - **Hard fail** if no row references the handler function name
        AND the handler does NOT carry a `// docs:skip` comment-marker
        on the line directly above the `func` declaration. Failure
        message names the handler + file:line + suggests adding a row
        OR the `// docs:skip` marker.
      - **Soft path** (no test fail): handlers carrying `// docs:skip`
        are tolerated as "intentionally not documented" — log via
        `t.Logf` so the count is visible but the test stays green.
- [ ] For each row in the markdown table:
      - **Hard fail** if the row's `Handler file:line` points to a
        non-existent function (orphan row — handler was renamed or
        removed). Failure message names the row + suggests removing
        or updating the row.
- [ ] Test runs under `make test` (default-tag build), exits 0 today
      after this story's row-additions / marker-additions.
- [ ] As part of this story, audit the existing `s3-api.md` table:
      add `// docs:skip` markers to internal handlers the hand-curate
      pass intentionally left out (if any), OR add rows for them.
      Either path resolves the test to green; document the count of
      `// docs:skip` markers added (if > 0) in progress.txt.
- [ ] Cassandra integration testcontainers NOT required — the test
      is pure AST parsing + markdown text grep; runs under standard
      `go test` without `-tags integration`.
- [ ] `go vet ./internal/s3api/...` passes; `go test -race
      ./internal/s3api/...` passes.

### US-005: Smoke validation + ROADMAP close-flip + PRD removal

**Description:** As a future-maintainer, I want one explicit
verification pass that proves all 4 fixes landed correctly, plus the 4
ROADMAP entries flipped + the PRD markdown removed per the
cycle-end-to-end discipline.

**Acceptance Criteria:**
- [ ] Run `make dev` → log stream attaches; capture readiness in
      progress.txt (e.g. "strata-a /readyz green 4.2s after
      compose-up"). `Ctrl-C` exits tail; `make dev-down` tears down
      cleanly.
- [ ] Run `make smoke-tikv-default-lab` after `make dev-down` + fresh
      `make up-all` → 4/4 scenarios green (proves US-001 doesn't
      regress the canonical default).
- [ ] Run the SHARDS=3 bench leg one-shot: bring up
      `--profile bench-3replica`, run
      `scripts/bench-rebalance-multi.sh` SHARDS=3, tear down. Capture
      one-line result (or one-line SKIP reason if local box can't
      handle the resource ceiling).
- [ ] Run `helm lint deploy/helm/strata/` → zero errors, zero
      warnings.
- [ ] Run `helm template deploy/helm/strata/ | kubectl apply
      --dry-run=client -f -` → all manifests valid.
- [ ] Run `go test ./internal/s3api/...` (default tag) → green
      including the new `docs_reference_test.go`.
- [ ] Run full `go test -race ./...` (default tag) → green; capture
      duration in progress.txt.
- [ ] Run `make vet` → green.
- [ ] Run `make docs-build` → green (the kubernetes.md update from
      US-003 + any get-started.md update from US-001 render
      cleanly).
- [ ] **ROADMAP close-flip** × 4 in the same commit:
      - `**P3 — \`make dev\` for one-command developer cluster.**`
        (line 274) → flipped to Done. Summary: `make dev` /
        `make dev-down` / `make dev-logs` targets ship; TiKV-default
        backend matches canonical lab; documented under
        `/get-started/`.
      - `**P3 — Restore 3-replica TiKV bench (SHARDS=3
        rebalance-multi).**` (line 276) → flipped to Done. Summary:
        new `bench-3replica` compose profile adds strata-c;
        `bench-rebalance-multi.sh` SHARDS=3 leg un-skipped; fresh
        numbers in `docs/site/content/architecture/benchmarks/rebalance-multi.md`
        (or document SKIP reason if local box hit resource ceiling).
      - `**P3 — Helm chart packaging for Kubernetes deployment.**`
        (line 291) → flipped to Done. Summary: `deploy/helm/strata/`
        chart authored with values.yaml + templates;
        `helm lint` wired into `make helm-lint`; documented in
        `/deploy/kubernetes/`; TiKV-only this cycle (Cassandra
        parked).
      - `**P3 — Drift-proof S3 ops reference table via Go lint
        test.**` (line 293) → flipped to Done. Summary:
        `internal/s3api/docs_reference_test.go` AST-parses
        `server.go` switch arms + cross-checks `s3-api.md` rows;
        `// docs:skip` marker available for intentionally-undocumented
        handlers; bidirectional drift (missing row OR orphan row)
        fails `go test`.
- [ ] Each close-flip carries `(commit pending)` placeholder per the
      established cycle convention; SHA backfill lands on `main`
      post-merge as the fast-follow commit.
- [ ] `tasks/prd-dx-lab.md` REMOVED via `git rm` per CLAUDE.md PRD
      lifecycle rule.
- [ ] `scripts/ralph/progress.txt` carries one US-005 block
      summarising smoke results + any learnings for future
      iterations.
- [ ] `go vet ./...` passes; `make vet` passes; all tests pass.

## Functional Requirements

- FR-1: `make dev` MUST bring up the canonical TiKV-default lab and
  attach a foreground log stream of strata-a / strata-b / nginx-lb.
- FR-2: `make dev-down` MUST tear down the compose stack via the
  existing `make down` recipe.
- FR-3: `make dev-logs` MUST re-attach the log stream after `Ctrl-C`
  exit.
- FR-4: `--profile bench-3replica` MUST add `strata-c` (port 10003,
  distinct STRATA_NODE_ID, shared jwt volume) to the canonical
  TiKV-default stack.
- FR-5: `scripts/bench-rebalance-multi.sh` SHARDS=3 leg MUST be
  un-skipped (or carry a documented resource-ceiling SKIP referencing
  the local box memory cap rather than the parked-profile excuse).
- FR-6: `deploy/helm/strata/` MUST contain a valid Helm v2 chart that
  passes `helm lint` with zero errors / zero warnings.
- FR-7: `helm template deploy/helm/strata/` output MUST pass
  `kubectl apply --dry-run=client`.
- FR-8: `monitoring.enabled` MUST gate the ServiceMonitor template
  (default false).
- FR-9: `internal/s3api/docs_reference_test.go` MUST hard-fail on
  missing row OR orphan row; MUST tolerate `// docs:skip`-marked
  handlers via `t.Logf` only.
- FR-10: ROADMAP MUST flip 4 P3 entries in the US-005 commit;
  no new ROADMAP gaps surfaced this cycle (so no
  "Discovering a new gap" entry).

## Non-Goals

- No Cassandra-backed Helm chart (parked — operators using Cassandra
  stick with raw `deploy/k8s/` manifests).
- No memory-backed dev variant of the Helm chart (canonical lab is
  not the chart's target).
- No `make dev` Cassandra variant (`make up-cassandra` already
  exists; don't add `make dev-cassandra` to keep the surface narrow).
- No PodMonitor template — only ServiceMonitor (`monitoring.enabled`
  toggles it).
- No nginx-config templating for the bench-3replica profile if the
  pattern is too invasive — fall back to direct-port bench against
  strata-c (10003) bypassing the LB; document the choice.
- No Go test for the env-vars reference table or admin-api reference
  table — those grep-derived / OpenAPI-derived pages are easier to
  keep in sync; s3-api is the hand-curated one that needs drift
  protection.
- No CI rewiring beyond adding `make helm-lint` if not already in
  the CI matrix.

## Design Considerations

- **`make dev` foreground vs background**: foreground (log tail
  attached) was chosen — `Ctrl-C` is the natural "I'm done watching"
  exit. Background variant (`make dev -d`) is the existing
  `make up-all` + manual `docker compose logs -f` — keep the
  composable underlying targets for advanced users.
- **bench-3replica nginx wiring**: if nginx config templating per-
  profile is too invasive (separate config file + `volumes:` swap),
  fall back to direct-port bench. The bench script can target
  strata-c (10003) explicitly — the LB hop is not load-balancing-
  validated by the bench anyway, the bench measures rebalance fan-out
  speedup across worker replicas.
- **Helm chart values structure**: mirror the env-var shape so an
  operator who has read the `STRATA_*` env vars reference page
  (US-002 of `ralph/docs-adr`) can map every knob 1:1 to a values key.
- **s3-api lint test marker**: `// docs:skip` lives on the line
  directly above the `func handle...` declaration so the test can
  match via simple line-pair check; no fancy comment-block parsing.

## Technical Considerations

- **Docker resource ceiling**: 3 strata replicas + 2 ceph clusters
  + pd + tikv + nginx + prometheus + grafana adds ~1.5 GiB memory
  over the bare default. Lima on macOS typically has 4-8 GiB cap;
  the bench leg may need to fall back to SKIP with documented
  reason.
- **Helm chart version pinning**: `Chart.yaml.version = 0.1.0` for
  this initial release; future increments under semver as the chart
  evolves. `appVersion` follows the strata binary release tag.
- **`helm` binary availability**: not in standard dev env; the
  `make helm-lint` target must degrade gracefully when helm is
  missing.
- **Go test build tag**: `docs_reference_test.go` runs under default
  build tag (no `//go:build` line) so it's part of every `make test`
  run. The AST parser only touches Go source files; markdown grep is
  pure stdlib.

## Success Metrics

- Fresh contributor goes from `git clone` to running stack in ≤ 1
  minute (`make dev` wall-clock).
- 3-replica bench leg captures fresh numbers (or documents the
  resource-ceiling SKIP with a concrete fix path).
- `helm lint deploy/helm/strata/` exits 0 in < 1 second.
- `internal/s3api/docs_reference_test.go` runtime < 100 ms.
- 4 ROADMAP P3 entries close in one cycle; no new P3 gaps surfaced.
- Cycle ships in ≤ 5 stories; no admin endpoint, no UI work.

## Open Questions

- ServiceAccount + RBAC in the Helm chart: include with sensible
  defaults (read-only on most resources, no Pod creation), or expect
  cluster admins to wire ServiceAccount externally? Default: include
  minimal SA + Role + RoleBinding in `_helpers.tpl` so the chart is
  install-ready out of the box.
- `make dev` first-time bootstrap (pulling images, building strata
  binary on a fresh checkout) may take longer than 1 minute. Document
  the warm-cache vs cold-cache expectation in the `/get-started/`
  page.
- Helm chart values for multi-cluster RADOS topology — single-line
  `STRATA_RADOS_CLUSTERS` env vs structured `data.clusters[]` array
  in values.yaml that templates render into the env var. Default to
  the structured shape for ergonomics; the template stringifies into
  the env var at render time.
