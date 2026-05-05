# PRD: Multi-replica Strata cluster (lab profile)

## Introduction

Strata's gateway is stateless — replicas don't form quorum among themselves; storage layer (TiKV / RADOS) provides durability + consistency. A single replica is a single point of failure for HTTP traffic; running ≥2 replicas behind a load balancer is the minimum HA shape.

This PRD adds a **`lab-tikv` compose profile** that spins up **two TiKV-backed Strata replicas** (`strata-tikv-a`, `strata-tikv-b`) behind an **nginx upstream**, sharing a JWT secret via a **docker volume** so console sessions survive replica swaps. Acceptance is proven by a **Playwright `multi-replica.spec.ts`** that exercises three failure scenarios:

1. Cluster Overview shows 2 healthy nodes; kill one → drops to 1 within heartbeat TTL (30 s).
2. PUT object via replica A, GET via replica B succeeds (cross-replica consistency through shared TiKV/RADOS).
3. Worker leader rotation: kill the replica holding `lifecycle-leader` lease → other replica picks up within `leader.DefaultTTL`.

Backend choice is **TiKV-only** for this lab — the recently-shipped TiKV heartbeat backend (commit `c37487b`) is the modern stack we want operators to validate. Cassandra HA is already proven by the existing `up-all` profile.

## Goals

- Run **2 strata-tikv replicas** behind a **single nginx LB** at host port 9000 (or 9999 for parity with `make smoke`).
- **Sticky JWT secret** shared via docker volume — sessions survive replica restarts.
- **Cluster Overview UI** lists both replicas, with `workers` chips per node (only one carries `lifecycle-leader` / `gc-leader` chip at any time — the elected leader).
- **Worker leader rotation** under failure: kill leader → other replica acquires within ~30 s.
- **Playwright multi-replica.spec.ts** covers cluster overview, cross-replica PUT/GET, and worker rotation.
- **`docs/multi-replica.md`** operator guide explaining the model, replica count, JWT secret distribution, LB wiring.
- **ROADMAP P3 entry** added and flipped to Done at cycle close.

## User Stories

### US-001: lab-tikv compose profile with 2 strata replicas
**Description:** As a developer, I want a `lab-tikv` compose profile that brings up 2 TiKV-backed strata replicas (`strata-tikv-a`, `strata-tikv-b`) so the multi-replica scenario is reproducible from one command.

**Acceptance Criteria:**
- [ ] `deploy/docker/docker-compose.yml` adds two services `strata-tikv-a` + `strata-tikv-b` under profile `lab-tikv`. Both extend the existing `strata-tikv` shape (image, env, depends_on pd+tikv+ceph) but pin distinct host ports (9001 + 9002) and distinct `STRATA_NODE_ID` (`strata-a` / `strata-b`).
- [ ] Profile activated via `docker compose --profile lab-tikv up -d pd tikv ceph strata-tikv-a strata-tikv-b prometheus grafana`.
- [ ] New Makefile target `up-lab-tikv` mirrors `up-tikv` but uses the lab profile.
- [ ] Both replicas pass `/readyz` after pd+tikv+ceph healthy.
- [ ] Typecheck passes (no Go changes; verify compose YAML lint).
- [ ] Tests pass

### US-002: nginx LB container at port 9999
**Description:** As an operator, I want a single host port (9999) routing requests to both replicas so clients have one URL regardless of replica count.

**Acceptance Criteria:**
- [ ] New compose service `strata-lb-nginx` under profile `lab-tikv`, image `nginx:1.27-alpine`, host port 9999 → container 80.
- [ ] `deploy/nginx/strata-lab.conf` (new) defines an `upstream strata { server strata-tikv-a:9000; server strata-tikv-b:9000; }` with `least_conn` and a passive `max_fails=2 fail_timeout=10s` health-check (active checks need nginx-plus; passive is good enough for lab).
- [ ] `proxy_pass http://strata` block forwards `/`, preserving `Host`, `X-Forwarded-For`, `X-Forwarded-Proto`. Long timeouts (`proxy_read_timeout 300s`) so multipart uploads do not get clipped.
- [ ] `aws --endpoint-url http://127.0.0.1:9999` succeeds against the LB (reaches one of the two replicas).
- [ ] Typecheck passes
- [ ] Tests pass — assert config file loads via `nginx -t -c /etc/nginx/conf.d/strata-lab.conf` in CI shell test

### US-003: shared JWT secret volume + bootstrap
**Description:** As an operator, I want the console JWT secret to be shared between replicas so a session cookie issued by replica A is accepted by replica B.

**Acceptance Criteria:**
- [ ] New named volume `strata-jwt-secret` in `docker-compose.yml`, mounted into both replicas at `/etc/strata/jwt-shared` (read-write).
- [ ] Strata startup logic in `internal/serverapp/serverapp.go::loadJWTSecret` extends to consult `/etc/strata/jwt-shared/secret` BEFORE the existing ephemeral fallback. If file missing or empty, the FIRST replica acquires `internal/leader.Session` lock `jwt-secret-bootstrap`, generates 32 random bytes, writes the file (mode 0600), releases lock. Other replicas read the now-existing file.
- [ ] Race-safe: two replicas starting at the same time both call the lock; only one writes; the other reads. Single test in `internal/serverapp/serverapp_test.go` exercises the dual-startup flow against a mocked locker.
- [ ] `STRATA_CONSOLE_JWT_SECRET` env still wins when set (existing precedence preserved).
- [ ] Login on `http://127.0.0.1:9999` (LB), then refresh — session sticky regardless of which replica handles the refresh.
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: lab smoke script for multi-replica scenarios
**Description:** As a developer, I want one shell script that drives the three acceptance scenarios end-to-end so I can validate the lab without Playwright.

**Acceptance Criteria:**
- [ ] New `scripts/multi-replica-smoke.sh` runs against `make up-lab-tikv` stack. Drives:
      (a) curl /admin/v1/cluster/nodes via LB → assert 2 nodes, both healthy
      (b) docker stop strata-tikv-a; sleep 35; curl again → assert 1 node healthy + 1 stale (or 1 node total since heartbeat row evicted)
      (c) docker start strata-tikv-a; sleep 30; curl again → assert 2 nodes healthy
      (d) PUT object via replica-a (host 9001 direct), GET via replica-b (host 9002 direct) — assert byte-equal
      (e) docker logs lifecycle-leader holder → kill that container → wait 35s → assert other replica's logs show "leader acquired worker=lifecycle"
- [ ] Exit code 0 on all green; non-zero with descriptive message on any fail.
- [ ] `make smoke-lab-tikv` Makefile target invokes the script.
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: Playwright multi-replica.spec.ts e2e
**Description:** As a maintainer, I want Playwright e2e for multi-replica failure scenarios so regressions surface in CI.

**Acceptance Criteria:**
- [ ] New `web/e2e/multi-replica.spec.ts` with three tests, all running against the lab-tikv stack (CI workflow brings it up via `make up-lab-tikv && make wait-strata-tikv`):
      - `cluster-overview-shows-2-nodes`: login → Cluster Overview → assert table has 2 rows, both badges `healthy`.
      - `cross-replica-put-get`: login → upload small file via console (per-part presign hits whichever replica nginx picks) → reload → assert object visible in bucket detail (which may go through the OTHER replica).
      - `worker-rotation`: login → Cluster Overview → identify which replica holds `lifecycle-leader` chip (text match) → `docker.compose.kill(thatReplica)` via test fixture → wait 35s → reload Cluster Overview → assert OTHER replica now carries the chip.
- [ ] CI workflow `.github/workflows/ci.yml` `e2e-ui` job adds `pnpm exec playwright test multi-replica.spec.ts` after the existing critical-path / admin / debug specs.
- [ ] Test fixture exposes `await dockerStop('strata-tikv-a')` / `dockerStart()` helpers — wraps `child_process.execFile('docker', ...)` so test logic stays in TS.
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill (manual replay of the three scenarios)

### US-006: docs/multi-replica.md + ROADMAP close-flip + cycle-end merge to main
**Description:** As an operator, I want a documented runbook for the multi-replica lab AND the ROADMAP entry flipped so the cycle is fully closed.

**Acceptance Criteria:**
- [ ] New `docs/multi-replica.md` covers: why replicas (HA, no quorum among gateways themselves); minimum count (1 = SPOF, 2 = HA, 3+ optional); JWT secret distribution model; LB wiring; expected behaviour under each failure scenario; how `internal/leader.Session` rotates worker leadership.
- [ ] ROADMAP gets a new entry under `Web UI` or `Developer experience` section: `~~**P3 — Multi-replica lab (TiKV).**~~ — **Done.** <one-line summary>. (commit \`<sha>\`)` — close-flip format per CLAUDE.md.
- [ ] `docs/ui.md` Capability Matrix gets a row referencing the multi-replica scenario for Cluster Overview.
- [ ] **Cycle-end merge**: fast-forward / squash-merge `ralph/multi-replica-cluster` into `main`, push origin/main, archive `scripts/ralph/prd.json` + `scripts/ralph/progress.txt` under `scripts/ralph/archive/2026-MM-DD-multi-replica-cluster/`. Mirror the web-ui-debug close shape (`6519036` / `db9e251`).
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- **FR-1**: Compose `lab-tikv` profile spins up 2 TiKV-backed strata replicas with distinct `STRATA_NODE_ID` env values.
- **FR-2**: nginx LB at host port 9999 routes traffic round-robin / least-conn to both replicas; passive health check evicts dead upstream within `fail_timeout`.
- **FR-3**: Both replicas mount `/etc/strata/jwt-shared` from a shared docker volume; first replica to start writes 32 random bytes, others read.
- **FR-4**: `internal/heartbeat` (already shipped — memory + cassandra + TiKV impls in lockstep) reports both replicas under `/admin/v1/cluster/nodes`.
- **FR-5**: Worker leader-election (`internal/leader.Session`) ensures only one replica holds each worker lease (`lifecycle-leader`, `gc-leader`); on leader replica failure, lease expires within `leader.DefaultTTL` (~30 s) and the surviving replica acquires.
- **FR-6**: Cluster Overview UI shows both replicas with their respective `workers` chips and `leader_for` chips. The `leader_for` chip set MUST update within polling interval (5 s) after lease rotation.
- **FR-7**: Cross-replica consistency: PUT object via replica A is immediately visible via GET on replica B — guaranteed by TiKV transaction commit + RADOS chunk write being shared state, not replica-local.
- **FR-8**: Playwright `multi-replica.spec.ts` covers cluster overview, cross-replica consistency, and worker rotation in CI.

## Non-Goals

- **No quorum among strata gateway replicas themselves.** They are stateless; storage layer (TiKV PD/TiKV ≥3, RADOS mon/OSD ≥3) provides durability quorum. Document this in `docs/multi-replica.md` so operators don't expect strata replica voting.
- **No active LB health checks.** nginx-plus is paid; HAProxy adds a service. Passive `max_fails`/`fail_timeout` is good enough for lab. Production may swap.
- **No automatic JWT secret rotation across replicas.** US-019 of Phase 2 web-ui-admin already lets the operator rotate via `/admin/v1/settings/jwt-secret` — the rotation invalidates all sessions, which is acceptable for a planned rotation. Multi-replica auto-rotation is a future P3.
- **No auto-scaling.** Replica count is fixed at 2 for this lab; `docker compose --scale strata-tikv-a=N` is not a goal.
- **No multi-region / multi-cluster.** Single TiKV cluster, single Ceph cluster, single host. Cross-cluster replication is out of scope.
- **No Cassandra-backed multi-replica lab.** TiKV-only this cycle; Cassandra HA is already validated by `up-all`.
- **No global DNS / TLS.** nginx serves plain HTTP at 9999; operators bring their own reverse proxy / cert in production.

## Design Considerations

- **Replica naming**: `strata-tikv-a` and `strata-tikv-b` — letters not numbers because numbered (`-1`, `-2`) reads like docker-compose autoscale instances.
- **Host port assignment**: 9001 (replica-a direct), 9002 (replica-b direct), 9999 (LB). Direct ports useful for the smoke script (PUT via replica A, GET via replica B); LB port for "real" client experience.
- **JWT shared file path**: `/etc/strata/jwt-shared/secret` — separate from the existing `/etc/strata/jwt-secret` (single-replica file fallback). Distinct path so existing single-replica deployments don't accidentally pick up the multi-replica shape.
- **nginx config path**: `deploy/nginx/strata-lab.conf` mounted at `/etc/nginx/conf.d/default.conf`.
- **Cluster Overview UI**: no changes needed for this PRD — it already reads `/admin/v1/cluster/nodes` and renders all returned rows. The TiKV heartbeat shipped in `c37487b` populates them.
- **Worker leader chips**: already shown in NodeDetailDrawer (Phase 3 US-011). For a "leader_for" badge in the Cluster Overview row, see the heartbeat `LeaderFor` gap noted earlier — small follow-up, not blocking this PRD.

## Technical Considerations

- **`internal/serverapp.loadJWTSecret`**: extend to read `/etc/strata/jwt-shared/secret` before the ephemeral fallback. Wrap the bootstrap step in a `leader.Session.Acquire("jwt-secret-bootstrap", 30*time.Second)` call so two replicas starting simultaneously don't race the file write. Bootstrap step uses `os.OpenFile(O_WRONLY|O_CREATE|O_EXCL)` so the second replica's attempt fails cleanly when the first's write already completed.
- **`STRATA_NODE_ID` collision**: today defaults to hostname (container ID first 12 chars). Setting it explicitly in compose env (`STRATA_NODE_ID=strata-a`) gives stable identity across container recreates so the heartbeat row remains the same — important for "kill one, see N-1" assertion.
- **nginx + multipart upload**: per-part PUT URLs are presigned with absolute Host (the original `r.Host` value when minted). The browser uploads to `http://127.0.0.1:9999/<bucket>/<key>?...`. nginx forwards to upstream preserving the URL — gateway re-validates SigV4 against the original Host (`9999`). No special configuration needed; verified during US-004 smoke pass.
- **Playwright docker control**: tests invoke `docker stop` / `docker start` via `execFile`. Requires the test runner to have docker socket access. CI workflow already has docker-in-docker for the e2e job; no extra wiring.
- **TiKV consistency**: PUT/GET cross-replica works because both replicas share the same TiKV cluster (PD endpoint = `pd:2379` on both). Object metadata + RADOS chunk pointers are persisted at PUT commit time; GET on the other replica reads the same row.
- **No CassandraStore changes**: the heartbeat infra already handles multi-writer (one row per node_id, TTL-based eviction).

## Success Metrics

- `make up-lab-tikv && make wait-strata-tikv && make smoke-lab-tikv` is green end-to-end on first try.
- Playwright `multi-replica.spec.ts` runs in <90 s on CI (chromium-only) without flaky retries.
- Operator guide `docs/multi-replica.md` is enough to bring up the same shape on a fresh checkout without reading source.
- Worker leader rotation lag is bounded by `leader.DefaultTTL` (~30 s); validated by the smoke script's stopwatch.

## Open Questions

- **`leader_for` heartbeat chip gap**: today the heartbeat row carries `Workers []string` but `LeaderFor []string` is always empty (no hook from `leader.Session` into the heartbeat writer). The Cluster Overview will show `Workers=[gc, lifecycle]` on both replicas but `LeaderFor=[]` on both — which is misleading, since exactly one carries each leader at any time. Fix scope: add a hook in `cmd/strata/server.go` that flips the heartbeat row's `LeaderFor` when the supervisor lease state changes. Decision: ship as a separate small follow-up commit (not blocking this PRD), but the Playwright `worker-rotation` test depends on the chip being accurate — do the chip fix as part of US-005's prep work (effectively folded into the test setup).
- **JWT bootstrap lock backend**: `leader.Session` exists for cassandra + tikv + memory. The bootstrap path runs BEFORE the worker supervisor wires the locker. Need to confirm the locker is buildable at JWT-bootstrap time — likely yes since `buildLocker` only needs `metaStore` which is built before. Validate at story-start of US-003.
- **nginx least-conn vs round-robin**: round-robin is fine for stateless gateways; least-conn helps when a long upload monopolises one replica. Decision at story-start of US-002 — pick least-conn unless it complicates the config.
