# PRD: s3-tests Pass Rate 84.7% → ≥90% (target ~95%)

## Introduction

The prior `ralph/s3-compat-90` cycle (commit `cb61770` merge into main) lifted the headline from **80.1% (141/176)** to **84.7% (150/177)** by closing 11 stories — multipart per-part offset tracking, ?partNumber=N GET, per-part composite checksum, multipart copy FlexibleChecksum, listing edge cases, versioning literal "null", multipart Complete preconditions + size-too-small + composite checksum input validation. **Did not hit the 90% headline target**, so the ROADMAP P1 entry stayed open.

This follow-up cycle closes the **16 remaining real failures** clustered in 4 P2 ROADMAP entries plus 4 misc edges, lifting the headline to **≥90% strict (target ~95%)**. With every real failure closed, expected: **166/177 = 93.8% strict** (or 100% real-bug-only excluding the 11 deliberate gaps).

Plus US-006 adds CI test coverage for the **two P1 latent bugs** the prior cycle surfaced (koanf empty-env override + Cassandra null-version timeuuid sentinel) so they cannot regress silently.

## Goals

- s3-tests pass rate **≥90% strict** on the default filter (target **≥95%**, expected 93.8%).
- Close all 16 in-scope real failures from the 2026-05-05 baseline.
- Zero regressions on currently-passing tests.
- Add CI tests for the two P1 latent bugs the prior cycle surfaced.
- Refresh `scripts/s3-tests/README.md` baseline + flip the **P1 — s3-tests 80% → 90%+** ROADMAP entry to Done.
- Cycle-end merge `ralph/s3-compat-95` → main per branch policy.

## User Stories

### US-001: Multipart copy edges (range parser + special chars + size handling)
**Description:** As an S3 client copying multipart parts, the gateway must handle copy-source-range edges, special-character source URLs, and size-aware copy variants the same way AWS does.

**Acceptance Criteria:**
- [ ] `internal/s3api/multipart_copy.go::uploadPartCopy` parses `x-amz-copy-source-range: bytes=start-end` rigorously: empty / malformed / `bytes=0-0` / out-of-bounds → `InvalidRange`. Range over the whole object (`bytes=` omitted) is OK.
- [ ] `x-amz-copy-source: /<src-bucket>/<src-key>` src-key URL-decoded BEFORE bucket-name validation. Special chars (`+`, ` ` (space), `?`, `#`, non-ASCII) round-trip cleanly.
- [ ] Multipart copy of variable-size sources (small + medium + large in one upload) returns the correct per-part ETag in `UploadPartCopyResult` XML — not the whole-source ETag.
- [ ] s3-tests `test_multipart_copy_small`, `_improper_range`, `_special_names`, `_multiple_sizes` pass.
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: ?partNumber=N GET quoted-ETag wire shape
**Description:** As an S3 client comparing a multipart object's per-part ETag from `?partNumber=N` against the part-upload's response ETag, the wire shape must match the upstream s3-tests' assertion (double-quoted hex).

**Acceptance Criteria:**
- [ ] Audit `internal/s3api/server.go::getObject` partNumber path: response `ETag` header MUST be `"<32hex>"` (literal double quotes around hex), matching the part-upload's response format. Today the path may strip / re-quote inconsistently.
- [ ] SSE-C variant of `?partNumber=N` GET returns the same quoted shape (the SSE wrapping must not double-quote or strip).
- [ ] Single-part-object `?partNumber=1` GET returns the whole-object ETag's per-part shape (still `"<32hex>"`).
- [ ] s3-tests `test_multipart_get_part`, `test_multipart_sse_c_get_part`, `test_multipart_single_get_part` pass.
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: Multipart composite checksum CRC32 / CRC32C / CRC64NVME
**Description:** As an S3 client using boto3 ≥1.35's auto-checksum (which ships `crc64nvme` as the default), `CompleteMultipartUpload` must return the correct composite checksum for CRC algos. SHA1/SHA256 closed in prior US-003; CRC family is still off.

**Acceptance Criteria:**
- [ ] `internal/s3api/multipart.go::completeMultipartUpload` computes composite CRC32 / CRC32C / CRC64NVME over per-part raw checksum bytes via the AWS-documented composite algorithm. Verify formula at story start (likely `crc(concat(raw_part_crcs))` — concat raw checksum bytes per part, then compute the CRC of the concat, encode base64).
- [ ] Helpers `FoldCRC32`, `FoldCRC32C`, `FoldCRC64NVME` placed in either `internal/auth` or `internal/data` (decide at story start).
- [ ] Response XML includes the correct `<ChecksumCRC32>` / `<ChecksumCRC32C>` / `<ChecksumCRC64NVME>` element matching what was set on Initiate.
- [ ] HEAD object with `x-amz-checksum-mode: ENABLED` returns the composite CRC header.
- [ ] s3-tests `test_multipart_use_cksum_helper_crc32`, `_crc32c`, `_crc64nvme` pass.
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: Multipart If-Match-on-missing-object: 404 NoSuchKey (revert RFC alignment)
**Description:** As an S3 client doing `If-Match` precondition on `CompleteMultipartUpload` against a non-existent destination object, AWS S3 returns 404 NoSuchKey rather than RFC 7232's 412 PreconditionFailed.

**Acceptance Criteria:**
- [ ] `internal/s3api/multipart.go::completeMultipartUpload` precondition path: when `If-Match` is set AND the destination object does NOT exist, return `404 NoSuchKey` (NOT 412). The match-but-mismatched ETag path stays 412.
- [ ] putObject's If-Match-missing-object path stays 412 (per RFC 7232 §3.1) — the divergence is multipart-Complete-only, AWS's exact shape.
- [ ] s3-tests `test_multipart_put_object_if_match` and `test_multipart_put_current_object_if_match` pass.
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: Misc edge cases (suspended_copy / delete_key_bucket_gone / return_data_versioning / resend_finishes_last)
**Description:** As an S3 client, the gateway must handle four small edge cases identified in the prior cycle's baseline.

**Acceptance Criteria:**
- [ ] `test_versioning_obj_suspended_copy`: copy from a suspended-bucket null-row src to dst, response wire shape includes the SOURCE's `x-amz-version-id: null` header (or `<VersionId>null</VersionId>` in copy result XML).
- [ ] `test_object_delete_key_bucket_gone`: DELETE on a non-existent bucket returns `404 NoSuchBucket` BEFORE the auth check rejects with 403. Swap the order in `internal/s3api/server.go` so the bucket-existence check runs ahead of the auth gate for DELETE.
- [ ] `test_bucket_list_return_data_versioning`: V1 ListObjectVersions `<Owner>` field has `<DisplayName>` set to a non-empty value (today empty string). Wire `bucket.OwnerDisplayName` (or owner name from IAM) into the response.
- [ ] `test_multipart_resend_first_finishes_last`: `meta.Store.CompleteMultipartUpload` flips upload status to `completing` BEFORE per-part ETag validation; a stale-ETag Complete leaks state and breaks idempotent retry. Fix: scan parts up front (status read-only) and only flip after every ETag matches. Lockstep across cassandra + memory + tikv impls.
- [ ] All 4 s3-tests pass.
- [ ] Typecheck passes
- [ ] Tests pass

### US-006: P1 follow-up — koanf + Cassandra null-sentinel CI coverage
**Description:** As a maintainer, I want CI tests covering the two P1 latent bugs the prior cycle surfaced so they cannot regress silently.

**Acceptance Criteria:**
- [ ] `internal/config/config_test.go` adds **koanf empty-env regression test**: TOML default `gc.interval="30s"`, env `STRATA_GC_INTERVAL=""`, assert `cfg.GC.Interval == 30*time.Second`. Asserts the prior cycle's `env.ProviderWithValue` callback skips empty values correctly.
- [ ] `internal/meta/cassandra/store_integration_test.go` (build tag `integration`) adds **Cassandra null-sentinel integration test**: PUT object into bucket with versioning=Suspended (so `o.IsNull=true && o.VersionID==NullVersionID`), GET it back, assert `gocql.ParseUUID` round-trip preserves `meta.NullVersionID` on read AND the Cassandra row stores the v1 sentinel `00000000-0000-1000-8000-000000000000` (verify via raw SELECT).
- [ ] `internal/meta/storetest/contract.go::caseVersioningNullSentinel` extended to also exercise an INSERT with the null sentinel under a real backend.
- [ ] CI workflow's `cassandra-integration` job ensures the new test runs (NOT skipped — print test name in CI output).
- [ ] Typecheck passes
- [ ] Tests pass — including `make test-integration` against a real Cassandra container.

### US-007: Refresh s3-tests baseline + ROADMAP close-flip + cycle-end merge to main
**Description:** As a maintainer, I want the latest s3-tests run captured in the baseline, the ROADMAP P1 flipped to Done (this time we should hit ≥90% strict), and the cycle merged back to main per branch policy.

**Acceptance Criteria:**
- [ ] Run `scripts/s3-tests/run.sh` against a clean `make up-all` stack (cassandra + ceph + strata, auth=required).
- [ ] Append new section to `scripts/s3-tests/README.md` baseline list: `### YYYY-MM-DD — <commit-sha> + ralph/s3-compat-95 (N%)` with the pass-rate line + remaining failure breakdown.
- [ ] Verify pass rate ≥90% strict (target ≥95%). With the 16 real failures closed, expected: 166/177 = 93.8% strict.
- [ ] If ≥90%: flip ROADMAP `**P1 — s3-tests 80% → 90%+.**` to Done close-flip format. Update headline in ROADMAP.md introduction: `**84.7% (150/177)** → **<new>% (<n>/177)**`.
- [ ] Close (Done close-flip) the four ROADMAP P2 entries created by the prior cycle: `Multipart copy edges`, `?partNumber=N quoted-ETag wire shape`, `Multipart composite checksum CRC32 / CRC32C / CRC64NVME`, `Multipart If-Match-on-missing-object error code`. Plus the resend-finishes-last P2.
- [ ] **Cycle-end merge**: fast-forward / squash-merge `ralph/s3-compat-95` → `main`, push origin/main, archive `scripts/ralph/prd.json` + `scripts/ralph/progress.txt` under `scripts/ralph/archive/2026-MM-DD-s3-compat-95/`. Mirror the s3-compat-90 close shape (`cb61770`).
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- **FR-1**: Multipart copy edges fully covered — range parser, URL decode, special chars, per-part ETag wire shape.
- **FR-2**: `?partNumber=N` GET returns `"<32hex>"` quoted ETag header on every variant (regular, SSE-C, single-part).
- **FR-3**: CRC32-family composite checksum implemented per AWS-documented algorithm; HEAD echo + CompleteMultipartUploadResult XML carry it.
- **FR-4**: Multipart Complete + If-Match + missing-object → 404 NoSuchKey (AWS shape, NOT RFC 7232 412).
- **FR-5**: DELETE on missing bucket → 404 NoSuchBucket BEFORE auth gate.
- **FR-6**: V1 ListObjectVersions `<DisplayName>` non-empty.
- **FR-7**: Suspended-bucket copy preserves source `null` version-id in dst response.
- **FR-8**: `CompleteMultipartUpload` validates all per-part ETags BEFORE flipping upload status — stale-ETag retry is idempotent.
- **FR-9**: koanf empty-env handling has CI regression test.
- **FR-10**: Cassandra null-sentinel handling has integration test that runs against a real container.
- **FR-11**: ROADMAP P1 entry `s3-tests 80% → 90%+` flipped to Done at cycle close.

## Non-Goals

- **No SigV2 implementation.** 8 deliberate gaps remain — out of scope forever.
- **No `test_bucket_list_prefix_unreadable`.** Non-UTF-8 prefix bytes — deliberate gap, won't fix.
- **No `test_bucket_list_unordered`.** RGW extension; AWS accepts but ignores. Not implementing.
- **No anonymous-list tests.** `auth-mode=required` rejects anonymous before bucket-policy gate; intentional posture.
- **No `test_bucket_create_exists`.** SDK-quirk (boto wants `e.status` attr); not a gateway issue.
- **No "while-we're-in-there" refactors.** Surgical fixes only — every commit must close at least one s3-test or one P1 follow-up test.
- **No new ROADMAP P-items for tests still failing post-cycle.** If 16 failures don't all close, document as deliberate gaps in `scripts/s3-tests/README.md` only.

## Technical Considerations

- **CRC composite formula**: AWS docs specify the composite as `base64(crc(concat(raw_part_crcs_in_part_number_order)))`. Not the SHA-style "concat-then-sha256". Verify against `https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity.html` at story start.
- **CRC64NVME polynomial**: `0xad93d23594c93659` (NVME / Rocksoft variant, not the standard `hash/crc64`'s ECMA / ISO). Go 1.21+ ships `crc64.MakeTable` with custom polynomial; alternatively vendor a small implementation.
- **`?partNumber=N` ETag wire**: audit BOTH the regular GET path and the partNumber-specific path in `internal/s3api/server.go::getObject`. Look for double-quoting + stripping inconsistencies. Likely the regular path emits `"<hex>"` while partNumber emits raw `<hex>`.
- **DELETE-on-missing-bucket auth ordering**: today auth middleware runs first across all verbs. Either add a special-case in `auth.Middleware` for DELETE (skip auth + run handler which does its own existence check) OR move the bucket-existence check into a router-level pre-check. Lower-risk: the latter, in `s3api.Server.handleObject` before auth dispatch.
- **`bucket.OwnerDisplayName`**: today `meta.Bucket` carries `Owner` (access-key) but not display name. Add the field, populate from IAM user record at bucket-create time, fall back to `Owner` value if no IAM user found.
- **`CompleteMultipartUpload` resend-race fix**: lockstep across cassandra + memory + tikv. The fix is a per-backend ordering change inside the existing transactional path. Storetest contract `caseMultipartCompleteResendRace` covers the invariant.

## Success Metrics

- s3-tests pass rate ≥90% strict (target 93.8%).
- All 16 real failures from the 2026-05-05 baseline pass.
- Zero regressions on currently-passing tests.
- Two new CI tests catch koanf empty-env + Cassandra null-sentinel regressions.
- ROADMAP P1 flipped to Done.

## Open Questions

- **CRC composite formula nuance**: AWS docs are not 100% clear on whether the composite is `base64(crc(concat(raw_crcs)))` or `base64(concat(raw_crcs))` with the algorithm carried in `<ChecksumType>COMPOSITE</ChecksumType>`. boto3's source is the authoritative reference — read `_calculate_composite_checksum` at story start.
- **`bucket.OwnerDisplayName` storage**: add as schema-additive field (`ALTER TABLE buckets ADD owner_display_name text`) or compute on-the-fly from IAM lookup at response time. Decision: schema-additive (consistent with US-049 manifest pattern).
- **Resend-race contract test**: the prior cycle's progress note flagged that the in-memory `caseMultipartCompleteResendRace` test was reduced; this cycle restores it. Verify cassandra + tikv impls support the full invariant before declaring US-005 done.
