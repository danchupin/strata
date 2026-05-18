# PRD: Collapse compose to single multi-cluster strata service

## Introduction

Closes ROADMAP P2 **"Compose `default` + `multi-cluster` profiles race for worker leases on shared Cassandra"**.

Root cause is the parallel maintenance of two strata service shapes (single-cluster `strata` + multi-cluster `strata-multi`) on shared Cassandra metadata. Worker supervisor leases (`gc-leader-N`, `rebalance-leader-N`, `lifecycle-leader-N`, etc.) are global on Cassandra; whichever container starts first wins each lease. When the single-cluster winner pops a GC entry for a cluster it doesn't know, the entry retries forever and `deregister_ready` stuck `false` permanently.

Fix is structural rather than band-aid: there is only ever ONE strata service in the lab — the multi-cluster shape. The single-cluster service `strata` and its companion `strata-features` are removed. The remaining `strata-multi` is renamed to `strata` and always starts. `ceph-b` becomes a regular dependency (no `multi-cluster` profile gate). `make up` / `make up-all` produce the multi-cluster stack with no flag.

Single-cluster lab is no longer a supported deploy shape. Operators wanting to test against a single RADOS cluster set `STRATA_RADOS_CLUSTERS=default:/etc/ceph-a/...` (drop the `cephb:` entry) — env-driven, not topology-driven. Multi-cluster is the realistic prod shape; the duality was operator cognitive bloat.

Boot-time coherence check, `STRATA_CLUSTER_ENV_STRICT` env, GC orphan side-channel, and operator console GC-orphan view are NOT in scope — they all addressed symptoms of the parallel-strata race that no longer exists.

## Goals

- Lab has exactly one strata service. Container name `strata` (preserves operator muscle memory).
- Bare `docker compose up -d` brings up cassandra + ceph + ceph-b + strata (multi-cluster shape).
- `strata` env knows both `default` + `cephb` clusters by default; operator overrides via `STRATA_RADOS_CLUSTERS` if a single-cluster smoke is desired.
- Port 9999 (existing single-cluster port) bound on the surviving strata service — operator muscle memory + Makefile + smoke scripts continue to address `:9999`.
- All references to `strata-multi`, `strata-features`, `:9998`, `--profile multi-cluster`, `--profile features` removed from compose / Makefile / scripts / docs / CI.
- The `features` worker set (notify, replicator, access-log, inventory, audit-export) folds into the single strata's `STRATA_WORKERS` knob; default keeps `gc,lifecycle,rebalance`; operator extends via env if needed.

## User Journey (pre-cycle walkthrough)

Happy path — fresh clone, bare bring-up:

1. Operator clones repo, `docker compose pull`.
2. Operator runs `make up && make wait-cassandra && make wait-ceph`.
3. Compose starts cassandra, ceph, ceph-b, strata (in order via depends_on). `strata-multi` container does NOT exist — already removed from compose.
4. `strata` boots, reconciles `cluster_state` rows for `default` + `cephb` (both `live` per existing reconcile logic when bucket_stats references them, else `pending`).
5. Gateway listens on `:9999`. Admin console at `http://localhost:9999/`.
6. Operator PUTs 100 objects via aws-cli. Chunks land on `default` (per weighted default routing; `cephb` weight 100 at boot since auto-reconciled).
7. Drain workflow for `cephb`: `POST /admin/v1/clusters/cephb/drain {mode:evacuate}` → rebalance worker migrates chunks → GC worker (one and only one, no race) drains queue → `deregister_ready=true` within minutes.

Happy path — single-cluster smoke (override):

1. Operator runs `STRATA_RADOS_CLUSTERS=default:/etc/ceph-a/ceph.conf:/etc/ceph-a/ceph.client.admin.keyring make up`.
2. ceph-b service still starts (compose dep), but strata only attaches to `default`.
3. cluster_state row for `cephb` is NOT created (boot reconcile uses local env as source of truth for initial seed); if a stale `cephb` row exists from prior run, it remains. Operator-tolerated: no new behavior change.

Edge case — Makefile / smoke scripts still address `:9998`:

1. Old scripts that hardcoded `:9998` against `strata-multi` get swept to `:9999` in US-002 docs sweep.

Negative path — operator runs `--profile features`:

1. `--profile features` no longer matches any service. `docker compose --profile features up -d` is a no-op for that profile (only cassandra + ceph + ceph-b + strata start since they have no profile gate).
2. No error, no warning. Documented as removed in `docs/site/content/architecture/migrations/compose-collapse.md`.

## User Stories

### US-001: Collapse `strata-multi` → single `strata` service; drop legacy single-cluster service

**Description:** As an operator, I want exactly one strata service in compose so worker leases cannot race between parallel containers.

**Acceptance Criteria:**
- [ ] `deploy/docker/docker-compose.yml`: remove existing `strata` service block (single-cluster, port 9999, mounts only `strata-ceph-etc`). Remove `strata-features` service block. Rename `strata-multi` block to `strata`; change `container_name: strata-multi` → `container_name: strata`; change port mapping `9998:9000` → `9999:9000`; remove `profiles: ["multi-cluster"]` line so the service always starts.
- [ ] `ceph-b` service: remove its `profiles: ["multi-cluster"]` line so it always starts (now a regular dependency of `strata`).
- [ ] `strata-ceph-b-etc` volume + the `ceph-b-quickstart` init container (if any) — keep, drop profile gate.
- [ ] Default `STRATA_WORKERS` env on the surviving strata: `${STRATA_WORKERS:-gc,lifecycle,rebalance}` (preserves multi-cluster default; rebalance is meaningful with two clusters).
- [ ] Operator can extend workers via env: `STRATA_WORKERS=gc,lifecycle,rebalance,notify,replicator,access-log,inventory,audit-export make up` — no service rename, no profile flag.
- [ ] Comment block above the `strata` service rewritten: single canonical service; multi-cluster is the default; operator overrides cluster set via `STRATA_RADOS_CLUSTERS`.
- [ ] `docker compose config` (no profile flag) lists `cassandra, ceph, ceph-b, strata` and nothing else.
- [ ] `make up` (which runs `docker compose up -d cassandra`) keeps working.
- [ ] `make up-all` (which runs `docker compose up -d`) brings up the full multi-cluster stack.
- [ ] `go vet ./...` passes (no Go changes; cycle CI gate)
- [ ] `make build` passes

### US-002: Sweep docs + scripts + Makefile + CI references to single-strata shape

**Description:** As a future-maintainer, I need every reference to `strata-multi`, `:9998`, `--profile multi-cluster`, `--profile features`, and the legacy single-cluster `strata` service updated so the repo reads consistently.

**Acceptance Criteria:**
- [ ] `Makefile`: drop / rename any target referencing `strata-multi` or `--profile multi-cluster` (likely `make smoke-multi-cluster`, `make up-multi-cluster` if present); rewrite to use the bare `strata` service. Drop `strata-features`-targeting targets if present.
- [ ] Scripts under `scripts/`: replace `strata-multi` container name + `:9998` URL with `strata` + `:9999`; drop `--profile multi-cluster` / `--profile features` arguments to `docker compose`. Concrete files known to touch: `scripts/smoke-effective-placement.sh`, `scripts/smoke-drain-transparency.sh`, `scripts/smoke-cluster-weights.sh`, `scripts/multi-replica-smoke.sh`, `scripts/bench-rebalance-multi.sh` — and any new from-this-cycle additions. Use a grep gate at end of sweep to assert zero residue.
- [ ] `docs/site/content/**/*.md`: replace operator-facing references to `strata-multi` / `:9998` / `--profile multi-cluster` / `strata-features` / `--profile features` with the single-strata shape. Concrete files known to touch via prior grep: `docs/site/content/best-practices/placement-rebalance.md`, `docs/site/content/best-practices/s3-multi-cluster.md`, `docs/site/content/architecture/observability.md`, `docs/site/content/architecture/migrations/*.md`. Add a new migration note `docs/site/content/architecture/migrations/compose-collapse.md` summarising the deletion + the env-driven override for single-cluster smokes.
- [ ] `.github/workflows/*.yml`: drop `--profile multi-cluster` / `--profile features` flags; ensure CI integration jobs hit `:9999` on `strata`.
- [ ] Project root `README.md`: refresh the "Quick start" + "Compose profiles" sections — drop multi-cluster as a separate profile; single-cluster smoke documented as `STRATA_RADOS_CLUSTERS=...` env override.
- [ ] `CLAUDE.md` (root project memory): update the "Cluster state machine" + "Background workers" sections wherever they reference the dual-service shape. Add one-line note that multi-cluster is the canonical compose shape; single-cluster is env-override only.
- [ ] Grep gate at end of US-002 (codified in the smoke script of US-003): zero matches for `strata-multi` / `:9998` / `--profile multi-cluster` / `--profile features` outside `scripts/ralph/archive/**`, `docs/site/public/**`, `docs/site/resources/**`, `progress.txt` / `prd.json`, the smoke script itself, and ROADMAP close-flip narratives that NARRATE the migration.
- [ ] `make docs-build` succeeds (Hugo renders without dangling links)
- [ ] `make vet`, `make test`, `pnpm run build` all green
- [ ] `go vet ./...` passes
- [ ] Typecheck passes

### US-003: Add `lab-cassandra-3` profile — 3-replica Cassandra-backed lab + nginx LB

**Description:** As a developer, I want a 3-replica Cassandra-backed lab that mirrors the existing `lab-tikv-3` shape so multi-replica behavior of the collapsed strata service is exercisable end-to-end.

**Acceptance Criteria:**
- [ ] `deploy/docker/docker-compose.yml`: add three new services `strata-cass-a` / `strata-cass-b` / `strata-cass-c`, all behind `profiles: ["lab-cassandra-3"]`, mirroring the `strata-tikv-{a,b,c}` shape from `lab-tikv-3` exactly — same image, same env (with `STRATA_META_BACKEND=cassandra` + `STRATA_CASSANDRA_HOSTS=cassandra:9042`), distinct `STRATA_NODE_ID=strata-cass-{a,b,c}`, distinct host ports `10001:9000` / `10002:9000` / `10003:9000`, shared `strata-ceph-etc` + `strata-ceph-b-etc` volumes + shared `strata.toml`, same healthcheck shape.
- [ ] All three replicas attach to BOTH `default` + `cephb` RADOS clusters via the canonical `STRATA_RADOS_CLUSTERS` env (same value as the bare `strata` service).
- [ ] Default workers: `STRATA_WORKERS: ${STRATA_WORKERS:-gc,lifecycle,rebalance}`. Default shard envs `STRATA_GC_SHARDS=3`, `STRATA_LIFECYCLE_SHARDS=3`, `STRATA_REBALANCE_SHARDS=3` set on each replica so all three worker fan-outs distribute across the 3 replicas. (Operator can override via host env.)
- [ ] New `strata-lb-nginx-cass` service behind `profiles: ["lab-cassandra-3"]`, fronts the three lab replicas on host port `10000:80`; config at `deploy/nginx/strata-lab-cassandra.conf` (round-robin upstream of `strata-cass-a:9000`, `strata-cass-b:9000`, `strata-cass-c:9000`).
- [ ] New `make up-lab-cassandra-3` Makefile target: `docker compose --profile lab-cassandra-3 up -d`. **Important**: the bare `strata` service has no profile gate and also starts under this invocation — operator is expected to either `docker compose stop strata` first or accept that the lab has 4 replicas total (3 lab + 1 default). Smoke harness in US-004 explicitly stops `strata` so the lab is isolated.
- [ ] Comment block above the new services explains the operator pattern: stop default `strata` before running lab harness (the lab shape is "3 replicas + LB on :10000"; default strata at :9999 stays online unless explicitly stopped).
- [ ] `docker compose --profile lab-cassandra-3 config` lists: cassandra, ceph, ceph-b, strata (default, unconditional), strata-cass-a, strata-cass-b, strata-cass-c, strata-lb-nginx-cass.
- [ ] `make build` passes
- [ ] `go vet ./...` passes
- [ ] Typecheck passes (no Go change in this story; cycle CI gate)

### US-004: Smoke + docs migration note + ROADMAP close-flip + PRD removal

**Description:** As an operator and as a future-maintainer, I need an end-to-end smoke proving the collapsed shape + the new multi-replica Cassandra lab work + documented context for the deletion + the ROADMAP entry flipped to Done.

**Acceptance Criteria:**
- [ ] New `scripts/smoke-compose-collapse.sh` covers four scenarios with `echo "==> Scenario X: ..."` headers:
  - **Scenario A** (bare bring-up): `docker compose up -d`; assert services running: cassandra, ceph, ceph-b, strata; assert `strata-multi` + `strata-features` containers do NOT exist (`docker compose ps` filter); PUT 10 objects via `:9999`; assert all GETs round-trip; drain `cephb` evacuate; assert `deregister_ready=true` within 5 min.
  - **Scenario B** (single-cluster env override): `STRATA_RADOS_CLUSTERS=default:... docker compose up -d strata`; assert strata attaches only to `default`; PUT + GET round-trip green; verify gateway log contains exactly one cluster connection entry.
  - **Scenario C** (lab-cassandra-3 multi-replica): `docker compose stop strata && docker compose --profile lab-cassandra-3 up -d`; assert `strata-cass-a/b/c` + `strata-lb-nginx-cass` running, `strata` stopped; PUT 30 objects via LB at `:10000`; inspect `worker_locks` table in Cassandra and assert at least 2 of 3 replicas hold at least one `gc-leader-N` / `rebalance-leader-N` lease (replicas distributed); drain `cephb` evacuate; assert `deregister_ready=true` within 5 min.
  - **Scenario D** (residue grep): run the grep gate from US-002 — assert zero residue across the codebase outside the documented exception set.
- [ ] `make smoke-compose-collapse` Makefile target wraps the script; exits non-zero on any failure.
- [ ] `docs/site/content/architecture/migrations/compose-collapse.md` (new) documents: what was removed (`strata-multi`, `strata-features`, single-cluster `strata`, `multi-cluster` profile, `features` profile, port 9998); how to do a single-cluster smoke now (env override); the new `lab-cassandra-3` profile (mirror of `lab-tikv-3`) for multi-replica testing; link to the closing ROADMAP entry.
- [ ] `docs/site/content/best-practices/placement-rebalance.md` "Multi-leader scaling" subsection (added by rebalance-scale-phase-2 cycle) gains a one-line cross-link to the new `lab-cassandra-3` profile.
- [ ] Linked from `docs/site/content/architecture/migrations/_index.md`.
- [ ] `ROADMAP.md` close-flip: the P2 entry → `~~**P2 — Compose `default` + `multi-cluster` profiles race for worker leases on shared Cassandra.**~~ — **Done.** <one-line summary referencing the compose collapse + env-driven single-cluster smoke + new lab-cassandra-3 profile + migration note>. (commit \`<pending>\`)`; closing SHA backfilled on main in a follow-up commit.
- [ ] `tasks/prd-compose-profile-isolation.md` REMOVED in the same commit (CLAUDE.md PRD lifecycle rule).
- [ ] `make docs-build`, `make vet`, `make test`, `pnpm run build`, `make smoke-compose-collapse` all green.

## Functional Requirements

- FR-1: Exactly one `strata` service in compose; no `strata-multi`, no `strata-features`.
- FR-2: `strata` always starts on `docker compose up -d` (no profile gate).
- FR-3: `strata` env attaches to BOTH `default` + `cephb` RADOS clusters by default; operator overrides via `STRATA_RADOS_CLUSTERS`.
- FR-4: Port `9999` preserved (operator muscle memory + scripts).
- FR-5: All references to removed shapes swept from Makefile / scripts / docs / CI / README / CLAUDE.md.
- FR-6: Migration note explains the deletion + single-cluster smoke via env override.
- FR-7: Smoke harness covers bare bring-up + single-cluster env override + residue grep.

## State Truth Tables

### Compose service start matrix (after collapse)

| Invocation | cassandra | ceph | ceph-b | strata | strata-multi | strata-features |
|---|---|---|---|---|---|---|
| `docker compose up -d` | ✓ | ✓ | ✓ | ✓ | n/a (removed) | n/a (removed) |
| `docker compose up -d strata` | (dep) | (dep) | (dep) | ✓ | n/a | n/a |
| `docker compose --profile multi-cluster up -d` | ✓ | ✓ | ✓ | ✓ | n/a | n/a |
| `docker compose --profile features up -d` | ✓ | ✓ | ✓ | ✓ | n/a | n/a |
| `docker compose --profile tikv up -d` | (off) | (off) | (off) | (off) | n/a | n/a |

Last row reflects the existing `tikv` profile-driven shape (TiKV + strata-tikv replace cassandra + strata — separate concern, not touched by this cycle).

### `cluster_state` set on first boot

| `STRATA_RADOS_CLUSTERS` env | Boot reconcile creates rows |
|---|---|
| `default:...,cephb:...` (compose default) | `default` (live, weight=100), `cephb` (live, weight=100) |
| `default:...` (env override) | `default` (live, weight=100). Stale `cephb` row from prior run remains as-is. |
| (empty) | None (existing fail-loud config validation already errors out). |

## Cache Invalidation Ledger

- No new caches.
- Existing `placement.DrainCache` (30 s TTL, invalidated by admin drain/undrain) unaffected.
- Existing TanStack queries (`clusters`, `drain-progress`, etc.) unaffected.

## Safety Claims Preconditions

- **Claim: worker leases cannot race between parallel strata instances.**
  - Precondition: there is only one strata service in the canonical compose stack. **Test**: Scenario A asserts `strata-multi` + `strata-features` containers do NOT exist.
- **Claim: drain workflow against `cephb` no longer wedges.**
  - Precondition: the surviving strata's gc worker has connections to both `default` + `cephb`, so every GC entry resolves regardless of which cluster the chunk lives on. **Test**: Scenario A asserts `deregister_ready=true` within 5 min after drain.
- **Claim: single-cluster smoke still possible without re-introducing dual-service shape.**
  - Precondition: `STRATA_RADOS_CLUSTERS` env override drops `cephb` at strata launch. **Test**: Scenario B asserts gateway log has exactly one cluster connection.

## Downstream Consumer Grep

Verified affected paths (search performed before drafting):

- `strata-multi` — touches `deploy/docker/docker-compose.yml`, `docs/site/content/**`, `scripts/**`, `Makefile`, `.github/workflows/**`, `README.md`, `CLAUDE.md`. US-002 sweeps all.
- `:9998` — touches scripts + docs.
- `--profile multi-cluster` / `--profile features` — touches Makefile + scripts + CI + docs.
- `strata-features` — touches compose + docs.

Surviving paths (intentionally retained):

- `--profile tikv` / `--profile lab-tikv` / `--profile lab-tikv-3` / `--profile tracing` — infrastructure variants (TiKV backend, multi-replica lab, OTel collector). Not removed; unrelated to the cluster-topology bug.

Endpoints / metrics — none introduced or modified in this cycle. Pure compose + docs cleanup.

## Worst-Case Thought Exercise

What if an operator has a checkout that predates this cycle and runs the smoke script from a prior cycle that addresses `:9998`?

- Pre-cycle smoke scripts no longer exist after US-002 sweep. If operator has a local stash with old scripts, they hit `:9998` and get connection refused. Documented in migration note + ROADMAP close-flip.

What if an existing prod deploy uses the `strata-multi` container name in their wrapper scripts?

- Compose is for the lab, not for prod. Real prod uses k8s / nomad with their own service names. No prod operator depends on the lab compose service name. Migration note explicitly states this is a lab compose change.

What if an operator wants the `features` worker subset (notify / replicator / access-log / inventory / audit-export)?

- Set `STRATA_WORKERS=gc,lifecycle,rebalance,notify,replicator,access-log,inventory,audit-export` on the surviving strata service. Same supervisor, same leader-election; just more registered workers. The two-strata split (`strata` + `strata-features`) was an artifact of compose-service-per-role, not a worker-supervisor requirement.

What if multiple replicas of `strata` are needed (HA lab)?

- `docker compose up -d --scale strata=3` already works (workers leader-elect on per-shard leases; gc and lifecycle already fan out; rebalance fan-out shipped in the prior cycle). Out of scope here; just noted as supported.

What if Cassandra retains a stale `strata-multi` row in `cluster_state` from a prior run?

- `cluster_state` rows are keyed by cluster id (`default` / `cephb`), not service name. No `strata-multi` row exists in `cluster_state`. No cleanup needed.

## Non-Goals

- Per-cluster lease scoping (`gc-leader-<cluster>-N` etc.) — would be the structural fix if multi-strata were ever reintroduced. Add P3 ROADMAP entry **"Scope worker leases per-cluster"** as a follow-up (the orphan-side-channel design from the original PRD draft moves there too).
- GC orphan side-channel + admin endpoints + UI — not needed; root cause removed.
- Boot-time `cluster_state`-vs-env coherence check + `STRATA_CLUSTER_ENV_STRICT` env — not needed; only one strata, env is the single source of truth.
- Single-cluster as a maintained deploy shape — operators wanting it use `STRATA_RADOS_CLUSTERS` env override at runtime, not a separate compose service.
- Touching `tikv` / `lab-tikv` / `lab-tikv-3` / `tracing` infra profiles — out of scope.
- Touching the `strata-tikv` service (TiKV-backed gateway under `tikv` profile) — same shape concern conceptually but driven by different code path; not in scope.

## Open Questions

- **Resource footprint**: `ceph-b` always-on adds ~1 GB RAM + a few hundred MB disk vs the prior bare-`up -d` baseline. Acceptable for the lab; documented in the migration note. (Decision: accept.)
- **CI minutes**: integration runners now spin up ceph-b alongside ceph. Adds ~30 s to integration setup. Acceptable. (Decision: accept.)
- **Migration of operator wrapper scripts (if any)**: out of scope — lab compose isn't load-bearing for prod.

## Technical Considerations

- Compose semantics: services without a `profiles:` key always start. The collapsed `strata` therefore needs no profile.
- The `multi-cluster` profile flag becomes a no-op (no service references it). Compose silently ignores unknown profiles; no error. Operators with muscle memory of `--profile multi-cluster` continue to work without surprise — except they get less than they expected (no extra service to start because there's only one).
- The `features` profile flag also becomes a no-op for the same reason.
- `strata-ceph-etc` + `strata-ceph-b-etc` volumes are now both populated unconditionally (both ceph + ceph-b always start). No volume cleanup needed.
- The surviving strata's `STRATA_RADOS_CLUSTERS` env points to BOTH `default` + `cephb` keyrings by default; operator override is a single env var swap.
- `cluster_reconcile.go` boot logic is unchanged — it already handles "env says default + cephb, table empty" by creating both rows as `pending` or `live` per `bucket_stats` reference.
- ROADMAP close-flip narrative DOES retain `strata-multi` mentions (it NARRATES the migration). That sits outside the grep gate scope (ROADMAP.md is excluded).

## Success Metrics

- `make smoke-compose-collapse` passes end-to-end.
- Grep gate at end of US-002 reports zero residue.
- `docker compose ps` after `docker compose up -d` shows exactly 4 services (cassandra, ceph, ceph-b, strata).
- Drain `cephb` evacuate completes within 5 min on the smoke load (10 objects).
