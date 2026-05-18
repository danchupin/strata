# PRD: TiKV-default 2-replica lab; Cassandra-backed shape moves under `cassandra` profile

## Introduction

Lab compose ships a Cassandra-backed canonical strata service as the bare `docker compose up -d` default. Operators learning the system see one Cassandra-backed gateway + an array of profile-gated alternatives (`tikv`, `lab-tikv`, `lab-tikv-3`, `lab-cassandra-3`). The profile names lean Cassandra-as-default; multi-replica TiKV testing requires explicit profile invocation.

Refactor: TiKV becomes the bare-`up -d` default with **two strata replicas** behind nginx LB. Cassandra-backed lab moves to `--profile cassandra` for side-by-side regression coverage (Cassandra meta backend remains first-class in code, tests, and CI — only the lab default flips). All single-cluster + multi-replica TiKV/Cassandra profile variants drop (`tikv`, `lab-tikv`, `lab-tikv-3`, `lab-cassandra-3` are retired); the bare-default IS the multi-replica HA shape now.

Operator-facing port `9999` preserved — fronted by nginx LB instead of a single Cassandra strata.

## Goals

- Bare `docker compose up -d` brings up: `pd, tikv, ceph, ceph-b, strata-a, strata-b, strata-lb-nginx, prometheus, grafana`. Two TiKV-backed strata replicas attached to both `default` + `cephb` RADOS clusters. nginx LB on host port `9999` round-robins requests.
- Cassandra meta backend remains first-class in code (memory + cassandra + tikv contract parity preserved). Lab shape moves under `--profile cassandra` — `cassandra` service + `strata-cassandra` single replica on port `9998`.
- Drop redundant profile variants: `tikv` (single TiKV replica), `lab-tikv` (already 2-replica TiKV; now the default), `lab-tikv-3` (3-replica TiKV), `lab-cassandra-3` (3-replica Cassandra).
- Orthogonal profiles preserved: `webhook-trap`, `tracing`, `ci`.
- Sweep all Makefile / scripts / docs / CI / README / CLAUDE.md references.
- Cassandra backend testing in CI continues via testcontainers (`make test-integration`); lab compose is for dev only.

## User Journey (pre-cycle walkthrough)

Happy path — fresh clone, bare bring-up:

1. Operator clones repo, `docker compose pull`, `docker compose build strata`.
2. Operator runs `make up-all && make wait-ceph`.
3. Compose starts (in order via `depends_on`): pd, tikv, ceph, ceph-b, strata-a, strata-b, strata-lb-nginx, prometheus, grafana. No cassandra container exists.
4. nginx LB binds host `9999`, round-robins to `strata-a:9000` + `strata-b:9000`. Direct-access ports `10001` (strata-a) and `10002` (strata-b) bound for smoke / debug.
5. UI at http://localhost:9999/console/ (creds `admin` / `adminpass`). PUT/GET round-trip through the LB. Worker leases distribute via TiKV — `strata-a` may hold gc-leader-0 and rebalance-leader-0; `strata-b` may hold lifecycle-leader-0; visible in admin console Cluster Overview.

Happy path — Cassandra-backed regression lab:

1. Operator runs `make up-cassandra` → `docker compose --profile cassandra up -d`.
2. Compose ALSO starts (alongside the default TiKV stack): cassandra, strata-cassandra (port 9998).
3. Operator drives smoke harnesses against `:9998` to verify Cassandra-backed code path.
4. Default TiKV stack stays up unaffected; both run side-by-side on shared `ceph + ceph-b`.

Single-cluster smoke (env override):

1. Operator runs `STRATA_RADOS_CLUSTERS=default:/etc/ceph-a/... docker compose up -d strata-a strata-b strata-lb-nginx`.
2. Both TiKV replicas attach only to `default`; PUT/GET round-trip green. Same env-override flow established in `ralph/compose-profile-isolation`.

Negative path — operator runs `--profile tikv` or `--profile lab-tikv*` or `--profile lab-cassandra-3`:

1. Compose ignores unknown profiles silently. Bare bring-up still happens. No error, no warning. Documented as removed in migration note.

Edge case — operator wants 3-replica HA testing (the old `lab-tikv-3` shape):

1. `docker compose up -d --scale strata-a=3` doesn't work directly (named services don't scale). Operator follows the migration note's path: spin a third replica via `--profile lab-3` ad-hoc OR uses `docker compose --scale` against a deprecated unnamed shape. Concrete path: documented + smoke covers only 2-replica; 3rd replica testing migrates to a separate bench follow-up cycle.

Edge case — `bench-rebalance-multi.sh` (added in `ralph/rebalance-scale-phase-2`):

1. Existing script used `lab-tikv-3` profile to bring up 3 replicas. With the profile retired, the bench breaks. US-005 of this cycle reworks the bench to use bare-default 2-replica TiKV; the third-replica bench shape is parked as a P3 follow-up (`Restore 3-replica TiKV bench`) if anyone needs it back.

## User Stories

### US-001: Compose rewrite — TiKV default, Cassandra under profile

**Description:** As an operator, I want bare `docker compose up -d` to bring up a 2-replica TiKV-backed gateway with nginx LB, with the Cassandra-backed lab available only under an explicit profile.

**Acceptance Criteria:**
- [ ] `deploy/docker/docker-compose.yml`:
  - Drop existing canonical `strata` service (Cassandra-backed, port 9999, lines ~83-129)
  - Drop existing `strata-tikv` service (single TiKV-backed replica, profile `tikv`)
  - Drop existing `strata-tikv-c` service (3rd lab-tikv replica, profile `lab-tikv-3`)
  - Drop existing `strata-cass-a/b/c` services (profile `lab-cassandra-3`)
  - Drop existing `strata-lb-nginx-cass` service (profile `lab-cassandra-3`)
  - Move `cassandra` service under `profiles: ["cassandra"]` (currently no profile = always-on)
  - Promote `pd`, `tikv` services to no-profile (currently profile `tikv` + `lab-tikv`; remove gate so they always start)
  - Rename existing `strata-tikv-a` → `strata-a`, `strata-tikv-b` → `strata-b`. Remove `profiles: ["lab-tikv"]` from both. Mount `strata-cephb-etc:/etc/ceph-b:ro` in addition to existing `strata-ceph-etc:/etc/ceph:ro` mount; ALSO rename the per-cluster ceph etc bind path to `strata-ceph-etc:/etc/ceph-a:ro` (multi-cluster shape matches the canonical strata service's path layout). Add multi-cluster env: `STRATA_RADOS_CLUSTERS: ${STRATA_RADOS_CLUSTERS:-default:/etc/ceph-a/ceph.conf:/etc/ceph-a/ceph.client.admin.keyring,cephb:/etc/ceph-b/ceph.conf:/etc/ceph-b/ceph.client.admin.keyring}`. Default workers `gc,lifecycle,rebalance`. Distinct host ports `10001:9000` (strata-a) and `10002:9000` (strata-b)
  - **PRESERVE per-replica identity + shared session JWT**: each strata-a/b keeps a distinct `STRATA_NODE_ID: strata-a` / `strata-b` env value AND mounts the shared `strata-jwt-shared:/etc/strata/jwt-shared` volume so a session token issued by replica A validates on replica B. Without these the round-robin LB breaks sessions across requests (operator logs into UI, next request hits other replica, gets 401)
  - **DEPENDS_ON expanded**: strata-a/b `depends_on` must include `pd: service_healthy`, `tikv: service_started`, `ceph: service_healthy`, AND `ceph-b: service_healthy`. Without ceph-b dep compose starts strata-a before ceph-b is ready → multi-cluster boot fail
  - **NO compose-level rebalance defaults**: do NOT add `STRATA_REBALANCE_INTERVAL` / `STRATA_REBALANCE_RATE_MB_S` envs to strata-a/b (gateway code defaults apply — 1h interval, 100 MB/s rate per CLAUDE.md). The retired Cassandra-backed `strata` service had explicit compose defaults of 30s/100MB; document the cadence change in the migration note (US-004) so operators relying on the 30s tick know to set env explicitly if needed
  - Promote `strata-lb-nginx` to no-profile (currently `lab-tikv`). Update nginx config at `deploy/nginx/strata-lab.conf` to round-robin over `strata-a:9000` + `strata-b:9000` (no longer over `strata-tikv-a/b`). Bind host port `9999:80`
  - New `strata-cassandra` service under `profiles: ["cassandra"]`: single Cassandra-backed replica, attaches to both RADOS clusters, port `9998:9000`, depends on cassandra + ceph + ceph-b. Mirrors the env shape of the surviving strata-a (multi-cluster, admin creds preset)
  - `ceph` and `ceph-b` keep their no-profile status (always-on dependency for any strata replica)
  - All retained services keep `STRATA_AUTH_MODE=optional` + `STRATA_STATIC_CREDENTIALS=admin:adminpass:owner` defaults (post-061424a fix)
- [ ] `deploy/nginx/strata-lab.conf` rewritten to point at `strata-a:9000` + `strata-b:9000`
- [ ] `docker compose config` (no profile flag) lists exactly: `cassandra-absent`, `ceph, ceph-b, grafana, pd, prometheus, strata-a, strata-b, strata-lb-nginx, tikv` (cassandra-absent meaning cassandra service should NOT appear in this output)
- [ ] `docker compose --profile cassandra config` ALSO lists `cassandra, strata-cassandra` (in addition to bare-default)
- [ ] `docker compose --profile tikv config` AND `--profile lab-tikv*` AND `--profile lab-cassandra-3` config — no additional services (profiles silently no-op)
- [ ] `go vet ./...` passes (no Go change; cycle CI gate)
- [ ] `make build` passes
- [ ] Typecheck passes

### US-002: Makefile sweep — `up-all` default + `up-cassandra` profile entry + drop dead targets

**Description:** As a future-maintainer, I need every Makefile target referencing the retired profile shapes updated or removed.

**Acceptance Criteria:**
- [ ] `Makefile` `up` and `up-all` targets rewritten to bare `docker compose up -d` (no profile). Brings up the 2-replica TiKV default stack.
- [ ] New `make up-cassandra` target: `docker compose --profile cassandra up -d`. Brings up the Cassandra-backed regression lab alongside the bare default.
- [ ] Drop `up-tikv` target (replaced by bare default).
- [ ] Drop `up-lab-tikv` target (replaced by bare default).
- [ ] Drop `up-lab-tikv-3` target (3-replica TiKV retired).
- [ ] Drop `up-lab-cassandra-3` target (3-replica Cassandra retired).
- [ ] Update `wait-cassandra` / `wait-ceph` / `wait-strata*` targets — drop strata-tikv-named waits, add strata-a / strata-b / strata-lb-nginx readyz waits.
- [ ] New `wait-tikv` target that loops on `pd:2379/pd/api/v1/health` returning 200.
- [ ] `run-cassandra` (run strata binary against Cassandra meta) target preserved but with comment update — it's the dev-only "no compose" path, not affected by lab compose changes.
- [ ] Bench targets that depended on retired profiles: `bench-rebalance-multi` updated in US-003 to use bare default + a workaround for the 3rd-replica scaling.
- [ ] `make smoke` target preserved verbatim (already targets bare default, which is now TiKV-backed multi-cluster — no rename, no collision). `make smoke-signed` similarly preserved.
- [ ] `make smoke-tikv` retired (now redundant — `make smoke` IS the TiKV smoke).
- [ ] `make smoke-multi-cluster` retired (the multi-cluster shape IS the default now).
- [ ] All Makefile help text updated.
- [ ] `make build` passes; `make vet` passes; `make test` passes.
- [ ] Typecheck passes

### US-003: Scripts + CI sweep — TiKV port shape + bench-rebalance-multi rework

**Description:** As a future-maintainer, I need every shell script + CI workflow referencing retired profile/service names updated to the TiKV-default shape.

**Acceptance Criteria:**
- [ ] Sweep `scripts/**/*.sh`: replace `strata-tikv-a`/`b`/`c` container references with `strata-a`/`b` (where the 3rd-replica shape is retired); replace `:9998` references (was Cassandra strata) with `:9999` (LB) where the shape is now LB-fronted, OR with `:9998` (Cassandra-profile strata) where the script is Cassandra-specific. Concrete files to inspect: `scripts/smoke-effective-placement.sh`, `scripts/smoke-drain-transparency.sh`, `scripts/smoke-drain-progress-ui.sh`, `scripts/smoke-cluster-weights.sh`, `scripts/smoke-compose-collapse.sh`, `scripts/smoke-rebalance-scale.sh`, `scripts/multi-replica-smoke.sh`, `scripts/bench-rebalance-multi.sh`.
- [ ] `scripts/bench-rebalance-multi.sh` rework: previously used `lab-tikv-3` profile to spin 3 replicas. Pick path (cleanest first; implementer may pick either):
  - **(preferred)** SHARDS=3 scenario explicitly SKIPPED with `echo "SKIP: lab-tikv-3 retired in ralph/tikv-default-lab cycle; SHARDS=3 bench parked as P3 follow-up"` and the script EXITS 0 after running SHARDS=1 + SHARDS=2 baselines. Operator sees the skip in the output; script doesn't fail.
  - **(alternative)** Spin a one-off third replica via `docker compose run --rm -p 10003:9000 -e STRATA_NODE_ID=strata-c -e STRATA_WORKERS=gc,lifecycle,rebalance strata-a` for the bench duration; teardown after. Higher complexity; only if SHARDS=3 numbers are genuinely needed
- [ ] Script must NOT silently succeed without emitting the skip-or-execute decision to stdout. operator reading the bench output knows what ran.
- [ ] `.github/workflows/*.yml`: drop `--profile lab-tikv*` / `--profile lab-cassandra-3` / `--profile tikv` flags from any integration job; default `docker compose up -d` (TiKV) is the lab shape. CI jobs requiring Cassandra-specific assertions move to `--profile cassandra` invocation.
- [ ] **Critical** — sweep `.github/workflows/*.yml` for `make wait-cassandra` invocations. The bare-default stack no longer auto-starts cassandra → `wait-cassandra` will hang/fail. Replace with `make wait-tikv` for TiKV-default jobs, or move the job to `--profile cassandra up -d && make wait-cassandra` if the job genuinely requires Cassandra. Same sweep applies to `scripts/**/*.sh` shell scripts that chain `make up && make wait-cassandra`.
- [ ] `make test-integration` (testcontainers — Cassandra backend regression coverage) PRESERVED unchanged. Cassandra meta-backend code testing happens via testcontainers, NOT via lab compose.
- [ ] Add new P3 ROADMAP entry **`Restore 3-replica TiKV bench (SHARDS=3 rebalance-multi)`** as a parked follow-up if `bench-rebalance-multi.sh` drops the SHARDS=3 scenario.
- [ ] Final grep gate (codified in US-005 smoke harness): zero matches for `strata-tikv-a`/`b`/`c` / `strata-cass-` / `strata-lb-nginx-cass` / `--profile lab-tikv` / `--profile lab-cassandra-3` / `--profile tikv` outside `scripts/ralph/archive/**`, `docs/site/public/**`, `docs/site/resources/**`, `progress.txt` / `prd.json`, the smoke script itself, and ROADMAP close-flip narratives (which NARRATE the migration).
- [ ] `make vet`, `make test`, `pnpm run build`, `make docs-build` all green.
- [ ] Typecheck passes

### US-004: Docs + README + CLAUDE.md sweep + new migration note

**Description:** As a future-maintainer, I need the operator-facing docs reflecting TiKV as the default lab shape with Cassandra-backed gracefully demoted under `--profile cassandra`.

**Acceptance Criteria:**
- [ ] Project root `README.md` Quick start section rewritten — bare `docker compose up -d` brings up the 2-replica TiKV stack; admin console at `http://localhost:9999/console/`; Cassandra-backed regression lab via `make up-cassandra`.
- [ ] Project root `CLAUDE.md` sections updated: "Common commands" table refreshed (drop old `up-tikv` row, add `up-cassandra`); "Big-picture architecture" diagram refreshed (TiKV as default meta path; Cassandra as profile-gated alt); the "Background workers" section's mention of leader-election explicitly stays cluster-agnostic (single shared lease key set on TiKV in the canonical path).
- [ ] `docs/site/content/architecture/backends/tikv.md` updated: lab shape is now TiKV-default 2-replica; the old "TiKV is opt-in via `--profile tikv`" narrative inverted.
- [ ] `docs/site/content/architecture/backends/cassandra.md` (if exists; else `cassandra/_index.md`) updated: Cassandra meta backend remains first-class in code; lab via `--profile cassandra`; tests via `make test-integration` (testcontainers).
- [ ] `docs/site/content/best-practices/placement-rebalance.md` "Multi-leader scaling" subsection updated — bare default is the multi-replica shape now; drop references to `lab-cassandra-3` / `lab-tikv-3`; cross-link to migration note.
- [ ] `docs/site/content/best-practices/multi-cluster-lab.md` updated similarly (or removed if it's now redundant with the bare-default shape).
- [ ] New migration note `docs/site/content/architecture/migrations/tikv-default-lab.md` documents:
  - What was removed (services, profiles, ports, Makefile targets) + the new bare-default shape + the Cassandra-profile path + the 3-replica regression follow-up + the `bench-rebalance-multi` rework
  - **Port-change warning** (explicit subsection): the Cassandra-backed strata moved from `:9999` to `:9998`. `:9999` is now the nginx LB fronting `strata-a` + `strata-b` (TiKV). Operator curl scripts targeting `:9999` for Cassandra-backed regression must update to `:9998`
  - **Makefile target breaking changes**: `make wait-cassandra` no longer succeeds against the bare default (cassandra is now profile-gated); operator scripts chaining `make up && make wait-cassandra` must switch to `make wait-tikv` OR use `make up-cassandra && make wait-cassandra` if they actually need Cassandra
  - **Compose-default rebalance cadence change**: pre-cycle Cassandra-backed `strata` shipped explicit compose defaults of `STRATA_REBALANCE_INTERVAL=30s` + `STRATA_REBALANCE_RATE_MB_S=100`. The new bare default (TiKV strata-a/b) uses gateway built-in defaults instead (1h interval, 100 MB/s rate per CLAUDE.md). Operators relying on the 30s rebalance tick must set `STRATA_REBALANCE_INTERVAL=30s` env explicitly on `make up`. Documented + cross-linked to `docs/site/content/best-practices/placement-rebalance.md`.
  - **Cassandra meta-backend remains first-class in code**: explicit reassurance that this is a lab compose change only. `internal/meta/cassandra/**` code is unchanged. `make test-integration` (testcontainers Cassandra) preserved. Cassandra parity with TiKV in contract tests + benchmarks remains. The cycle is a *deployment shape* flip, not a backend deprecation.
  - Linked from `docs/site/content/architecture/migrations/_index.md`
- [ ] Hugo site builds with no dangling links.
- [ ] `make docs-build` passes.
- [ ] Typecheck passes

### US-005: Smoke + ROADMAP close-flip + PRD removal

**Description:** As an operator and as a future-maintainer, I need an end-to-end smoke proving the new TiKV-default + Cassandra-profile shape works + documented migration + the ROADMAP entry flipped.

**Acceptance Criteria:**
- [ ] New `scripts/smoke-tikv-default-lab.sh` covers four scenarios with `echo "==> Scenario X: ..."` headers:
  - **Scenario A** (bare bring-up — TiKV default): `docker compose up -d`; assert services running: pd, tikv, ceph, ceph-b, strata-a, strata-b, strata-lb-nginx, prometheus, grafana; assert `cassandra`, `strata-tikv-a/b/c`, `strata-cass-*`, `strata-lb-nginx-cass` containers do NOT exist; assert nginx LB on `:9999` round-robins by checking access logs after a 20-request burst (e.g. `docker logs strata-a --since 30s | grep "GET" | wc -l` returns ≥5 AND same for `strata-b`); PUT 10 objects via `:9999`; assert all GETs round-trip; drain `cephb` evacuate; assert `deregister_ready=true` within 5 min.
  - **Scenario B** (Cassandra profile side-by-side): `docker compose --profile cassandra up -d`; assert ADDITIONAL services: cassandra, strata-cassandra; assert `:9998` (Cassandra-backed strata) and `:9999` (TiKV LB) both serve; PUT 10 objects on each; assert independent metadata state (Cassandra strata has its own bucket index in cassandra meta, TiKV strata in tikv meta).
  - **Scenario C** (env-override single-cluster on TiKV default): `STRATA_RADOS_CLUSTERS=default:... docker compose up -d strata-a strata-b strata-lb-nginx`; both replicas attach only to `default`; PUT + GET round-trip green; verify gateway logs show single-cluster connection.
  - **Scenario D** (residue grep): run the grep gate from US-003 — assert zero matches for retired profile / service names outside the documented exception set.
- [ ] `make smoke-tikv-default-lab` Makefile target wraps the script; exits non-zero on any failure.
- [ ] `ROADMAP.md` close-flip the P3 entry (if added) → `~~**P3 — TiKV-default 2-replica lab.**~~ — **Done.** <one-line summary referencing TiKV default + 2-replica + nginx LB + cassandra profile + bench rework>. (commit \`<pending>\`)`; closing SHA backfilled on main. **NOTE**: this entry doesn't exist on ROADMAP yet — add it as a new entry at the same commit (the cycle was operator-requested, not pre-roadmapped, so the cycle adds + closes the entry simultaneously per CLAUDE.md "Discovering a new gap" rule).
- [ ] Add P3 ROADMAP entry **`Restore 3-replica TiKV bench (SHARDS=3 rebalance-multi)`** as a parked follow-up (per US-003).
- [ ] `tasks/prd-tikv-default-lab.md` REMOVED in the same commit (CLAUDE.md PRD lifecycle rule).
- [ ] `make docs-build`, `make vet`, `make test`, `pnpm run build`, `make smoke-tikv-default-lab` all green.
- [ ] Typecheck passes

## Functional Requirements

- FR-1: Bare `docker compose up -d` brings up TiKV-backed 2-replica strata + nginx LB on `:9999` + PD + TiKV + Ceph + Ceph-b + observability.
- FR-2: Cassandra service + Cassandra-backed strata move under `--profile cassandra`. Cassandra meta-backend code unchanged.
- FR-3: Retired profile names (`tikv`, `lab-tikv`, `lab-tikv-3`, `lab-cassandra-3`) silently no-op when invoked.
- FR-4: All Makefile / scripts / docs / CI references swept to the new shape.
- FR-5: `bench-rebalance-multi.sh` reworked; SHARDS=3 scenario either replicated via ad-hoc 3rd container OR parked behind P3 follow-up.
- FR-6: Smoke harness covers bare bring-up + Cassandra side-by-side + single-cluster env override + residue grep.
- FR-7: New migration note documents the deletion + the new shape.

## State Truth Tables

### Compose service start matrix (after refactor)

| Invocation | pd | tikv | ceph | ceph-b | strata-a | strata-b | strata-lb-nginx | cassandra | strata-cassandra |
|---|---|---|---|---|---|---|---|---|---|
| `docker compose up -d` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ |
| `docker compose up -d strata-a` | (dep) | (dep) | (dep) | (dep) | ✓ | ✗ | ✗ | ✗ | ✗ |
| `docker compose --profile cassandra up -d` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `docker compose --profile tikv up -d` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ |
| `docker compose --profile lab-tikv up -d` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ |
| `docker compose --profile lab-tikv-3 up -d` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ |
| `docker compose --profile lab-cassandra-3 up -d` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ |

(Retired profiles match no services and silently no-op; bare default still starts.)

### Port allocation

| Service | Host port | Notes |
|---|---|---|
| strata-lb-nginx | 9999 | Operator-facing canonical entry |
| strata-a | 10001 | Direct access (smoke / debug) |
| strata-b | 10002 | Direct access (smoke / debug) |
| strata-cassandra (profile) | 9998 | Cassandra-backed regression lab |
| cassandra | 9042 | Cassandra CQL (profile cassandra only) |
| pd | (internal) | TiKV PD; no host port |
| tikv | (internal) | TiKV storage; no host port |
| prometheus | 9090 | |
| grafana | 3000 | |

## Cache Invalidation Ledger

- No new caches.
- No invalidation changes to existing caches (DrainCache, ClusterStatsCache).

## Safety Claims Preconditions

- **Claim: Cassandra meta-backend code remains first-class.**
  - Precondition: `internal/meta/cassandra/**` code untouched (lab compose change only). `make test-integration` (testcontainers Cassandra) preserved. **Test**: `make test-integration` green; existing Cassandra unit / contract tests unchanged.
- **Claim: lab compose flip doesn't break prod deploys.**
  - Precondition: prod uses k8s/Nomad, not lab compose. **Test**: documented in migration note; no enforcement needed.
- **Claim: multi-cluster behavior parity preserved.**
  - Precondition: bare TiKV replicas attach to both `default` + `cephb` (same as pre-cycle Cassandra strata). **Test**: Scenario A drain + deregister flow passes.
- **Claim: round-robin LB exercises stateless gateway invariants.**
  - Precondition: nginx LB rotation hits different replicas; both replicas serve full S3 surface. **Test**: Scenario A asserts LB round-robin via `STRATA_NODE_ID` differentiation across HEAD-loop responses.

## Downstream Consumer Grep

Verified affected paths (search performed before drafting):

- `strata-tikv-a` / `strata-tikv-b` / `strata-tikv-c` — touch compose + nginx config + Makefile targets + scripts + CI workflows + docs. US-001 + US-002 + US-003 + US-004 sweep all.
- `strata-cass-a/b/c` + `strata-lb-nginx-cass` — touch compose + Makefile (`up-lab-cassandra-3`) + scripts + docs. Sweep all.
- `--profile tikv` / `lab-tikv` / `lab-tikv-3` / `lab-cassandra-3` — touch compose + Makefile + scripts + CI + docs. Sweep all.
- `cassandra` profile name (new) — no prior consumers.
- `strata-cassandra` service name (new) — no prior consumers.

Surviving paths:

- `--profile webhook-trap`, `--profile tracing`, `--profile ci` — orthogonal infra, untouched.
- `internal/meta/cassandra/**` — Cassandra meta-backend code, untouched.
- `make test-integration` — testcontainers Cassandra regression, untouched.

## Worst-Case Thought Exercise

What if an operator has muscle memory of `--profile lab-tikv` for 2-replica?

- Profile silently no-ops. Bare default IS 2-replica. They get what they wanted, just without the flag. Documented in migration note.

What if an operator's local script hits `:9998` expecting Cassandra-backed strata?

- Bare default has no `:9998` (cassandra-profile gated). Script fails connection. Operator notices, switches to `--profile cassandra` OR `:9999` (TiKV LB). Documented as known regression for cassandra-port assumers.

What if `bench-rebalance-multi.sh` is invoked by an existing CI workflow?

- US-003 reworks the bench. If the rework drops SHARDS=3, CI workflow needs adjustment (separate sweep). US-003 includes the CI sweep too.

What if production deploy used compose lab (unusual but possible)?

- Lab compose isn't load-bearing for prod. Documented. Operators who copied lab shape to prod need to update their k8s manifests separately.

What if operator wants 3-replica TiKV HA validation (the old `lab-tikv-3`)?

- Documented in migration note: spin a third replica via `docker compose run --rm -p 10003:9000 strata-a` (one-off) OR fork the compose locally. P3 follow-up entry parks the proper restoration if demand justifies.

What about the rebalance-scale-phase-2 cycle's bench documentation (`docs/site/content/architecture/benchmarks/rebalance.md`)?

- Bench numbers from that doc were measured on lab-tikv-3. US-004 updates the doc with a "(measured on now-retired lab-tikv-3 profile; restoration parked as P3 follow-up)" footnote.

What about gateway restart mid-test or replica failover?

- nginx LB skips unhealthy upstreams. Operator hits surviving replica via LB. Same HA semantic as the prior lab-tikv shape.

## Non-Goals

- Production multi-region deploy guidance (PD ≥3, TiKV ≥3 — already documented in `tikv.md`; not re-touched).
- Restoring 3-replica TiKV bench in the same cycle (P3 follow-up).
- Restoring 3-replica Cassandra bench (no consumer; not added).
- Auto-detection of stale compose state (operator manually `docker compose down` after pulling new compose).
- Migrating prod k8s manifests (lab compose only).
- Removing Cassandra code from the repo (Cassandra meta-backend remains first-class).

## Open Questions

- **Strata cassandra service name**: `strata-cassandra` vs `strata-cass`. Picked `strata-cassandra` for clarity (no abbreviation collision with the `cassandra` service). Decision: `strata-cassandra`.
- **Profile invocation for orthogonal CI override**: `--profile ci` keeps working but stacking with `--profile cassandra` is untested in this cycle. Decision: defer; CI workflows pick exactly one profile per job.

## Technical Considerations

- TiKV PD ≥3 + TiKV ≥3 for production raft majority — documented; lab keeps 1+1 (current shape).
- nginx LB requires both replicas to be reachable on the compose network — preserved via `depends_on`.
- Cassandra under `--profile cassandra` means `cassandra` service AND `strata-cassandra` service both must declare that profile; otherwise compose orchestration breaks (cassandra would start but strata-cassandra wouldn't, or vice versa).
- `make up-all` semantics shift: was "bring up Cassandra + ceph + strata", becomes "bring up TiKV stack default". Operators reading old docs may be surprised; the migration note covers this.
- Worker leader-election: bare default has 2 strata replicas sharing TiKV meta; leases distribute exactly as the rebalance-scale-phase-2 + gc-lifecycle-scale-phase-2 design intended. No new lease-shape concerns.
- Cassandra-backed strata (`--profile cassandra`) runs alongside the TiKV replicas. Both backends are separate code paths in `internal/meta/{cassandra,tikv}`; the binary supports exactly one meta backend per process (per `STRATA_META_BACKEND`).
- nginx LB config at `deploy/nginx/strata-lab.conf` — already exists for the now-retired `lab-tikv` profile. Just needs upstream block updated to `strata-a:9000` / `strata-b:9000`.

## Success Metrics

- `make smoke-tikv-default-lab` passes end-to-end.
- Grep gate at end of US-003 reports zero residue.
- `docker compose ps` after `docker compose up -d` shows exactly 9 services (pd, tikv, ceph, ceph-b, strata-a, strata-b, strata-lb-nginx, prometheus, grafana).
- Cassandra regression coverage preserved via `make test-integration` (testcontainers).
- Drain `cephb` evacuate completes within 5 min on smoke load.
- ROADMAP P3 entry added + closed.
