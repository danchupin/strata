# PRD: s3-tests Pass Rate 80% → 90%+

## Introduction

The `ceph/s3-tests` suite is the de-facto S3 compatibility benchmark and
Strata's headline compatibility number. Current pass rate on the executable
subset (`scripts/s3-tests/run.sh`'s default filter) is **80.1%
(141/176)** as recorded in `scripts/s3-tests/README.md`.

35 tests fail. 8 are deliberate SigV2 gaps (`test_headers.py` — won't fix:
SigV2 is intentionally unsupported and out of scope). 1 more is a deliberate
deprecated-input gap (`test_bucket_list_prefix_unreadable` — non-UTF-8
prefix bytes; see Non-Goals). The remaining 26 are real S3 surface gaps
clustered into:

- 5: multipart per-part composite checksum response shape (modern
  FlexibleChecksum SDK path)
- 3: `?partNumber=N` GET on multipart objects (per-part offset tracking)
- 4: multipart copy `FlexibleChecksum` path
- 5: versioning literal "null" version-id (PUT to unversioned bucket later
  enabled, plus Suspended-versioning semantics)
- 4: listing edge cases (`delimiter+prefix`, V2 continuation-token
  opacity)
- 5: misc (multipart resend ordering, size-too-small validation, checksum
  SHA256 input validation, multipart `If-Match` on Complete in versioned
  bucket)

Closing all 26 lifts the headline to 167/176 = **94.9%**. Closing 80% of
them (~21 of 26) lifts to 162/176 = **92.0%** — comfortably ≥90%.

This PRD covers the per-cluster fixes. Each cluster gets its own user
story (or pair) sized for one Ralph iteration.

**Scope audit (2026-04-29).** The first draft included a story for
non-UTF-8 prefix bytes (`test_bucket_list_prefix_unreadable`) and listed
allow-unordered + V1-marker + encoding-type as candidate listing fixes.
After review:
- **Dropped:** non-UTF-8 prefix story — declared deliberate gap. Real
  clients do not pass invalid UTF-8 in query params; AWS itself does not
  guarantee clean behaviour. Robustness-only test, not a customer-impact
  feature.
- **Out of this PRD:** allow-unordered, V1-marker semantics, encoding-type
  edge cases. `test_bucket_list_unordered` is an RGW extension; V1
  ListObjects is deprecated by AWS in favour of V2; encoding-type cases
  are SDK-quirk-driven. None block "drop-in RGW replacement" for modern
  client surface.
- **Kept:** all of multipart per-part checksum + per-part GET + copy
  FlexibleChecksum + versioning null (incl. Suspend) + delimiter+prefix +
  V2 opaque continuation token + multipart preconditions + checksum
  validation. Modern boto / aws-cli 2.x / Athena / Glue / Snowflake all
  exercise these paths.

## Goals

- s3-tests pass rate ≥90% on the default filter
- No regressions on any currently-passing test
- Update `scripts/s3-tests/README.md` baseline section per cluster closed
- Per-commit ROADMAP.md sync — flip P1 entry to Done when ≥90% verified

## User Stories

### US-001: Multipart per-part offset tracking in `data.Manifest`
**Description:** As a developer fixing `?partNumber=N` GET, I need each part's
byte-range to be queryable from the manifest so a ranged GET can serve the
exact bytes of part N without scanning the whole object.

**Acceptance Criteria:**
- [ ] Add `PartChunks []PartRange` field to `data.Manifest` (struct: `PartNumber
      int`, `Offset int64`, `Size int64`, `ETag string`, `ChecksumValue
      string`, `ChecksumAlgorithm string`)
- [ ] Field is optional (`json:",omitempty"` + matching protobuf tag in
      `manifest.proto` + helper updates in `manifest_codec.go`)
- [ ] `CompleteMultipartUpload` populates `PartChunks` from the
      `multipart_uploads.parts` rows on the LWT-success path
- [ ] Single-PUT objects leave `PartChunks` nil (not an empty slice — `nil`
      sentinels "not multipart")
- [ ] Both encodings (JSON + protobuf) round-trip via existing
      `data.EncodeManifest` / `DecodeManifest`
- [ ] Migration: existing rows decode with `PartChunks=nil` and serve normally
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: `?partNumber=N` GET serves the exact part body
**Description:** As an S3 client, when I GET an object with `?partNumber=3`, I
expect to receive only the bytes of part 3, plus headers
`x-amz-mp-parts-count` and `Content-Range: bytes <off>-<end>/<total>`.

**Acceptance Criteria:**
- [ ] `internal/s3api/object_get.go::getObject` handles `?partNumber=N`
      query param: looks up `PartChunks[N-1]` from the decoded manifest
- [ ] Returns `416 InvalidRange` when N <= 0 or N > len(PartChunks)
- [ ] Returns `416` for non-multipart objects when `partNumber` is set
- [ ] Sets response headers: `x-amz-mp-parts-count`, `Content-Range`,
      `Content-Length=<part size>`, `ETag` of the part (not whole object)
- [ ] Streams `[Offset, Offset+Size)` of the underlying chunk stream
- [ ] `Range: bytes=...` combined with `?partNumber=` is part-relative
      (offsets resolve within the part, not the whole object)
- [ ] s3-tests `test_multipart_*_get_part` (3 tests) pass
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: Per-part composite-checksum response shape
**Description:** As an S3 client using SDK auto-checksum, the multipart
`CompleteMultipartUpload` response must carry the composite checksum AND
`?partNumber=N` HEAD/GET must echo per-part checksums in
`x-amz-checksum-<algo>` headers when `ChecksumMode=ENABLED`.

**Acceptance Criteria:**
- [ ] `CompleteMultipartUploadResult` XML body includes `<ChecksumCRC32>`,
      `<ChecksumCRC32C>`, `<ChecksumSHA1>`, `<ChecksumSHA256>` (whichever was
      set on Initiate) — composite computed across part checksums per AWS
      formula (concatenate raw part-digests, SHA-256 of concat, base64)
- [ ] Response also includes `<ChecksumType>FULL_OBJECT</ChecksumType>` or
      `COMPOSITE` matching what was set on Initiate
- [ ] HEAD object with `x-amz-checksum-mode: ENABLED` returns the composite
      checksum header
- [ ] HEAD object with `?partNumber=N` + `ChecksumMode=ENABLED` returns the
      per-part checksum from `PartChunks[N-1].ChecksumValue`
- [ ] s3-tests `test_multipart_use_cksum_helper_*` (5 tests) pass
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: Multipart copy FlexibleChecksum path
**Description:** As an S3 client copying a multipart object via
`UploadPartCopy`, the boto SDK auto-generates a checksum on the copy body and
expects the gateway to recompute and validate it. Today we accept the body
without checksum validation, which the SDK's `FlexibleChecksumError`
catches.

**Acceptance Criteria:**
- [ ] `internal/s3api/multipart_copy.go::uploadPartCopy` reads
      `x-amz-checksum-algorithm` header from request
- [ ] When set, recompute the named checksum over the body bytes during
      streaming (do not buffer)
- [ ] Compare with `x-amz-checksum-<algo>` header value if client supplied;
      mismatch returns `BadDigest`
- [ ] Store the computed checksum in `multipart_uploads.parts[N].checksum_value`
      so US-001's `PartChunks` carries it forward
- [ ] s3-tests `test_multipart_copy_*` (4 tests) pass
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: Listing — `delimiter+prefix` interaction
**Description:** As an S3 client listing with both `prefix` and `delimiter`,
the response must group keys under the prefix into `CommonPrefixes` based on
the delimiter, with `NextMarker` reflecting the last grouped prefix correctly.

**Acceptance Criteria:**
- [ ] `internal/s3api/list_objects.go` handles `prefix=foo/&delimiter=/` by
      extracting the substring between the prefix end and the next delimiter
      occurrence in each key
- [ ] `CommonPrefixes` deduplicates within a page, in lexical order
- [ ] `NextMarker` (V1) / `NextContinuationToken` (V2) is set to the last
      key OR the last `CommonPrefix` returned, whichever is lexically later
- [ ] s3-tests `test_bucket_list_delimiter_prefix*` pass (currently 1-2
      failures in this category)
- [ ] Typecheck passes
- [ ] Tests pass

### US-006: Listing — V2 continuation token as opaque base64
**Description:** As an S3 client paging through `ListObjectsV2`, the
`ContinuationToken` is an opaque token (AWS-style: base64 of an internal
struct). Today we treat it as a literal marker, which fails when the marker
contains base64-decode-incompatible bytes.

**Acceptance Criteria:**
- [ ] `ContinuationToken` in response is base64 of a struct
      `{Marker string; Shard int}` (JSON-encoded then base64)
- [ ] `?continuation-token=<token>` request parses the token back into Marker
      + Shard
- [ ] Backward compat: tokens that don't decode as base64-JSON fall back to
      the literal-marker interpretation (so existing clients in flight don't
      break mid-page)
- [ ] V1 `marker` parameter unchanged — opaque tokens are V2-only
- [ ] s3-tests `test_bucket_listv2_continuationtoken*` pass
- [ ] Typecheck passes
- [ ] Tests pass

### US-007: Versioning — literal "null" version-id semantics
**Description:** As an S3 client, when I PUT an object into an unversioned
bucket and later enable versioning, the original object must be addressable
via `versionId=null`. PUTs while versioning is suspended also produce
`null`-versioned objects.

**Acceptance Criteria:**
- [ ] `meta.Object.VersionID` accepts the literal string `"null"` for
      unversioned-bucket PUTs and suspended-bucket PUTs
- [ ] `?versionId=null` on GET / HEAD / DELETE resolves the row with
      `VersionID="null"`
- [ ] On versioning Enable + subsequent overwrite, the prior `null`
      version is preserved (not deleted; new version generated UUID)
- [ ] On versioning Suspend + new PUT, any existing `null` version is
      overwritten (DELETE old null + INSERT new null) atomically via LWT
- [ ] `ListObjectVersions` shows `null` rows in clustering order alongside
      UUID versions
- [ ] s3-tests `test_versioning_obj_plain_null_version_*` +
      `test_versioning_obj_suspend*` (5 tests) pass
- [ ] Typecheck passes
- [ ] Tests pass

### US-008: Multipart — Complete preconditions (`If-Match` / `If-None-Match`)
**Description:** As an S3 client completing a multipart upload conditionally,
the gateway must gate the LWT path on `If-Match` / `If-None-Match` headers
referring to the eventual object ETag (not the upload ID).

**Acceptance Criteria:**
- [ ] `internal/s3api/multipart.go::completeMultipartUpload` reads
      `If-Match` / `If-None-Match` from the request
- [ ] When `If-None-Match: *` is set and the object exists, returns
      `412 PreconditionFailed`
- [ ] When `If-Match` is set and does NOT match the existing object's ETag,
      returns `412 PreconditionFailed`
- [ ] Precondition check happens BEFORE the LWT flip (so a conflicting Complete
      attempt does not leak `completing` state)
- [ ] `VersionId` header is set on the response when the bucket is versioned
- [ ] s3-tests `test_multipart_put_object_if_*` +
      `test_multipart_put_current_object_if_match` pass
- [ ] Typecheck passes
- [ ] Tests pass

### US-009: Multipart — size-too-small + resend-ordering edge cases
**Description:** As an S3 client, the gateway must reject `CompleteMultipartUpload`
when any non-last part is smaller than 5 MiB, AND must handle the case where
a resent part finishes after a later part (resend-first-finishes-last).

**Acceptance Criteria:**
- [ ] `completeMultipartUpload` enumerates parts in order, asserts every
      part except the last is ≥5 MiB (5\*1024\*1024 bytes); returns
      `EntityTooSmall` on violation
- [ ] When two `UploadPart` calls land for the same part number with
      different ETags (race), the LATER write wins (LWT on
      `multipart_uploads.parts` keyed on `part_number`)
- [ ] `CompleteMultipartUpload` body's `<Part>` list specifies ETags;
      mismatch with stored part ETag = `InvalidPart`
- [ ] s3-tests `test_multipart_upload_size_too_small`,
      `test_multipart_upload_resend_part`,
      `test_multipart_resend_first_finishes_last` pass
- [ ] Typecheck passes
- [ ] Tests pass

### US-010: Multipart — `CompleteMultipartUpload` SHA256 input validation
**Description:** As an S3 client supplying a composite-checksum SHA256 on
`CompleteMultipartUpload`, the gateway must validate the supplied digest
against the recomputed value and reject mismatches.

**Acceptance Criteria:**
- [ ] `completeMultipartUpload` reads `x-amz-checksum-sha256` from request
      headers
- [ ] When set, recomputes the composite SHA-256 over part digests and
      compares; mismatch returns `BadDigest`
- [ ] Same handling for CRC32, CRC32C, SHA1
- [ ] s3-tests `test_multipart_checksum_sha256` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-011: Refresh s3-tests baseline + ROADMAP close-flip
**Description:** As a maintainer, I want the latest s3-tests run captured in
`scripts/s3-tests/README.md` and the ROADMAP P1 flipped to Done.

**Acceptance Criteria:**
- [ ] Run `scripts/s3-tests/run.sh` against a clean `make up-all` stack
- [ ] Append a new section to `scripts/s3-tests/README.md` baseline list:
      `### YYYY-MM-DD — <commit-sha>` with the pass-rate line + remaining
      failure breakdown
- [ ] Verify pass rate ≥90% (target ≥94%; closing all 26 in-scope failures
      reaches 94.9%)
- [ ] If <90%: file the remaining gaps as new P1/P2 entries under appropriate
      ROADMAP sections; do NOT flip the headline
- [ ] If ≥90%: flip ROADMAP P1 entry to Done close-flip format with the
      closing SHA (or `(commit pending)`)
- [ ] Document `test_bucket_list_prefix_unreadable`,
      `test_bucket_list_unordered`, `test_bucket_create_exists`, and any V1
      ListObjects / encoding-type / SDK-quirk failures as **deliberate gaps**
      in `scripts/s3-tests/README.md` (not as new ROADMAP entries) so they
      stop reading as open work
- [ ] Update headline number in ROADMAP.md introduction:
      `**80.1% (141/176)**` → `**<new>% (<n>/176)**`
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- FR-1: `data.Manifest` carries optional per-part offset+size+checksum data
  for multipart objects, schema-additive on both JSON and protobuf encodings
- FR-2: `?partNumber=N` GET / HEAD serves part-N body + per-part headers
- FR-3: `CompleteMultipartUpload` response and HEAD include composite +
  per-part checksums when configured on Initiate
- FR-4: `UploadPartCopy` validates `x-amz-checksum-*` headers on the body
  stream; mismatch returns `BadDigest`
- FR-5: `ListObjects[V2]` correctly handles `delimiter+prefix` and opaque V2
  continuation tokens (modern paginator surface). V1 marker semantics,
  encoding-type edge cases, allow-unordered, and non-UTF-8 prefix bytes
  are explicitly out of scope (see Non-Goals)
- FR-6: Versioning supports the literal `"null"` version-id for
  unversioned-bucket and suspended-bucket PUTs, including the atomic-replace-
  on-suspend invariant
- FR-7: `CompleteMultipartUpload` honors `If-Match` / `If-None-Match` before
  the LWT flip, sets `VersionId` on response in versioned buckets, validates
  composite checksum input, and rejects undersized non-last parts
- FR-8: `scripts/s3-tests/README.md` baseline section is appended (not
  replaced) on every closing commit; the headline number in `ROADMAP.md` is
  kept in sync per the project root CLAUDE.md "Roadmap maintenance" rule

## Non-Goals

- **No SigV2 implementation.** The 8 `test_headers.py` failures are
  deliberate; SigV2 is out of scope and stays out of scope. This PRD targets
  the 26 non-SigV2 / non-deprecated failures only
- **No non-UTF-8 prefix support.** `test_bucket_list_prefix_unreadable`
  exercises a robustness corner where the client passes invalid UTF-8 in
  query params. AWS itself does not guarantee clean behaviour. Real
  clients do not do this. Declared a deliberate gap in
  `scripts/s3-tests/README.md`
- **No V1 ListObjects marker / encoding-type fixes.** AWS deprecated V1 in
  favour of V2; modern SDKs (boto3, aws-sdk-go-v2, aws-cli 2.x) use V2.
  V1 stays served-but-not-perfect. Not blocking "drop-in RGW replacement"
  for current S3 client surface
- **No `allow-unordered=true` extension.** This is an RGW-specific extension
  query parameter; AWS accepts but ignores. We will not even accept-and-
  ignore in this cycle. Document as deliberate gap if `test_bucket_list_unordered`
  remains failing post-cycle
- **No "all-tests" coverage.** Default filter
  (`test_bucket_create or test_bucket_list or ...`) defines the executable
  subset. Tests outside the filter (ACLs, website, replication, etc.) are
  scoped to their own future PRDs
- **No new ROADMAP P-items for tests still failing post-cycle.** Anything
  fixed during this cycle gets its closing acknowledgement; anything left
  failing is documented in `scripts/s3-tests/README.md` only and explicitly
  triaged either as deliberate gap or future work
- **No protocol behaviour changes outside the failing test list.** "While
  we're in there" tweaks defer to separate PRDs

## Technical Considerations

- `data.Manifest` is encoded into the `objects.manifest` blob column —
  `PartChunks` must serialise via both `data.EncodeManifest` (current JSON or
  protobuf format selected at startup via `STRATA_MANIFEST_FORMAT`) and
  `data.DecodeManifest` (sniffs format)
- The protobuf manifest schema (`internal/data/manifest.proto`) needs
  additive field updates with stable field numbers; renumbering is forbidden
- Per-part checksums must be stored at upload time
  (`internal/s3api/multipart.go::uploadPart` + `multipart_uploads.parts`
  schema), not recomputed at Complete time — the data may have been streamed
  past
- Listing fixes must not regress the existing fan-out / heap-merge path; all
  changes are in the response-shape layer, not the storage layer
- Versioning `null` semantics interact with LWT on `objects` clustering key
  `(version_id DESC)` — the literal string `"null"` sorts after all UUID
  versions in CQL `text` clustering, which is the desired behaviour (latest
  null shows on top of any earlier UUID-versioned ancestors when versioning
  was off)

## Success Metrics

- s3-tests pass rate ≥90% on default filter (target ≥94% — closing all 26
  in-scope failures lifts headline to 94.9%)
- Zero regressions on currently-passing tests (numeric assert: passing count
  before ≤ passing count after)
- All 11 stories shipped within one Ralph cycle
- ROADMAP P1 flipped to Done

## Open Questions

- `test_bucket_create_exists` shape (boto wants `e.status` attr) might be a
  boto SDK version artefact, not a gateway issue. If still failing
  post-cycle, document as a deliberate gap (SDK-quirk) in
  `scripts/s3-tests/README.md` rather than spending a story on it
- The next pass-rate target (`95% → 100%`) would mean closing the SigV2
  gap, which is explicitly never going to happen. Practical ceiling on the
  current default filter is ~95% (167-168 / 176, depending on whether the
  SDK-quirk test counts). Expanding the default filter (ACLs, replication,
  website, lifecycle full surface) is a separate PRD decision and should
  wait until current-filter failures are <3
