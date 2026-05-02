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

### Option 3: full stack in Docker (Cassandra + Ceph + strata)

```bash
make up-all                # cassandra + ceph + strata (image built with -tags ceph)
make wait-cassandra
make wait-ceph
make smoke
```

### Option 4: TiKV-backed metadata (PD + TiKV + Ceph + strata)

```bash
make up-tikv               # pd + tikv + ceph + strata-tikv (host port 9998)
make wait-pd
make wait-tikv
make wait-strata-tikv
make smoke-tikv            # bash scripts/smoke.sh http://127.0.0.1:9998
```

TiKV is a first-class equal-tier metadata backend (`STRATA_META_BACKEND=tikv`).
Native ordered range scans short-circuit Cassandra's 64-way fan-out on
`ListObjects`. See [`docs/backends/tikv.md`](docs/backends/tikv.md) for the
operator guide and [`docs/benchmarks/meta-backend-comparison.md`](docs/benchmarks/meta-backend-comparison.md)
for the comparison numbers.

End-to-end with real Ceph runs natively on both arm64 and amd64. The cluster image (`deploy/docker/ceph-bootstrap/`) is a custom bootstrap on top of the multi-arch `quay.io/ceph/ceph:v19.2.3` (Squid). MON+MGR+OSD in a single container, OSD backed by `memstore` (4 GiB, held in process memory). Healthy in ~5 seconds.

The runtime image (`deploy/docker/Dockerfile`) is built on the same `quay.io/ceph/ceph:v19.2.3` base so the linked librados version exactly matches the cluster. `librados-devel-19.2.3` for the build stage is pulled from `download.ceph.com/rpm-squid/el9/$arch/`. Image entrypoint is `/usr/local/bin/strata`, default `CMD ["server"]`; `strata-admin` is also installed for operator verbs.

A successful `make smoke` validates bucket CRUD, object PUT/GET/HEAD/DELETE (including a 10 MiB blob that ends up as three RADOS objects in pool `strata.rgw.buckets.data`), and ListObjectsV2 with prefix/delimiter.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `STRATA_LISTEN` | `:9000` | HTTP listen address |
| `STRATA_DATA_BACKEND` | `memory` | `memory`, `rados` (requires build tag `ceph`) |
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
| `STRATA_RADOS_CLUSTERS` | `` | multi-cluster RADOS map: `<id>:<conf>[:<keyring>],...`. Referenced by `@cluster` in `STRATA_RADOS_CLASSES`. Empty falls back to the legacy single-cluster fields under id `default`. |
| `STRATA_AUTH_MODE` | `off` | `off` accepts anything, `required` enforces SigV4 |
| `STRATA_STATIC_CREDENTIALS` | `` | comma-separated `accesskey:secret[:owner]` entries for dev credentials |
| `STRATA_WORKERS` | `` | comma-separated worker list selected on `strata server` (e.g. `gc,lifecycle,notify,replicator,access-log,inventory,audit-export,manifest-rewriter`) |
| `STRATA_LIFECYCLE_INTERVAL` | `60s` | `lifecycle` worker tick interval (Go duration) |
| `STRATA_LIFECYCLE_UNIT` | `day` | interpretation of `Days` in lifecycle rules: `day`\|`hour`\|`minute`\|`second` (use `second` for fast tests) |
| `STRATA_GC_INTERVAL` | `30s` | `gc` worker tick interval |
| `STRATA_GC_GRACE` | `5m` | how long to keep queued chunks before deleting (protects in-flight GETs) |

## Repository layout

```
cmd/
  strata/             unified S3 gateway binary; `strata server` runs the
                      gateway plus the workers selected via STRATA_WORKERS
                      (gc, lifecycle, notify, replicator, access-log,
                      inventory, audit-export, manifest-rewriter)
  strata-admin/       operator CLI (rewrap, IAM, lifecycle ticks, ...)
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
examples/             copy-paste aws-cli / boto3 / mc / s3cmd workflows
                      (see examples/README.md). examples/smoke.sh boots a
                      fresh in-memory gateway and runs every example.
```

## Examples

`examples/` ships runnable scripts covering bucket setup, multipart upload,
presigned URLs, lifecycle, replication, SSE-S3, and IAM access-key
rotation for `aws-cli`, `boto3`, `mc`, and `s3cmd`. Run them all in one
shot against a fresh in-memory gateway:

```bash
bash examples/smoke.sh
```

Pick one to copy-paste from, e.g. [`examples/aws-cli/06-sse-s3.sh`](examples/aws-cli/06-sse-s3.sh)
or [`examples/boto3/07-rotate-key.py`](examples/boto3/07-rotate-key.py).
