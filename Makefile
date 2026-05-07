SHELL := bash
COMPOSE := docker compose -f deploy/docker/docker-compose.yml

.PHONY: build build-ceph docker-build web-build web-typecheck web-clean vet test up up-all up-tikv up-lab-tikv down wait-cassandra wait-ceph wait-pd wait-tikv wait-strata-tikv wait-strata-lab ceph-pool run-memory run-cassandra run-strata run-gateway smoke smoke-tikv smoke-signed smoke-signed-tikv smoke-grafana smoke-lab-tikv race-soak-tikv lint-nginx-lab clean

GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# build depends on web-build so the embedded console FS is populated
# before `go build` runs. Direct `go build` for cmd/strata without web-build
# will fail with: pattern web/dist: no matching files found
build: web-build
	go build -o bin/strata ./cmd/strata
	go build -o bin/strata-admin ./cmd/strata-admin

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
# docs/multi-replica.md for the failure-scenario walkthrough.
up-lab-tikv:
	$(COMPOSE) --profile lab-tikv up -d pd tikv ceph strata-tikv-a strata-tikv-b strata-lb-nginx prometheus grafana

down:
	$(COMPOSE) --profile tikv --profile lab-tikv --profile features --profile tracing down

wait-cassandra:
	@echo "waiting for cassandra to report healthy..."
	@until [ "$$($(COMPOSE) ps --format '{{.Health}}' cassandra)" = "healthy" ]; do sleep 3; done
	@echo "cassandra ready"

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

clean:
	rm -rf bin
