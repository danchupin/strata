# Strata

![CI](https://github.com/danchupin/strata/actions/workflows/ci.yml/badge.svg)

Scalable drop-in replacement for Ceph RGW written in Go. Metadata lives in Cassandra, data goes to RADOS via the `go-ceph` bindings.

Solves the main bottleneck of stock RGW — the bucket index ceiling and costly resharding — by moving the index out of RADOS omap into a horizontally scalable Cassandra keyspace. Listing fans out across N partitions (`hash(key) % N`, configurable per bucket) and heap-merges at the gateway. Object data is chunked into 4 MiB RADOS objects; the manifest lives in Cassandra (column `objects.manifest`).

- **Metadata plane**: Cassandra (gocql), multi-DC `NetworkTopologyStrategy`, `LOCAL_QUORUM` writes, `LOCAL_ONE` / `LOCAL_QUORUM` reads. LWT only where S3 semantics strictly require CAS (global bucket-name uniqueness; versioning latest-flip, multipart-complete, Object Lock — landing in later phases).
- **Data plane**: RADOS via `github.com/ceph/go-ceph/rados`. One `*rados.Conn` per process, `IOContext` cached by `(pool, namespace)`. Chunked PUT/GET; deletes are physical for now. Async/parallel multipart completion and background GC land in later phases.
- **Storage classes**: 3-tier design is locked (see plan); code currently exposes a single class (`STANDARD`). Tiered routing is Phase 6.

## Phase status

| Phase | Status |
|---|---|
| 0. Bootstrap (skeleton, schema.cql) | done |
| 1. Data plane (RADOS chunked PUT/GET) | done (build tag `ceph`) |
| 2. Metadata MVP (sharded Cassandra index, fan-out list) | done |
| 3. Multipart | done |
| 4. Versioning + delete markers | done |
| 5. SigV4 + auth | done (static credentials) |
| 6. Storage classes (3-tier routing) | done |
| 7. Lifecycle + Object Lock + Tags | done (worker executes transitions + expirations) |
| 8. GC + Prometheus | done (leader election, CAS, noncurrent lifecycle, presigned URLs, testcontainers integration tests) |

## How to run

### Option 1: fully in-memory smoke (fastest)

```bash
make run-memory            # listens on :9999 with data=memory, meta=memory
make smoke                 # bash scripts/smoke.sh
```

### Option 2: Cassandra metadata + in-memory data

```bash
make up                    # docker compose up cassandra
make wait-cassandra
make run-cassandra         # data=memory, meta=cassandra@localhost:9042
make smoke
```

### Option 3: full stack in Docker (Cassandra + Ceph + gateway)

```bash
make up-all                # cassandra + ceph + gateway (gateway built with -tags ceph)
make wait-cassandra
make wait-ceph
make smoke
```

End-to-end with real Ceph runs natively on both arm64 and amd64. The cluster image (`deploy/docker/ceph-bootstrap/`) is a custom bootstrap on top of the multi-arch `quay.io/ceph/ceph:v19.2.3` (Squid). MON+MGR+OSD in a single container, OSD backed by `memstore` (4 GiB, held in process memory). Healthy in ~5 seconds.

The gateway image (`deploy/docker/Dockerfile`) is built on the same `quay.io/ceph/ceph:v19.2.3` base so the linked librados version exactly matches the cluster. `librados-devel-19.2.3` for the build stage is pulled from `download.ceph.com/rpm-squid/el9/$arch/`.

A successful `make smoke` validates bucket CRUD, object PUT/GET/HEAD/DELETE (including a 10 MiB blob that ends up as three RADOS objects in pool `strata.rgw.buckets.data`), and ListObjectsV2 with prefix/delimiter.

### Option 4: Strata + Cassandra + MinIO (S3-over-S3 backend, no Ceph)

```bash
make up-s3-backend         # cassandra + minio + init-minio + strata-s3
make smoke-s3-backend      # smoke pass + 1:1 backend object assertions
```

This stack runs the gateway with `STRATA_DATA_BACKEND=s3` against a single-node MinIO sidecar — no Ceph, no librados, smaller footprint (idle RSS ≈ 3 GB vs. the Ceph stack's larger envelope). MinIO is the development-time stand-in for any S3-compatible endpoint (AWS S3, Ceph RGW, Garage, Wasabi, B2-S3); switching to a different backend in production is an env-var change. See [docs/backends/s3.md](docs/backends/s3.md) for the operator guide, capability matrix, and pitfalls.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `STRATA_LISTEN` | `:9000` | HTTP listen address |
| `STRATA_DATA_BACKEND` | `memory` | `memory`, `rados` (requires build tag `ceph`), `s3` (S3-compatible endpoint) |
| `STRATA_META_BACKEND` | `memory` | `memory`, `cassandra` |
| `STRATA_BUCKET_SHARDS` | `64` | default partition-shard count for the `objects` table |
| `STRATA_CASSANDRA_HOSTS` | `127.0.0.1` | comma-separated |
| `STRATA_CASSANDRA_KEYSPACE` | `strata` | auto-migrated at startup |
| `STRATA_CASSANDRA_DC` | `datacenter1` | local DC for DCAwareRoundRobin |
| `STRATA_CASSANDRA_REPLICATION` | `SimpleStrategy rf=1` | raw CQL literal |
| `STRATA_RADOS_CONF` | `/etc/ceph/ceph.conf` | |
| `STRATA_RADOS_USER` | `admin` | cephx id (short form, not `client.admin`) |
| `STRATA_RADOS_KEYRING` | `` | optional path override |
| `STRATA_RADOS_POOL` | `strata.rgw.buckets.data` | |
| `STRATA_RADOS_NAMESPACE` | `` | optional per-tenant namespace |
| `STRATA_RADOS_CLASSES` | `` | class routing map: `CLASS=pool[@cluster[/ns]],...`. If empty, STANDARD points at `STRATA_RADOS_POOL`. |
| `STRATA_S3_BACKEND_ENDPOINT` | `` | full backend URL (`http://host:port`); empty falls back to AWS region-based resolution. Required for MinIO / Ceph RGW / Garage / Wasabi / B2-S3 |
| `STRATA_S3_BACKEND_REGION` | `` | required when `STRATA_DATA_BACKEND=s3` |
| `STRATA_S3_BACKEND_BUCKET` | `` | required; single backend bucket every Strata object lands in. Must be pre-created — Strata refuses to start if missing or unwritable (boot-time PUT/DELETE probe on key `.strata-readyz-canary`) |
| `STRATA_S3_BACKEND_ACCESS_KEY` | `` | static access key; both empty falls back to SDK default credential chain (env / `~/.aws` / IRSA / IMDS). Half-set is misconfig and fails at startup |
| `STRATA_S3_BACKEND_SECRET_KEY` | `` | static secret key; see access-key note |
| `STRATA_S3_BACKEND_FORCE_PATH_STYLE` | `false` | `true` for MinIO + Ceph RGW; `false` (default) for AWS / virtual-hosted-style endpoints |
| `STRATA_S3_BACKEND_PART_SIZE` | `16777216` | multipart-upload part size in bytes (default 16 MiB; SDK minimum 5 MiB) |
| `STRATA_S3_BACKEND_UPLOAD_CONCURRENCY` | `4` | parallel part uploads per Put. Memory peak ≈ part size × concurrency (default 64 MiB) |
| `STRATA_S3_BACKEND_MAX_RETRIES` | `5` | total SDK attempts per request (initial + retries) under adaptive retry mode. Retries on 503 SlowDown / 429 / 5xx / network errors; never on 4xx auth/not-found |
| `STRATA_S3_BACKEND_OP_TIMEOUT_SECS` | `30` | per-op deadline for small ops (Get / GetRange / DeleteObject / DeleteBatch / Probe). Multipart Put has a separate 10-min ceiling. Bound includes body-stream lifetime — operators with slow links should bump |
| `STRATA_S3_BACKEND_SSE_MODE` | `passthrough` | encryption disposition for backend writes (US-013). `passthrough` (default) forwards `x-amz-server-side-encryption` to the backend per Put — backend handles encryption-at-rest, GET surfaces the SSE header back to clients via `Manifest.SSE`. `strata` sends no backend SSE header (gateway-side envelope encryption is plumbed but not yet wired here). `both` runs Strata envelope encryption AND backend SSE for two independent boundaries. Mode is recorded per-object on `Manifest.SSE.Mode` |
| `STRATA_S3_BACKEND_SSE_KMS_KEY_ID` | `` | when set in `passthrough`/`both` mode, switches the backend SSE header from `AES256` (SSE-S3) to `aws:kms` with this key id (SSE-KMS). Ignored in `strata` mode |
| `STRATA_AUTH_MODE` | `off` | `off` accepts anything, `required` enforces SigV4 |
| `STRATA_STATIC_CREDENTIALS` | `` | comma-separated `accesskey:secret[:owner]` entries for dev credentials |
| `STRATA_LIFECYCLE_INTERVAL` | `60s` | `strata-lifecycle` tick interval (Go duration) |
| `STRATA_LIFECYCLE_UNIT` | `day` | interpretation of `Days` in lifecycle rules: `day`\|`hour`\|`minute`\|`second` (use `second` for fast tests) |
| `STRATA_LIFECYCLE_METRICS_LISTEN` | `:9101` | Prometheus port for `strata-lifecycle` |
| `STRATA_GC_INTERVAL` | `30s` | `strata-gc` tick interval |
| `STRATA_GC_GRACE` | `5m` | how long to keep queued chunks before deleting (protects in-flight GETs) |
| `STRATA_GC_METRICS_LISTEN` | `:9100` | Prometheus port for `strata-gc` |

## Repository layout

```
cmd/
  strata-gateway/     main S3 gateway binary
  strata-lifecycle/   lifecycle worker (phase 7)
  strata-gc/          GC for orphan tail objects (phase 8)
internal/
  s3api/              HTTP handlers, XML, errors, routing
  meta/
    memory/           in-memory store for dev
    cassandra/        gocql: session, schema auto-migration, fan-out listing
  data/
    memory/           in-memory data backend for dev
    rados/            go-ceph backend, build tag `ceph`
  config/             env-var loader
deploy/
  cassandra/schema.cql
  docker/
    Dockerfile                      multi-stage on quay.io/ceph/ceph:v19.2.3
    docker-compose.yml
    ceph-bootstrap/
      Dockerfile
      bootstrap.sh                  MON + OSD (memstore) + MGR bootstrap
scripts/
  smoke.sh            curl-driven sanity pass over bucket/object/list/delete
```
