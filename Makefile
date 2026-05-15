SHELL := bash
COMPOSE := docker compose -f deploy/docker/docker-compose.yml

.PHONY: build build-ceph docker-build web-build web-typecheck web-clean vet test up up-all up-all-ci up-tikv up-lab-tikv up-lab-tikv-3 down wait-cassandra wait-ceph wait-pd wait-tikv wait-strata wait-strata-tikv wait-strata-lab ceph-pool run-memory run-cassandra run-strata run-gateway smoke smoke-tikv smoke-signed smoke-signed-tikv smoke-grafana smoke-lab-tikv smoke-drain-lifecycle smoke-drain-transparency smoke-cluster-weights smoke-drain-cleanup race-soak race-soak-tikv lint-nginx-lab bench-gc bench-lifecycle bench-gc-multi bench-lifecycle-multi docs-serve docs-build clean

GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# build depends on web-build so the embedded console FS is populated
# before `go build` runs. Direct `go build` for cmd/strata without web-build
# will fail with: pattern web/dist: no matching files found
build: web-build
	go build -o bin/strata ./cmd/strata
	go build -o bin/strata-admin ./cmd/strata-admin
	go build -o bin/strata-racecheck ./cmd/strata-racecheck

web-build:
	cd web && pnpm install --frozen-lockfile && pnpm run build

web-typecheck:
	cd web && pnpm run typecheck

web-clean:
	rm -rf web/dist web/node_modules

build-ceph:
	$(COMPOSE) build strata

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

up:
	$(COMPOSE) up -d cassandra

up-all:
	$(COMPOSE) up -d cassandra ceph strata prometheus grafana

# CI-trimmed stack for the nightly race-soak workflow (US-005). Layers the
# `docker-compose.ci.yml` override on top of the base file: caps Ceph's
# memstore + osd_memory_target at 1 GiB, raises Cassandra heap to 2G/400M,
# disables the Ceph mgr dashboard module, and skips Prometheus + Grafana
# (gated behind the `full` profile in the override). The `--profile ci`
# flag is decorative for the explicit service list — it preserves the spec
# command shape and leaves room for future ci-only services.
# Existing `make up-all` is unchanged (does not load the override).
up-all-ci:
	docker compose -f deploy/docker/docker-compose.yml -f deploy/docker/docker-compose.ci.yml --profile ci up -d cassandra ceph strata

# Bring up the TiKV-backed gateway stack (PD + TiKV + ceph + strata-tikv +
# observability). Mutually exclusive with `up-all` in practice — running both
# at once works (different host ports) but the cassandra service goes idle.
# strata-tikv binds host port 9998 (vs the cassandra-backed strata's 9999).
# Use `make wait-strata-tikv && make smoke-tikv` to drive the smoke suite.
up-tikv:
	$(COMPOSE) --profile tikv up -d pd tikv ceph strata-tikv prometheus grafana

# Bring up the multi-replica lab: 2 TiKV-backed strata replicas behind nginx LB
# at host port 9999. PD + TiKV + ceph back the metadata + data tier; the
# strata-tikv-{a,b} replicas hit them via the lab-tikv profile. Replica-direct
# host ports are 9001 (strata-tikv-a) and 9002 (strata-tikv-b). See
# docs/site/content/deploy/multi-replica.md for the failure-scenario walkthrough.
up-lab-tikv:
	$(COMPOSE) --profile lab-tikv up -d pd tikv ceph strata-tikv-a strata-tikv-b strata-lb-nginx prometheus grafana

# 3-replica lab: layers strata-tikv-c (lab-tikv-3 profile) on top of the
# 2-replica lab. Operator points STRATA_GC_SHARDS=3 in the host env before
# bringing up the stack so all three replicas race for shards 0..2. Used by
# `make bench-gc-multi` / `make bench-lifecycle-multi` (US-006 Phase 2).
up-lab-tikv-3:
	STRATA_GC_SHARDS=$${STRATA_GC_SHARDS:-3} $(COMPOSE) --profile lab-tikv --profile lab-tikv-3 \
		up -d pd tikv ceph strata-tikv-a strata-tikv-b strata-tikv-c strata-lb-nginx prometheus grafana

down:
	$(COMPOSE) --profile tikv --profile lab-tikv --profile features --profile tracing down

wait-cassandra:
	@echo "waiting for cassandra to report healthy..."
	@until [ "$$($(COMPOSE) ps --format '{{.Health}}' cassandra)" = "healthy" ]; do sleep 3; done
	@echo "cassandra ready"

# Wait for the cassandra-backed strata gateway to report ready on /readyz.
# Used by the CI race-soak driver script (scripts/racecheck/run.sh, US-006)
# after `make up-all-ci`. Ceiling 8 min on cold ubuntu-latest pulls per
# US-005 acceptance; the smoke step in the workflow times out at 10 min.
wait-strata:
	@echo "waiting for strata /readyz on 9999..."
	@until [ "$$(curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:9999/readyz)" = "200" ]; do sleep 2; done
	@echo "strata ready"

wait-ceph:
	@echo "waiting for ceph to report healthy..."
	@until [ "$$($(COMPOSE) ps --format '{{.Health}}' ceph)" = "healthy" ]; do sleep 5; done
	@echo "ceph ready"

wait-pd:
	@echo "waiting for pd to report healthy..."
	@until [ "$$($(COMPOSE) ps --format '{{.Health}}' pd)" = "healthy" ]; do sleep 3; done
	@echo "pd ready"

# TiKV has no HTTP healthcheck (the upstream image's status server returns
# plain text and the alpine-glibc base ships no curl); a TCP probe is the
# most portable shape across docker engines. Runs from the host, not from
# inside the container, so it works on macOS+Lima and Linux CI. SHELL is
# bash at the top of this file so /dev/tcp is available.
wait-tikv:
	@echo "waiting for tikv to accept TCP connections on 20160..."
	@until (echo > /dev/tcp/127.0.0.1/20160) 2>/dev/null; do sleep 2; done
	@echo "tikv ready"

# Wait for the TiKV-backed gateway to report ready on /readyz. The
# strata-tikv container exposes port 9998 on the host (vs the default
# cassandra-backed strata's 9999) so both can coexist under
# `--profile tikv`. /readyz fans out probes — a 200 means the gateway
# dialled PD + TiKV cleanly.
wait-strata-tikv:
	@echo "waiting for strata-tikv /readyz to report 200..."
	@until [ "$$(curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:9998/readyz)" = "200" ]; do sleep 2; done
	@echo "strata-tikv ready"

# Poll readyz on the lab-tikv profile: the LB at 9999 + both replica-direct
# ports (9001, 9002). All three must come up green before the smoke harness
# (US-005) drives the multi-replica scenarios.
wait-strata-lab:
	@echo "waiting for strata-tikv-a /readyz on 9001..."
	@until [ "$$(curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:9001/readyz)" = "200" ]; do sleep 2; done
	@echo "strata-tikv-a ready"
	@echo "waiting for strata-tikv-b /readyz on 9002..."
	@until [ "$$(curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:9002/readyz)" = "200" ]; do sleep 2; done
	@echo "strata-tikv-b ready"
	@echo "waiting for nginx LB /readyz on 9999..."
	@until [ "$$(curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:9999/readyz)" = "200" ]; do sleep 2; done
	@echo "strata-lb-nginx ready"

ceph-pool:
	docker exec strata-ceph ceph osd pool create strata.rgw.buckets.data 8 8 replicated || true
	docker exec strata-ceph ceph osd pool application enable strata.rgw.buckets.data rgw || true

run-memory: build
	STRATA_LISTEN=:9999 STRATA_META_BACKEND=memory STRATA_DATA_BACKEND=memory \
		./bin/strata server

run-cassandra: build
	STRATA_LISTEN=:9999 \
	STRATA_META_BACKEND=cassandra STRATA_DATA_BACKEND=memory \
	STRATA_CASSANDRA_HOSTS=127.0.0.1 STRATA_CASSANDRA_DC=datacenter1 \
	STRATA_WORKERS=gc,lifecycle \
		./bin/strata server

run-strata:
	$(COMPOSE) up -d strata

# Backwards-compatible alias for the old per-binary target name.
run-gateway: run-strata

smoke:
	bash scripts/smoke.sh http://127.0.0.1:9999

# Same smoke pass against the TiKV-backed gateway brought up by
# `make up-tikv`. Host port 9998 — see wait-strata-tikv comment above.
smoke-tikv:
	bash scripts/smoke.sh http://127.0.0.1:9998

smoke-signed:
	bash scripts/smoke-signed.sh http://127.0.0.1:9999

# SigV4-signed smoke pass against the TiKV-backed gateway.
smoke-signed-tikv:
	bash scripts/smoke-signed.sh http://127.0.0.1:9998

smoke-grafana:
	bash scripts/grafana-smoke.sh

# Drive the multi-replica failure scenarios end-to-end against a stack
# brought up by `make up-lab-tikv && make wait-strata-lab`. Requires
# STRATA_STATIC_CREDENTIALS exported with the same value the gateway
# booted with (the first comma-separated entry's access:secret pair is
# used for the admin login + SigV4-signed cross-replica PUT/GET).
# See scripts/multi-replica-smoke.sh for scenario coverage.
smoke-lab-tikv:
	bash scripts/multi-replica-smoke.sh

# Drain-lifecycle walkthrough smoke (US-007 of ralph/drain-lifecycle).
# Drives the full 15-step operator journey + 4 negative paths against a
# running `multi-cluster` compose profile (`docker compose --profile
# multi-cluster up -d`). Skips with exit 77 when the lab is not reachable;
# set REQUIRE_LAB=1 to convert the skip into a hard fail. See
# scripts/smoke-drain-lifecycle.sh for env knobs (BASE, SMOKE_DRAIN_*).
smoke-drain-lifecycle:
	bash scripts/smoke-drain-lifecycle.sh

# Drain-transparency walkthrough smoke (US-008 of ralph/drain-transparency).
# Drives the three operator scenarios (A: stop-writes drain, B: full evacuate
# with /drain-impact + bulk-fix, C: upgrade readonly → evacuate) against a
# running `multi-cluster` compose profile (`docker compose --profile
# multi-cluster up -d`). Skips with exit 77 when the lab is not reachable;
# set REQUIRE_LAB=1 to convert the skip into a hard fail. See
# scripts/smoke-drain-transparency.sh for env knobs (BASE, SMOKE_DRAIN_*).
smoke-drain-transparency:
	bash scripts/smoke-drain-transparency.sh

# Cluster-weights walkthrough smoke (US-005 of ralph/cluster-weights).
# Drives the four operator scenarios (A: new-cluster activation pending →
# live + ramp, B: existing-live auto-detect at boot, C: bucket policy wins
# over cluster weights, D: pending excluded from default routing) against
# a running `multi-cluster` compose profile (`docker compose --profile
# multi-cluster up -d`). Wipes `cluster_state` rows via cqlsh and restarts
# strata-multi between scenarios to exercise the boot-time reconcile.
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
# against a running `multi-cluster` compose profile (`docker compose
# --profile multi-cluster up -d`). Skips with exit 77 when the lab is
# not reachable; set REQUIRE_LAB=1 to convert the skip into a hard
# fail. See scripts/smoke-drain-cleanup.sh for env knobs (BASE,
# SMOKE_DC_*).
smoke-drain-cleanup:
	bash scripts/smoke-drain-cleanup.sh

# Race-soak driver (US-006). Brings up the cassandra-backed stack
# (`make up-all-ci` when CI=true, else `make up-all`), waits for /readyz on
# 9999, then runs `bin/strata-racecheck` for RACE_DURATION (default 1h) at
# RACE_CONCURRENCY (default 32). The harness caps --concurrency at 64 and
# refuses to start above that.
#
# Captures pre/post `df -h /` + `free -m` into report/host.txt and per-
# container docker logs (strata, strata-cassandra, strata-ceph) into
# report/. Exits with the harness's exit code (0 clean / 1 inconsistencies
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
#
# The race-harness PRD (tasks/prd-race-harness.md) lands a duration-bounded
# binary (cmd/strata-racecheck) in a future cycle; this target is the
# iter-based stop-gap that satisfies US-016 of the TiKV cycle.
race-soak-tikv:
	RACE_ITERS=$${RACE_ITERS:-100000} \
	RACE_WORKERS=$${RACE_WORKERS:-32} \
	RACE_KEYS=$${RACE_KEYS:-4} \
	  go test -tags integration -timeout $${RACE_DURATION:-1h} \
	  -run '^TestRaceMixedOpsTiKV$$' ./internal/s3api/...

# Validate the nginx LB config used by the lab-tikv profile.
# nginx -t resolves upstream hostnames at parse time, so the test container
# carries --add-host stubs for strata-tikv-{a,b}; the real names resolve via
# Docker's embedded DNS at runtime when the lab-tikv profile is up.
lint-nginx-lab:
	docker run --rm \
		--add-host=strata-tikv-a:127.0.0.1 \
		--add-host=strata-tikv-b:127.0.0.1 \
		-v $(CURDIR)/deploy/nginx/strata-lab.conf:/etc/nginx/conf.d/default.conf:ro \
		nginx:1.27-alpine nginx -t

# bench-gc / bench-lifecycle drive strata-admin against the lab-tikv stack
# (TiKV meta + RADOS data) at five concurrency levels and tee one JSON line
# per level into bench-gc-results.jsonl / bench-lifecycle-results.jsonl. The
# gateway must already be up via `make up-lab-tikv && make wait-strata-lab`;
# strata-admin connects to TiKV directly (PD endpoints from the docker-compose
# default) so the bench bypasses the HTTP gateway and measures the worker's
# per-replica throughput cap.
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
		./bin/strata-admin bench-gc --entries=$(BENCH_GC_ENTRIES) --concurrency=$$c \
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
		./bin/strata-admin bench-lifecycle --objects=$(BENCH_LC_OBJECTS) --concurrency=$$c \
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
		./bin/strata-admin bench-gc --entries=$(BENCH_GC_ENTRIES) --concurrency=$$c \
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
		./bin/strata-admin bench-lifecycle --objects=$(BENCH_LC_OBJECTS) --concurrency=$$c \
		  --replicas=$(BENCH_LC_REPLICAS) --buckets=$(BENCH_LC_BUCKETS) \
		  | tee -a bench-lifecycle-multi-results.jsonl; \
	done

# Hugo docs site (docs/site/). `docs-serve` runs the local dev preview on
# :1313 with drafts enabled; `docs-build` produces the minified static bundle
# under docs/site/public/ which the GitHub Actions workflow publishes to
# gh-pages. Requires Hugo extended (>= 0.128) on PATH; the theme lives at
# docs/site/themes/hugo-book/ as a Git submodule, so a fresh checkout needs
# `git submodule update --init --recursive` first.
docs-serve:
	cd docs/site && hugo server -D

docs-build:
	cd docs/site && hugo --minify

clean:
	rm -rf bin
	rm -f bench-gc-results.jsonl bench-lifecycle-results.jsonl
	rm -f bench-gc-multi-results.jsonl bench-lifecycle-multi-results.jsonl
	rm -rf docs/site/public docs/site/resources
