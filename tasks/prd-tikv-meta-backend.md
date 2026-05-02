# PRD: TiKV as a First-class Metadata Backend

## Introduction

Strata's `internal/meta.Store` interface today has three implementations: an
in-memory backend (test/dev only), a Cassandra backend (primary production
path; ScyllaDB is a drop-in via gocql), and the contract test suite that
keeps both backends in lockstep. The interface itself is intentionally
Cassandra-flavoured — LWT (`IF EXISTS` / `IF NOT EXISTS`) for read-after-
write coherence, clustering-ordered reads (`(bucket_id, shard, key,
version_id DESC)`), 64-way fan-out paging on `ListObjects`, schema-additive
ALTERs.

This shape leaves real performance on the table. The 64-way fan-out is a
workaround for the fact that Cassandra cannot do native ordered range scans
across partitions. A backend that *can* — TiKV, FoundationDB, TiDB,
Postgres+Citus — would resolve `ListObjects` in one scan instead of 64.

This PRD adds **TiKV as a first-class metadata backend on equal footing
with Cassandra**. Both become "primary production" paths. The picker —
`STRATA_META_BACKEND={memory,cassandra,tikv}` — stays operator-driven.
Cassandra remains the right choice for shops with existing Cassandra ops;
TiKV becomes the right choice for shops who want native range scans, single-
binary ops surface (one storage layer, no separate KV+SQL split), and
PD-managed auto-rebalancing.

To make the picker honest, this PRD also introduces a small optional-
interface — `RangeScanStore` — that the gateway type-asserts against to
pick the better path on `ListObjects` when available, falling back to the
fan-out / heap-merge default when not. Cassandra explicitly does *not*
implement the optional interface (keeps fan-out — the only shape it can
serve efficiently). Memory and TiKV implement it.

This PRD also explicitly **drops** the previously-listed community-tier
candidates from `ROADMAP.md` — FoundationDB, PostgreSQL+Citus, generic
TiDB. The supported set becomes: memory + cassandra (with scylla drop-in)
+ tikv. No additional community backends. Smaller surface, deeper
guarantees on each.

## Goals

- New `internal/meta/tikv/` package implementing the full `meta.Store`
  interface, equal-tier with the Cassandra backend
- TiKV chosen over TiDB (no SQL layer overhead — Strata's meta surface is
  pure KV: Get / Put / CAS / Range), TiKV's two-layer ops surface (PD +
  TiKV) is closer to "one storage to operate" than TiDB's three layers
- New optional `RangeScanStore` interface in `internal/meta/store.go`;
  memory + tikv implement it; cassandra does not. Gateway type-asserts at
  `ListObjects` time
- Full `internal/meta/storetest/contract.go` suite passes against TiKV
  (testcontainers); all surface-equivalent semantics — IAM, multipart,
  versioning, lifecycle, audit, queues, leader election, reshard
- New CI workflow `.github/workflows/ci-tikv.yml` mirroring the existing
  `ci-scylla.yml` job shape
- Race harness (this PRD assumes `prd-race-harness.md` has shipped or is
  shipping in parallel) covers TiKV as a separate run
- New `docs/backends/tikv.md` operator guide
- ROADMAP.md cleanup — promote TiKV to equal-tier with Cassandra; drop
  FDB / Postgres+Citus / generic-TiDB from the alternative-metadata-
  backends section
- No migration path from Cassandra to TiKV in this PRD — new deployments
  only. Existing Cassandra deployments stay on Cassandra

## User Stories

### US-001: tikv/client-go dep + `internal/meta/tikv` skeleton
**Description:** As a developer, I want a starting `internal/meta/tikv`
package that compiles, satisfies the `meta.Store` interface with stub
implementations, and is reachable via the existing backend factory so
later stories can fill in real implementations.

**Acceptance Criteria:**
- [ ] Add `github.com/tikv/client-go/v2` to `go.mod`. Run `go mod tidy`
- [ ] New `internal/meta/tikv/store.go` with `type Store struct{}` and
      stub methods returning `errors.ErrUnsupported`. Methods conform to
      `meta.Store`
- [ ] New `internal/meta/tikv/store_test.go` with one happy-path unit
      test asserting the stub is wired (calls `Probe`, expects
      `ErrUnsupported`)
- [ ] No production dispatch yet — `STRATA_META_BACKEND=tikv` is
      reserved but not selectable until US-016
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: Key encoding scheme + namespace prefix design
**Description:** As a developer, I need a documented key-encoding scheme
that maps Strata's compound keys (bucket-id + shard + object-key + version-
id) onto TiKV's flat byte-keyed KV store, so every later story uses the
same convention.

**Acceptance Criteria:**
- [ ] New file `internal/meta/tikv/keys.go` exposes:
  - `func ObjectKey(bucketID uuid.UUID, key string, versionID string) []byte`
    (lex-sorted by `(bucket_id, key, version_id-DESC)` — version-id-DESC
    via two's-complement of timestamp prefix)
  - `func BucketKey(bucketID uuid.UUID) []byte`
  - `func MultipartKey(bucketID uuid.UUID, key, uploadID string) []byte`
  - `func PrefixForBucket(bucketID uuid.UUID) []byte` (range-scan start)
  - `func IAMUserKey(accessKey string) []byte`
  - …and so on for every `meta.Store` entity
- [ ] Top-level prefix `s/` (single byte for namespace; cheap to keep
      out of the way of other tenants on shared TiKV)
- [ ] All keys round-trip via decoder: encode → decode produces input
      back. Property-test 1k random inputs
- [ ] Document the format in a `internal/meta/tikv/keys.md` so future
      contributors do not invent a parallel convention
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: Bucket CRUD + versioning state
**Description:** As Strata's gateway, I want to Create, Get, Delete,
List, and update bucket-level state on TiKV with the same semantics
Cassandra provides today (LWT-equivalent for create-if-not-exists,
update-if-versioning-state).

**Acceptance Criteria:**
- [ ] `Store.CreateBucket` uses TiKV pessimistic txn `Get(bucketKey)` +
      `Put` with conflict → returns `meta.ErrBucketExists` on conflict
      (Cassandra LWT equivalent)
- [ ] `Store.GetBucket` is a single Get
- [ ] `Store.DeleteBucket` is a pessimistic txn that asserts bucket is
      empty (no objects under prefix) before delete; returns
      `ErrBucketNotEmpty` otherwise
- [ ] `Store.ListBuckets` is a range scan over bucket-prefix
- [ ] `Store.SetBucketVersioning` uses pessimistic txn (`READ_FOR_UPDATE`
      semantics) — must NOT use plain `Put` because read-after-write
      coherence requires it (mirrors the Cassandra LWT lesson)
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: Object CRUD with version-id-DESC ordering
**Description:** As Strata, I want object writes / reads / deletes against
TiKV that produce the same observable behaviour as the Cassandra path —
including correct ordering of versions (latest first) for `ListObjectVersions`
and HEAD-of-clustering-row reads.

**Acceptance Criteria:**
- [ ] `Store.PutObject` is a single Put against `ObjectKey(bucketID, key,
      versionID)`
- [ ] `Store.GetObject` (no `versionID`) uses range scan with `Limit(1)`
      over `Prefix(bucketID, key)` — returns the lex-first key, which
      under our encoding is the latest version
- [ ] `Store.GetObject` (with `versionID`) is a single Get
- [ ] `Store.DeleteObject` removes the row + creates a delete-marker row
      (versioned-bucket case)
- [ ] `Store.SetObjectStorage` (lifecycle CAS) uses pessimistic txn —
      asserts `current.StorageClass == expected` before flipping; returns
      `applied=false` on conflict (mirrors Cassandra-CAS API)
- [ ] Conditional PUT (`If-Match`/`If-None-Match`) maps to pessimistic
      txn read + asserted Put
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: ListObjects + ListObjectVersions via native range scan
**Description:** As Strata, I want listing operations against TiKV to
use a single ordered range scan instead of the 64-way fan-out the
Cassandra path needs.

**Acceptance Criteria:**
- [ ] `Store.ListObjects(ctx, bucketID, prefix, marker, delimiter, limit)`
      issues one TiKV range scan with `start = ObjectKey(bucketID, marker
      OR prefix)`, `end = ObjectKey(bucketID, prefix-upper-bound)` —
      no fan-out, no heap-merge
- [ ] Delimiter handling done in-process on the scan iterator — group
      keys into `CommonPrefixes` as keys are emitted
- [ ] `NextMarker` / `NextContinuationToken` is the last-emitted key; on
      next page, scan resumes from `marker + 0x00` (next-key)
- [ ] `Store.ListObjectVersions` similar shape, no version filtering
- [ ] Concurrent writes during scan produce results consistent with
      TiKV's snapshot isolation — every page is from the snapshot at
      scan-start time
- [ ] Performance benchmark: 100k-object bucket, page size = 1000.
      TiKV scan completes in <50 ms (vs Cassandra 64-way fan-out
      ~150-300 ms). Capture in US-019 benchmarks doc
- [ ] Typecheck passes
- [ ] Tests pass

### US-006: Multipart upload state lifecycle
**Description:** As Strata, I want multipart upload state (init / parts /
complete / abort) on TiKV with the same LWT-equivalent flip on Complete
that Cassandra provides today.

**Acceptance Criteria:**
- [ ] `Store.InitiateMultipartUpload` writes
      `MultipartKey(bucketID, key, uploadID)` with `status="uploading"`
- [ ] `Store.UploadPart` writes
      `MultipartKey(bucketID, key, uploadID, partNumber)` part rows
- [ ] `Store.CompleteMultipartUpload` is a pessimistic txn:
      `Get(multipartKey)` → assert `status=="uploading"` →
      `Put(multipartKey, status="completing")` (CAS-style flip) → on
      success, copy parts info into the final object row → `Delete` the
      multipart rows. On `status != "uploading"` returns
      `ErrMultipartInProgress` (mirrors Cassandra LWT behavior)
- [ ] `Store.AbortMultipartUpload` is a single delete of the multipart
      rows; idempotent
- [ ] `Store.ListMultipartUploads` is a range scan over multipart-prefix
- [ ] Concurrent Complete attempts: only ONE succeeds (LWT-equivalent
      via TiKV txn; tested in race harness — see US-017)
- [ ] Typecheck passes
- [ ] Tests pass

### US-007: All bucket-level config blobs via blob helper
**Description:** As Strata, I want every bucket-level XML/JSON config
endpoint (lifecycle, CORS, policy, public-access-block, ownership-controls,
notification, replication, encryption, accelerate, request-payment,
tagging, intelligent-tiering, inventory, website, logging, access-points,
versioning) to use a single shared blob-helper pattern on TiKV — same
shape as the Cassandra `setBucketBlob/getBucketBlob/deleteBucketBlob`
trio.

**Acceptance Criteria:**
- [ ] New `internal/meta/tikv/blobs.go` with
      `setBucketBlob(ctx, bucketID, kind, payload)`,
      `getBucketBlob(ctx, bucketID, kind) ([]byte, error)`,
      `deleteBucketBlob(ctx, bucketID, kind)` — keys
      `BucketBlobKey(bucketID, kind)`
- [ ] All `Store.{Set,Get,Delete}<Config>` methods route through the
      helper (just like Cassandra's `internal/meta/cassandra/buckets.go`
      pattern)
- [ ] Helper is tested once; per-config wrapper methods are thin
- [ ] Typecheck passes
- [ ] Tests pass

### US-008: IAM — users, access keys, policies
**Description:** As Strata, I want IAM entities (users, access keys,
attached policies, role bindings) backed by TiKV with the same
operations Cassandra provides today.

**Acceptance Criteria:**
- [ ] `Store.CreateIAMUser` / `GetIAMUser` / `DeleteIAMUser` / `ListIAMUsers`
- [ ] `Store.PutAccessKey` / `GetAccessKey` / `DeleteAccessKey` —
      access-key lookups (used by SigV4 verifier on every request) are
      hot-path; ensure single-Get latency
- [ ] `Store.AttachPolicy` / `DetachPolicy` / `ListAttachedPolicies` —
      uses range scan over per-user prefix
- [ ] `Store.PutManagedPolicy` / `GetManagedPolicy` / `ListManagedPolicies`
- [ ] All entity-creation operations use `CreateBucket`-style pessimistic
      txn for "create if not exists" semantics
- [ ] Typecheck passes
- [ ] Tests pass

### US-009: Audit log with retention sweep
**Description:** As Strata, I want the audit log on TiKV with the same
retention semantics Cassandra provides via `USING TTL` — except TiKV does
not have a native TTL, so this story implements a periodic sweeper.

**Acceptance Criteria:**
- [ ] `Store.AppendAudit(ctx, event)` writes
      `AuditKey(timestampNano, requestID)` with payload (timestamp prefix
      makes ordered scans cheap and sweeping efficient)
- [ ] `Store.ListAudit(ctx, bucketID, after, before, limit)` is a range
      scan filtered by bucket-id (filter in-process on the iterator)
- [ ] New `internal/meta/tikv/sweeper.go`: a goroutine started by
      `Store.Open` that runs every `STRATA_AUDIT_SWEEP_INTERVAL` (default
      1 h) and deletes audit rows where `timestampNano <
      now - STRATA_AUDIT_RETENTION` (default 30d)
- [ ] Sweeper is leader-elected (uses the existing `internal/leader`
      with key `audit-sweeper-leader-tikv`) so multiple Strata replicas
      do not race on deletion
- [ ] Sweeper observable via `strata_meta_tikv_audit_sweep_deleted_total`
      counter
- [ ] Typecheck passes
- [ ] Tests pass

### US-010: Worker queues + DLQ + access-log buffer
**Description:** As Strata's background workers (notify / replicator /
access-log / manifest-rewriter), I want their queue/state surfaces
backed by TiKV with the same ordering / claim / DLQ semantics the
Cassandra path provides.

**Acceptance Criteria:**
- [ ] `notify_queue` (notify worker), `replication_queue` (replicator),
      `access_log_buffer` (access-log), `manifestRewriter` state — all
      modelled as keyed prefixes with timestamp-ordered keys for FIFO
      claim semantics
- [ ] `Store.EnqueueNotify(event)` writes one row keyed on
      `(now-nano, requestID)`; ordered iteration claims oldest first
- [ ] `Store.ClaimNotifyBatch(workerID, limit)` is a range scan + per-row
      pessimistic txn `status: pending → claimed`. Claim TTL via row's
      `claim_expires_at` field; sweeper reclaims expired claims
- [ ] DLQ for notify uses `DLQ:<original-key>` prefix; can be queried
      via `Store.ListDLQ` for operator visibility
- [ ] Same shape mirrored for replication_queue + manifest-rewriter
      cursor; access_log_buffer rotates per-bucket on flush
- [ ] Typecheck passes
- [ ] Tests pass

### US-011: Reshard state + leader election + per-bucket inventory
**Description:** As Strata's online-shard-resize worker (US-045 family),
its leader-election locker, and the inventory worker, I want their state
on TiKV with the same primitives Cassandra provides today.

**Acceptance Criteria:**
- [ ] `internal/leader.Session` already abstracts; provide
      `internal/meta/tikv.Locker` (Cassandra has
      `internal/meta/cassandra.Locker`) backing the `Locker` interface.
      Lease via TiKV pessimistic txn with row TTL
- [ ] `Store.SetReshardState` / `Store.GetReshardState` — single-row
      blob, pessimistic-txn updates only (LWT-equivalent for state-machine
      transitions)
- [ ] `Store.PutInventoryConfig` / `Get` / `Delete` / `List` — same
      blob-helper pattern as US-007 but per (bucket-id, config-id) key
- [ ] `Store.SetReshardCursor(bucketID, shardID, cursor)` — per-shard
      progress tracker for the resize worker; pessimistic-txn updates
- [ ] Typecheck passes
- [ ] Tests pass

### US-012: `RangeScanStore` optional interface
**Description:** As an architecture maintainer, I want the gateway to be
able to discover at runtime whether the meta backend supports native
range scans, and use the better code path when available — without
forcing every backend to implement the same shape.

**Acceptance Criteria:**
- [ ] New optional interface in `internal/meta/store.go`:
  ```go
  type RangeScanStore interface {
      Store
      ScanObjects(ctx context.Context, bucketID uuid.UUID,
          prefix, marker, delimiter string, limit int) (*ListResult, error)
  }
  ```
- [ ] Memory backend: implement (wraps the existing in-memory tree map's
      iterator)
- [ ] TiKV backend: implement (single ordered range scan)
- [ ] Cassandra backend: explicitly does NOT implement (keeps the
      fan-out path — this is the only shape Cassandra serves
      efficiently). Document this decision in a comment on
      `cassandra.Store`
- [ ] Gateway `ListObjects` handler: type-assert
      `if rs, ok := store.(meta.RangeScanStore); ok { return rs.ScanObjects(...) }`
      else fall back to current `store.ListObjects(...)`. Single dispatch
      site
- [ ] Typecheck passes
- [ ] Tests pass

### US-013: storetest contract suite — TiKV factory
**Description:** As a maintainer, I want every test in
`internal/meta/storetest/contract.go` to pass against TiKV via
testcontainers, so we know surface-equivalence with Cassandra holds at
every commit.

**Acceptance Criteria:**
- [ ] New `internal/meta/tikv/store_integration_test.go` (build tag
      `integration`) starts a TiKV testcontainer (PD + TiKV in one
      compose file via dockertest or testcontainers-go), configures the
      client, runs `storetest.Run(t, factory)`
- [ ] All existing contract test cases pass without modification (or with
      capability-gated skips for tests that exercise Cassandra-specific
      semantics like `USING TTL` — those skip on TiKV, with
      `t.Skipf("backend does not support native TTL — see audit sweeper
      story")` and the audit-sweeper story has its own unit tests)
- [ ] Test runs in <5 min on CI runner (testcontainers TiKV start ~30 s,
      bench ~3 min, teardown ~30 s)
- [ ] Typecheck passes
- [ ] Tests pass

### US-014: docker-compose `tikv` profile + sidecars
**Description:** As a developer or CI runner, I want to bring up Strata
running on top of TiKV with one compose command.

**Acceptance Criteria:**
- [ ] `deploy/docker/docker-compose.yml` gains `pd` (PlacementDriver)
      and `tikv` services under profile `tikv`. Use upstream images
      `pingcap/pd:v8.x` and `pingcap/tikv:v8.x` (latest stable at
      authoring time)
- [ ] Single PD + single TiKV (production-shape PD ≥3 + TiKV ≥3 is
      operator concern; CI shape is single-node)
- [ ] `strata` service gains conditional env: when running under
      `--profile tikv`, sets `STRATA_META_BACKEND=tikv`,
      `STRATA_TIKV_PD_ENDPOINTS=pd:2379`. Mutually exclusive with
      `STRATA_META_BACKEND=cassandra` — picker validates exactly-one
- [ ] `make up-tikv` brings up `tikv-pd + tikv + ceph + strata` (no
      Cassandra) and waits for `/readyz`
- [ ] Typecheck passes (compose validation via `docker compose config -q`)
- [ ] Tests pass

### US-015: Smoke test + Strata wiring (`STRATA_META_BACKEND=tikv`)
**Description:** As a CI maintainer, I want `make smoke` and `make smoke-
signed` to work with the TiKV backend, and the gateway to dispatch through
the new `meta.Store` impl when `STRATA_META_BACKEND=tikv` is set.

**Acceptance Criteria:**
- [ ] `internal/serverapp.buildMetaStore` reads `STRATA_META_BACKEND`,
      dispatches to `internal/meta/tikv.Open(...)` when value is `tikv`
- [ ] Misconfiguration (empty `STRATA_TIKV_PD_ENDPOINTS`, unreachable
      PD) fails fast at startup with a clear message
- [ ] New `make smoke-tikv` target runs the existing smoke script
      against `make up-tikv` — all PUT / GET / DELETE / multipart / ACL
      / lifecycle paths exercised
- [ ] `make smoke-signed-tikv` runs the SigV4-signed variant
- [ ] Both pass with no regressions vs the Cassandra-backed smoke
- [ ] Typecheck passes
- [ ] Tests pass

### US-016: Race harness coverage against TiKV
**Description:** As a correctness maintainer, I want the race harness
(see `prd-race-harness.md`) to also run against the TiKV backend so
LWT-equivalent semantics are validated under sustained concurrent load.

**Acceptance Criteria:**
- [ ] `cmd/strata-racecheck` (from prd-race-harness.md) accepts
      `--meta-backend=tikv` (or env var) — the gateway it talks to is
      TiKV-backed
- [ ] `make race-soak-tikv` brings up the TiKV stack instead of the
      RADOS+Cassandra stack and runs the harness for 1 h
- [ ] Concurrent versioning flip + Complete + DeleteObjects races all
      produce the same observable behaviour as the Cassandra-backed
      run (zero inconsistencies)
- [ ] Nightly CI workflow gains a parallel `race-nightly-tikv` job
- [ ] Typecheck passes
- [ ] Tests pass

### US-017: CI matrix entry — `.github/workflows/ci-tikv.yml`
**Description:** As a maintainer, I want every PR to also exercise
the TiKV backend via the contract test + smoke pass on CI, so
regressions on the new backend surface on the same SHA.

**Acceptance Criteria:**
- [ ] New `.github/workflows/ci-tikv.yml` — mirrors
      `.github/workflows/ci-scylla.yml` shape
- [ ] Job 1: `integration-tikv` runs `go test -tags integration -timeout
      15m ./internal/meta/tikv/...` against testcontainers TiKV
- [ ] Job 2: `e2e-tikv` brings up `make up-tikv` + `make smoke-tikv`
      + `make smoke-signed-tikv`; uploads logs as artefact on failure
- [ ] Both run on `pull_request` and `push` to `main`; timeout 30 min
- [ ] Existing `ci.yml` and `ci-scylla.yml` jobs unchanged; this is
      purely additive
- [ ] Typecheck passes
- [ ] Tests pass

### US-018: Benchmarks doc — TiKV vs Cassandra hot-path latency
**Description:** As an operator picking a meta backend, I want
documented latency / throughput numbers comparing TiKV and Cassandra on
Strata's hot operations so the decision is data-driven.

**Acceptance Criteria:**
- [ ] New `docs/benchmarks/meta-backend-comparison.md` with measurements
      for: CreateBucket (LWT-equivalent), GetObject (single Get),
      ListObjects 100k-object bucket page=1000, CompleteMultipartUpload
      (LWT-flip), GetAccessKey (hot-path SigV4), audit append + retention
      sweep
- [ ] Numbers measured on a single laptop docker stack (3-node TiKV
      cluster + 3-node Cassandra cluster) — operator-runnable, not
      production-grade. Document the rig
- [ ] Methodology: 60 s warmup, 5 min measurement, 50 concurrent
      writers. Tool: existing `cmd/strata-racecheck` repurposed with
      `--measure-only --no-verify` flag
- [ ] Update `ROADMAP.md` "alternative metadata backends" introduction
      with the headline numbers (e.g. "TiKV: ListObjects-100k 50 ms;
      Cassandra: same workload 250 ms — 5x")
- [ ] Typecheck passes
- [ ] Tests pass

### US-019: docs/backends/tikv.md
**Description:** As an operator evaluating TiKV, I need a single-page
operator guide covering setup, ops topology, capability matrix, and
caveats.

**Acceptance Criteria:**
- [ ] New `docs/backends/tikv.md` (alongside existing
      `docs/backends/scylla.md`) with:
  - When to choose TiKV over Cassandra (decision matrix)
  - Required env vars + sample compose / Kubernetes config (TiKV
    operator on k8s, plain compose for laptop)
  - Production sizing: PD ≥3 (raft majority), TiKV ≥3 (default-3
    replication), Strata gateway as separate fleet
  - Capability matrix: native range scan (yes), TTL (no — sweeper),
    multi-DC (yes via TiKV regions), hot/cold tier (yes via PD label
    rules)
  - Performance characteristics: latency floor, throughput per node,
    cost model (TiKV is on-prem-friendly — no managed-cloud assumption)
  - Common operational pitfalls: PD leader split-brain on partition,
    Region replica placement labels, raft entry GC
- [ ] CLAUDE.md "Big-picture architecture" diagram updated:
      `meta.Store  memory | cassandra | tikv` (vs current
      `memory | cassandra`)
- [ ] CLAUDE.md "Cassandra gotchas" section gains a parallel "TiKV
      gotchas" subsection captured during US-001..US-016 work
- [ ] README.md "How to run" section gains
      `make up-tikv` as a 5th option
- [ ] Typecheck passes
- [ ] Tests pass

### US-020: ROADMAP cleanup — promote TiKV, drop FDB / Postgres / TiDB
**Description:** As a maintainer, I want `ROADMAP.md`'s "alternative
metadata backends" section honestly reflect the supported set after this
PRD: cassandra + tikv as primary (memory for tests). Drop the previously-
listed P3 community candidates that no longer have a path.

**Acceptance Criteria:**
- [ ] Edit `ROADMAP.md` "Alternative metadata backends" section:
  - Reword introduction: "Strata supports two production metadata
    backends: Cassandra (with ScyllaDB as a CQL-compatible drop-in)
    and TiKV. Both are first-class. Memory is for tests only"
  - Drop the bullets for TiKV-as-P3-community, FoundationDB,
    PostgreSQL+Citus/Yugabyte (those are no longer goals)
  - Keep "non-goals" section listing single-node-only stores
- [ ] Edit `ROADMAP.md` "Consolidation & validation" section: race
      harness P1 entry already accounts for TiKV (see US-016 of this
      PRD); update wording to mention both backends
- [ ] Add ROADMAP entry to "Consolidation & validation" closed list:
      `~~**P1 — TiKV first-class metadata backend.**~~ — **Done.**
      ... (commit pending)` after this PRD's last commit
- [ ] CLAUDE.md "alternative metadata backends" prose updated to
      mirror new ROADMAP wording (single source of truth)
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- FR-1: `internal/meta/tikv` package implements all of `meta.Store` with
  surface-equivalent semantics to the Cassandra backend; passes the full
  `internal/meta/storetest/contract.go` suite (capability-gated skips
  for native-TTL tests; sweeper covers the use case)
- FR-2: TiKV CRUD operations use the appropriate transaction shape:
  pessimistic txns for LWT-equivalent ("create if not exists",
  "update if state == X", versioning flip); single Get/Put for plain
  reads/writes
- FR-3: `meta.RangeScanStore` is an optional interface; gateway
  type-asserts at `ListObjects` time and uses the native scan when
  available, falling back to fan-out otherwise
- FR-4: TiKV-specific config is env-only:
  `STRATA_TIKV_PD_ENDPOINTS` (required, comma-list of PD addresses),
  `STRATA_TIKV_TIMEOUT_SEC` (default 30s),
  `STRATA_AUDIT_SWEEP_INTERVAL` (default 1h),
  `STRATA_AUDIT_RETENTION` (default 30d, shared with cassandra path)
- FR-5: Audit log retention is enforced by a leader-elected periodic
  sweeper goroutine that runs alongside the gateway (or as part of a
  dedicated worker if `STRATA_WORKERS=audit-sweeper`)
- FR-6: All worker queues (notify / replication / access-log /
  manifest-rewriter) and reshard state map onto TiKV with FIFO claim
  semantics, DLQ visibility, and per-row claim TTLs reclaimable by a
  sweeper
- FR-7: Leader election uses TiKV pessimistic txn with row TTL; same
  semantics as the Cassandra-backed Locker
- FR-8: `STRATA_META_BACKEND` validates and dispatches at startup
  (`memory` | `cassandra` | `tikv`); unknown values fail fast with a
  clear error
- FR-9: CI runs the storetest contract suite + smoke pass against
  TiKV on every PR alongside the existing cassandra/scylla matrix
- FR-10: Race harness coverage runs against TiKV nightly (mirrors
  the cassandra path)
- FR-11: ROADMAP.md and CLAUDE.md documentation reflect dual-primary
  backend status; TiKV "alternative metadata backends" section
  cleanup drops FDB / Postgres+Citus / generic-TiDB

## Non-Goals

- **No SQL layer (TiDB)**. The TiKV picker is intentional: Strata's
  meta surface is pure KV; a SQL layer would add latency without
  benefit. Operators who want SQL stay on Cassandra (CQL via gocql)
- **No migration path from Cassandra to TiKV.** This is for new
  deployments only. Strata-admin gains no `migrate-meta` subcommand
  in this PRD. If a customer asks, file as a separate P2 follow-up
- **No additional community backends.** FoundationDB,
  PostgreSQL+Citus/Yugabyte, generic TiDB are dropped from
  `ROADMAP.md`. The supported set is `memory | cassandra | tikv`.
  Adding new backends has high maintenance cost; we do not add them
  speculatively
- **No multi-meta mode (cassandra + tikv simultaneously, per-bucket
  policy).** One backend per Strata deployment. If a customer asks,
  file as P2 follow-up
- **No native TTL.** TiKV's lack of native row TTL is filled by an
  application-level sweeper. We do not vendor a TTL extension or
  advocate one
- **No PD-side scheduling tuning.** PD's auto-rebalancing is left at
  defaults; operator concern to tune
- **No raw-mode TiKV.** We use TxnKV (transactional) mode exclusively;
  raw-mode TiKV does not provide the LWT-equivalent semantics we need
- **No backend-side encryption integration.** Strata's existing SSE
  (envelope encryption at the data layer) is independent of meta
  backend; TiKV may have at-rest encryption configured by the operator,
  but Strata does not interact with it
- **No replication of meta across geographic regions** beyond what TiKV
  itself provides (Region replica placement labels). Cross-region
  active-active is operator concern

## Technical Considerations

### Why TiKV over TiDB
- **Match to our API.** Our `meta.Store` is pure KV (Get/Put/CAS/Range/
  Txn). TiKV's TxnKV API is a 1:1 fit. TiDB adds a SQL parser + planner
  on the hot path with no user-visible benefit
- **Operations.** TiKV is 2 layers (PD + TiKV); TiDB adds a 3rd. Less
  to operate
- **Latency.** TiDB's SQL layer adds 2-4 ms p50 vs raw TiKV. On
  Strata's hot path (every SigV4 verify hits AccessKey lookup), this
  matters
- **Schema management.** We don't need DDL — we encode keys and
  serialise blobs (manifest is protobuf already). TiDB's schema
  story is value-add only when the application is genuinely tabular
- **The reverse direction is open.** If a customer ever needs SQL-side
  inspection of meta, point a TiDB instance at the same TiKV cluster
  and write SQL views. TiKV does not foreclose that future

### LWT-equivalent semantics
- Cassandra LWT (`IF EXISTS`) uses Paxos round-trips; on Strata
  versioning flips, that's ~5-10 ms even on a co-located cluster
- TiKV's pessimistic transactions (Percolator-style 2PC) typically run
  3-5 ms on a co-located cluster
- Optimistic transactions are faster (1-2 ms) but retry on conflict;
  versioning flips are typically uncontended, so optimistic is fine
  there. CompleteMultipartUpload is contended (concurrent client
  retries) — pessimistic is the right choice
- Race harness (US-016) is the source-of-truth on whether semantics
  match. Discrepancies fail the harness; we fix them rather than
  weakening the interface

### Schema migrations
- Cassandra has `tableDDL` + `alterStatements` in
  `internal/meta/cassandra/schema.go` for additive evolution
- TiKV has no DDL — schema is implicit in the key-encoding scheme
  (US-002). Adding a new entity = adding a new key-prefix +
  encoder/decoder pair. The "migration" story is "old code deserialises
  rows it knows about and ignores rows it doesn't"
- Manifest blobs (which we already store in protobuf, see US-049 of
  prior cycle) carry their own schema-additive evolution; that's
  unchanged

### Backwards compatibility
- Existing Cassandra-backed deployments: untouched. The
  `STRATA_META_BACKEND=cassandra` code path is the default and
  continues to work
- Memory-backed deployments (tests/dev): minor changes from US-013
  (RangeScanStore optional interface implementation) — additive
- TiKV is a fresh impl; no migration concerns

### Why we don't keep FDB / Postgres / generic TiDB on the roadmap
- **FDB**: distinct ops profile (FDB cluster), high learning curve,
  zero customer demand recorded. Distinct enough from TiKV to require
  separate maintenance. Not worth one slot
- **Postgres+Citus / Yugabyte**: would require advisory-lock-based
  emulation of LWT (fragile) + manual sharding (we'd need to
  implement what TiKV does for free). Architectural mismatch
- **Generic TiDB**: pure SQL frontend over TiKV. Adds latency
  without benefit. Operators who want SQL inspection of TiKV-backed
  meta can run TiDB themselves on the side; Strata doesn't need to
  ship two paths

### Concurrent writes
- TiKV is strongly consistent (Percolator 2PC + Raft). Read-after-
  write coherence is automatic for txn reads
- Race harness exists to catch regressions where our application code
  doesn't use txns where it should — same role it plays for the
  Cassandra path

## Success Metrics

- All 20 stories shipped within one Ralph cycle on
  `ralph/tikv-meta-backend`
- `make smoke-tikv` and `make smoke-signed-tikv` pass on every PR
  (CI green)
- `internal/meta/storetest/contract.go` passes against TiKV with no
  test additions (capability-gated skips only for the native-TTL
  case)
- Race harness 1-h soak against TiKV produces zero inconsistencies
  (matches the Cassandra baseline)
- `docs/backends/tikv.md` is single-page, operator-actionable
- ROADMAP.md "alternative metadata backends" section reflects the
  supported set: `memory | cassandra | tikv`. No leftover community-
  tier slots
- Benchmark headline (US-018): TiKV ListObjects 100k-objects ≥3× faster
  than Cassandra fan-out

## Open Questions

(none — all decisions captured in Goals + Non-Goals)
