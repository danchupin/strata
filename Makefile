SHELL := bash
COMPOSE := docker compose -f deploy/docker/docker-compose.yml

.PHONY: build build-ceph docker-build vet test up up-all down wait-cassandra wait-ceph ceph-pool run-memory run-cassandra run-strata run-gateway smoke smoke-grafana clean

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

down:
	$(COMPOSE) down

wait-cassandra:
	@echo "waiting for cassandra to report healthy..."
	@until [ "$$($(COMPOSE) ps --format '{{.Health}}' cassandra)" = "healthy" ]; do sleep 3; done
	@echo "cassandra ready"

wait-ceph:
	@echo "waiting for ceph to report healthy..."
	@until [ "$$($(COMPOSE) ps --format '{{.Health}}' ceph)" = "healthy" ]; do sleep 5; done
	@echo "ceph ready"

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
