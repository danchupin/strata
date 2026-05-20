# PRD: TiKV meta backend — fill in 22 stubbed methods

## Introduction

Replace 22 `errors.ErrUnsupported` stubs in
`internal/meta/tikv/store.go` (lines 100-186) with prod-ready
implementations. Surfaced by `ralph/storage-correctness` US-010
smoke — `make smoke` against the TiKV-default lab fails at the
TAGGING leg (HTTP 500 `InternalError`) because
`s3api.putObjectTagging` → `Meta.SetObjectTags` returns the
sentinel; the gateway translates the unmapped backend error to 500.
Cassandra backend implements all 22 (smoke against `--profile
cassandra` passes the TAGGING leg).

**Background**: TiKV backend was built up incrementally via
`ralph/tikv-meta-backend` (US-001..US-015). US-001 landed the
skeleton with all 22 methods stub'd as "fill in subsequent
stories"; the cycle wrapped at US-015 (production routing)
before the "cold" S3 surface (tags, retention, legal-hold, access
points, SSE rewrap, replication status, grants, raw manifest)
returned for impl. Compile + integration tests stayed green
because the TiKV `storetest.Run` factory partially exercised the
stubbed methods (SetRewrapProgress / GetObjectManifestRaw /
CreateAccessPoint are referenced in `contract.go` but the lab
default flip to TiKV in `ralph/tikv-default-lab` did not run a
TAGGING smoke). US-010 storage-correctness was the first cycle to
exercise the TiKV TAGGING leg → caught the gap.

**Pre-launch product** per [Pre-launch no deploys] memory rule —
no backwards-compat shims, no migration backfills, hard cutovers
on schema changes.

Branch: `ralph/tikv-stubs`. Starts from `main`.

## Goals

- Replace all 22 stubs with real TiKV implementations matching
  the Cassandra backend's S3 semantics 1:1.
- Every new method MUST go through the shared
  `internal/meta/storetest/contract.go` factory so memory +
  Cassandra + TiKV all pass the same contract cases in lockstep.
- `make smoke` against TiKV-default lab MUST pass the TAGGING +
  retention legs (currently fails).
- `scripts/s3-tests/run.sh` against TiKV-default lab MUST not
  regress vs the Cassandra-default baseline.
- TiKV impl p99 latency for the new methods MUST stay within
  2× of the Cassandra baseline on a tag-heavy workload (capture
  the ratio in US-006 smoke). Worse than 2× spawns a follow-up
  perf P3.
- Close ROADMAP P2 entry `TiKV meta backend stubs 19 meta.Store
  methods` (note: actual count is **22**, not 19 — close-flip
  text MUST correct the count).
- Fix the ROADMAP formatting bug at line 415 where the P2 entry
  is currently merged into the prior P1 "Fixed" RADOS-probe
  block (visually one bullet, reads as two concatenated
  paragraphs). Split into a proper standalone bullet during
  close-flip.
- Extract a shared `internal/meta/tikv/keycodec.go` helper
  (FoundationDB byte-stuffing + JSON marshal/unmarshal
  primitives) in US-001 so US-002..US-004 reuse without drift.

## User Journey

Three personas covered:

- **Operator running TiKV-default lab + putting object tags via
  aws-cli.** Today: HTTP 500 InternalError, `make smoke` red.
  After cycle: 200 OK, smoke green, TiKV is a first-class lab
  default the way CLAUDE.md `## Compose shape` already documents.
- **Operator setting Object Lock retention on a TiKV-backed
  deployment.** Today: 500. After cycle: AWS-parity retention
  flow (PUT/GET/expiry).
- **Operator wiring SSE master-key rotation against a TiKV
  meta backend.** Today: `strata admin rewrap` partially works
  but rewrap_progress is unwritable on TiKV. After cycle: full
  rewrap lifecycle including progress checkpoint.

## User Stories

### US-001: Shared key codec + object tags + object grants

**Description:** As an S3 client, I want PUT/GET/DELETE for
object tags and per-object ACL grants on a TiKV-backed
deployment. This story also lands the shared
`internal/meta/tikv/keycodec.go` helper that US-002..US-005
reuse so the FoundationDB byte-stuffing pattern stays consistent
across implementations.

**Acceptance Criteria:**
- [ ] New file `internal/meta/tikv/keycodec.go` exporting:
      - `PackKey(prefix string, segments ...interface{}) []byte`
        — accepts string / []byte / `uuid.UUID` segments; applies
        FoundationDB byte-stuffing (`0x00 → 0x00 0xFF`,
        terminator `0x00 0x00`) to variable-length string +
        []byte segments per CLAUDE.md "Variable-length string
        segments in keys use FoundationDB-style byte-stuffing"
        gotcha. UUID segments encoded as raw 16 bytes (no
        stuffing — fixed length).
      - `MarshalBlob(v interface{}) ([]byte, error)` — wraps
        `json.Marshal` with consistent encoding.
      - `UnmarshalBlob(data []byte, v interface{}) error` —
        wraps `json.Unmarshal`.
      - Unit tests in `keycodec_test.go` covering edge cases:
        empty segments, segments containing 0x00, UUID encoding,
        round-trip.
- [ ] 5 TiKV impls in `internal/meta/tikv/store.go` using
      `keycodec.PackKey` + `MarshalBlob` / `UnmarshalBlob`:
      `SetObjectTags` (line 120), `GetObjectTags` (124),
      `DeleteObjectTags` (128), `SetObjectGrants` (112),
      `GetObjectGrants` (116).
- [ ] **Key shape** (FoundationDB-style byte-stuffing per
      CLAUDE.md TiKV gotchas):
      `object_meta:<bucket-uuid-raw-16>:<key-fdbstuffed>:<version-id-or-null-sentinel>:<kind>`
      where `kind ∈ {tags, retention, legal_hold, restore_status, grants}`.
      Version-id null sentinel matches the existing `objects`
      table convention for null-versionId resolution.
- [ ] **Payload**: JSON-blob (`mapstructure`-friendly Go struct →
      `json.Marshal`). Match the `ec_policy` precedent from
      `ralph/storage-correctness` US-007.
- [ ] **Write path**: pessimistic txn per CLAUDE.md gotcha
      "Plain Put on a key with prior LWT history breaks read-
      after-write". `Begin(pessimistic) → LockKeys → Get →
      Set/Delete → Commit`. Explicit `txn.Rollback()` on every
      non-error early return (CLAUDE.md gotcha "Pessimistic txns
      with EARLY-RETURN paths must call txn.Rollback() explicitly").
- [ ] **Read path**: pessimistic txn NOT required —
      `s.kv.Get(ctx, key)` is sufficient for read-only fetches.
- [ ] **Delete semantics**:
      - `DeleteObjectTags(bucket, key, version)` → delete the
        `kind=tags` key only; other kinds untouched.
      - `SetObjectGrants` / `SetObjectTags` with empty
        collection writes an empty-list / empty-map JSON blob
        (do NOT delete the row — empty != absent per S3 semantics).
- [ ] **Cassandra parity** (verify against
      `internal/meta/cassandra/store.go` impls): same return
      shapes, same error sentinels (`meta.ErrObjectNotFound`
      when version doesn't exist), same null-handling for
      empty tag-map / empty grants list.
- [ ] **Contract test additions** in
      `internal/meta/storetest/contract.go`:
      For each of the 5 methods, add a case block exercising:
      (a) happy-path round-trip; (b) overwrite (Set twice,
      second value wins); (c) delete (where applicable);
      (d) read on absent key returns zero value (NOT
      `ErrObjectNotFound` for tags — AWS returns empty
      TagSet on un-tagged object); (e) version-specific
      isolation (Set on v1 doesn't affect v2);
      (f) null-versionId routing.
- [ ] Cassandra integration test parity: the contract
      additions MUST pass against the existing testcontainers
      Cassandra factory without code changes (Cassandra impl
      already covers these methods — the test additions just
      run new cases against existing impl).
- [ ] `go vet ./...` passes; `go test -race ./internal/meta/...`
      passes (memory + Cassandra contract green).
- [ ] TiKV integration test (`store_integration_test.go`) MUST
      pass the new cases against the testcontainers PD+TiKV
      factory — verify the existing
      `host.docker.internal` gateway alias is reachable on
      the dev box, else document `t.Skipf` per CLAUDE.md
      TiKV gotcha.
- [ ] No new env knob; no migration backfill (pre-launch).
- [ ] Typecheck passes; tests pass.

### US-002: Object retention + legal-hold + restore-status

**Description:** As an S3 client, I want PUT/GET for Object Lock
retention, legal-hold, and restore-status on a TiKV-backed
deployment. Splits from the original mega-story to keep each
Ralph iteration within context window.

**Acceptance Criteria:**
- [ ] 3 TiKV impls in `internal/meta/tikv/store.go` reusing the
      `keycodec` helper from US-001:
      `SetObjectRetention` (line 132),
      `SetObjectLegalHold` (line 136),
      `SetObjectRestoreStatus` (line 140).
- [ ] **Key shape**:
      `object_meta:<bucket-uuid-raw-16>:<key-fdbstuffed>:<version-id>:<kind>`
      where `kind ∈ {retention, legal_hold, restore_status}`.
      Consistent with US-001 key shape.
- [ ] **Payload**: JSON-blob per Cassandra parity:
      - retention: `{mode, until}` (mode ∈ GOVERNANCE / COMPLIANCE;
        until is RFC3339 time).
      - legal_hold: `{on bool}`.
      - restore_status: `{status string}`.
      Verify exact field names by reading the corresponding
      Cassandra impls.
- [ ] **Write path**: pessimistic txn with explicit Rollback on
      non-error early returns.
- [ ] **COMPLIANCE retention reduction**: PRD `ralph/storage-correctness`
      US-006 added `objectlock:ComplianceRetentionAttemptedReduce`
      audit verb. This story's `SetObjectRetention` impl MUST
      preserve that wiring — when caller attempts to shorten
      a COMPLIANCE retention, return `meta.ErrComplianceImmutable`
      (verify sentinel exists; if not, add) and the s3api layer
      stamps the audit row.
- [ ] **Cassandra parity** check against
      `internal/meta/cassandra/store.go` `SetObjectRetention` /
      `SetObjectLegalHold` / `SetObjectRestoreStatus` — same
      return shape, same error sentinels.
- [ ] **Contract test additions** in `contract.go`: for each of
      the 3 methods, happy-path round-trip + version-specific
      isolation + COMPLIANCE reduction returns the sentinel.
- [ ] `go vet ./...` passes; `go test -race ./internal/meta/...`
      passes for memory + Cassandra + TiKV.
- [ ] Typecheck passes; tests pass.

### US-003: Bucket grants — Set / Get / Delete

**Description:** As an S3 client, I want PUT/GET/DELETE for
bucket ACL grants on a TiKV-backed deployment.

**Acceptance Criteria:**
- [ ] 3 TiKV impls replacing stubs at lines 100-109:
      `SetBucketGrants`, `GetBucketGrants`, `DeleteBucketGrants`.
- [ ] **Key shape**: `bucket_meta:<bucket-uuid-raw-16>:grants`.
      Single PK per bucket (no version dimension).
- [ ] **Payload**: JSON-blob of `[]meta.Grant`.
- [ ] **Write path**: pessimistic txn with explicit Rollback on
      non-error early returns.
- [ ] **Delete semantics**: `DeleteBucketGrants` removes the
      key; `GetBucketGrants` on absent key returns
      `(nil, nil)` (NOT error — matches Cassandra impl —
      verify by reading the Cassandra method).
- [ ] **Cassandra parity** (verify against
      `internal/meta/cassandra/store.go` `SetBucketGrants` /
      `GetBucketGrants` / `DeleteBucketGrants`): same return
      shapes, same nil-handling.
- [ ] **Contract test additions** in `contract.go`: happy-path
      round-trip; overwrite; delete; read on absent key returns
      `(nil, nil)`.
- [ ] `go vet ./...` passes; `go test -race ./internal/meta/...`
      passes for memory + Cassandra + TiKV.
- [ ] Typecheck passes; tests pass.

### US-004: Access points — Create / Get / GetByAlias / Delete / List

**Description:** As an operator using S3 Access Points on a
TiKV-backed deployment, I want full lifecycle (create / get-by-
name / get-by-alias / delete / list-by-bucket) so the access-
point routing in `internal/s3api/access_points.go` works
identically to the Cassandra backend.

**Acceptance Criteria:**
- [ ] 5 TiKV impls replacing stubs at lines 144-161:
      `CreateAccessPoint`, `GetAccessPoint`,
      `GetAccessPointByAlias`, `DeleteAccessPoint`,
      `ListAccessPoints`.
- [ ] **Three-index key shape** for coherent lookups:
      (a) `access_point:by_name:<name-fdbstuffed>` →
          full `*meta.AccessPoint` JSON blob.
      (b) `access_point:by_alias:<alias-fdbstuffed>` →
          name pointer (name as bytes; reader does a second
          `Get` on the by_name key).
      (c) `access_point:by_bucket:<bucket-uuid-raw-16>:<name-fdbstuffed>` →
          name pointer (for `ListAccessPoints(bucketID)` range
          scan via `access_point:by_bucket:<bucket-uuid>:`
          prefix).
- [ ] **CreateAccessPoint**: single pessimistic txn writes all
      three index keys atomically. Returns
      `meta.ErrAccessPointAlreadyExists` (sentinel verified to
      exist in `internal/meta/store.go:66`) on duplicate name.
- [ ] **GetAccessPoint(name)**: single snapshot `Get` on by_name
      key, decode JSON. Returns `meta.ErrAccessPointNotFound`
      (verified to exist in `internal/meta/store.go:67`) on
      absent key. NO pessimistic txn — read-only.
- [ ] **GetAccessPointByAlias(alias)**: snapshot `Get` on
      by_alias → derived name → snapshot `Get` on by_name →
      decode. NO pessimistic txn. **Race semantics**: if the
      second `Get` returns `ErrAccessPointNotFound`
      (concurrent Delete after the first read but before the
      second), the function MUST return
      `ErrAccessPointNotFound` (treat as already-deleted —
      caller sees the linearizable "deleted" state). Document
      this race resolution inline.
- [ ] **DeleteAccessPoint(name)**: read by_name to derive
      alias + bucket → delete all three index keys atomically
      via pessimistic txn.
- [ ] **ListAccessPoints(bucketID)**: range scan
      `access_point:by_bucket:<bucket-uuid>:` prefix → derive
      name list → `Get` each by_name in a follow-up loop.
      Order matches Cassandra impl (verify — likely lex by
      name).
- [ ] **Cassandra parity** check against
      `internal/meta/cassandra/store.go` `CreateAccessPoint` /
      etc. — same return shape, same sentinels.
- [ ] **Contract test additions** in `contract.go`: build on
      existing `CreateAccessPoint` cases (lines 1774, 1777
      verified) — add cases for `GetByAlias`,
      `DeleteAccessPoint`, `ListAccessPoints` with multi-AP
      seed.
- [ ] `go vet ./...` passes; `go test -race ./internal/meta/...`
      passes across all three backends.
- [ ] Typecheck passes; tests pass.

### US-005: SSE rewrap progress + raw manifest + replication status

**Description:** As an operator running `strata admin rewrap`
on a TiKV-backed deployment, I want rewrap progress
checkpointing to work; as an operator running replication, I
want per-object replication status writable.

**Acceptance Criteria:**
- [ ] 6 TiKV impls replacing stubs at lines 164-185:
      `UpdateObjectSSEWrap`, `SetRewrapProgress`,
      `GetRewrapProgress`, `GetObjectManifestRaw`,
      `UpdateObjectManifestRaw`, `SetObjectReplicationStatus`.
- [ ] **UpdateObjectSSEWrap(bucket, key, version, wrapped, keyID)**:
      writes to the existing `objects` table row.
      `meta.Object` struct ALREADY carries `SSEKey []byte` +
      `SSEKeyID string` fields (`internal/meta/store.go:579-580`)
      — TiKV objects payload already serializes them. Impl:
      pessimistic txn → Read existing object via the established
      TiKV object CRUD path → set `o.SSEKey = wrapped` +
      `o.SSEKeyID = keyID` → write back. Match the
      `versionID == ""` path (resolves to latest version) per
      Cassandra `UpdateObjectSSEWrap` lines 4015-4021. NOT a
      schema-additive change — fields are already there.
- [ ] **SetRewrapProgress(progress)**: key
      `sse_rewrap_progress:<bucket-uuid-raw-16>` →
      `*meta.RewrapProgress` JSON blob. Pessimistic txn.
- [ ] **GetRewrapProgress(bucket)**: single `Get`; returns
      `(nil, nil)` on absent key (matches Cassandra null-row
      semantics — verify).
- [ ] **GetObjectManifestRaw(bucket, key, version)**: reads
      the manifest blob from the existing `objects` row
      WITHOUT decoding. Single `Get` + extract `manifest`
      field as raw bytes.
- [ ] **UpdateObjectManifestRaw(bucket, key, version, raw)**:
      writes raw bytes to the manifest field of the
      existing `objects` row. Pessimistic txn.
- [ ] **SetObjectReplicationStatus(bucket, key, version, status)**:
      writes a `replication_status` field to the existing
      `objects` row. Pessimistic txn. status ∈
      {PENDING, COMPLETE, FAILED, REPLICA} (verify exact
      enum in Cassandra impl).
- [ ] **Cassandra parity** check against the 6 corresponding
      Cassandra impls — same return shape, same sentinels,
      same null-handling.
- [ ] **Contract test additions** in `contract.go`: build on
      existing `SetRewrapProgress` (line 651) +
      `GetObjectManifestRaw` (lines 694/721/737/746) cases
      — add cases for the missing
      `UpdateObjectSSEWrap` / `UpdateObjectManifestRaw` /
      `SetObjectReplicationStatus`.
- [ ] **Note**: `manifest_rewriter` worker
      (`strata server --workers=manifest-rewriter`) MUST
      keep working against TiKV after this story — its
      JSON→proto bulk conversion uses
      `GetObjectManifestRaw` + `UpdateObjectManifestRaw`.
      Verify by running the worker against a TiKV-backed
      lab with a few seeded JSON manifests; expected outcome:
      manifests rewritten to proto.
- [ ] `go vet ./...` passes; `go test -race ./internal/meta/...`
      passes across all three backends.
- [ ] Typecheck passes; tests pass.

### US-006: Smoke validation + perf budget + ROADMAP close-flip + PRD removal

**Description:** As a future-maintainer, I want one explicit
verification pass that proves the 22 implementations landed +
TiKV-default lab smoke passes the previously-failing legs +
ROADMAP entry flipped + PRD markdown removed.

**Acceptance Criteria:**
- [ ] **Pre-cycle baseline capture** (run BEFORE any US-001
      impl lands): record current `make smoke` output against
      TiKV-default lab (expected: fails at TAGGING leg).
      Stash in progress.txt as the before-state.
- [ ] Run `make smoke` against TiKV-default lab → green
      (TAGGING + retention legs pass).
- [ ] Run `make smoke-signed` → green.
- [ ] Run `make smoke-tikv-default-lab` → all 4 scenarios
      pass.
- [ ] Run full `go test -race ./...` (default tag) → green;
      capture duration.
- [ ] Run `make test-integration` (Cassandra testcontainers)
      → green (Cassandra parity check — no regression).
- [ ] **STRICT CI gate**: `go test ./internal/meta/tikv/...
      -tags integration` against testcontainers PD+TiKV MUST
      pass in CI before merge. If local box can't run
      testcontainers (lima docker context not aligned with
      `host.docker.internal` gateway alias per CLAUDE.md
      gotcha), document the local SKIP in progress.txt AND
      verify CI workflow `.github/workflows/ci-tikv.yml`
      runs the suite via `STRATA_TIKV_TEST_PD_ENDPOINTS`
      against compose-managed PD. Merge to main BLOCKED
      until CI green — no exceptions.
- [ ] Run `scripts/s3-tests/run.sh` against TiKV-default lab
      → record pass-rate. Should be ≥ pre-cycle baseline
      (Cassandra-default run); any regression is a bug.
      Update `scripts/s3-tests/README.md` baseline section
      per CLAUDE.md `## Commits and PRs` rule.
- [ ] Run `make vet` + `make docs-build` → green.
- [ ] **Manifest-rewriter strict smoke** (validates US-005's
      `GetObjectManifestRaw` + `UpdateObjectManifestRaw`
      against TiKV): (a) seed 10 objects with
      `STRATA_MANIFEST_FORMAT=json` against TiKV-default lab;
      (b) restart lab with `STRATA_MANIFEST_FORMAT=proto` +
      `STRATA_WORKERS=manifest-rewriter` env; (c) wait for
      one rewriter tick (env default 24h — set
      `STRATA_MANIFEST_REWRITER_INTERVAL=10s` for the smoke);
      (d) read all 10 manifests via TiKV and assert they
      are proto-shaped (first byte not `{`). Capture seeded
      count + rewritten count in progress.txt.
- [ ] **Perf budget**: bench TiKV p99 latency vs Cassandra
      baseline for the new methods. Run a tag-heavy
      workload (50k SetObjectTags + 50k GetObjectTags
      concurrent) against TiKV-default and Cassandra-default
      labs. Capture ratio in progress.txt:
      `TiKV/Cassandra p99 ratio = X.Yx`. If ratio > 2× for
      any method, ROADMAP gets a new P3 entry
      `TiKV meta backend perf — Y method 2.Xx slower than
      Cassandra` parked under `## Scalability & performance`.
- [ ] **ROADMAP close-flip** × 1 on line 415 of `ROADMAP.md`
      (P2 entry "TiKV meta backend stubs 19 `meta.Store`
      methods..." — the entry says 19 but actual count is
      **22**; the close-flip text MUST correct the count).
      Close-flip summary references each of US-001..US-005
      + the contract test additions + the smoke leg that
      went red → green + the perf budget ratio.
- [ ] **Fix the ROADMAP formatting bug at line 415** in the
      same commit: the P2 entry is currently merged into
      the prior P1 "Fixed" RADOS-probe block (visually one
      bullet, reads as two concatenated paragraphs). Split
      into a proper standalone bullet during the close-flip.
- [ ] Close-flip carries `(commit pending)` placeholder per
      the established convention; SHA backfill lands on
      `main` post-merge as the fast-follow commit.
- [ ] `tasks/prd-tikv-stubs.md` REMOVED via `git rm`.
- [ ] `scripts/ralph/progress.txt` carries one US-005 block
      summarising before/after smoke output + contract
      coverage delta + s3-tests pass-rate.
- [ ] Typecheck passes; all tests pass.

## Functional Requirements

- FR-1: `internal/meta/tikv/store.go` MUST contain NO
  `errors.ErrUnsupported` returns (grep
  `errors.ErrUnsupported internal/meta/tikv/store.go` returns
  zero matches after this cycle).
- FR-2: Every implemented method MUST follow CLAUDE.md TiKV
  gotchas: FoundationDB byte-stuffing on variable-length string
  segments; pessimistic txn for RMW with explicit
  `txn.Rollback()` on non-error early returns; raw-UUID
  16-byte encoding for bucket-id in keys.
- FR-3: TiKV `storetest.Run` factory in
  `internal/meta/tikv/store_integration_test.go` MUST pass
  the existing contract suite + all new contract cases added
  by this cycle.
- FR-4: Cassandra + memory backends MUST also pass the new
  contract cases without code changes.
- FR-5: `make smoke` against TiKV-default lab MUST pass the
  TAGGING + retention legs (today fails on TAGGING).
- FR-6: `scripts/s3-tests/run.sh` against TiKV-default lab
  MUST not regress vs Cassandra-default baseline.
- FR-7: ROADMAP P2 entry on line 415 MUST be flipped to Done
  in the US-006 commit, with the count corrected from "19"
  to "22" and the formatting bug (P2 merged into prior P1
  block) fixed.
- FR-8: TiKV impl p99 latency MUST stay within 2× of the
  Cassandra baseline on the new methods. Worse than 2×
  surfaces as a new P3 ROADMAP entry under
  `## Scalability & performance`.
- FR-9: Shared `internal/meta/tikv/keycodec.go` helper MUST
  be introduced in US-001 and reused by US-002..US-005.
  No per-story re-implementation of FoundationDB stuffing
  or JSON marshal primitives.

## Non-Goals

- No optimisations / perf bench for the new methods — match
  Cassandra behaviour 1:1 first; optimise later if a workload
  surfaces a hot path.
- No new env knobs.
- No new admin endpoints (the methods serve existing S3 API
  + admin handlers that already exist).
- No data backend changes — TiKV stays as the meta store;
  RADOS / S3 / memory data backends untouched.
- No reshape of the `objects` table key layout — US-004 SSE
  wrap + raw manifest + replication status add fields to
  the existing payload, not new key shapes.
- No KMS adapter work (parked for Cycle 2 `ralph/auth-dx`).

## Design Considerations

- **Key prefix discipline** — every new key prefix
  (`object_meta:`, `bucket_meta:`, `access_point:`,
  `sse_rewrap_progress:`) should be documented in
  `internal/meta/tikv/keys.md` (verify file exists; if not,
  create as part of US-001).
- **Pessimistic txn cost** — each new RMW path adds a PD
  round-trip. Acceptable for the cold S3 surface (tags,
  retention) where call frequency is low; would be a problem
  on the chunk-write hot path but that's not in scope.
- **Null-handling parity with Cassandra** — Cassandra returns
  `(nil, nil)` for absent rows in most "blob config" getters
  (lifecycle, CORS, policy, public-access-block).
  Match the exact shape per method by reading the Cassandra
  impl as the parity oracle.

## Technical Considerations

- **`storetest.Run` factory** is wired into
  `internal/meta/tikv/store_integration_test.go:66`; existing
  contract cases for SSE rewrap / manifest raw / access-point
  Create are already exercised but currently fail with
  `ErrUnsupported`. Either these tests are `t.Skip`'d under
  some gating condition (verify) OR they have been silently
  failing in the TiKV integration suite. Resolve in US-001.
- **`meta.ErrAccessPointNotFound` / `ErrAccessPointAlreadyExists`
  sentinels** — verify they exist in `internal/meta/store.go`;
  if not, add per the existing `ErrObjectNotFound` /
  `ErrChunkNotFound` pattern. New sentinels go in a
  build-tag-free file.
- **manifest_rewriter worker** uses
  `GetObjectManifestRaw` + `UpdateObjectManifestRaw` for
  bulk JSON→proto conversion. After US-004 lands, run the
  worker against TiKV with a JSON manifest seeded to verify
  the chain works.
- **`s3-tests` runner** can be slow (5-10 min for the full
  suite). For per-story validation in US-001..US-004, the
  contract test additions are sufficient signal; the full
  `s3-tests` run lives in US-005.

## Success Metrics

- 0 `errors.ErrUnsupported` returns in
  `internal/meta/tikv/store.go` (today: 22).
- `make smoke` TAGGING + retention legs green against
  TiKV-default lab (today: TAGGING fails with HTTP 500).
- `scripts/s3-tests/run.sh` pass-rate ≥ Cassandra-default
  baseline.
- 1 ROADMAP P2 entry closes in one cycle.
- Cycle ships in 5 stories.

## Open Questions

- Existing TiKV `storetest.Run` integration test result on
  `main` today — does it `t.Skip` when stubs return
  `ErrUnsupported`, OR does it silently fail certain
  contract cases? Resolve in US-001 pre-impl pass (file
  exists at `internal/meta/tikv/store_integration_test.go:34`
  with `t.Skipf` shape; verify gating condition).
- `meta.ErrComplianceImmutable` sentinel for US-002 — verify
  via `grep -nE 'ErrCompliance' internal/meta/store.go`; if
  absent, add per the existing sentinel pattern.

## Resolved (verified during PRD review)

- `meta.ErrAccessPointAlreadyExists` + `meta.ErrAccessPointNotFound`
  sentinels exist at `internal/meta/store.go:66-67`.
- `meta.AccessPoint` struct carries `BucketID uuid.UUID` field
  at `internal/meta/store.go:909-919` — US-004 by_bucket index
  key has the data.
- `meta.Object` struct carries `SSEKey []byte` +
  `SSEKeyID string` at `internal/meta/store.go:579-580` —
  US-005 `UpdateObjectSSEWrap` is in-place Read-Set-Write,
  not a schema-additive change.
- Cassandra `UpdateObjectSSEWrap`
  (`internal/meta/cassandra/store.go:4013`) updates `sse_key`
  + `sse_key_id` columns on `objects` row via UPDATE — TiKV
  parity goes through the established objects-row CRUD.
- TAGGING smoke leg at `scripts/smoke.sh:112` — US-006 smoke
  validation has the exact leg to assert green.
