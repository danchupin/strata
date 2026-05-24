# PRD: Final non-globals bundle — per-bucket signing keys + module-split + trailer algos + lima envelope

## Introduction

Final bundle to drain remaining non-global ROADMAP entries. After
cycle ships, ROADMAP holds **only 5 globals** (P2 ScyllaDB benchmarks,
P2 Content-addressed dedup, P3 Intelligent-Tiering, P3 Select Object
Content, plus consolidated globals).

4 entries close:

- **P3 (line 254)**: Per-bucket request signing keys (KMS-backed).
  ROADMAP wording: *"Rotate the signing material on a schedule,
  reject keys older than `STRATA_KEY_MAX_AGE`. Hooks onto the
  existing Vault provider."* — entry itself confirms KMS provider
  already exists; this cycle WIRES it to per-bucket signing key
  path, not builds adapter from scratch.
- **P3 (line 260)**: bench-rgw-comparison lima envelope — CI-based
  bench runner with auto-PR for cycled numbers.
- **P3 (line 386)**: Module tags cleanup — go-ceph real module split.
  Verified `go.mod` still has `require github.com/ceph/go-ceph
  v0.39.0` direct → split is real fix, not parked premise.
- **P3 (line 478)**: Streaming chunked-trailer non-sha256 algos —
  crc32 + crc32c + sha1.

**Pre-launch product** per [Pre-launch no deploys] memory.

Branch: `ralph/auth-dx-trailer-lima`. Starts from `main`. **6 stories**
after PRD review collapsed scope: `internal/crypto/kms` package
**already ships** with full `Provider` interface + AWS + Vault +
LocalHSM impls. PRD US-001..US-003 (build adapters from scratch)
DROPPED — wire existing provider instead.

## Goals

- New per-bucket signing key shape via meta column additions; auth
  middleware route lookups DEK + unwraps via existing
  `internal/crypto/kms.Provider` (no new KMS code).
- TTL cache for unwrapped DEK (5 min default, env-tunable). Hit ≥
  95% steady-state target.
- Fail-closed on KMS unreachable: HTTP 503 `KMSUnavailable` +
  `Retry-After: 30`. No legacy-key fallback.
- New `STRATA_KEY_MAX_AGE` env (default `90d`) — auth rejects
  per-bucket signing keys older than max-age with 401.
- LocalStack + Vault dev-mode integration tests added to existing
  KMS package (today fake-based only).
- `internal/data/rados/cephimpl/` as separate Go module +
  `go.work` checked into repo. Default `go build ./...` truly
  hermetic (`go mod graph | grep -c go-ceph` = 0).
- Streaming chunked-trailer accepts sha256 + sha1 + crc32 + crc32c
  via shared parsing shape. All stdlib (no new deps).
- `.github/workflows/bench-rgw.yml` self-hosted runner + weekly
  cron + auto-PR with 10% regression threshold.

## User Journey

Four personas:

- **Operator under PCI-DSS / SOX rotation requirements.** Today:
  no per-bucket signing key concept — IAM access keys are user-
  scoped. After cycle: `POST /admin/v1/buckets/{name}/signing-key/rotate`
  generates fresh DEK + wraps via existing KMS provider + persists
  to bucket meta.
- **Contributor cloning repo on Linux without librados.** Today:
  `go.mod` lists `github.com/ceph/go-ceph v0.39.0` as direct
  require → `go mod tidy` complains. After cycle: main module's
  `go.mod` drops the require entirely; `go.work` lists both
  modules.
- **aws-cli 2.22+ user uploading with crc32c trailer.** Today: 400
  `ErrUnsupportedChecksumAlgorithm`. After cycle: 200 OK.
- **Maintainer running `make bench-rgw-comparison` on lima box.**
  Today: full 144-row sweep blocked. After cycle: lima runs
  reduced smoke; full sweep cycled in CI self-hosted runner
  weekly.

## User Stories

### US-001: Per-bucket signing key — schema + meta CRUD + KMS provider wiring

**Description:** As an operator, I want per-bucket signing keys
backed by the existing `internal/crypto/kms.Provider` so DEKs are
wrapped via KMS (AWS / Vault / LocalHSM) and persisted to bucket
meta. Auth middleware unwraps on cache miss.

**Acceptance Criteria:**
- [ ] **Verify existing KMS shape** (no new adapter work — just
      wiring): `internal/crypto/kms.Provider` interface
      (`GenerateDataKey` + `UnwrapDEK`) + 3 impls
      (`AWSKMSProvider`, `VaultProvider`, `LocalHSMProvider`)
      already exist. `FromEnv(opts...)` precedence vault > aws-kms
      > local-hsm.
- [ ] **New meta columns**: `buckets.signing_wrapped_dek blob`,
      `buckets.signing_key_id text` (KMS key handle), `buckets.signing_key_created_at timestamp`.
      All 3 columns additive across memory + Cassandra + TiKV in
      lockstep.
- [ ] **Cassandra**: `ALTER TABLE buckets ADD signing_wrapped_dek
      blob` + `signing_key_id text` + `signing_key_created_at
      timestamp` via `alterStatements`.
- [ ] **TiKV**: `meta.Bucket` struct gains 3 fields; existing
      `updateBucket` helper covers persistence.
- [ ] **Memory**: struct fields added; no schema concept.
- [ ] **New `meta.Store` methods**: `GetBucketSigningKey(ctx, name)
      (wrapped []byte, keyID string, createdAt time.Time, err
      error)` + `SetBucketSigningKey(ctx, name, wrapped []byte,
      keyID string)` (updates createdAt to now() on every call).
- [ ] **Contract test additions** in
      `internal/meta/storetest/contract.go`: `caseBucketSigningKey`
      — set → get round-trip; absent → returns
      `meta.ErrBucketSigningKeyNotSet` (new sentinel).
- [ ] **Auth middleware integration** in `internal/auth/sigv4.go`
      (or where SigV4 chain lives — verify): on bucket-scoped
      request: lookup `meta.GetBucketSigningKey` → if present:
      check DEK cache (sync.Map TTL 5 min) → on cache miss:
      `Provider.UnwrapDEK(ctx, keyID, wrapped)` → cache plaintext
      DEK → use as SigV4 signing material. If
      `ErrBucketSigningKeyNotSet`: fall through to existing IAM
      access-key path (per-bucket signing keys are OPT-IN per
      bucket, not mandatory).
- [ ] **DEK plaintext zeroed via `subtle.ConstantTimeCopy(0, dek,
      zeros)`** before cache eviction (5-min TTL eviction OR
      manual on Rotate).
- [ ] **TTL knob**: new `STRATA_DEK_CACHE_TTL` env (default `5m`,
      range `[30s, 1h]`); documented in env-vars reference.
- [ ] **Counter** `strata_kms_decrypt_total{provider, outcome}` —
      provider ∈ `{aws_kms, vault, local_hsm}`; outcome ∈
      `{cache_hit, cache_miss_ok, unavailable, denied, tampered}`.
- [ ] **LocalStack KMS integration test** (new):
      `internal/crypto/kms/aws_integration_test.go` (build tag
      `integration`) spins LocalStack via testcontainers, creates
      CMK via `aws kms create-key`, exercises `AWSKMSProvider`
      `GenerateDataKey` + `UnwrapDEK` round-trip. Pin
      `localstack/localstack:3.x` (NOT `latest`).
- [ ] **Vault Transit dev-mode integration test** (new):
      `internal/crypto/kms/vault_integration_test.go` (build tag
      `integration`) spins `hashicorp/vault:1.15` dev-mode
      container, enables transit engine, creates CMK, exercises
      `VaultProvider` round-trip.
- [ ] `make test-integration` extended to include the 2 new
      integration tests.
- [ ] `go vet ./...` + `go test -race ./internal/meta/...
      ./internal/auth/... ./internal/crypto/kms/...` pass.
- [ ] Typecheck passes; tests pass.

### US-002: Admin signing-key API + STRATA_KEY_MAX_AGE enforcement + fail-closed

**Description:** As an operator, I want `POST
/admin/v1/buckets/{name}/signing-key/rotate` to trigger fresh DEK
generation + admin GET status endpoint + auth rejects keys older
than `STRATA_KEY_MAX_AGE` with 401.

**Acceptance Criteria:**
- [ ] **New admin endpoints**:
      `POST /admin/v1/buckets/{name}/signing-key/rotate` → invokes
      `Provider.GenerateDataKey(ctx, defaultKeyID)` (new DEK +
      wrapped) → `Meta.SetBucketSigningKey(name, wrapped, keyID)`
      → cache invalidation for the bucket → audit verb
      `admin:RotateBucketSigningKey`. Request body optional `{key_id
      string}` to override default CMK.
      `GET /admin/v1/buckets/{name}/signing-key/status` → returns
      `{key_id, created_at, age_days, max_age_days, expired bool}`.
      Audit verb `admin:GetBucketSigningKeyStatus`.
      `DELETE /admin/v1/buckets/{name}/signing-key` → drops the
      per-bucket signing key (falls back to IAM access-key auth).
      Audit verb `admin:DeleteBucketSigningKey`.
- [ ] **`STRATA_KEY_MAX_AGE` env** (new, default `90d`, range
      `[1d, 365d]`). Auth middleware: on per-bucket DEK lookup, if
      `now() - createdAt > maxAge` → return HTTP 401 `KeyExpired`
      with audit log entry. Operator must Rotate to recover.
- [ ] **Fail-closed semantics**: on `kms.ErrKMSUnavailable`
      (`Provider.UnwrapDEK` returns transient error AND cache
      miss) → return HTTP 503 `KMSUnavailable` with
      `Retry-After: 30`. **NO fallback** to IAM access-key path —
      explicit per-bucket signing key means operator opted in;
      degrading silently breaks rotation semantics.
- [ ] **OpenAPI YAML** updated with 3 new endpoints + their
      schemas + audit verb annotations.
- [ ] **Unit tests** cover: happy-path Rotate flow, GET status on
      unset key returns 404, GET status on expired key returns
      `expired: true`, Rotate invalidates cache, KMS-unavailable
      returns 503 + `Retry-After`, max-age exceeded returns 401.
- [ ] **Integration test** against LocalStack: full rotation
      cycle — set → rotate → old wrapped fails decrypt under
      new CMK → new wrapped succeeds.
- [ ] **Docs**: `/operate/key-rotation.md` (new) — operator
      workflow + max-age tuning + fail-closed semantics.
      `STRATA_KEY_MAX_AGE` + `STRATA_DEK_CACHE_TTL` in
      `/reference/env-vars.md`.
- [ ] `go vet ./...` + `go test -race ./internal/auth/...
      ./internal/adminapi/...` pass.
- [ ] Typecheck passes; tests pass.

### US-003: Module split — `internal/data/rados/cephimpl/` as separate Go module

**Description:** As a contributor cloning the repo on Linux without
librados, I want `go build ./...` truly hermetic — `go.mod` should
NOT carry `github.com/ceph/go-ceph` as direct require. Real module
split via separate `cephimpl/` Go module + `go.work` workspace.

**Acceptance Criteria:**
- [ ] **Verified premise**: `go.mod` currently has `require
      github.com/ceph/go-ceph v0.39.0` direct — split is real fix
      (NOT parked-as-already-hermetic premise from prior cycles).
      Stub files already exist (`internal/data/rados/stub.go`,
      `ops_stub.go`, `pool_stub.go`) — leverage them as the
      main-module surface; cephimpl/ provides the real impl.
- [ ] **New module** `internal/data/rados/cephimpl/`:
      Own `go.mod`:
      ```
      module github.com/danchupin/strata/cephimpl
      go 1.23
      require github.com/ceph/go-ceph v0.39.0
      // … other transitive deps as needed
      ```
      Move ALL `//go:build ceph` files from
      `internal/data/rados/*.go` (`backend.go`, `rebalance.go`,
      `ops.go`, `pool.go`, `health.go` — grep `//go:build ceph`
      for full list) into `cephimpl/`. Adjust import paths.
- [ ] **Main module `go.mod` DROPS** `github.com/ceph/go-ceph`
      require entirely. `go mod tidy` on main module from clean
      state confirms zero go-ceph entries via `go mod graph |
      grep -c 'github.com/ceph/go-ceph'` = 0.
- [ ] **`go.work` checked into repo root**:
      ```
      go 1.23
      use (
        .
        ./internal/data/rados/cephimpl
      )
      ```
      Documented in `CONTRIBUTING.md` (or root README — verify
      preferred home): `go.work` is canonical dev shape; IDE
      auto-discovers both modules.
- [ ] **Interface boundary** between stub-surface and cephimpl/:
      `internal/data/rados/` stub package exposes typed
      `Backend` interface; cephimpl/ exposes concrete impl that
      satisfies the interface. `internal/serverapp/serverapp.go`
      `buildDataBackend` branches via build tag to import either
      stub `New()` (returns `data.ErrRADOSNotCompiled`) or
      cephimpl `New()` (real backend). NO direct cephimpl import
      from main module's build-tag-free files.
- [ ] **CI matrix**: `.github/workflows/ci.yml` runs `go test
      ./...` against BOTH modules separately:
      Main module under default tag (no ceph image needed).
      cephimpl module under `ceph/ceph:v19.2.3` Docker image
      (librados available).
- [ ] **Release build path**: `make build` produces default-tag
      binary (zero go-ceph in binary); `make build-ceph`
      produces ceph-capable binary that boots against
      `STRATA_DATA_BACKEND=rados`.
- [ ] **Clean-clone hermetic check** (regression smoke):
      `cd /tmp && rm -rf strata-clean && git clone <local-repo>
      strata-clean && cd strata-clean && go mod graph | grep -c
      'github.com/ceph/go-ceph'` → expected `0`.
- [ ] `make vet` passes on both modules; `make build` + `make
      build-ceph` both produce working binaries.
- [ ] Typecheck passes; tests pass.

### US-004: Streaming chunked-trailer non-sha256 algos (crc32 + crc32c + sha1)

**Description:** As an aws-cli 2.22+ user uploading with crc32c
trailer (default for AWS SDK > v2 in some configurations), I want
the chunked-trailer decoder to accept all 3 algos beyond sha256.

**Acceptance Criteria:**
- [ ] `internal/auth/streaming.go` trailer-algo classifier
      extended:
      `x-amz-checksum-sha256` → existing sha256 path
      (from ralph/storage-correctness US-009).
      `x-amz-checksum-sha1` → `crypto/sha1.New()` + base64-decode
      + compare.
      `x-amz-checksum-crc32` → `hash/crc32.NewIEEE()` (default
      polynomial) + 4-byte big-endian decode + compare.
      `x-amz-checksum-crc32c` →
      `hash/crc32.New(crc32.MakeTable(crc32.Castagnoli))` +
      4-byte big-endian decode + compare.
- [ ] **Shared parsing shape**: trailer decoder factory
      `selectTrailerHash(algoHeader string) (hash.Hash, decodeFn
      func(b64 string) ([]byte, error), err error)` slots each
      algo into the same chain-HMAC scheme already shipped for
      sha256.
- [ ] All stdlib (`hash/crc32`, `crypto/sha1`) — no new deps.
- [ ] Trailer-signature validation unchanged (chain-HMAC over
      `prevSig + canonical-trailer`).
- [ ] Mismatch → `ErrSignatureInvalid` (matches sha256 path —
      AWS-parity).
- [ ] Unsupported algo header → `ErrUnsupportedChecksumAlgorithm`
      (HTTP 400 `InvalidRequest`) — covers future algo additions.
- [ ] **Test fixtures** captured for each algo via aws-cli 2.22+:
      `internal/auth/testdata/chunked-trailer-{sha256,sha1,crc32,crc32c}.bin`.
- [ ] **Unit test** `internal/auth/streaming_trailer_test.go`
      extended: 4 happy-path decodes (one per algo), 4 mismatch
      cases, 1 unsupported-algo case.
- [ ] **Integration test** against aws-cli 2.22+ PUT with
      `--checksum-algorithm sha1 / crc32 / crc32c` (in addition
      to existing sha256) — all 4 return 200.
- [ ] `go vet ./...` + `go test -race ./internal/auth/...` pass.
- [ ] Typecheck passes; tests pass.

### US-005: Lima envelope fix — GitHub Actions cycled bench + auto-PR

**Description:** As a maintainer, I want the full rgw-comparison
bench cycled in CI on a self-hosted runner so docs numbers stay
fresh + auto-PR with updated numbers (auto-merge if regression
< 10%, manual review > 10%).

**Acceptance Criteria:**
- [ ] **Self-hosted runner setup doc**
      `docs/site/content/developers/bench-runner-setup.md` (or
      wherever developers/runbook lives — verify):
      Required box: 16GB RAM, 200GB disk, Docker 24+, Ubuntu 22.04
      LTS (or equivalent). Setup steps:
      `gh runner configure` + token + register as `self-hosted,
      strata-bench`. Documented for operator to provision once.
- [ ] **New workflow** `.github/workflows/bench-rgw.yml`:
      Trigger: weekly cron (`schedule: cron '0 4 * * 0'` — Sun
      04:00 UTC) + manual `workflow_dispatch` for ad-hoc runs.
      Runs-on: `[self-hosted, strata-bench]`.
      Steps: checkout → `make up-all && make up-bench-rgw` →
      `make bench-rgw-comparison` → upload
      `scripts/bench-results/rgw-comparison-*.jsonl` as artifact
      → run `scripts/bench-update-doc.sh` (new — generates
      markdown table from jsonl + writes to
      `docs/site/content/architecture/benchmarks/rgw-comparison.md`)
      → diff vs main → if doc changed, open PR with
      auto-generated title `bench: rgw-comparison refresh (week
      of <date>)`.
- [ ] **Auto-merge gate**: GitHub Action step compares numerical
      values in old vs new doc. If max p99 regression < 10%
      (Strata side), auto-merge via `gh pr merge --auto --squash`.
      If > 10%, leave PR open + mention `@danchupin` for review.
      Threshold tunable via workflow env
      `BENCH_REGRESSION_THRESHOLD` (default 10).
- [ ] **`scripts/bench-update-doc.sh`** (new) — reads jsonl +
      generates markdown table per existing
      `rgw-comparison.md` table schema (workload column + Strata
      p99 + RGW p99 + ratio + stddev). Idempotent — running twice
      on same jsonl yields identical doc.
- [ ] **PR description** carries: bench runner specs
      (CPU/RAM/disk observed via `lscpu` / `free` / `df`), Strata
      commit SHA, RGW image tag, full jsonl run id, regression
      analysis.
- [ ] **Doc page footer** updated to reflect last-refresh date:
      `Last refresh: <YYYY-MM-DD> via .github/workflows/bench-rgw.yml`
      injected by `bench-update-doc.sh`.
- [ ] **Manual trigger smoke**: `gh workflow run bench-rgw.yml`
      from main against configured self-hosted runner →
      end-to-end (artifact uploaded, PR opens). Cleanup test PR
      after.
- [ ] **Lima box reduced smoke**: `make smoke-rgw-lab-restart` +
      `scripts/bench-rgw-comparison.sh put-small both` (1 workload
      only) STILL works on lima box for operator-side regression
      detection.
- [ ] Typecheck passes; tests pass (no Go code touched).

### US-006: Smoke + ROADMAP close-flip × 4 + PRD removal

**Description:** As a future-maintainer, I want all 4 ROADMAP
entries closed + smoke validation + PRD removed.

**Acceptance Criteria:**
- [ ] Run `make smoke` → green.
- [ ] Run `make smoke-signed` → green (covers SigV4 + new
      per-bucket signing key chain).
- [ ] Run `make smoke-tikv-default-lab` → 4/4 scenarios pass.
- [ ] Run `make smoke-rgw-lab-restart` → 3 cycles green.
- [ ] Run full `go test -race ./...` (main module, default tag) →
      green; capture duration.
- [ ] Run `go test -race ./...` against cephimpl/ module
      separately (under docker ceph image) → green.
- [ ] Run `make test-integration` (Cassandra + LocalStack KMS +
      Vault dev-mode testcontainers) → all 3 integration suites
      green.
- [ ] Run `make vet` against both modules + `make docs-build` →
      green.
- [ ] **Default-build hermetic check** (verifies US-003):
      `cd / && rm -rf /tmp/strata-clean && git clone
      <local-repo> /tmp/strata-clean && cd /tmp/strata-clean &&
      go mod graph | grep -c 'github.com/ceph/go-ceph'` →
      expected `0`.
- [ ] **aws-cli 2.22+ chunked-trailer smoke** (verifies US-004):
      ```
      for algo in sha256 sha1 crc32 crc32c; do
        AWS_REQUEST_CHECKSUM_CALCULATION=when_required \
          aws --endpoint-url http://localhost:9999 s3 cp \
          /tmp/test s3://bucket/key-$algo \
          --checksum-algorithm $algo
      done
      ```
      → all 4 return 200.
- [ ] **Workflow manual trigger smoke** (verifies US-005):
      `gh workflow run bench-rgw.yml` → workflow runs on
      self-hosted runner → completes → PR opens (or doc
      unchanged if bench numbers identical).
- [ ] **ROADMAP close-flip × 4 in same commit**:
      (a) **P3** line 254 (Per-bucket signing keys, KMS-backed)
          → Done. Summary references US-001..US-002 + meta column
          add + admin endpoints + STRATA_KEY_MAX_AGE +
          fail-closed + reuse of existing `internal/crypto/kms`
          provider.
      (b) **P3** line 260 (bench-rgw lima envelope) → Done.
          Summary references US-005 + self-hosted runner +
          weekly cron + auto-PR with 10% regression threshold.
      (c) **P3** line 386 (module tags cleanup) → Done. Summary
          references US-003 + separate `cephimpl/` go.mod +
          go.work + hermetic default-tag build verified via
          clean-clone smoke.
      (d) **P3** line 478 (chunked-trailer non-sha256) → Done.
          Summary references US-004 + 3 new algos via stdlib +
          shared parsing shape.
- [ ] **NO new ROADMAP entries** from this cycle (4 entries
      close, ROADMAP shrinks by 4).
- [ ] Each close-flip carries `(commit pending)` placeholder;
      SHA backfill on `main` as fast-follow commit.
- [ ] `tasks/prd-auth-dx-trailer-lima.md` REMOVED via `git rm`.
- [ ] `scripts/ralph/progress.txt` carries one US-006 block
      summarising smoke + clean-clone hermetic check + workflow
      smoke + 4 close-flips.
- [ ] Typecheck passes; tests pass.

## Functional Requirements

- FR-1: `meta.Bucket` MUST gain `signing_wrapped_dek` +
  `signing_key_id` + `signing_key_created_at` columns across all
  3 backends.
- FR-2: New `meta.Store` methods `GetBucketSigningKey` +
  `SetBucketSigningKey` MUST exist + pass contract test.
- FR-3: Auth middleware MUST consult existing
  `internal/crypto/kms.Provider` (NO new adapter code) +
  cache DEK with TTL (default 5m).
- FR-4: Auth middleware MUST fail-closed on KMS unavailable
  (HTTP 503 `KMSUnavailable` + `Retry-After: 30`).
- FR-5: `STRATA_KEY_MAX_AGE` MUST be enforced; expired keys
  return HTTP 401 `KeyExpired`.
- FR-6: 3 admin endpoints MUST ship: Rotate + Status + Delete.
- FR-7: 2 new integration tests MUST cover LocalStack KMS +
  Vault dev-mode.
- FR-8: `internal/data/rados/cephimpl/` MUST be a separate Go
  module with its own `go.mod`; main module MUST NOT carry
  `github.com/ceph/go-ceph` direct require.
- FR-9: `go.work` MUST be checked into repo root.
- FR-10: Clean-clone smoke `go mod graph | grep -c go-ceph` MUST
  return 0.
- FR-11: Streaming trailer MUST accept sha256 + sha1 + crc32 +
  crc32c; unsupported algos return
  `ErrUnsupportedChecksumAlgorithm`.
- FR-12: `.github/workflows/bench-rgw.yml` MUST run weekly on
  self-hosted runner + open auto-PR with updated numbers
  (auto-merge if regression < 10%).
- FR-13: ROADMAP MUST close 4 entries in US-006 commit (no new
  entries surfaced).

## Non-Goals

- No new KMS adapter (AWS / Vault / LocalHSM already shipped in
  `internal/crypto/kms` per verified state).
- No KMS provider refactor (existing `Provider` interface
  satisfies cycle needs).
- No removal of IAM access-key path — per-bucket signing keys
  are OPT-IN per bucket; absent key falls through to IAM auth.
- No additional checksum algos beyond crc32 + crc32c + sha1 +
  sha256 (no CRC64 — not used by AWS today).
- No GCP-cloud-runner option for US-005 (self-hosted only per
  prior scoping).
- No per-PR bench (cost-prohibitive per prior scoping).

## Design Considerations

- **Per-bucket signing key opt-in**: bucket without
  `signing_wrapped_dek` falls back to IAM access-key auth. No
  global flag-day migration. Operator opts a bucket in via
  `POST /signing-key/rotate` (first call generates initial key).
- **DEK cache size**: sync.Map naturally unbounded — for a
  10000-bucket tenant with 5m TTL + 95% hit rate, ~10000 entries
  × ~32 bytes plaintext + ~64 bytes metadata = ~1MB heap budget.
  Acceptable. If buckets > 100k, revisit cache eviction strategy.
- **`STRATA_KEY_MAX_AGE` default 90d**: matches typical
  enterprise rotation policy (PCI-DSS, SOX). Range
  `[1d, 365d]` for test or low-risk environments.
- **`go.work` checked in**: simplest dev UX; contributors clone
  + IDE sees both modules; no per-contributor bootstrap.
- **Self-hosted runner** (zero ongoing cost): operator
  provisions box once; weekly bench is free.

## Technical Considerations

- **`internal/crypto/kms` package** already provides:
  `Provider` interface (`GenerateDataKey` + `UnwrapDEK`),
  `AWSKMSProvider` (`aws-sdk-go-v2/service/kms`),
  `VaultProvider` (`hashicorp/vault/api` already a dep),
  `LocalHSMProvider`, `FromEnv(opts)` factory with
  vault > aws-kms > local-hsm precedence. Per CLAUDE.md in
  `internal/crypto/kms/CLAUDE.md`.
- **`go.mod` current state**: `require github.com/ceph/go-ceph
  v0.39.0` confirmed via grep — module split is real fix.
- **Module split + `go.work`**: Go 1.18+; IDE support universal.
  Replace directive in `go.mod` is fallback for environments
  without `GOWORK` env.
- **CI matrix for cephimpl**: GitHub Actions standard runner
  doesn't have librados — cephimpl integration test runs inside
  `ceph/ceph:v19.2.3` docker container (same image as Strata
  ceph build).
- **LocalStack KMS surface**: GenerateDataKey + Decrypt +
  ReEncrypt + DescribeKey match `Provider` interface 1:1.
- **Vault Transit dev-mode**: auto-unseal on container start;
  `vault secrets enable transit` + `vault write -f
  transit/keys/strata-test` to provision.
- **Self-hosted runner security**: weekly bench is
  operator-controlled hardware; PR auto-merge requires GitHub
  Actions bot to have `contents: write` permission — limit to
  the `bench-rgw.yml` workflow only via workflow-scoped
  GITHUB_TOKEN.

## Success Metrics

- 4 ROADMAP P3 entries close in this cycle.
- Post-cycle ROADMAP holds ONLY globals (5 entries: P2
  ScyllaDB benchmarks, P2 Content-addressed dedup, P3
  Intelligent-Tiering, P3 Select Object Content + any other
  consolidated globals).
- `go mod graph | grep -c go-ceph` on main module clone = 0.
- `strata_kms_decrypt_total{outcome=cache_hit}` dominates
  `cache_miss_ok` after warmup (target > 95% cache hit rate
  steady-state).
- aws-cli 2.22+ uploads with crc32c trailer return 200 (today
  400).
- `.github/workflows/bench-rgw.yml` runs weekly + auto-PRs land
  green with auto-merge for < 10% regression.
- Cycle ships in 6 stories (shrunk from initial 9 after
  production-grade PRD review found `internal/crypto/kms`
  already fully implemented).

## Open Questions

- Default KMS CMK ID — operator-provided via
  `STRATA_KMS_DEFAULT_KEY_ID` OR per-bucket rotation request
  body MUST carry `key_id`? Default: env-provided default + per-
  request override (matches existing pattern in
  `internal/crypto/kms` package).
- DEK cache eviction strategy at scale — sync.Map naturally
  unbounded; for tenants > 100k buckets, may need explicit
  LRU eviction. Park if surfaces.
- Self-hosted runner box ownership for US-005 — Avito-provided
  OR operator personal? Default: operator personal for alpha
  cycle.
