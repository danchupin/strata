# PRD: Dynamic RADOS Cluster Registry + Zero-Downtime Add

## Introduction

Today the gateway loads the RADOS cluster set ONCE from the
`STRATA_RADOS_CLUSTERS` environment variable at startup via
`internal/serverapp/serverapp.go::buildDataBackend` → `datarados.ParseClusters`
→ `Backend.clusters` map. Adding a new RADOS cluster requires a full gateway
restart. US-044 of the prior cycle shipped the multi-cluster connection map
(`b.connFor`, `b.ioctx`) but not its lifecycle.

This PRD covers persisting the cluster catalogue in `meta.Store` (new
`ClusterRegistry` CRUD across the memory + Cassandra + TiKV backends),
exposing admin API endpoints to register / list / de-register clusters at
runtime, wiring a polling watcher on `rados.Backend` that hot-reloads the
connection pool, and **fully retiring the `STRATA_RADOS_CLUSTERS` env var
as a config source**. Registry is the SINGLE source of truth — no
two-source confusion at boot.

Operator workflow: `POST /admin/v1/storage/clusters` registers a new
cluster; within one poll interval (30 s default) the gateway lazy-dials on
first traffic; `DELETE` drains within one poll interval + closes the cached
connection. Bootstrap = POST first cluster via admin API after gateway
starts (admin API works without RADOS). No existing deployments to
migrate — no migration tooling.

Per-storage-class routing via the existing `[rados] classes` mapping +
`ClassSpec.Cluster` continues to work — the cluster id resolves against
the live `Backend.clusters` map at request time.

S3-backend half is deferred: the S3 data backend is single-bucket-per-instance
today; per-instance lifecycle is a separate P2 follow-up. The registry surface
ships in this cycle; the S3 watcher consumer does not.

Closes ROADMAP P2 'Dynamic RADOS / S3 cluster registry + zero-downtime add.'
on cycle close.

## Goals

- Persist cluster catalogue across all three meta backends (memory + Cassandra + TiKV) via a single `meta.Store.ClusterRegistry*` API surface
- Expose admin HTTP endpoints `GET /admin/v1/storage/clusters`, `POST /admin/v1/storage/clusters`, `DELETE /admin/v1/storage/clusters/{id}`
- Format-only validation on POST (id pattern + JSON shape + backend ∈ {rados, s3}); no probe-dial
- DELETE refuses if the cluster id is referenced by any `ClassSpec.Cluster` in the running config → 409 `ClusterReferenced` with the referencing class names in the body
- Wire `rados.Backend` registry watcher that polls `meta.ListClusters` every `STRATA_CLUSTER_REGISTRY_INTERVAL` (default 30 s, range [5 s, 5 m]) and reloads the in-memory `Backend.clusters` map
- **Retire `STRATA_RADOS_CLUSTERS` env var entirely.** Registry is the single source of truth
- Closes ROADMAP P2 on cycle close

## User Stories

### US-001: meta.Store ClusterRegistry CRUD + memory backend
**Description:** As a developer, I want `meta.Store` to expose
`ListClusters / GetCluster / PutCluster / DeleteCluster` on the memory
backend so the admin API + watcher have a persistent catalogue source.

**Acceptance Criteria:**
- [ ] Add `ClusterRegistryEntry` struct to `internal/meta/store.go`: fields `ID string`, `Backend string` (`"rados"` / `"s3"`), `Spec []byte` (opaque JSON-encoded config), `CreatedAt time.Time`, `UpdatedAt time.Time`, `Version int64` (monotonic counter for CAS-on-update)
- [ ] Add to `meta.Store` interface: `ListClusters(ctx) ([]*ClusterRegistryEntry, error)`, `GetCluster(ctx, id string) (*ClusterRegistryEntry, error)`, `PutCluster(ctx, e *ClusterRegistryEntry) error` (insert or CAS-update on Version), `DeleteCluster(ctx, id string) error`
- [ ] Sentinel errors: `ErrClusterNotFound`, `ErrClusterVersionMismatch`
- [ ] Implement on `internal/meta/memory/store.go` under the existing per-store mutex; `PutCluster` validates Version-on-update == stored.Version, bumps Version, stamps UpdatedAt
- [ ] Add `caseClusterRegistry` to `internal/meta/storetest/contract.go`: insert two clusters → List returns both sorted by ID → Get returns one → Put with stale Version returns `ErrClusterVersionMismatch` → Delete + Get returns `ErrClusterNotFound` → List-empty returns nil slice (NOT error)
- [ ] Memory backend test (`internal/meta/memory/store_test.go`) wires `caseClusterRegistry`
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: Cassandra backend ClusterRegistry CRUD
**Description:** As a developer, I want the Cassandra meta backend to persist
the cluster registry so a multi-replica gateway sees the same catalogue.

**Acceptance Criteria:**
- [ ] Add `cluster_registry` table to `internal/meta/cassandra/schema.go::tableDDL`: `CREATE TABLE IF NOT EXISTS cluster_registry (id text PRIMARY KEY, backend text, spec blob, created_at timestamp, updated_at timestamp, version bigint)`. Idempotent
- [ ] Implement `ListClusters / GetCluster / PutCluster / DeleteCluster` on `internal/meta/cassandra/store.go`. `PutCluster` uses LWT `IF NOT EXISTS` for insert and `IF version = ?` for update — LWT-on-LWT pattern (per CLAUDE.md gotcha)
- [ ] `PutCluster` on stale Version returns `meta.ErrClusterVersionMismatch`
- [ ] Cassandra integration test (`internal/meta/cassandra/store_integration_test.go`, build tag `integration`) runs `caseClusterRegistry` against testcontainers Cassandra
- [ ] Typecheck passes
- [ ] Tests pass (memory contract still green; `make test-integration` green)

### US-003: TiKV backend ClusterRegistry CRUD
**Description:** As a developer, I want the TiKV meta backend to persist
the cluster registry so the TiKV-backed gateway has parity.

**Acceptance Criteria:**
- [ ] Add cluster_registry key encoding to `internal/meta/tikv/keys.go`: prefix `clusters/<id>` (variable-length string segment via FoundationDB-style byte-stuffing per `keys.md`). Schema-additive
- [ ] Implement `ListClusters / GetCluster / PutCluster / DeleteCluster` on `internal/meta/tikv/`. `PutCluster` uses pessimistic txn (`Begin → LockKeys → Get → CAS-on-Version → Set → Commit`) — same shape as `SetBucketVersioning` (per CLAUDE.md gotcha)
- [ ] Early-return CAS-reject path calls `txn.Rollback()` explicitly — no LockKeys lease leak (per CLAUDE.md gotcha)
- [ ] TiKV contract test passes (`caseClusterRegistry` runs against all 3 backends)
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: Admin API GET/POST/DELETE /admin/v1/storage/clusters
**Description:** As an operator, I want HTTP endpoints to list, register,
and de-register cluster catalogue entries so I can add a new RADOS cluster
without restarting the gateway.

**Acceptance Criteria:**
- [ ] Add `internal/adminapi/storage_clusters.go` with three handlers wired into the router: `handleListClusters` (GET), `handleCreateCluster` (POST), `handleDeleteCluster` (DELETE)
- [ ] POST request body shape: `{id, backend, spec}` JSON. Validates `backend ∈ {rados, s3}`, `id` matches `[a-z0-9-]{1,64}`, `spec` is well-formed JSON and non-empty. Format-only — no dial-test
- [ ] POST conflict (id already exists) → 409 `ClusterAlreadyExists` (new APIError sentinel)
- [ ] DELETE on missing → 404 `NoSuchCluster`
- [ ] DELETE on cluster referenced by a `ClassSpec.Cluster` in the running config → 409 `ClusterReferenced` with response body `{referenced_by: ["STANDARD", "COLD"]}`. Use the in-memory `Backend.classes` map snapshot — no manifest scan
- [ ] GET list returns `[{id, backend, created_at, updated_at, spec}]` sorted by id
- [ ] Audit-log integration: handlers call `s3api.SetAuditOverride(ctx, action, resource, bucket, principal)` with `action="admin:CreateCluster"|"DeleteCluster"|"ListClusters"`, `resource="cluster:<id>"`, `bucket="-"` (per CLAUDE.md admin audit pattern)
- [ ] OpenAPI contract `internal/adminapi/openapi.yaml` updated with the three endpoints
- [ ] Unit tests `internal/adminapi/storage_clusters_test.go`: happy path (POST → GET → DELETE → GET-after-delete = 404); validation (malformed JSON → 400, unknown backend → 400, id pattern → 400); conflicts (POST-twice → 409, DELETE on referenced → 409 with `referenced_by`)
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: RADOS Backend registry watcher + hot-reload
**Description:** As an operator, I want the running `rados.Backend` to
pick up cluster catalogue changes within one poll interval — lazy-dial on
add, safe-drain on remove — without restarting the gateway.

**Acceptance Criteria:**
- [ ] Add `internal/data/rados/registry_watcher.go`: `RegistryWatcher` struct wraps `meta.Store` reference; `Start(ctx)` spawns a polling goroutine (every replica polls — diff is idempotent, no leader needed). Poll cadence via `STRATA_CLUSTER_REGISTRY_INTERVAL` (default 30 s, clamped [5 s, 5 m]) read once at watcher construction
- [ ] On each tick: compute set-diff against `Backend.clusters` map under `b.mu`. Added clusters → merge spec, `connFor` lazy-dials on next traffic. Removed clusters → close cached conn + ioctxes under lock, remove from map. Updated specs (same id, bumped Version) → replace + re-dial on next traffic
- [ ] Watcher honours `ctx.Done` — exits cleanly on `Backend.Close`
- [ ] Add `Close(ctx) error` to `data.Backend` interface (idempotent; memory backend Close is noop; rados backend Close drains pool + stops watcher; s3 backend Close drops the client)
- [ ] Initial sync: `rados.New(cfg)` fetches the registry once synchronously via `meta.ListClusters`, populates `Backend.clusters` from it. If registry empty, `Backend.clusters` is empty; subsequent RADOS traffic fails fast with `unknown cluster` until operator POSTs at least one entry. Gateway starts cleanly + admin API works
- [ ] Wire `cfg.Meta` in `internal/serverapp/serverapp.go::buildDataBackend` (after `buildMetaStore` runs first)
- [ ] Unit test `internal/data/rados/registry_watcher_test.go`: fake `meta.Store` returns 3 clusters → tick → `backend.clusters` has 3 entries. Fake returns 2 (one removed) → tick → 2 entries, removed-cluster's cached conn closed. Fake returns 4 (one added) → tick → 4 entries. Race-detector clean
- [ ] Goroutine leak test: `Backend.Close(ctx)` cancels the watcher; assert no leak via `runtime.NumGoroutine()` baseline + delta within tolerance
- [ ] Prometheus counter `strata_cluster_registry_changes_total{op="add"|"remove"|"update"}` exported on the default registry — incremented on each watcher reconciliation
- [ ] Reference doc `docs/site/content/reference/_index.md` gains `STRATA_CLUSTER_REGISTRY_INTERVAL` entry
- [ ] Typecheck passes
- [ ] Tests pass

### US-006: Retire STRATA_RADOS_CLUSTERS env path
**Description:** As a developer, I want the legacy env-based cluster config
fully removed so there is exactly ONE source of truth — the registry.

**Acceptance Criteria:**
- [ ] Remove `Clusters` field from `internal/config/config.go` `Config.RADOS` struct AND the `"STRATA_RADOS_CLUSTERS": "rados.clusters"` entry from the koanf env-map
- [ ] Remove `cfg.RADOS.Clusters` argument from `datarados.New(...)` call in `internal/serverapp/serverapp.go::buildDataBackend`
- [ ] Remove `ParseClusters` / `BuildClusters` / `ValidateClusterRefs` from `internal/data/rados/clusters.go` if they no longer have callers; keep the `ClusterSpec` struct (still used by the registry + ioctx path)
- [ ] Update existing tests + examples that lean on `STRATA_RADOS_CLUSTERS` to pre-seed the in-memory ClusterRegistry via `meta.Store.PutCluster` directly
- [ ] Update `deploy/docker/docker-compose.yml` + `deploy/docker/docker-compose.ci.yml` to drop `STRATA_RADOS_CLUSTERS` from the strata service env; document the new bootstrap path (POST via admin API after gateway start)
- [ ] Update `scripts/s3-tests/run.sh` + `scripts/s3-tests/README.md` if they reference the env var; switch to admin API POST in the smoke harness
- [ ] `make smoke` still passes — smoke harness uses admin API POST in startup hook to register the test cluster
- [ ] Typecheck passes
- [ ] Tests pass

### US-007: Docs + ROADMAP close-flip
**Description:** As a developer, I want operator-facing docs + the ROADMAP
P2 entry flipped in the same commit when the cycle closes.

**Acceptance Criteria:**
- [ ] Add `docs/site/content/best-practices/dynamic-clusters.md` covering: catalogue shape (`ClusterRegistry` schema), admin API curl examples (POST register, DELETE drain, GET list), watcher behaviour (poll interval, lazy-dial-on-add, safe-drain-on-remove), bootstrap workflow (POST first cluster via admin API after gateway starts), DELETE-when-referenced behaviour (409 `ClusterReferenced`), per-storage-class routing via `[rados] classes` mapping + `ClassSpec.Cluster`, audit + observability cross-links
- [ ] Update `docs/site/content/best-practices/_index.md` table with the new page row
- [ ] Reference doc `docs/site/content/reference/_index.md` already gained `STRATA_CLUSTER_REGISTRY_INTERVAL` in US-005; add admin API curl examples here too; REMOVE `STRATA_RADOS_CLUSTERS` row
- [ ] ROADMAP.md close-flip in the same commit per CLAUDE.md Roadmap maintenance rule: `~~**P2 — Dynamic RADOS / S3 cluster registry + zero-downtime add.**~~ — **Done.** ... (commit `<pending>`)`
- [ ] File a new P2 ROADMAP entry 'S3 backend per-instance lifecycle + cluster registry consumer' BEFORE flipping the headline (per CLAUDE.md 'Discovering a new gap' rule) — S3 watcher consumer is intentionally deferred
- [ ] Delete `tasks/prd-dynamic-clusters.md` per CLAUDE.md PRD lifecycle rule (Ralph snapshot is the canonical record)
- [ ] `make docs-build` clean (Hugo strict-ref resolution catches dangling refs)
- [ ] Closing-SHA backfill follow-up commit on main per established pattern
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- FR-1: `meta.Store` exposes `ListClusters / GetCluster / PutCluster / DeleteCluster` with `ErrClusterNotFound` and `ErrClusterVersionMismatch` sentinels; all three production backends (memory, Cassandra, TiKV) implement them
- FR-2: `PutCluster` enforces CAS-on-Version: stale Version → `ErrClusterVersionMismatch`; first-insert leaves Version=1; subsequent updates bump
- FR-3: Admin API `GET /admin/v1/storage/clusters` returns the catalogue as a JSON array sorted by id; `POST` accepts `{id, backend, spec}` and validates format-only; `DELETE /admin/v1/storage/clusters/{id}` removes
- FR-4: `POST` returns 409 `ClusterAlreadyExists` on id collision; `DELETE` returns 404 `NoSuchCluster` on missing; `DELETE` returns 409 `ClusterReferenced` with `{referenced_by: [...]}` body when the cluster id appears in any in-memory `ClassSpec.Cluster`
- FR-5: `rados.Backend.RegistryWatcher` polls `meta.ListClusters` every `STRATA_CLUSTER_REGISTRY_INTERVAL` (default 30 s, range [5 s, 5 m]) and reconciles the in-memory `Backend.clusters` map: add (insert + lazy-dial), remove (close cached conn + ioctxes + remove from map), update (replace + re-dial on next traffic)
- FR-6: Registry is the SOLE source of cluster config. `STRATA_RADOS_CLUSTERS` env var is removed from the config layer; gateway with empty registry starts cleanly but cannot serve RADOS traffic until an operator POSTs at least one cluster (admin API + meta layer work without RADOS)
- FR-7: Admin handlers stamp audit override `action=admin:<Verb>`, `resource=cluster:<id>` per CLAUDE.md admin audit pattern
- FR-8: `data.Backend` interface gains idempotent `Close(ctx) error`; rados backend closes the watcher + drains the connection pool
- FR-9: Prometheus counter `strata_cluster_registry_changes_total{op="add"|"remove"|"update"}` exported

## Non-Goals

- **No probe-dial on POST.** Validation is format-only; broken specs surface as runtime errors on first traffic
- **No S3-backend watcher consumer.** Separate P2 follow-up
- **No leader-elected watcher.** Every replica polls — diff is idempotent
- **No streaming watcher / WATCH primitive.** Plain poll loop
- **No per-storage-class admin write API.** `[rados] classes` env-mapped routing still requires restart to change. Separate P3 follow-up if requested
- **No chunk-refcount scan on DELETE.** Only `ClassSpec.Cluster` references checked
- **No automatic rebalance on cluster add/remove.** Separate P2 entry in ROADMAP
- **No migration tooling.** No existing deployments to migrate
- **No env-based fallback.** Registry is the SOLE source

## Technical Considerations

### Single source of truth

`STRATA_RADOS_CLUSTERS` env var is fully removed in US-006. Gateway reads
cluster config exclusively from `meta.ClusterRegistry` via the watcher.
Bootstrap = POST first cluster via admin API after the gateway starts
(admin API works without any RADOS cluster — it only needs meta).

### Cluster catalogue persistence shape

`ClusterRegistryEntry.Spec []byte` is opaque JSON, registry-agnostic. RADOS
decodes via `json.Unmarshal(e.Spec, &rados.ClusterSpec{})`. Allows S3 to use
its own spec shape later without registry-side churn.

### CAS-on-Version concurrency model

`PutCluster` uses CAS-on-Version (LWT for Cassandra, pessimistic txn for
TiKV, mutex for memory). Two concurrent operators racing a POST → one
succeeds with Version=1, other gets `ErrClusterAlreadyExists`. Two operators
racing an UPDATE → one succeeds with Version=N+1, other gets
`ErrClusterVersionMismatch` and retries.

### Watcher cadence

Every replica polls independently. 30 s default. N replicas × poll =
N × (1 list-call / 30 s) — negligible meta load. Diff is idempotent.

### Lazy-dial on add

`connFor` already lazy-dials on first use. Watcher add-path merges into
`Backend.clusters`; no eager dial. First traffic to the new cluster pays
the dial cost.

### Safe-drain on remove

`Backend.clusters` removal happens under `b.mu`. Cached conn / ioctx for
the removed id are closed in the same critical section. In-flight requests
that already grabbed an `*goceph.IOContext` reference continue to use it
(IOContext is goroutine-safe per ceph docs); subsequent `ioctx(...)` calls
for the removed cluster fail fast with `unknown cluster`.

### Cold-start with empty registry

Gateway start with zero entries in `meta.ClusterRegistry` is legal: gateway
process starts, admin API + readiness/liveness probes work, RADOS-bound
S3 requests fail fast with `503 ServiceUnavailable` (no cluster mapped
for the requested storage class). Operator POSTs the first cluster via
admin API; within one poll interval the gateway lazy-dials on first
RADOS-bound request.

### S3-backend deferral

S3 backend is single-bucket-per-instance today. To consume the registry
the backend would need to become a `map[id]*Backend` registry itself —
non-trivial refactor. Out of scope; file as separate P2 follow-up.

## Success Metrics

- Operator can register a new RADOS cluster via `POST /admin/v1/storage/clusters` and see traffic land on it within `STRATA_CLUSTER_REGISTRY_INTERVAL` (30 s default) without restarting the gateway
- Operator can `DELETE` an unreferenced cluster and the cached conn/ioctx close within one poll interval
- `DELETE` on a referenced cluster returns 409 with the offending class names — no silent loss
- Cassandra integration test + TiKV contract test green; memory contract test green; race detector clean across all touch points

## Open Questions

- Should the watcher emit per-cluster `up/down` gauges in addition to the change counter? Recommended for completeness; defer to US-005 implementer
- 5-minute upper bound on poll interval — too long for some operators? Range [5 s, 5 m] feels conservative; raise upper to 30 m if needed. Defer to operator feedback
