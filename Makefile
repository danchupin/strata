SHELL := bash
COMPOSE := docker compose -f deploy/docker/docker-compose.yml

.PHONY: build build-ceph docker-build vet test up up-all up-tikv down wait-cassandra wait-ceph wait-pd wait-tikv ceph-pool run-memory run-cassandra run-strata run-gateway smoke smoke-grafana clean

GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

build:
	go build -o bin/strata ./cmd/strata
	go build -o bin/strata-admin ./cmd/strata-admin

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
# strata-tikv health depends on internal/serverapp wiring landed by US-015;
# until then `make wait-strata-tikv` may time out. PD + TiKV come up cleanly
# regardless, so this target is enough for `make test-integration` against
# STRATA_TIKV_TEST_PD_ENDPOINTS=127.0.0.1:2379.
up-tikv:
	$(COMPOSE) --profile tikv up -d pd tikv ceph strata-tikv prometheus grafana

down:
	$(COMPOSE) --profile tikv --profile features --profile tracing down

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

smoke-signed:
	bash scripts/smoke-signed.sh http://127.0.0.1:9999

smoke-grafana:
	bash scripts/grafana-smoke.sh

clean:
	rm -rf bin
