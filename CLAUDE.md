# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this project is

Strata is a Go-based, S3-compatible object gateway designed as a drop-in replacement for Ceph RGW. Metadata lives in
Cassandra (sharded `objects` table to dodge the bucket-index ceiling that bites RGW), data lives in RADOS as 4 MiB
chunks, and the gateway speaks S3 over HTTP. The compatibility goal is tracked against Ceph's upstream `s3-tests`
suite — see `tasks/prd-s3-compatibility.md` for the active PRD and `ROADMAP.md` for what is shipped vs pending.

The metadata interface (`internal/meta.Store`) is intentionally minimal and Cassandra-flavoured (LWT, clustering order,
fan-out paging). Cassandra is the primary backend; ScyllaDB is a drop-in. The in-memory backend is for tests and the
smoke pass.

## Common commands

| Task                                                                   | Command                                                                  |
|------------------------------------------------------------------------|--------------------------------------------------------------------------|
| Build everything                                                       | `make build` (`go build ./...`)                                          |
| Build with RADOS data backend                                          | `go build -tags ceph ./...`                                              |
| Vet                                                                    | `make vet`                                                               |
| Unit tests                                                             | `make test` (`go test ./...`)                                            |
| Race-detector tests                                                    | `make test-race`                                                         |
| Cassandra integration tests (testcontainers, needs Docker)             | `make test-integration` (`go test -tags integration -timeout 10m ./...`) |
| RADOS integration tests (in-container, requires `make up-all` running) | `make test-rados`                                                        |
| Run a single test                                                      | `go test -run TestBucketCRUD ./internal/s3api`                           |
| Bring up Cassandra only                                                | `make up && make wait-cassandra`                                         |
| Bring up full stack (Cassandra + Ceph + gateway)                       | `make up-all && make wait-cassandra && make wait-ceph`                   |
| Run gateway against in-memory backends                                 | `make run-memory`                                                        |
| Run gateway against Cassandra metadata + memory data                   | `make run-cassandra`                                                     |
| Smoke pass                                                             | `make smoke` (signed: `make smoke-signed`)                               |
| Take stack down                                                        | `make down`                                                              |
| S3 compatibility suite                                                 | `scripts/s3-tests/run.sh` (see `scripts/s3-tests/README.md`)             |

macOS + lima Docker note: `make test-integration` needs `DOCKER_HOST=unix:///Users/.../.lima/.../sock/docker.sock` for
testcontainers to find the engine.

## Big-picture architecture

```
                +-----------------------------+
                |  S3 client (aws-cli, mc)    |
                +--------------+--------------+
                               |
                  HTTP S3 (path-style URLs)
                               |
                +--------------v--------------+
                | cmd/strata-gateway          |
                |  -> auth.Middleware (SigV4) |
                |  -> s3api.Server (router)   |
                +-------+--------------+------+
                        |              |
                        v              v
              +-------------------+   +---------------------+
              | meta.Store        |   | data.Backend        |
              |  memory | cassandra|   |  memory | rados    |
              +---------+----------+   +---------+----------+
                        |                        |
                +-------v-------+        +-------v-------+
                | Cassandra     |        | RADOS         |
                | (sharded)     |        | (4 MiB chunks)|
                +---------------+        +---------------+

  cmd/strata-lifecycle  -> meta.Store + data.Backend (transitions / expirations / mp-abort)
  cmd/strata-gc         -> meta.Store (GCEntry queue) + data.Backend (chunk delete)
                          both wrapped in internal/leader (Cassandra LWT-based lease)
```

The S3 router is in `internal/s3api/server.go`. Bucket-scoped queries (`?cors`, `?policy`, `?lifecycle`, …) dispatch via
`handleBucket`; object-scoped (`?uploads`, `?uploadId=`, `?tagging`, …) via `handleObject`. New endpoints follow the
same query-string router pattern.

Auth lives in `internal/auth/`: SigV4 (`sigv4.go`), presigned URLs (`presigned.go`), streaming chunk decoder (
`streaming.go`, **chain HMAC validation TODO** — see ROADMAP P2), static credentials store (`static.go`). Identity flows
through context: `auth.FromContext(ctx).Owner`.

## meta.Store interface — the contract

`internal/meta/store.go` is the abstraction every backend must satisfy. **Both `internal/meta/memory`
and `internal/meta/cassandra` implement it in lockstep** — adding a method to the interface means updating both, plus
the contract tests.

`internal/meta/storetest/contract.go` defines `Run(t, factory)` — the shared test suite. New methods that have a
backend-agnostic semantic should grow a `case<Name>` here so both backends are exercised. Memory tests live in
`internal/meta/memory/store_test.go`; Cassandra integration tests in
`internal/meta/cassandra/store_integration_test.go` (build tag `integration`).

There is a generic blob-config helper pattern for "bucket has one XML/JSON document of kind X" endpoints (lifecycle,
CORS, policy, public-access-block, ownership-controls). Reuse `setBucketBlob` / `getBucketBlob` / `deleteBucketBlob` in
both backends instead of writing fresh CRUD per endpoint.

`data.Manifest` is JSON-encoded into the `objects.manifest` blob column (`internal/meta/cassandra/codec.go`). New
fields tagged `json:",omitempty"` are schema-additive — old rows decode with zero-values, and you avoid an `ALTER`.
Use this for per-object metadata that the GET path reads but Cassandra never filters on (e.g. `Manifest.PartChunks`
for the SSE multipart locator).

## Cassandra gotchas (real ones, hit during this codebase's lifetime)

- **No subqueries.** CQL does not support `WHERE name IN (SELECT name FROM ... WHERE id=?)`. If you need that,
  denormalise (store the natural PK in the row you need to update) or do a two-step round trip.
- **LWT (`IF EXISTS` / `IF NOT EXISTS`) is required for read-after-write coherence on the same row.**
  `SetBucketVersioning` learned this the hard way: a plain `UPDATE` after an `INSERT … IF NOT EXISTS` left Paxos state
  stale and `LOCAL_QUORUM` reads observed the pre-update value. Any UPDATE on a row that may be read with quorum after a
  previous LWT must itself be LWT.
- **`MapScanCAS` not `ScanCAS(nil...)` for `INSERT … IF NOT EXISTS`.** `CreateBucket` had a column-count bug fixed via
  `MapScanCAS`.
- **`ALLOW FILTERING` on a non-PK column is an antipattern.** If you need to look up by a non-PK, denormalise into a
  second table or add a secondary index — but secondary indexes are also a smell at this scale. Prefer denormalisation.
- **Schema migrations are additive.** `internal/meta/cassandra/schema.go` has `tableDDL` (idempotent
  `CREATE TABLE IF NOT EXISTS`) and `alterStatements` (idempotent `ALTER TABLE ADD column`, swallowed by
  `isColumnAlreadyExists`). Existing keyspaces need to upgrade in place — never write a destructive migration.
- **Two UUID flavours coexist.** Outside the cassandra package use `github.com/google/uuid` (`uuid.UUID`). Inside, gocql
  exposes its own `gocql.UUID`. Convert via the `gocqlUUID()` / `uuidFromGocql()` helpers at the boundary.

## Sharded objects table — listing fans out

The `objects` table is partitioned by `(bucket_id, shard)` where `shard = hash(key) % N` (default `N=64`, configurable
via `STRATA_BUCKET_SHARDS` at bucket creation). `ListObjects` therefore queries `N` partitions concurrently and
heap-merges by clustering order (key ASC, version_id DESC). See `cassandra/store.go: ListObjects` and the `cursorHeap` /
`versionHeap` types. A new range-scan-capable backend (e.g. TiKV) would short-circuit this via the optional
`RangeScanStore` interface (see ROADMAP).

## S3-specific conventions in this repo

- **Bucket names must be ≥3 chars, lowercase, DNS-safe.** `internal/s3api/validate.go: validBucketName` enforces this on
  `PUT /<bucket>`. Tests use `/bkt`, never `/b` — the latter rejects with 400 InvalidBucketName.
- **Test harness:** `internal/s3api/testutil_test.go` exposes `newHarness(t)` returning a server hooked up to in-memory
  `data` + `meta`. Drive it with `h.doString(method, path, body, headers...)` and `h.mustStatus(resp, code)`.
- **Conditional headers (RFC 7232) on GET** flow through `checkConditional` in `internal/s3api/conditional.go`. PUT-side
  `If-Match` / `If-None-Match` checks live inline in `putObject`.
- **Lifecycle worker uses CAS on transition** via
  `Store.SetObjectStorage(ctx, …, expectedClass, newClass, manifest) (applied bool, err error)`. If `applied=false`, the
  worker discards the freshly written tier-2 chunks via the GC queue — this is intentional, the concurrent client write
  wins.
- **Multipart Complete uses LWT** — `IF status='uploading'` flips to `completing` so concurrent retries get
  `ErrMultipartInProgress` rather than racing to write the object row twice.

## Where to look when adding S3 surface

1. Read the corresponding entry in `tasks/prd-s3-compatibility.md` (or `ROADMAP.md` for older items).
2. Wire the route into `handleBucket` / `handleObject` in `internal/s3api/server.go`.
3. Add a sentinel error in `internal/meta/store.go` and an `APIError` in `internal/s3api/errors.go`.
4. Add the `Set/Get/Delete` triple to `meta.Store` and implement in both backends. Use the blob-config helpers if it's a
   single-document config.
5. Add a Cassandra schema migration via `alterStatements` or a new entry in `tableDDL`.
6. Tests: round-trip happy path, malformed body, not-configured 404. Bucket name `/bkt`.

## Commits and PRs

- Subject is `<area>: <imperative summary>` (e.g. `s3api: implement DeleteObjects`).
- Co-authored-by trailers are present on AI-assisted commits.
- Don't push `main` without an explicit ask. CI (`.github/workflows/ci.yml`) runs lint+build, unit (`-race`), Cassandra
  integration (testcontainers), Docker build (proves librados linking), and a full e2e via `smoke.sh` +
  `smoke-signed.sh` + RADOS integration tests.
- `s3-tests` pass-rate is the headline compatibility number. After meaningful surface changes, re-run
  `scripts/s3-tests/run.sh` and update `scripts/s3-tests/README.md` baseline section.

## Ralph autonomous runs

`scripts/ralph/` contains a Ralph loop runner that drives `claude --print` (or Amp) through `scripts/ralph/prd.json`. It
commits per story and writes `progress.txt`. `scripts/ralph/CLAUDE.md` is Ralph's *task prompt* — do not put project
knowledge there. This root `CLAUDE.md` is the project memory and is auto-loaded by every Claude Code invocation,
including Ralph's. Update this file (not Ralph's) when you discover something a future iteration should know.
