# PRD: S3 Protocol Compatibility — full roadmap to >80% s3-tests pass rate

## 1. Introduction

Strata is an S3-compatible object gateway. Today it passes ~58% of the executable subset of Ceph's `s3-tests` (11/19) and ~3 of 1046 on the full suite, because many S3 surface areas are missing or stubbed: ACL plumbing, bucket policies, IAM, checksums, SSE, notifications, website hosting, replication, and several smaller endpoints.

This PRD scopes the work needed to bring Strata to **>80% pass rate on the executable subset of s3-tests** (default filter: `bucket_create | bucket_list | object_write | object_read | object_delete | multipart | versioning_obj | bucket_list_versions`) and to lift the full-suite pass count out of the single digits. Block 1 (CORS / bucket policy / public-access-block / ownership-controls) has already shipped; this document covers blocks 2-7 plus the auth/protocol gaps left in `ROADMAP.md`.

The intended reader is a developer or AI agent picking up implementation. Each User Story is sized for a single focused session: one feature, route + store + tests.

## 2. Goals

- Lift the s3-tests pass rate on the executable subset above 80%.
- Implement every S3 endpoint that has at least one test in the upstream `ceph/s3-tests` suite for the current default filter.
- Add real authorization: ACL parsing/enforcement, bucket policy enforcement, IAM principal model.
- Support modern AWS-SDK defaults: trailing checksum headers (CRC32 minimum), `x-amz-bucket-region`, anonymous public-read access.
- Keep both memory and Cassandra metadata backends fully working through every change. `meta.Store` interface evolves freely (pre-1.0); both implementations must update in lockstep.
- All new code lands with unit tests. Integration tests against `ceph/s3-tests` runner re-run after each block; expected pass-rate delta documented in the PR.

## 3. User Stories

User stories are grouped by block. Each story is a focused, ship-shaped change: route + store + tests. UI is not in scope (Strata is a backend); the dev-browser acceptance criterion is omitted.

### Block 2 — Real ACL & Policy Enforcement

#### US-001: Parse `AccessControlPolicy` XML body for PutBucketAcl / PutObjectAcl
**Description:** As a client, I want PutBucketAcl/PutObjectAcl to accept a full XML grant list, not just the `x-amz-acl` canned header, so I can grant fine-grained permissions to specific principals.

**Acceptance Criteria:**
- [ ] Parse `<AccessControlPolicy>` body with `<Owner>` and `<AccessControlList><Grant>...` per AWS spec.
- [ ] Persist grant list in `meta.Store` (new `BucketGrants` / `ObjectGrants` blob columns).
- [ ] GetBucketAcl/GetObjectAcl returns the persisted grants (not just rebuilt-from-canned).
- [ ] Reject grants with unknown grantee types or permissions with `MalformedACLError`.
- [ ] Tests: round-trip XML body, mixed canned + body, malformed body 400.
- [ ] `go test ./internal/s3api ./internal/meta/...` green.

#### US-002: Enforce bucket policy on object-level requests
**Description:** As a bucket owner, I want a JSON bucket policy with `Allow`/`Deny` statements to actually gate access — anonymous and cross-account requests should be checked against the policy.

**Acceptance Criteria:**
- [ ] Implement `internal/auth/policy.go` evaluator: minimal IAM-style `Effect`, `Principal`, `Action`, `Resource`, `Condition` (string equality only).
- [ ] Middleware (`auth.go`) consults policy after SigV4 succeeds; for anonymous, skips SigV4 and consults policy directly.
- [ ] Object handlers gain a `requireAccess(action, resource)` call before any state change.
- [ ] Tests: anonymous GET allowed by `s3:GetObject` Allow; anonymous GET denied without; explicit `Deny` overrides `Allow`.
- [ ] Pass rate on `test_bucket_policy_*` tests measured before/after.

#### US-003: Enforce bucket ACL on object-level requests
**Description:** As a bucket owner, when ACL is set to `public-read`, anonymous GETs should succeed; when `private`, they should 403. Today the canned ACL is stored but never checked.

**Acceptance Criteria:**
- [ ] Anonymous GET `/<bucket>/<key>` succeeds when bucket ACL is `public-read` or `public-read-write`.
- [ ] Anonymous PUT succeeds only when ACL is `public-read-write`.
- [ ] Object-level ACL grants override bucket ACL on per-object reads.
- [ ] Tests covering the four canned values × {GET, PUT, DELETE} × {anon, owner, alt}.
- [ ] `test_bucket_acl_*` and `test_object_acl_*` pass-rate measured.

### Block 3 — IAM & Dynamic Credentials

#### US-004: Cassandra-backed credentials store
**Description:** As an operator, I want to add and rotate access keys without restarting the gateway, so I can onboard new clients in production. Today the only credentials source is the static env-string `STRATA_STATIC_CREDENTIALS`.

**Acceptance Criteria:**
- [ ] New `cassandra.CredentialsStore` implementing the `auth.Store` interface against the `access_keys` table (already in schema).
- [ ] Composable: `auth.MultiStore` tries static first, then Cassandra.
- [ ] Background reload every 60s for cache freshness, or invalidation hook on writes.
- [ ] Memory `auth.Store` for tests preserved.
- [ ] Tests: lookup hit/miss/disabled, key rotation, deleted-key returns `InvalidAccessKeyId`.
- [ ] Integration test under `-tags integration` writes a key, restarts gateway, reads it back.

#### US-005: IAM endpoints — CreateUser / CreateAccessKey / DeleteUser / DeleteAccessKey / ListUsers / ListAccessKeys
**Description:** As an admin tool, I want to call `POST /?Action=CreateUser` etc. against the gateway to provision principals, so the s3-tests fixtures (`[iam root]`, `[iam alt root]`) can spin up isolated identities.

**Acceptance Criteria:**
- [ ] New handler `internal/s3api/iam.go` routing `?Action=...` to the right method.
- [ ] AWS-style XML form-encoded request/response shapes (verify against `iam:CreateUser` AWS docs).
- [ ] `iam_users` and `access_keys` tables in Cassandra; mirror in memory.
- [ ] `[iam root]` credentials gate access to `?Action=` endpoints (admin).
- [ ] Tests: round-trip CreateUser → ListUsers → DeleteUser; CreateAccessKey rotates immediately.
- [ ] s3-tests collection-error count drops measurably (158 → ?).

#### US-006: STS AssumeRole minimal subset
**Description:** As a multi-account test fixture, I want temporary credentials with an expiry, so cross-account scenarios in s3-tests can run.

**Acceptance Criteria:**
- [ ] `POST /?Action=AssumeRole` returns `AccessKeyId`, `SecretAccessKey`, `SessionToken`, `Expiration`.
- [ ] SigV4 middleware honors `x-amz-security-token` header against the temporary key.
- [ ] Token storage in-memory only (acceptable given expiry).
- [ ] Tests: assume → use → expire → 403.

### Block 4 — Checksums

#### US-007: Trailing checksum headers on PutObject (CRC32, CRC32C, SHA1, SHA256, CRC64NVME)
**Description:** As an AWS SDK v3 client (which sends checksums by default), I want my PutObject requests with `x-amz-checksum-crc32` / `x-amz-sdk-checksum-algorithm` to succeed and have the checksum stored, so my idempotency and retry logic works.

**Acceptance Criteria:**
- [ ] Parse `x-amz-checksum-<algo>` request headers; verify against streamed body content.
- [ ] Persist checksum in object metadata (new `meta.Object.Checksums map[string]string`).
- [ ] Return `x-amz-checksum-<algo>` on GetObject / HeadObject.
- [ ] Mismatch → `BadDigest` 400.
- [ ] Five algorithms: crc32, crc32c, sha1, sha256, crc64nvme (use stdlib + go-ceph for crc64nvme polynomial 0xad93d23594c93659).
- [ ] Tests per algorithm: positive, mismatch, missing.
- [ ] `test_*checksum*` pass-rate measured.

#### US-008: Multipart UploadPart checksum support
**Description:** As an SDK v3 client uploading multipart, I want each part's checksum verified and the composite checksum returned by CompleteMultipartUpload.

**Acceptance Criteria:**
- [ ] UploadPart accepts and verifies `x-amz-checksum-*` per part.
- [ ] CompleteMultipartUpload computes composite checksum (algo-specific concat-and-hash rule) and returns it.
- [ ] Per-part checksum stored in `meta.MultipartPart.Checksums`.
- [ ] Tests: round-trip a 3-part multipart with crc32 checksum, verify response.

### Block 5 — Server-Side Encryption Headers (passthrough)

#### US-009: SSE-S3 metadata storage
**Description:** As a client setting `x-amz-server-side-encryption: AES256` on PutObject, I want the header to be accepted, persisted, and echoed back on Get/Head, even if Strata does not actually re-encrypt at rest yet.

**Acceptance Criteria:**
- [ ] `meta.Object.SSE` field stores algorithm string.
- [ ] PutObject / Initiate Multipart accept `x-amz-server-side-encryption`.
- [ ] GetObject / HeadObject return the same header.
- [ ] PutBucketEncryption / GetBucketEncryption / DeleteBucketEncryption endpoints (XML config blob, persist as bucket-level config).
- [ ] Tests: round-trip per object, round-trip per bucket default.

#### US-010: SSE-C (customer-provided keys) headers
**Description:** As a client uploading with SSE-C, I want the gateway to accept and validate the customer key headers and reject Get requests that omit them.

**Acceptance Criteria:**
- [ ] Accept `x-amz-server-side-encryption-customer-{algorithm,key,key-MD5}` on PutObject.
- [ ] Validate base64 key and MD5; mismatch → 400 `InvalidArgument`.
- [ ] GetObject without matching headers → 400 `InvalidRequest`.
- [ ] CopyObject `x-amz-copy-source-server-side-encryption-customer-*` mirrors source-side keys.
- [ ] Note: actual encryption is out of scope; we store the MD5 and reject mismatches.
- [ ] Tests: positive, missing key on GET, wrong MD5.

### Block 6 — Object-Level Smaller Endpoints

#### US-011: GetObjectAttributes
**Description:** As a client preparing a multipart copy, I want a single low-cost call returning ETag, Checksum, ObjectParts, ObjectSize, StorageClass — instead of HEAD + ListParts.

**Acceptance Criteria:**
- [ ] `GET /<bucket>/<key>?attributes` route.
- [ ] Honors `x-amz-object-attributes` header (comma-separated subset).
- [ ] XML response per AWS schema.
- [ ] Tests: full response, partial via header, missing key 404.

#### US-012: RestoreObject (stub)
**Description:** As a client calling `POST ?restore`, I want a 200 acknowledgment so SDKs that issue restore-on-read for GLACIER objects don't fail outright. Restore is a no-op (Strata has no archival tier yet).

**Acceptance Criteria:**
- [ ] `POST /<bucket>/<key>?restore` parses `RestoreRequest` XML.
- [ ] Returns 200 OK if object exists, 404 otherwise.
- [ ] Object's `RestoreStatus` field set to `ongoing-request="false", expiry-date=…` and surfaced on HEAD as `x-amz-restore`.
- [ ] Tests: round-trip + 404 + malformed XML.

#### US-013: PutBucketObjectLockConfiguration / GetBucketObjectLockConfiguration
**Description:** As a regulated client, I want to set bucket-level default retention so new objects inherit it. Today only object-level retention works.

**Acceptance Criteria:**
- [ ] `PUT /<bucket>?object-lock` parses `ObjectLockConfiguration` XML and persists per bucket.
- [ ] PutObject inherits `Mode` and `RetainUntilDate` when the headers are not on the request.
- [ ] CreateBucket with `x-amz-bucket-object-lock-enabled: true` sets the bucket flag.
- [ ] Tests: round-trip config; inheritance on PutObject; locked PUT cannot delete.

### Block 7 — Bucket-Level Endpoints

#### US-014: PutBucketNotificationConfiguration / GetBucketNotificationConfiguration (storage-only)
**Description:** As a client configuring event notifications, I want the gateway to accept and return my notification configuration XML, even if Strata has no event publisher yet.

**Acceptance Criteria:**
- [ ] `PUT/GET /<bucket>?notification` round-trips the XML blob through `meta.Store`.
- [ ] Schema validation: at least one of `TopicConfiguration`, `QueueConfiguration`, `LambdaFunctionConfiguration`.
- [ ] Tests: round-trip and malformed.

#### US-015: PutBucketWebsite / GetBucketWebsite / DeleteBucketWebsite + index doc routing
**Description:** As a static-site host, I want bucket website config and have GET on a virtual-host serve `index.html` when the request path is empty.

**Acceptance Criteria:**
- [ ] `PUT/GET/DELETE /<bucket>?website` round-trips XML.
- [ ] When website config is present, `GET /<bucket>/` serves the configured `IndexDocument` object.
- [ ] When path matches no key, serve the configured `ErrorDocument` with 404.
- [ ] Tests: round-trip; index serving; error doc.

#### US-016: PutBucketReplication / GetBucketReplication / DeleteBucketReplication (storage-only)
**Description:** As a client wiring up cross-region replication on the source side, I want the configuration round-trip even though replication is not actually executed yet.

**Acceptance Criteria:**
- [ ] `PUT/GET/DELETE /<bucket>?replication` round-trips the XML.
- [ ] Validation: at least one `Rule` with `Destination`.
- [ ] PutObject sets `x-amz-replication-status: PENDING` when replication is configured (header-only).
- [ ] Tests: round-trip + status header presence.

#### US-017: PutBucketLogging / GetBucketLogging
**Description:** As an audit setup, I want to round-trip server-access logging config.

**Acceptance Criteria:**
- [ ] `PUT/GET /<bucket>?logging` round-trips XML.
- [ ] Empty body on PUT clears the config.
- [ ] Tests: round-trip and clear.

#### US-018: PutBucketTagging / GetBucketTagging / DeleteBucketTagging
**Description:** As a billing/audit consumer, I want bucket-level tags. Object tagging already works.

**Acceptance Criteria:**
- [ ] `PUT/GET/DELETE /<bucket>?tagging` round-trips per AWS schema.
- [ ] Tests: round-trip + 404 when not set.

#### US-019: GetBucketLocation + `x-amz-bucket-region` on HEAD
**Description:** As an SDK doing pre-flight region detection, I want `GET /<bucket>?location` and `HEAD /<bucket>` to return the configured region.

**Acceptance Criteria:**
- [ ] `GET /<bucket>?location` returns `<LocationConstraint>` (region or empty for `default`).
- [ ] HEAD bucket sets `x-amz-bucket-region` header.
- [ ] Tests: both shapes.

#### US-020: CreateBucket parses `<LocationConstraint>` body
**Description:** As an SDK creating a bucket in a non-default region, I want the LocationConstraint XML body parsed and persisted; today it is silently ignored, and a region-aware client can fail.

**Acceptance Criteria:**
- [ ] CreateBucket parses optional XML body.
- [ ] Region persisted in `meta.Bucket.Region`.
- [ ] GetBucketLocation returns it.
- [ ] Tests: round-trip with region; no-body still works.

### Block 8 — Auth & Protocol Hardening

#### US-021: Per-chunk signature validation in STREAMING-AWS4-HMAC-SHA256-PAYLOAD
**Description:** As a security-conscious operator, I want the chained per-chunk HMAC verified — today we decode the framing but skip the chain, so an attacker can mutate chunked uploads.

**Acceptance Criteria:**
- [ ] Implement chain: `sig(chunk_n) = HMAC(signing_key, "AWS4-HMAC-SHA256-PAYLOAD\n<date>\n<scope>\n<prev-sig>\n<hash("")>\n<hash(chunk)>")`.
- [ ] Reject mismatched chunk → 403 `SignatureDoesNotMatch`.
- [ ] Final empty chunk also verified.
- [ ] Tests: positive vector from AWS docs; mutated chunk rejected.

#### US-022: CompleteMultipartUpload idempotency on retry
**Description:** As a flaky-network client, I want a retry of CompleteMultipartUpload after a successful first attempt to return success, not LWT failure.

**Acceptance Criteria:**
- [ ] After successful Complete, persist `(uploadID, etag)` for 10 minutes.
- [ ] Second Complete with same uploadID returns the stored ETag/result.
- [ ] After 10 minutes, returns `NoSuchUpload`.
- [ ] Tests: success → retry returns same XML; expiry → 404.

#### US-023: Anonymous bucket access mode
**Description:** As an unauthenticated client requesting a bucket flagged public-read, I want the request to skip SigV4 entirely and be authorized by the bucket policy/ACL.

**Acceptance Criteria:**
- [ ] Middleware: when no `Authorization` header AND no presigned query params, call `auth.AnonymousIdentity` instead of denying.
- [ ] Anonymous identity passes through to policy/ACL evaluation (US-002, US-003).
- [ ] `STRATA_AUTH_MODE=required` config flag still rejects anonymous globally; `STRATA_AUTH_MODE=optional` enables anonymous gating.
- [ ] Tests: anonymous on public bucket OK; anonymous on private bucket 403.

#### US-024: MFA Delete (header-gated)
**Description:** As a bucket owner with MFA Delete enabled, I want delete-version requests without an `x-amz-mfa` header to be rejected.

**Acceptance Criteria:**
- [ ] PutBucketVersioning accepts `<MfaDelete>Enabled</MfaDelete>` and persists.
- [ ] DeleteObjectVersion requires `x-amz-mfa: <serial> <code>` when set.
- [ ] Static-config TOTP secret accepted (no real MFA backend); good for tests.
- [ ] Tests: enabled + missing → 403; enabled + valid → 204; disabled → 204 always.

### Block 9 — CopyObject Polish

#### US-025: CopyObject metadata directives
**Description:** As a client copying with metadata changes, I want `x-amz-metadata-directive: REPLACE` to honor the new content-type and user-meta headers, and `COPY` (default) to preserve source metadata.

**Acceptance Criteria:**
- [ ] Parse `x-amz-metadata-directive` (default COPY).
- [ ] On REPLACE: use request headers; on COPY: copy from source object.
- [ ] Same for `x-amz-tagging-directive` (already tag headers parsed).
- [ ] Honor `x-amz-copy-source-if-match` / `if-none-match` / `if-modified-since` / `if-unmodified-since` precondition headers.
- [ ] Tests: each combination; precondition mismatch → 412.

### Block 10 — ListBuckets Owner from Auth Context

#### US-026: ListBuckets returns the authenticated owner
**Description:** Today the response hardcodes `<Owner>strata</Owner>`. With multi-tenant credentials, every owner sees everyone's identity. Filter and label by `auth.FromContext(r.Context()).Owner`.

**Acceptance Criteria:**
- [ ] Response `<Owner>` reflects the authenticated principal.
- [ ] `ListBuckets` only returns buckets owned by that principal (cross-checked against `meta.Bucket.Owner`).
- [ ] Tests: two principals see disjoint bucket lists.

## 4. Functional Requirements

- FR-1: All routes documented in `ROADMAP.md` § "S3 API surface" and § "Auth" (P1+P2 levels) must be present and return spec-conformant XML/JSON.
- FR-2: Both `internal/meta/memory` and `internal/meta/cassandra` implement the full extended `meta.Store` interface; the `storetest.Run` contract suite covers each new method against both.
- FR-3: Every new endpoint has at least three unit tests: happy path, malformed input, not-configured / not-found.
- FR-4: SigV4 middleware enforces per-chunk signatures on streaming uploads (US-021). Without the chain, the gateway must reject all `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` requests.
- FR-5: Anonymous requests are gated by `STRATA_AUTH_MODE=optional`; in `required` mode the existing 403 behaviour is preserved.
- FR-6: `meta.Bucket` and `meta.Object` gain new fields (`Region`, `Checksums`, `SSE`, `Grants`, `RestoreStatus`); Cassandra schema migration is additive (`ALTER TABLE ... ADD column`) only.
- FR-7: `scripts/s3-tests/run.sh` runs after each block ships; the new pass rate is recorded in `scripts/s3-tests/README.md` baseline section.
- FR-8: After all blocks land, executable-subset pass rate is at minimum 80% (16/19 → 80% threshold = 16 passing on the 19-test default subset, plus expansion as new endpoints unblock more tests).
- FR-9: All existing unit tests, integration tests, and smoke scripts (`smoke.sh`, `smoke-signed.sh`) keep passing.
- FR-10: No skipped hooks (`--no-verify`), no force pushes to main, no destructive git operations during implementation.

## 5. Non-Goals (Out of Scope)

- **Real server-side encryption.** SSE-S3 / SSE-KMS / SSE-C are header-passthrough only — Strata does not actually encrypt data at rest in this PRD. Real encryption is a separate ROADMAP item.
- **Real cross-region replication.** PutBucketReplication is storage-only — no replicator process is built.
- **Real notification delivery.** PutBucketNotificationConfiguration is storage-only — no SNS/SQS/Kafka publisher.
- **S3 Object Lambda, Inventory, Analytics, Intelligent-Tiering.** Listed in ROADMAP as P3 — explicitly out of scope here.
- **S3 Select / Select Object Content.** Out of scope.
- **SigV2.** s3-tests skip these for us by design; we keep returning 403 and treat the 8 SigV2 FAILs as expected.
- **TiKV / FoundationDB / Postgres metadata backends.** Memory + Cassandra only.
- **UI changes.** Strata has no UI.
- **Performance work.** Parallel chunk upload, RADOS connection pool tuning, etc. — separate ROADMAP item.
- **Audit log.** Out of scope; covered by a separate P3 item.

## 6. Design Considerations

- **Reuse the existing blob-config pattern.** Block 1 introduced `setBucketBlob` / `getBucketBlob` helpers in both backends; reuse for notification, website, replication, logging, tagging, object-lock-config, encryption-config.
- **Reuse the existing `mapMetaErr` switch.** Add new sentinel errors next to the four added in Block 1 (`ErrNoSuchCORS`, `ErrNoSuchBucketPolicy`, etc.).
- **Reuse `corsPreflight`'s wildcard matcher** for IAM resource matching (`arn:aws:s3:::bucket/*` patterns).
- **No new dependencies** unless absolutely necessary. Stdlib `crypto/*`, `hash/crc32`, `encoding/xml`, `encoding/json` cover everything except CRC64NVME (vendor a small implementation if needed).

## 7. Technical Considerations

- **Order matters.** Block 2 (real ACL/policy enforcement) gates Block 3's `[iam alt root]` and Block 8's anonymous mode. Land in order: 2 → 3 → 4 → 5 → 6 → 7 → 8 → 9 → 10.
- **Cassandra schema additions are append-only.** Use the `alterStatements` pattern from `internal/meta/cassandra/schema.go` so existing keyspaces upgrade in place.
- **`meta.Store` interface evolves.** Pre-1.0 freedom: add methods directly to the interface (no optional-interface gymnastics yet). Both backends must implement before merge.
- **`auth.Identity` gains `IsAnonymous bool`.** Used by middleware to short-circuit SigV4 and by policy evaluator as principal `*` matcher.
- **IAM endpoints share the gateway port.** Routed on `?Action=` query param at the root path; no separate listener.
- **CRC64NVME polynomial.** Use `0xad93d23594c93659` reflected; can implement as a small `hash.Hash64` with a precomputed table. Unit tests must include the AWS test vector.
- **Test runner is golden source.** After each block, run `STRATA_AUTH_MODE=required scripts/s3-tests/run.sh` and capture pass rate; commit the new number into `scripts/s3-tests/README.md`.

## 8. Success Metrics

- **Primary:** Executable-subset pass rate on s3-tests `>=80%` (i.e. ≥16 of 19 passing, plus growth as new endpoints unblock previously-filtered tests).
- **Secondary:** Full-suite pass count grows from 3 → at least 200 (rough estimate: ACL + policy + IAM unblock ~120 collection errors; checksums and SSE unblock another ~80 PUTs; smaller endpoints unblock ~40 more).
- **Operational:** All new code has ≥80% statement coverage on the new files; CI green; smoke scripts pass.
- **Per-block delta documented:** Each block PR records a "before / after pass rate" line in `ROADMAP.md` and `scripts/s3-tests/README.md`.

## 9. Resolved Decisions

The following design decisions were resolved before implementation began:

- **Anonymous mode:** Both global flag AND per-bucket gating. `STRATA_AUTH_MODE` env var takes values `required` (no anonymous, default), `optional` (anonymous allowed, gated by bucket policy/ACL), `disabled` (dev only). In `optional` mode, requests without `Authorization` header carry `auth.AnonymousIdentity` and pass through to policy/ACL evaluation.
- **SSE-C storage:** MD5 only, no per-object "encrypted" flag. PutObject with SSE-C headers stores the customer-key MD5; GetObject must present matching headers. Mixed-mode reads on the same key are undefined (overwrites replace the object whole, so the question is moot).
- **IAM endpoint authorization:** Hardcode `[iam root]` as superuser for Block 3. Full IAM policy attachment is deferred to a future P3 item. The hardcoded model can extend later without breaking the wire API.
- **Per-chunk signature validation:** Mandatory. No `STRATA_AUTH_RELAXED_STREAMING` flag. Treating this as a compatibility knob would create a security footgun for the lifetime of the flag; all living AWS SDKs send the correct chain.
- **ListBuckets admin override:** Not in this PRD. US-026 implements owner filtering only. An admin override belongs in the future `strata-admin` CLI / admin API, not in the public S3 surface.
- **Replication status header:** Set `x-amz-replication-status: PENDING` only when at least one configured rule's `Filter` matches the new object's key/tags. Buckets with replication configured but no matching rule must not emit the header — this matches AWS behaviour and the s3-tests assertions.
