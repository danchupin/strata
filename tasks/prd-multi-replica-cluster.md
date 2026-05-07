# PRD: Multi-replica Strata cluster (lab profile)

## Introduction

Strata's gateway is stateless — replicas don't form quorum among themselves; the storage layer (TiKV / RADOS) provides durability + consistency. A single replica is a single point of failure for HTTP traffic; running ≥2 replicas behind a load balancer is the minimum HA shape.

This PRD adds a **`lab-tikv` compose profile** that spins up **two TiKV-backed Strata replicas** (`strata-tikv-a`, `strata-tikv-b`) behind an **nginx upstream**, sharing a JWT secret via a **docker volume** so console sessions survive replica swaps. Acceptance is proven by:

1. **`scripts/multi-replica-smoke.sh`** drives the three failure scenarios end-to-end without Playwright (host-side smoke; default validation path).
2. **Playwright `multi-replica.spec.ts`** runs the same scenarios in a CI job gated by `[multi-replica]` in the PR title (heavyweight: docker-in-docker, ceph image pull, ~5 min stack boot — too expensive for every PR but valuable on the merge train and as a manual trigger).

Backend choice is **TiKV-only** for this lab — the recently-shipped TiKV heartbeat backend (commit `c37487b`) is the modern stack we want operators to validate. Cassandra HA is already proven by the existing `up-all` profile.

## Goals

- Run **2 strata-tikv replicas** behind a **single nginx LB** at host port 9999.
- **Sticky JWT secret** shared via docker volume — sessions survive replica restarts.
- **`leader_for` chip** on Cluster Overview reflects the actual lease holder per worker — exactly one replica carries each `lifecycle-leader` / `gc-leader` chip at any time.
- **Worker leader rotation** under failure: kill leader → other replica acquires within `leader.DefaultTTL` (~30 s).
- **`scripts/multi-replica-smoke.sh`** covers cluster overview, cross-replica PUT/GET, and worker rotation.
- **Playwright `multi-replica.spec.ts`** mirrors the smoke scenarios in a `[multi-replica]`-gated CI job.
- **`docs/multi-replica.md`** operator guide explaining the model, replica count, JWT secret distribution, LB wiring.
- **ROADMAP P3 entry** added and flipped to Done at cycle close.

## User Stories

### US-001: Wire Supervisor lease state to Heartbeater (`leader_for` chip)
**Description:** As an operator, I want the Cluster Overview's `leader_for` chip to reflect the actual lease holder per worker so that worker-rotation in a multi-replica deployment is observable from the console.

**Acceptance Criteria:**
- [ ] `cmd/strata/workers.Supervisor` exposes a notification channel / callback that emits `(workerName string, acquired bool)` on every lease state transition (acquire on `runOnce` enter, release on `runOnce` exit). Channel is buffered (cap 8) so a transient stall in the consumer never blocks supervision.
- [ ] `internal/heartbeat.Heartbeater` exposes `SetLeaderFor(worker string, owned bool)` that mutates `Heartbeater.Node.LeaderFor` under a mutex. Next `write` tick picks up the new slice.
- [ ] `internal/serverapp.runApp` wires the channel: `for evt := range supervisor.LeaderEvents() { hb.SetLeaderFor(evt.Worker, evt.Acquired) }` in a goroutine cancelled on shutdown.
- [ ] Unit test in `cmd/strata/workers/supervisor_test.go` covers a single-worker run that acquires + releases + verifies events fire in order.
- [ ] Unit test in `internal/heartbeat/heartbeat_test.go` covers `SetLeaderFor` adding + removing entries; `Run` picks up the change at the next tick.
- [ ] ROADMAP gets a new entry under `## Web UI`: `~~**P3 — Heartbeat 'leader_for' chip wired to actual lease state.**~~ — **Done.** ...` (close-flip happens in this same commit per CLAUDE.md "Discovering a new gap" + "Closing a roadmap item" rules).
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: lab-tikv compose profile with 2 strata replicas
**Description:** As a developer, I want a `lab-tikv` compose profile that brings up 2 TiKV-backed strata replicas (`strata-tikv-a`, `strata-tikv-b`) so the multi-replica scenario is reproducible from one command.

**Acceptance Criteria:**
- [ ] `deploy/docker/docker-compose.yml` adds two services `strata-tikv-a` + `strata-tikv-b` under profile `lab-tikv`. Both extend the existing `strata-tikv` shape (image, env, depends_on pd+tikv+ceph) but pin distinct host ports (9001 + 9002) and distinct `STRATA_NODE_ID` (`strata-a` / `strata-b`).
- [ ] Both replicas inherit the same `STRATA_STATIC_CREDENTIALS`, `STRATA_AUTH_MODE`, `STRATA_TIKV_PD_ENDPOINTS`, `STRATA_WORKERS` from the host env (so SigV4 stays valid across replicas — different creds across replicas would 403 cross-replica requests).
- [ ] Profile activated via `docker compose --profile lab-tikv up -d pd tikv ceph strata-tikv-a strata-tikv-b strata-lb-nginx prometheus grafana` (single command).
- [ ] New Makefile target `up-lab-tikv` mirrors `up-tikv` but uses the lab profile + LB.
- [ ] New Makefile target `wait-strata-lab` polls `http://127.0.0.1:9999/readyz` (the LB) until 200, plus polls `http://127.0.0.1:9001/readyz` and `http://127.0.0.1:9002/readyz` for direct-replica readiness.
- [ ] Both replicas pass `/readyz` after pd+tikv+ceph healthy.
- [ ] Typecheck passes (no Go changes; verify compose YAML syntax with `docker compose --profile lab-tikv config -q`).
- [ ] Tests pass

### US-003: nginx LB container at port 9999
**Description:** As an operator, I want a single host port (9999) routing requests to both replicas so clients have one URL regardless of replica count.

**Acceptance Criteria:**
- [ ] New compose service `strata-lb-nginx` under profile `lab-tikv`, image `nginx:1.27-alpine`, host port 9999 → container 80.
- [ ] `deploy/nginx/strata-lab.conf` (new) defines an `upstream strata { least_conn; server strata-tikv-a:9000; server strata-tikv-b:9000 max_fails=2 fail_timeout=10s; ... }` (least_conn — long uploads shouldn't pin one replica).
- [ ] `proxy_pass http://strata` block forwards `/`, preserves `Host`, `X-Forwarded-For`, `X-Forwarded-Proto`. **Streaming-friendly settings (REQUIRED for multipart uploads not to clip):** `proxy_request_buffering off`, `proxy_buffering off`, `client_max_body_size 0`, `proxy_read_timeout 300s`, `proxy_send_timeout 300s`, `proxy_http_version 1.1`.
- [ ] `aws --endpoint-url http://127.0.0.1:9999` succeeds against the LB (reaches one of the two replicas).
- [ ] CI shell test asserts config file syntax via `docker run --rm -v ./deploy/nginx/strata-lab.conf:/etc/nginx/conf.d/default.conf nginx:1.27-alpine nginx -t`.
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: shared JWT secret via atomic file bootstrap
**Description:** As an operator, I want the console JWT secret to be shared between replicas so a session cookie issued by replica A is accepted by replica B.

**Acceptance Criteria:**
- [ ] New named docker volume `strata-jwt-shared` in `docker-compose.yml`, mounted into both replicas at `/etc/strata/jwt-shared` (read-write, 0700 perms).
- [ ] `internal/serverapp.loadJWTSecret` extends to consult `/etc/strata/jwt-shared/secret` BEFORE the existing `STRATA_JWT_SECRET_FILE` fallback. Precedence: `STRATA_CONSOLE_JWT_SECRET` env > `STRATA_JWT_SECRET_FILE` > `/etc/strata/jwt-shared/secret` (NEW) > ephemeral generated secret.
- [ ] **Bootstrap is file-based, not lock-based** (avoids the chicken-and-egg of needing the locker before serverapp wires it): if the file is missing, the replica calls `os.OpenFile(path, O_WRONLY|O_CREATE|O_EXCL, 0600)`. Exactly one writer wins per POSIX; the loser's `O_EXCL` returns `EEXIST` and it falls through to a read of the now-existing file. Re-read up to 3 times with a 100 ms backoff to absorb the race window.
- [ ] Single test in `internal/serverapp/jwt_secret_test.go` (new file) exercises:
      - Empty dir, single replica → file written + 32 bytes returned.
      - Empty dir, two concurrent calls → one writes, one reads; both return identical 32 bytes.
      - Pre-existing file (other content) → file content returned verbatim.
- [ ] `STRATA_CONSOLE_JWT_SECRET` env still wins when set (existing precedence preserved). When env is set, the shared-file path is NOT touched (avoids surprising file write on first boot of an env-configured replica).
- [ ] Login on `http://127.0.0.1:9999` (LB), then refresh — session sticky regardless of which replica handles the refresh.
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: lab smoke script for multi-replica scenarios
**Description:** As a developer, I want one shell script that drives the three acceptance scenarios end-to-end so I can validate the lab without Playwright.

**Acceptance Criteria:**
- [ ] New `scripts/multi-replica-smoke.sh` runs against `make up-lab-tikv` stack. Drives:
      (a) `curl http://127.0.0.1:9999/admin/v1/cluster/nodes` (after login) → assert 2 nodes, both `healthy`, exactly one carries `lifecycle-leader` chip and exactly one carries `gc-leader` chip (chips are now wired per US-001).
      (b) `docker stop strata-tikv-a` → sleep 35 → `curl ...` → assert 1 node remains healthy + the other surviving replica carries BOTH leader chips now.
      (c) `docker start strata-tikv-a` → sleep 30 → `curl ...` → assert 2 nodes healthy again.
      (d) PUT object via replica-a (host 9001 direct), GET via replica-b (host 9002 direct) → assert byte-equal payload.
      (e) Identify which replica holds `lifecycle-leader` → kill that container → wait 35 s → assert OTHER replica's `/admin/v1/cluster/nodes` row now carries `lifecycle-leader` chip.
- [ ] Login flow at the script start: POST `/admin/v1/auth/login` with `STRATA_STATIC_CREDENTIALS` access-key, capture the session cookie, reuse for the rest of the curls.
- [ ] Exit code 0 on all green; non-zero with a descriptive `echo "FAIL: <scenario>"` line on any failure.
- [ ] `make smoke-lab-tikv` Makefile target invokes the script.
- [ ] Typecheck passes
- [ ] Tests pass (the script itself is the test; CI gates it on `[multi-replica]` in PR title — see US-006 for the wiring rationale)

### US-006: Playwright multi-replica.spec.ts e2e (gated CI job)
**Description:** As a maintainer, I want Playwright e2e for multi-replica failure scenarios so regressions surface in CI when the lab profile changes — but gated so the default fast e2e job stays under 5 min.

**Acceptance Criteria:**
- [ ] New `web/e2e/multi-replica.spec.ts` with three tests:
      - `cluster-overview-shows-2-nodes`: login → Cluster Overview → assert table has 2 rows, both badges `healthy`.
      - `cross-replica-put-get`: login → upload small file via console (per-part presign through nginx, may hit either replica) → reload → assert object visible in Bucket Detail (which may go through the other replica).
      - `worker-rotation`: login → Cluster Overview → identify which replica holds `lifecycle-leader` chip (text match on the row badge) → `await dockerStop('strata-tikv-a' | 'strata-tikv-b')` via test fixture → wait 35 s → reload Cluster Overview → assert OTHER replica now carries the chip.
- [ ] `web/e2e/fixtures/docker.ts` (new) exports `dockerStop(name) / dockerStart(name)` wrapping `child_process.execFile('docker', ['stop'|'start', name])`. No mocking — tests assume real docker.
- [ ] **NEW dedicated CI job `e2e-ui-multi-replica`** in `.github/workflows/ci.yml`:
      - Triggered ONLY when PR title contains `[multi-replica]` OR via `workflow_dispatch` (manual trigger).
      - Runs on `ubuntu-latest`.
      - Steps: checkout → setup-go → setup-node → pnpm install → playwright install → `make up-lab-tikv` → `make wait-strata-lab` → `pnpm exec playwright test multi-replica.spec.ts` → `make down`.
      - Timeout: 15 min total (5 min stack boot + 5 min spec + 5 min slack).
      - Uploads `playwright-report/` artifact on failure.
- [ ] **No change to the existing `e2e-ui` job** — multi-replica.spec.ts is excluded from the default invocation list. (`testIgnore: '**/multi-replica.spec.ts'` on the default Playwright project, and a separate project / CLI flag for the gated job.)
- [ ] Multi-replica spec uses a separate `playwright.config.ts` invocation that points `webServer` at the existing docker-compose stack (no `webServer` block — instead `baseURL: http://127.0.0.1:9999`).
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill (manual replay of the three scenarios)

### US-007: docs/multi-replica.md + ROADMAP close-flip + cycle-end merge
**Description:** As an operator, I want a documented runbook for the multi-replica lab AND the ROADMAP entry flipped so the cycle is fully closed.

**Acceptance Criteria:**
- [ ] New `docs/multi-replica.md` covers: why replicas (HA, no quorum among gateways themselves); minimum count (1 = SPOF, 2 = HA, 3+ optional); JWT secret distribution model (file-based atomic bootstrap, env override); LB wiring; expected behaviour under each failure scenario; how `internal/leader.Session` rotates worker leadership; how `Heartbeater.SetLeaderFor` (US-001) reflects rotation in the UI within the next heartbeat tick (max ~10 s lag).
- [ ] ROADMAP gets a new entry: `~~**P3 — Multi-replica lab (TiKV).**~~ — **Done.** <one-line summary>. (commit \`<sha>\`)` under `## Web UI` or `## Developer experience` — close-flip format per CLAUDE.md "Closing a roadmap item" rule, in the same commit as the work.
- [ ] `docs/ui.md` Capability Matrix gets a row referencing the multi-replica scenario for Cluster Overview.
- [ ] **Cycle-end merge**: fast-forward merge `ralph/multi-replica-cluster` into `main`, push `origin/main`. Mirror the **web-ui-storage-status close shape (`9d839fc`)**: ff-only merge + single follow-up archive commit on main. (DO NOT use the older "merge commit + separate archive commit" shape from `web-ui-debug` — current pattern is ff-only.)
- [ ] Markdown PRD `tasks/prd-multi-replica-cluster.md` is REMOVED in the close-flip commit per CLAUDE.md PRD lifecycle rule (canonical record is the ralph snapshot).
- [ ] `archive_cycle` in `scripts/ralph/ralph.sh` snapshots `prd.json + progress.txt` under `scripts/ralph/archive/2026-MM-DD-multi-replica-cluster-complete/` on `<promise>COMPLETE</promise>`. The archive folder is committed in a follow-up `ralph: archive ...` commit on main.
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- **FR-1**: Compose `lab-tikv` profile spins up 2 TiKV-backed strata replicas with distinct `STRATA_NODE_ID` env values, identical `STRATA_STATIC_CREDENTIALS`, identical TiKV/PD endpoints.
- **FR-2**: nginx LB at host port 9999 routes traffic least-conn to both replicas; passive `max_fails=2 fail_timeout=10s` evicts dead upstream within ~10 s. Streaming bypass settings (`proxy_request_buffering off`, `client_max_body_size 0`) ensure multipart uploads are not clipped.
- **FR-3**: Both replicas mount `/etc/strata/jwt-shared` from a shared docker volume; first replica to start writes 32 random bytes via `O_CREATE|O_EXCL` atomic primitive; others read the existing file.
- **FR-4**: `internal/heartbeat.Heartbeater` reflects worker lease state via `SetLeaderFor(worker, owned)` driven by a `workers.Supervisor.LeaderEvents()` channel; the next `/admin/v1/cluster/nodes` poll picks up the change within `heartbeat.DefaultInterval` (~10 s).
- **FR-5**: `internal/leader.Session` ensures only one replica holds each worker lease (`lifecycle-leader`, `gc-leader`); on leader replica failure, lease expires within `leader.DefaultTTL` (~30 s) and the surviving replica acquires.
- **FR-6**: Cluster Overview UI shows both replicas with their respective `workers` chips and `leader_for` chips. The `leader_for` chip set updates within the next polling interval (5 s) after lease rotation completes.
- **FR-7**: Cross-replica consistency: PUT object via replica A is immediately visible via GET on replica B — guaranteed by TiKV transaction commit + RADOS chunk write being shared state, not replica-local.
- **FR-8**: Playwright `multi-replica.spec.ts` covers cluster overview, cross-replica consistency, and worker rotation — runs in a dedicated `e2e-ui-multi-replica` CI job gated by `[multi-replica]` PR title or `workflow_dispatch`.

## Non-Goals

- **No quorum among strata gateway replicas themselves.** They are stateless; storage layer (TiKV PD/TiKV ≥3, RADOS mon/OSD ≥3) provides durability quorum. Documented in `docs/multi-replica.md` so operators don't expect strata replica voting.
- **No active LB health checks.** nginx-plus is paid; HAProxy adds a service. Passive `max_fails`/`fail_timeout` is good enough for lab. Production may swap.
- **No automatic JWT secret rotation across replicas.** US-019 of Phase 2 web-ui-admin already lets the operator rotate via `/admin/v1/settings/jwt-secret` — the rotation invalidates all sessions, which is acceptable for a planned rotation. Multi-replica auto-rotation is a future P3.
- **No auto-scaling.** Replica count is fixed at 2 for this lab; `docker compose --scale` is not a goal.
- **No multi-region / multi-cluster.** Single TiKV cluster, single Ceph cluster, single host. Cross-cluster replication is out of scope.
- **No Cassandra-backed multi-replica lab.** TiKV-only this cycle; Cassandra HA is already validated by `up-all`.
- **No global DNS / TLS.** nginx serves plain HTTP at 9999; operators bring their own reverse proxy / cert in production.
- **No multi-replica e2e in the default `e2e-ui` CI job.** Cost is too high (5 min docker-compose boot, ceph image pull) for every PR. Gated job covers regression detection on the merge train and via manual `workflow_dispatch`.

## Design Considerations

- **Replica naming**: `strata-tikv-a` and `strata-tikv-b` — letters not numbers because numbered (`-1`, `-2`) reads like docker-compose autoscale instances.
- **Host port assignment**: 9001 (replica-a direct), 9002 (replica-b direct), 9999 (LB). Direct ports useful for the smoke script (PUT via replica A, GET via replica B); LB port for "real" client experience.
- **JWT shared file path**: `/etc/strata/jwt-shared/secret` — separate from the existing `/etc/strata/jwt-secret` (single-replica file fallback). Distinct path so existing single-replica deployments don't accidentally pick up the multi-replica shape.
- **nginx config path**: `deploy/nginx/strata-lab.conf` mounted at `/etc/nginx/conf.d/default.conf`.
- **Cluster Overview UI**: no changes needed for this PRD — it already reads `/admin/v1/cluster/nodes` and renders all returned rows including `leader_for`. The TiKV heartbeat shipped in `c37487b` populates them; US-001 wires the chip to actual lease state.
- **Worker leader chips**: already shown in NodeDetailDrawer (Phase 3 US-011) and Cluster Overview row (`web/src/pages/Overview.tsx:403`). US-001 makes them accurate.

## Technical Considerations

- **`internal/serverapp.loadJWTSecret`** — extends to read `/etc/strata/jwt-shared/secret` after the env + `STRATA_JWT_SECRET_FILE` paths. Bootstrap step uses `os.OpenFile(path, O_WRONLY|O_CREATE|O_EXCL, 0600)` so the second replica's attempt fails cleanly with `EEXIST`. On `EEXIST`, retry-read with a 100 ms backoff up to 3 times to absorb the write→fsync race window. **NO `leader.Session` involved** — the locker isn't built before serverapp wires it, and atomic file primitives are sufficient.
- **`STRATA_NODE_ID` collision** — defaults to hostname (container ID first 12 chars). Setting it explicitly in compose env (`STRATA_NODE_ID=strata-a`) gives stable identity across container recreates so the heartbeat row remains the same — important for "kill one, see N-1" assertion.
- **nginx + multipart upload** — per-part PUT URLs are presigned with absolute Host (the original `r.Host` value when minted). The browser uploads to `http://127.0.0.1:9999/<bucket>/<key>?...`. nginx forwards to upstream preserving the URL — gateway re-validates SigV4 against the original Host (`9999`). For streaming bodies (multipart UploadPart hot path), set `proxy_request_buffering off` so nginx pipes the body chunked to upstream rather than buffering the whole part to disk.
- **Playwright docker control** — tests invoke `docker stop` / `docker start` via `execFile`. Requires the test runner to have docker socket access. The new `e2e-ui-multi-replica` CI job runs on `ubuntu-latest` with the docker socket mounted by default (no docker-in-docker; just talks to the host docker daemon).
- **TiKV consistency** — PUT/GET cross-replica works because both replicas share the same TiKV cluster (PD endpoint = `pd:2379` on both). Object metadata + RADOS chunk pointers are persisted at PUT commit time; GET on the other replica reads the same row.
- **No CassandraStore changes** — heartbeat infra already handles multi-writer (one row per node_id, TTL-based eviction).
- **Supervisor → Heartbeater channel** (US-001) — buffered cap 8 channel; if consumer stalls (e.g. deadlocks), supervisor never blocks. On supervisor shutdown the channel is closed; the consumer goroutine in `serverapp.runApp` exits via the closed-channel signal.
- **Streaming-aware nginx settings** — `client_max_body_size 0` (disable upload size cap; AWS S3 spec says individual PUT can be 5 GiB, multipart parts up to 5 GiB each; relying on the gateway's own enforcement). `proxy_buffering off` for response streaming on GET. `proxy_http_version 1.1` to enable persistent connections + chunked encoding.

## Success Metrics

- `make up-lab-tikv && make wait-strata-lab && make smoke-lab-tikv` is green end-to-end on first try.
- Playwright `multi-replica.spec.ts` runs in <5 min on CI (chromium-only, gated job) without flaky retries.
- Operator guide `docs/multi-replica.md` is enough to bring up the same shape on a fresh checkout without reading source.
- Worker leader rotation lag is bounded by `leader.DefaultTTL` (~30 s) for the lease handover + `heartbeat.DefaultInterval` (~10 s) for the chip refresh = ~40 s end-to-end; validated by the smoke script's stopwatch.
- `leader_for` chip is correct in steady state (within ~10 s of any lease change).

## Open Questions — RESOLVED before cycle launch

- **`leader_for` heartbeat chip gap** — RESOLVED: extracted as US-001, its OWN story. Adds Supervisor → Heartbeater channel + `SetLeaderFor` mutator. Closes a real gap (the chip ships in the UI today but is always empty) — that's significant enough to warrant a dedicated story rather than folding into the smoke/Playwright stories.
- **JWT bootstrap mechanism** — RESOLVED: file-based atomic `O_CREATE|O_EXCL`, no `leader.Session`. Avoids the chicken-and-egg (locker isn't built before serverapp's loadJWTSecret runs) and is race-safe per POSIX. 3-retry read-back loop absorbs the fsync race window.
- **e2e CI cadence** — RESOLVED: dedicated `e2e-ui-multi-replica` job gated by `[multi-replica]` in PR title or `workflow_dispatch`. Default `e2e-ui` job stays fast (<5 min); regressions on the lab profile detected via gated runs.
- **nginx least-conn vs round-robin** — RESOLVED: least_conn. Long uploads do not pin one replica, and nginx's least_conn includes idle-connection bookkeeping that round-robin lacks.
- **nginx streaming settings** — RESOLVED: `proxy_request_buffering off`, `proxy_buffering off`, `client_max_body_size 0`, `proxy_read_timeout 300s`, `proxy_send_timeout 300s`, `proxy_http_version 1.1`. Without these, multipart UploadPart on >100 MiB parts gets buffered to nginx temp dir and either fills disk or times out.
