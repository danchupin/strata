SHELL := bash
COMPOSE := docker compose -f deploy/docker/docker-compose.yml

.PHONY: build build-ceph web-build web-typecheck web-clean vet test up up-all down wait-cassandra wait-ceph ceph-pool run-memory run-cassandra run-gateway smoke clean

# build depends on web-build so the embedded console FS is populated
# before `go build` runs. Direct `go build ./...` without web-build will
# fail with: pattern web/dist: no matching files found
build: web-build
	go build ./...

web-build:
	cd web && pnpm install --frozen-lockfile && pnpm run build

web-typecheck:
	cd web && pnpm run typecheck

web-clean:
	rm -rf web/dist web/node_modules

build-ceph:
	$(COMPOSE) build gateway

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
	$(COMPOSE) up -d cassandra ceph gateway

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

run-memory:
	STRATA_LISTEN=:9999 STRATA_META_BACKEND=memory STRATA_DATA_BACKEND=memory \
		go run ./cmd/strata-gateway

run-cassandra:
	STRATA_LISTEN=:9999 \
	STRATA_META_BACKEND=cassandra STRATA_DATA_BACKEND=memory \
	STRATA_CASSANDRA_HOSTS=127.0.0.1 STRATA_CASSANDRA_DC=datacenter1 \
		go run ./cmd/strata-gateway

run-gateway:
	$(COMPOSE) up -d gateway

smoke:
	bash scripts/smoke.sh http://127.0.0.1:9999

smoke-signed:
	bash scripts/smoke-signed.sh http://127.0.0.1:9999

clean:
	rm -rf bin
