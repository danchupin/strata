SHELL := bash
COMPOSE := docker compose -f deploy/docker/docker-compose.yml
COMPOSE_CEPH := $(COMPOSE) --profile ceph-backend
COMPOSE_S3 := $(COMPOSE) --profile s3-backend

.PHONY: build build-ceph vet test up up-all up-s3-backend down wait-cassandra wait-ceph wait-minio wait-strata-s3 ceph-pool run-memory run-cassandra run-gateway smoke smoke-s3-backend smoke-s3-backend-ci clean

build:
	go build ./...

build-ceph:
	$(COMPOSE_CEPH) build gateway

build-s3-backend:
	$(COMPOSE_S3) build strata-s3

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
	$(COMPOSE_CEPH) up -d cassandra ceph gateway

up-s3-backend:
	$(COMPOSE_S3) up -d cassandra minio init-minio strata-s3
	@$(MAKE) wait-cassandra
	@$(MAKE) wait-minio
	@$(MAKE) wait-strata-s3

down:
	$(COMPOSE) --profile ceph-backend --profile s3-backend down -v

wait-cassandra:
	@echo "waiting for cassandra to report healthy..."
	@until [ "$$($(COMPOSE) ps --format '{{.Health}}' cassandra)" = "healthy" ]; do sleep 3; done
	@echo "cassandra ready"

wait-ceph:
	@echo "waiting for ceph to report healthy..."
	@until [ "$$($(COMPOSE) ps --format '{{.Health}}' ceph)" = "healthy" ]; do sleep 5; done
	@echo "ceph ready"

wait-minio:
	@echo "waiting for minio to report healthy..."
	@until [ "$$($(COMPOSE_S3) ps --format '{{.Health}}' minio)" = "healthy" ]; do sleep 2; done
	@echo "minio ready"

wait-strata-s3:
	@echo "waiting for strata-s3 to report healthy (/readyz)..."
	@until [ "$$($(COMPOSE_S3) ps --format '{{.Health}}' strata-s3)" = "healthy" ]; do sleep 2; done
	@echo "strata-s3 ready"

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
	$(COMPOSE_CEPH) up -d gateway

smoke:
	bash scripts/smoke.sh http://127.0.0.1:9999

smoke-s3-backend: up-s3-backend
	bash scripts/smoke-s3-backend.sh http://127.0.0.1:9999

# CI variant: also asserts `make down` clears the strata-minio-data volume.
# Destructive — tears down the stack at the end.
smoke-s3-backend-ci: up-s3-backend
	SKIP_DOWN=0 bash scripts/smoke-s3-backend.sh http://127.0.0.1:9999

smoke-signed:
	bash scripts/smoke-signed.sh http://127.0.0.1:9999

clean:
	rm -rf bin
