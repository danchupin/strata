# Strata — repo guide for Claude / agents

Strata is a horizontally scalable, S3-compatible object gateway written in
Go. It separates the metadata plane (Cassandra) from the data plane
(RADOS or any S3-compatible store), and replaces RGW's bucket-index
ceiling with a sharded Cassandra keyspace.

## Big-picture architecture

```
                           ┌─────────────────────────────┐
                           │       S3 client (curl,      │
                           │       aws-cli, mc, boto3)   │
                           └──────────────┬──────────────┘
                                          │ HTTP / HTTPS
                                          ▼
                  ┌────────────────────────────────────────────┐
                  │  cmd/strata-gateway   (cmd/strata-lifecycle│
                  │  internal/s3api       cmd/strata-gc)       │
                  │  internal/auth (SigV4)                     │
                  └─────────────┬──────────────────┬───────────┘
                                │                  │
                       meta.Store                 data.Backend
                                │                  │
                  ┌─────────────▼─────┐  ┌─────────▼──────────┐
                  │   memory (test)   │  │  memory (test)     │
                  │   cassandra (P1)  │  │  rados (default)   │
                  └───────────────────┘  │  s3   (alternative)│
                                         └────────────────────┘
                                                  │
                                                  ▼
                                       Ceph cluster
                                       AWS S3 / MinIO / Ceph RGW / Garage
```

- **`internal/s3api`** — HTTP handlers, XML, errors, S3 routing.
- **`internal/auth`** — SigV4 (header + presigned-URL).
- **`meta.Store`** (`internal/meta/`) — interface; backends `memory`,
  `cassandra`. Optional capability surfaces (`RangeScanStore`, etc.) let
  backends opt into faster code paths.
- **`data.Backend`** (`internal/data/`) — interface; backends
  `memory | rados | s3`. RADOS is the default and recommended data
  backend; S3-over-S3 (this PRD's addition) is an equal-tier alternative
  for operators who already run MinIO / AWS S3 / Ceph RGW / Garage.
  Optional capability surfaces (`MultipartBackend`, `LifecycleBackend`,
  `CORSBackend`, `PresignBackend`) let the s3 backend offload work to
  the underlying store.

## Alternative data backends

Strata's recommended production data plane is **RADOS** via `go-ceph`;
the S3 backend is an **equal-tier alternative** built on `aws-sdk-go-v2`
that targets any S3-compatible endpoint (AWS S3, MinIO, Ceph RGW, Garage,
Wasabi, B2-S3). The supported set is exactly two: `rados` and `s3` (plus
`memory` for tests). Filesystem / Azure Blob / GCS are explicitly **not
planned** — operators needing those use the S3 backend pointed at any
S3-compatible service (MinIO over filesystem, s3-proxy over Azure, GCS
S3-interop API).

The `data.Backend` interface stays minimal and stream-shaped (`Put` /
`Get` / `GetRange` / `Delete`); features that some backends do natively
(multipart pass-through, lifecycle translation, CORS mirror, presign
passthrough) live behind **optional interfaces** that backends opt into.
Gateway code uses type-assertion to pick the better path and falls back
to the chunk-based / worker-based default otherwise.

See [docs/backends/s3.md](docs/backends/s3.md) for the S3 operator guide
(capability matrix, tested-against backends, pitfalls).

## Roadmap maintenance rule

Every commit closing a `ROADMAP.md` item flips the bullet to
strikethrough Done format in the SAME commit, with the closing SHA
inline (or `(commit pending)` with a follow-up SHA edit). Every commit
discovering a new gap adds a new entry under the right severity section
in the same commit.

## Where else to look

- `README.md` — How to run (4 options including `make up-s3-backend`),
  full env-var table, repo layout.
- `ROADMAP.md` — gaps and direction; severity-tagged P1/P2/P3.
- `docs/backends/s3.md` — S3 operator guide.
- `scripts/s3-tests/README.md` — Ceph s3-tests harness, baseline
  pass-rate, interpretation notes.
- `scripts/ralph/` — autonomous-cycle PRD + progress log driving the
  current ralph branches.
