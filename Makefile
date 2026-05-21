SHELL := bash
COMPOSE := docker compose -f deploy/docker/docker-compose.yml

.PHONY: build build-ceph docker-build web-build web-typecheck web-clean vet test \
	up up-all up-cassandra up-all-ci up-bench-rgw down \
	dev dev-down dev-logs \
	wait-cassandra wait-ceph wait-pd wait-tikv wait-strata wait-strata-a wait-strata-b wait-strata-lb-nginx wait-strata-lab wait-rgw \
	ceph-pool run-memory run-cassandra run-strata run-gateway \
	smoke smoke-signed smoke-grafana smoke-lab-tikv \
	smoke-drain-lifecycle smoke-drain-transparency smoke-drain-progress-ui smoke-cluster-weights \
	smoke-drain-cleanup smoke-drain-followup smoke-effective-placement smoke-rebalance-scale \
	smoke-single-binary smoke-tikv-default-lab \
	race-soak race-soak-tikv lint-nginx-lab helm-lint \
	bench-gc bench-lifecycle bench-gc-multi bench-lifecycle-multi bench-rebalance-multi \
	docs-serve docs-build docs-openapi-copy clean

GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# build depends on web-build so the embedded console FS is populated
# before `go build` runs. Direct `go build` for cmd/strata without web-build
# will fail with: pattern web/dist: no matching files found
build: web-build
	go build -o bin/strata ./cmd/strata
	go build -o bin/strata-racecheck ./cmd/strata-racecheck

web-build:
	cd web && pnpm install --frozen-lockfile && pnpm run build

web-typecheck:
	cd web && pnpm run typecheck

web-clean:
	rm -rf web/dist web/node_modules

# Build the strata image used by every gateway service (strata-a, strata-b,
# strata-cassandra all share the same build target). Single service name
# suffices — `docker compose build` reuses the image across services.
build-ceph:
	$(COMPOSE) build strata-a

docker-build:
	docker build \
		--build-arg GIT_SHA=$(GIT_SHA) \
		-f deploy/docker/Dockerfile \
		-t strata:ceph \
		-t strata:$(GIT_SHA) \
		.

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

test-verbose:
	go test -v ./...

test-integration:
	go test -tags integration -timeout 10m ./...

# Run RADOS integration tests inside a container that already has librados
# (reuses the Dockerfile build stage). Assumes `make up-all` is running.
test-rados:
	docker build --target build -f deploy/docker/Dockerfile -t strata:test .
	docker run --rm --network docker_default \
		-v docker_strata-ceph-etc:/etc/ceph:ro \
		-e STRATA_TEST_CEPH_CONF=/etc/ceph/ceph.conf \
		-e STRATA_TEST_CEPH_POOL=strata.rgw.buckets.data \
		strata:test go test -tags "ceph integration" -timeout 10m ./internal/data/rados/

# Bare bring-up: TiKV-default 2-replica lab (pd + tikv + ceph + ceph-b +
# strata-a + strata-b + strata-lb-nginx + prometheus + grafana). The nginx
# LB sits on host port 9999; direct replica probes on 10001 + 10002.
# Cassandra is profile-gated — use `make up-cassandra` to additionally
# layer the Cassandra-backed regression lab on host port 9998.
up:
	$(COMPOSE) up -d

# Alias for `make up` — preserved for historical muscle memory. Brings up
# the TiKV-default lab; no profile flags, no service list.
up-all:
	$(COMPOSE) up -d

# Layer the Cassandra-backed regression lab on top of the bare default.
# Adds `cassandra` (metadata) + `strata-cassandra` (gateway on host port
# 9998) under `--profile cassandra`. Cassandra meta backend remains
# first-class in code (internal/meta/cassandra/** + test-integration
# testcontainers preserved); this target only flips the lab compose shape.
up-cassandra:
	$(COMPOSE) --profile cassandra up -d

# CI-trimmed Cassandra-backed stack for the nightly race-soak workflow
# (US-005). Layers the `docker-compose.ci.yml` override on top of the base
# file: caps Ceph's memstore + osd_memory_target at 1 GiB, raises Cassandra
# heap to 2G/400M, disables the Ceph mgr dashboard module, and skips
# Prometheus + Grafana (gated behind the `full` profile in the override).
# Race-soak driver (scripts/racecheck/run.sh) targets the Cassandra-backed
# gateway, so `--profile cassandra` brings up cassandra + strata-cassandra.
# `--profile ci` activates the override's CI-specific knobs.
up-all-ci:
	docker compose -f deploy/docker/docker-compose.yml -f deploy/docker/docker-compose.ci.yml --profile ci --profile cassandra up -d cassandra ceph strata-cassandra

# Layer the RGW comparison target on the bare-default stack. Standalone
# `ceph/ceph:v19.2.3` container that joins the existing ceph-a cluster via
# the shared `strata-ceph-etc` volume; bootstraps minimal realm/zonegroup/
# zone (default/default/default) + bench S3 user. Host port 9991:8080.
# US-001 of ralph/rgw-benchmarks. Operator-run-only (NOT in CI matrix).
up-bench-rgw:
	$(COMPOSE) --profile bench-rgw up -d rgw

# Tear down the full stack — every profile-gated service included so
# explicit-profile bring-ups (cassandra / tracing / webhook-trap / ci /
# bench-rgw) clean up too. Retired profile names (`tikv`, `lab-tikv*`,
# `lab-cassandra-3`) are no-ops on this compose file; dropping them avoids
# stale flags.
down:
	$(COMPOSE) --profile cassandra --profile tracing --profile webhook-trap --profile ci --profile bench-3replica --profile bench-rgw down

# Wait for the Cassandra container to report healthy. Cassandra is gated
# behind `--profile cassandra`, so the `ps` query includes the profile flag
# — without it, docker compose silently filters profile-gated containers
# from the output and the until-loop hangs forever.
wait-cassandra:
	@echo "waiting for cassandra to report healthy..."
	@until [ "$$($(COMPOSE) --profile cassandra ps --format '{{.Health}}' cassandra)" = "healthy" ]; do sleep 3; done
	@echo "cassandra ready"

# Wait for the nginx LB at host port 9999 to report ready. The LB fronts
# strata-a + strata-b under the TiKV-default lab — a 200 on /readyz means
# at least one replica is up + fans out probes (PD + TiKV + RADOS) cleanly.
wait-strata:
	@echo "waiting for strata-lb-nginx /readyz on 9999..."
	@until [ "$$(curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:9999/readyz)" = "200" ]; do sleep 2; done
	@echo "strata-lb-nginx ready"

wait-ceph:
	@echo "waiting for ceph to report healthy..."
	@until [ "$$($(COMPOSE) ps --format '{{.Health}}' ceph)" = "healthy" ]; do sleep 5; done
	@echo "ceph ready"

wait-pd:
	@echo "waiting for pd to report healthy..."
	@until [ "$$($(COMPOSE) ps --format '{{.Health}}' pd)" = "healthy" ]; do sleep 3; done
	@echo "pd ready"

# Wait for PD's HTTP health endpoint to return 200 on host port 2379.
# `/pd/api/v1/health` is the stable public-contract probe (vs `ps --format`
# which depends on the compose-level healthcheck firing first). Useful from
# CI workflows that bring up the bare-default stack and need a backend-
# readiness gate before driving smoke against the gateway.
wait-tikv:
	@echo "waiting for pd /pd/api/v1/health to return 200 on 2379..."
	@until [ "$$(curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:2379/pd/api/v1/health)" = "200" ]; do sleep 2; done
	@echo "pd ready"

# Granular replica-direct waits. strata-a binds host port 10001; strata-b
# binds 10002. Useful for tests that need to drive a specific replica
# (round-robin LB hides per-replica state).
wait-strata-a:
	@echo "waiting for strata-a /readyz on 10001..."
	@until [ "$$(curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:10001/readyz)" = "200" ]; do sleep 2; done
	@echo "strata-a ready"

wait-strata-b:
	@echo "waiting for strata-b /readyz on 10002..."
	@until [ "$$(curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:10002/readyz)" = "200" ]; do sleep 2; done
	@echo "strata-b ready"

wait-strata-lb-nginx:
	@echo "waiting for strata-lb-nginx /readyz on 9999..."
	@until [ "$$(curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:9999/readyz)" = "200" ]; do sleep 2; done
	@echo "strata-lb-nginx ready"

# Combined readiness gate for the TiKV-default lab: both replicas + LB.
wait-strata-lab: wait-strata-a wait-strata-b wait-strata-lb-nginx

# Wait for the bench RGW container to be serving HTTP on host port 9991.
# RGW root returns 403 (anonymous AccessDenied), so any 2xx/3xx/4xx confirms
# the beast frontend is up — 5xx / no response = not ready. 60s timeout.
wait-rgw:
	@echo "waiting for rgw on 9991..."
	@for i in $$(seq 1 60); do \
	  code=$$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:9991/ 2>/dev/null || echo 000); \
	  if [ "$$code" -ge 200 ] && [ "$$code" -lt 500 ] && [ "$$code" != "000" ]; then \
	    echo "rgw ready (HTTP $$code)"; \
	    exit 0; \
	  fi; \
	  sleep 1; \
	done; \
	echo "rgw not ready after 60s" >&2; exit 1

# One-command developer cluster: bring up the canonical TiKV-default lab,
# wait for PD + ceph + both strata replicas + LB to report ready, then
# stream the last 20 log lines per replica and the LB and follow live.
# Ctrl-C kills only the log stream — the compose stack stays up so the
# operator can re-attach via `make dev-logs` or drive smoke harnesses.
# Backend default is TiKV (matches CLAUDE.md `## Compose shape`). The
# Cassandra-backed regression lab stays under `make up-cassandra`.
dev: up-all wait-tikv wait-ceph wait-strata-lab
	$(COMPOSE) logs -f --tail=20 strata-a strata-b strata-lb-nginx

# Tear down the dev cluster. Mirrors `make down` — same profile cleanup,
# preserves named volumes by default. For a full data wipe run `docker
# compose -f deploy/docker/docker-compose.yml down -v` directly.
dev-down: down

# Re-attach to the dev cluster log stream after Ctrl-C without re-tailing
# the backlog. Use this between `make dev` and `make dev-down` to peek at
# live traffic. Stack must already be up (run `make dev` first).
dev-logs:
	$(COMPOSE) logs -f strata-a strata-b strata-lb-nginx

ceph-pool:
	docker exec strata-ceph ceph osd pool create strata.rgw.buckets.data 8 8 replicated || true
	docker exec strata-ceph ceph osd pool application enable strata.rgw.buckets.data rgw || true

run-memory: build
	STRATA_LISTEN=:9999 STRATA_META_BACKEND=memory STRATA_DATA_BACKEND=memory \
		./bin/strata server

# Dev-only path: runs the strata binary directly (no compose) against a
# host-local Cassandra instance. Independent of the lab compose flip — the
# new bare default is TiKV-backed via compose, but this target stays the
# Cassandra dev shortcut for running a single strata process from source.
run-cassandra: build
	STRATA_LISTEN=:9999 \
	STRATA_META_BACKEND=cassandra STRATA_DATA_BACKEND=memory \
	STRATA_CASSANDRA_HOSTS=127.0.0.1 STRATA_CASSANDRA_DC=datacenter1 \
	STRATA_WORKERS=gc,lifecycle \
		./bin/strata server

# Bring up the strata gateway replicas + nginx LB without (re-)starting
# pd/tikv/ceph/ceph-b. Assumes infra is already healthy.
run-strata:
	$(COMPOSE) up -d strata-a strata-b strata-lb-nginx

# Backwards-compatible alias for the old per-binary target name.
run-gateway: run-strata

smoke:
	bash scripts/smoke.sh http://127.0.0.1:9999

smoke-signed:
	bash scripts/smoke-signed.sh http://127.0.0.1:9999

smoke-grafana:
	bash scripts/grafana-smoke.sh

# Drive the multi-replica failure scenarios end-to-end against the
# TiKV-default bare-default stack (`make up && make wait-strata-lab`).
# Requires STRATA_STATIC_CREDENTIALS exported with the same value the
# gateway booted with (the first comma-separated entry's access:secret
# pair is used for the admin login + SigV4-signed cross-replica PUT/GET).
# See scripts/multi-replica-smoke.sh for scenario coverage.
smoke-lab-tikv:
	bash scripts/multi-replica-smoke.sh

# Drain-lifecycle walkthrough smoke (US-007 of ralph/drain-lifecycle).
# Drives the full 15-step operator journey + 4 negative paths against a
# running compose stack (`docker compose up -d` — multi-cluster is the
# default shape after the compose-collapse cycle). Skips with exit 77
# when the lab is not reachable; set REQUIRE_LAB=1 to convert the skip
# into a hard fail. See scripts/smoke-drain-lifecycle.sh for env knobs
# (BASE, SMOKE_DRAIN_*).
smoke-drain-lifecycle:
	bash scripts/smoke-drain-lifecycle.sh

# Drain-transparency walkthrough smoke (US-008 of ralph/drain-transparency).
# Drives the three operator scenarios (A: stop-writes drain, B: full evacuate
# with /drain-impact + bulk-fix, C: upgrade readonly → evacuate) against a
# running compose stack (`docker compose up -d` — multi-cluster is the
# default shape). Skips with exit 77 when the lab is not reachable;
# set REQUIRE_LAB=1 to convert the skip into a hard fail. See
# scripts/smoke-drain-transparency.sh for env knobs (BASE, SMOKE_DRAIN_*).
smoke-drain-transparency:
	bash scripts/smoke-drain-transparency.sh

# Drain-progress 3-state smoke (US-003 of ralph/drain-progress-physical).
# Recreates the bare `docker compose up -d` strata container with
# throttled env (STRATA_REBALANCE_RATE_MB_S=1 + STRATA_GC_GRACE=60s
# — smoke-only, NOT prod defaults) so all three drain-progress phases
# (Migrating / Awaiting GC / Ready to deregister) are observable
# within the 5-minute timeout. Seeds 300 ~1 MB objects on a split
# bucket {default:1,cephb:1}, drains cephb evacuate, polls
# /admin/v1/clusters/cephb/drain-progress every 3 s, asserts each
# state observed at least once. Restores prod-default env on the
# strata container on exit. Skips with exit 77 when the lab is not
# reachable; set REQUIRE_LAB=1 to convert the skip into a hard fail.
# Closes ROADMAP P3 "Drain progress UI shows manifest counts instead
# of physical chunks". See scripts/smoke-drain-progress-ui.sh for
# env knobs (BASE, SMOKE_DPU_*).
smoke-drain-progress-ui:
	bash scripts/smoke-drain-progress-ui.sh

# Cluster-weights walkthrough smoke (US-005 of ralph/cluster-weights).
# Drives the four operator scenarios (A: new-cluster activation pending →
# live + ramp, B: existing-live auto-detect at boot, C: bucket policy wins
# over cluster weights, D: pending excluded from default routing) against
# a running compose stack (`docker compose up -d` — multi-cluster is the
# default shape). Wipes `cluster_state` rows via cqlsh and restarts the
# strata container between scenarios to exercise the boot-time reconcile.
# Skips with exit 77 when the lab is not reachable; set REQUIRE_LAB=1 to
# convert the skip into a hard fail. See scripts/smoke-cluster-weights.sh
# for env knobs (BASE, SMOKE_CW_*).
smoke-cluster-weights:
	bash scripts/smoke-cluster-weights.sh

# Drain-cleanup walkthrough smoke (US-005 of ralph/drain-cleanup).
# Drives the 13-step operator journey closing the seven ROADMAP entries
# bundled in this cycle (drawer 3-category render, /drain-impact cache
# invalidation, Pools chunk_count rename, force-empty GC enqueue,
# deregister_ready hard-safety, state-aware buttons, trace browser list)
# against a running compose stack (`docker compose up -d` — multi-cluster
# is the default shape). Skips with exit 77 when the lab is not reachable;
# set REQUIRE_LAB=1 to convert the skip into a hard fail. See
# scripts/smoke-drain-cleanup.sh for env knobs (BASE, SMOKE_DC_*).
smoke-drain-cleanup:
	bash scripts/smoke-drain-cleanup.sh

# Drain follow-up walkthrough smoke (US-006 of ralph/drain-followup).
# Drives the 16-step operator journey closing the four ROADMAP entries
# bundled in this cycle (P3 trace browser filter/search, P2 UI confusion
# chip+button, P2 Cassandra multipart probe no-op, P3 ALLOW FILTERING
# denormalize) against a running compose stack (`docker compose up -d` —
# multi-cluster is the default shape). Skips with exit 77 when the lab
# is not reachable; set REQUIRE_LAB=1 to convert the skip into a hard
# fail. See scripts/smoke-drain-followup.sh for env knobs (BASE,
# SMOKE_DF_*).
smoke-drain-followup:
	bash scripts/smoke-drain-followup.sh

# Effective-placement walkthrough smoke (US-006 of ralph/effective-placement).
# Drives the four operator scenarios (A: weighted bucket auto-fallback via
# cluster.weights on drain, B: strict bucket blocks drain + 503 DrainRefused,
# C: flip strict→weighted clears stuck_single_policy via cache invalidation,
# D: all clusters drained → 503 with no fallback) against a running compose
# stack (`docker compose up -d` — multi-cluster is the default shape).
# Closes ROADMAP P2 "Effective-policy fallback to cluster weights".
# Skips with exit 77 when the lab is not reachable; set REQUIRE_LAB=1 to
# convert the skip into a hard fail. See scripts/smoke-effective-placement.sh
# for env knobs (BASE, SMOKE_EP_*).
smoke-effective-placement:
	bash scripts/smoke-effective-placement.sh

# Rebalance-scale Phase 2 walkthrough smoke (US-005 of
# ralph/rebalance-scale-phase-2). Drives scenarios against the bare-default
# stack (`docker compose up -d` — TiKV-default 2-replica with rebalance
# worker). The retired 3-replica lab-tikv-3 scenario is parked as a P3
# follow-up (see scripts/bench-rebalance-multi.sh skip handling). Closes
# ROADMAP P2 "Rebalance worker not sharded". Skips with exit 77 when
# the lab is not reachable; set REQUIRE_LAB=1 to convert the skip
# into a hard fail. See scripts/smoke-rebalance-scale.sh for env knobs
# (BASE, SMOKE_*).
smoke-rebalance-scale:
	bash scripts/smoke-rebalance-scale.sh

# TiKV-default 2-replica lab walkthrough smoke (US-005 of
# ralph/tikv-default-lab). Drives the four operator scenarios end-to-end
# against a running compose stack (`make up && make wait-strata-lab` —
# bare default is the TiKV-default lab). Scenario A asserts the bare
# service set + LB round-robin + PUT/GET round-trip + drain cephb
# evacuate convergence; Scenario B probes the Cassandra-profile lab on
# :9998 (skips if absent — bring up via `make up-cassandra`); Scenario C
# is opt-in via SMOKE_TDL_SCENARIO_C=1; Scenario D is the repo-wide
# residue grep gate (zero retired-profile / service-name matches outside
# the documented exception set). Skips with exit 77 when the lab is not
# reachable; set REQUIRE_LAB=1 to convert the skip into a hard fail. See
# scripts/smoke-tikv-default-lab.sh for env knobs (BASE, CASS_BASE,
# SMOKE_TDL_*).
smoke-tikv-default-lab:
	bash scripts/smoke-tikv-default-lab.sh

# Single-binary dispatcher smoke (US-002 of ralph/single-binary).
# Builds bin/strata and verifies post-consolidation shape: --help lists
# server+admin, admin --help lists 9 subcommands, admin rewrap --help
# prints rewrap usage, legacy bin/strata-admin gone, unknown subcommand
# exits 2, no `strata-admin` residue in scoped trees.
smoke-single-binary:
	bash scripts/smoke-single-binary.sh

# Race-soak driver (US-006). Brings up the Cassandra-backed stack
# (`make up-all-ci` when CI=true, else `make up-cassandra`), waits for
# /readyz on 9998 (strata-cassandra), then runs `bin/strata-racecheck`
# for RACE_DURATION (default 1h) at RACE_CONCURRENCY (default 32). The
# harness caps --concurrency at 64 and refuses to start above that.
#
# Captures pre/post `df -h /` + `free -m` into report/host.txt and per-
# container docker logs (strata-cassandra, strata-cassandra-db, strata-ceph)
# into report/. Exits with the harness's exit code (0 clean / 1 inconsistencies
# / 2 setup-failure) so the nightly workflow (US-007) can flip the badge
# distinctly per outcome.
#
# Override RACE_DURATION / RACE_CONCURRENCY / RACE_BUCKETS /
# RACE_KEYS_PER_BUCKET / RACE_ENDPOINT via env. STACK_UP=0 skips the stack
# bring-up so an operator can re-drive an already-running gateway.
race-soak: build
	bash scripts/racecheck/run.sh

# Race-soak the TiKV-backed gateway: brings up a PD + TiKV pair via
# testcontainers (or uses STRATA_TIKV_TEST_PD_ENDPOINTS for an
# operator-supplied cluster) and runs the integration race scenario
# (TestRaceMixedOpsTiKV) for RACE_DURATION (default 1h, set via the
# go test -timeout flag).
#
# Workload size scales with RACE_ITERS / RACE_WORKERS / RACE_KEYS env vars
# (see race_test.go envIntDefault). Defaults yield a quick sanity pass; the
# soak target raises RACE_ITERS to 100k so the scenario runs long enough to
# surface any concurrency divergence vs the Cassandra-backed run.
race-soak-tikv:
	RACE_ITERS=$${RACE_ITERS:-100000} \
	RACE_WORKERS=$${RACE_WORKERS:-32} \
	RACE_KEYS=$${RACE_KEYS:-4} \
	  go test -tags integration -timeout $${RACE_DURATION:-1h} \
	  -run '^TestRaceMixedOpsTiKV$$' ./internal/s3api/...

# Helm chart lint for the TiKV-only deploy/helm/strata/ chart. Degrades
# to a one-line hint + exit 0 when the helm binary is not installed so
# `make test` is not gated on the toolchain. Operators with helm on PATH
# get the full `helm lint` run.
helm-lint:
	@command -v helm > /dev/null || { echo "helm not installed — skip helm-lint (install: https://helm.sh/docs/intro/install/)"; exit 0; }
	helm lint deploy/helm/strata/

# Validate the nginx LB config used by the TiKV-default 2-replica lab.
# nginx -t resolves upstream hostnames at parse time, so the test container
# carries --add-host stubs for strata-a/b; the real names resolve via
# Docker's embedded DNS at runtime when the bare-default stack is up.
lint-nginx-lab:
	docker run --rm \
		--add-host=strata-a:127.0.0.1 \
		--add-host=strata-b:127.0.0.1 \
		-v $(CURDIR)/deploy/nginx/strata-lab.conf:/etc/nginx/conf.d/default.conf:ro \
		nginx:1.27-alpine nginx -t

# bench-gc / bench-lifecycle drive `strata admin` against the TiKV-default
# bare stack (TiKV meta + RADOS data) at five concurrency levels and tee
# one JSON line per level into bench-gc-results.jsonl / bench-lifecycle-
# results.jsonl. The gateway must already be up via `make up && make
# wait-strata-lab`; `strata admin` connects to TiKV directly (PD endpoints
# from the docker-compose default) so the bench bypasses the HTTP gateway
# and measures the worker's per-replica throughput cap.
#
# Override BENCH_GC_ENTRIES / BENCH_LC_OBJECTS / BENCH_CONCURRENCY_LEVELS for
# custom sweeps. STRATA_PROM_PUSHGATEWAY (optional) pushes a per-level gauge
# to a configured push gateway so Grafana can compare runs over time.
BENCH_GC_ENTRIES ?= 10000
BENCH_LC_OBJECTS ?= 10000
BENCH_CONCURRENCY_LEVELS ?= 1 4 16 64 256

bench-gc: build
	@rm -f bench-gc-results.jsonl
	@for c in $(BENCH_CONCURRENCY_LEVELS); do \
		echo "bench-gc concurrency=$$c entries=$(BENCH_GC_ENTRIES)" >&2; \
		STRATA_META_BACKEND=tikv STRATA_DATA_BACKEND=rados \
		STRATA_TIKV_PD_ENDPOINTS=$${STRATA_TIKV_PD_ENDPOINTS:-127.0.0.1:2379} \
		STRATA_RADOS_CONFIG_FILE=$${STRATA_RADOS_CONFIG_FILE:-/etc/ceph/ceph.conf} \
		STRATA_RADOS_POOL=$${STRATA_RADOS_POOL:-strata.rgw.buckets.data} \
		./bin/strata admin bench-gc --entries=$(BENCH_GC_ENTRIES) --concurrency=$$c \
		  | tee -a bench-gc-results.jsonl; \
	done

bench-lifecycle: build
	@rm -f bench-lifecycle-results.jsonl
	@for c in $(BENCH_CONCURRENCY_LEVELS); do \
		echo "bench-lifecycle concurrency=$$c objects=$(BENCH_LC_OBJECTS)" >&2; \
		STRATA_META_BACKEND=tikv STRATA_DATA_BACKEND=rados \
		STRATA_TIKV_PD_ENDPOINTS=$${STRATA_TIKV_PD_ENDPOINTS:-127.0.0.1:2379} \
		STRATA_RADOS_CONFIG_FILE=$${STRATA_RADOS_CONFIG_FILE:-/etc/ceph/ceph.conf} \
		STRATA_RADOS_POOL=$${STRATA_RADOS_POOL:-strata.rgw.buckets.data} \
		./bin/strata admin bench-lifecycle --objects=$(BENCH_LC_OBJECTS) --concurrency=$$c \
		  | tee -a bench-lifecycle-results.jsonl; \
	done

# Phase 2 multi-leader bench: drives the same TiKV+RADOS stack but layers
# `--shards` / `--replicas` on top so the bench measures the multi-leader
# fan-out curve. Default shards/replicas = 3 mirrors the recommended 3-replica
# deploy. Output JSONL goes to bench-{gc,lifecycle}-multi-results.jsonl.
BENCH_GC_SHARDS ?= 3
BENCH_LC_REPLICAS ?= 3
BENCH_LC_BUCKETS ?= 9

bench-gc-multi: build
	@rm -f bench-gc-multi-results.jsonl
	@for c in $(BENCH_CONCURRENCY_LEVELS); do \
		echo "bench-gc-multi shards=$(BENCH_GC_SHARDS) concurrency=$$c entries=$(BENCH_GC_ENTRIES)" >&2; \
		STRATA_META_BACKEND=tikv STRATA_DATA_BACKEND=rados \
		STRATA_TIKV_PD_ENDPOINTS=$${STRATA_TIKV_PD_ENDPOINTS:-127.0.0.1:2379} \
		STRATA_RADOS_CONFIG_FILE=$${STRATA_RADOS_CONFIG_FILE:-/etc/ceph/ceph.conf} \
		STRATA_RADOS_POOL=$${STRATA_RADOS_POOL:-strata.rgw.buckets.data} \
		./bin/strata admin bench-gc --entries=$(BENCH_GC_ENTRIES) --concurrency=$$c \
		  --shards=$(BENCH_GC_SHARDS) \
		  | tee -a bench-gc-multi-results.jsonl; \
	done

bench-lifecycle-multi: build
	@rm -f bench-lifecycle-multi-results.jsonl
	@for c in $(BENCH_CONCURRENCY_LEVELS); do \
		echo "bench-lifecycle-multi replicas=$(BENCH_LC_REPLICAS) concurrency=$$c objects=$(BENCH_LC_OBJECTS)" >&2; \
		STRATA_META_BACKEND=tikv STRATA_DATA_BACKEND=rados \
		STRATA_TIKV_PD_ENDPOINTS=$${STRATA_TIKV_PD_ENDPOINTS:-127.0.0.1:2379} \
		STRATA_RADOS_CONFIG_FILE=$${STRATA_RADOS_CONFIG_FILE:-/etc/ceph/ceph.conf} \
		STRATA_RADOS_POOL=$${STRATA_RADOS_POOL:-strata.rgw.buckets.data} \
		./bin/strata admin bench-lifecycle --objects=$(BENCH_LC_OBJECTS) --concurrency=$$c \
		  --replicas=$(BENCH_LC_REPLICAS) --buckets=$(BENCH_LC_BUCKETS) \
		  | tee -a bench-lifecycle-multi-results.jsonl; \
	done

# Rebalance worker multi-leader bench (US-004 of ralph/rebalance-scale-phase-2).
# Unlike bench-gc-multi / bench-lifecycle-multi (which drive `strata admin
# bench-*` against a single in-process simulation), this target drives the
# rebalance worker end-to-end through the HTTP admin API: seeds buckets,
# triggers /drain, polls /drain-progress until chunks_on_cluster=0, repeats
# at SHARDS=1 baseline and SHARDS=2 fan-out, and prints a ratio + verdict.
# The SHARDS=3 baseline is parked as a P3 follow-up (3-replica TiKV lab
# retired in ralph/tikv-default-lab).
#
# Requires a running multi-cluster TiKV-backed lab with the rebalance worker
# (operator-managed — see scripts/bench-rebalance-multi.sh header for the
# compose recipe and BENCH_RESTART_HOOK contract). Skip behaviour: exits 77
# if $BASE/readyz is not reachable; REQUIRE_LAB=1 turns the skip into a fail.
#
# Override BENCH_BUCKETS / BENCH_CHUNKS_PER_BUCKET / BENCH_OBJECT_SIZE_KB
# / BENCH_DRAIN_TIMEOUT_S / BENCH_SHARDS_BASELINE / BENCH_SHARDS_FANOUT via
# env. Results JSONL lands in bench-rebalance-multi-results.jsonl.
bench-rebalance-multi:
	bash scripts/bench-rebalance-multi.sh

# Hugo docs site (docs/site/). `docs-serve` runs the local dev preview on
# :1313 with drafts enabled; `docs-build` produces the minified static bundle
# under docs/site/public/ which the GitHub Actions workflow publishes to
# gh-pages. Requires Hugo extended (>= 0.128) on PATH; the theme lives at
# docs/site/themes/hugo-book/ as a Git submodule, so a fresh checkout needs
# `git submodule update --init --recursive` first.
# Copy the canonical Admin-API OpenAPI contract into the Hugo static dir so
# Hugo serves it at /openapi.yaml. The destination is gitignored — source of
# truth is internal/adminapi/openapi.yaml. Wired as a prerequisite of both
# `docs-build` and `docs-serve` so every Hugo run grabs the latest YAML and
# the Redoc viewer at /reference/admin-api-viewer/ stays drift-proof.
docs-openapi-copy:
	cp internal/adminapi/openapi.yaml docs/site/static/openapi.yaml

docs-serve: docs-openapi-copy
	cd docs/site && hugo server -D

docs-build: docs-openapi-copy
	cd docs/site && hugo --minify

clean:
	rm -rf bin
	rm -f bench-gc-results.jsonl bench-lifecycle-results.jsonl
	rm -f bench-gc-multi-results.jsonl bench-lifecycle-multi-results.jsonl
	rm -f bench-rebalance-multi-results.jsonl
	rm -rf docs/site/public docs/site/resources
