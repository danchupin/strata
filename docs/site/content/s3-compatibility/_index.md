---
title: 'S3 Compatibility'
weight: 50
bookFlatSection: true
description: 'Supported / unsupported S3 surface — read this first if evaluating Strata as an RGW replacement.'
---

# S3 Compatibility

Strata is intended as a **drop-in Ceph RGW replacement**. The honest measure of
that claim is the [Ceph `s3-tests`][s3-tests] suite — the de-facto upstream
compatibility benchmark.

[s3-tests]: https://github.com/ceph/s3-tests

## Headline

```
tests=177  passed=162  failed=15  errors=0  skipped=0
pass rate: 91.5%
```

(Run on the default executable subset:
`test_bucket_create or test_bucket_list or test_object_write or
test_object_read or test_object_delete or test_multipart or
test_versioning_obj or test_bucket_list_versions`. See
`scripts/s3-tests/README.md` for the per-test gap breakdown and historical
runs.)

The 15 remaining failures split into:

- **11 deliberate gaps** — SigV2 (deprecated 2018, never implemented),
  `test_bucket_list_prefix_unreadable` (URL-decode happens before our
  handler sees the byte), anonymous-list against `STRATA_AUTH_MODE=required`
  (intentional posture).
- **4 real bugs** tracked as separate P2 ROADMAP entries — multipart copy
  GET-side checksum echo divergence (3 tests) + multipart Complete duplicate
  PartNumber (1 test).

Re-run the suite locally with `scripts/s3-tests/run.sh`. Output lands in
`scripts/s3-tests/report/`.

---

## Bucket-level operations

Status legend: ✅ supported · ⚠️ partial · ❌ not supported.

| Operation                       | Status | Notes                                                                                                        |
|---------------------------------|:------:|--------------------------------------------------------------------------------------------------------------|
| `CreateBucket` (PUT /)          |   ✅   | Owner-equality idempotent. `x-amz-acl`, `x-amz-bucket-object-lock-enabled`, `LocationConstraint` honoured.   |
| `DeleteBucket` (DELETE /)       |   ✅   | Returns `BucketNotEmpty` when objects remain.                                                                |
| `HeadBucket`                    |   ✅   | Echoes `x-amz-bucket-region`.                                                                                |
| `ListBuckets` (GET /)           |   ✅   | Owner-scoped.                                                                                                |
| `ListObjects` (V1)              |   ✅   | Prefix / delimiter / marker / max-keys / encoding-type=url. NextMarker on truncate.                          |
| `ListObjectsV2`                 |   ✅   | Opaque continuation token (base64-URL of internal cursor). `start-after`, `fetch-owner`.                     |
| `ListObjectVersions`            |   ✅   | Per-version Owner.DisplayName populated.                                                                     |
| `ListMultipartUploads` (?uploads) |  ✅   | Cluster-wide list also exposed via `/admin/v1/multipart`.                                                    |
| `?versioning`                   |   ✅   | Enabled / Suspended. MFA Delete via `x-amz-mfa` when configured.                                             |
| `?lifecycle`                    |   ✅   | Transition / Expiration / NoncurrentVersionTransition / NoncurrentVersionExpiration / AbortIncompleteMultipartUpload. Worker drives transitions / expirations. |
| `?cors`                         |   ✅   | Preflight `OPTIONS` honoured.                                                                                |
| `?policy`                       |   ✅   | JSON policy doc; gates listing + access via `requireAccess` shape.                                           |
| `?tagging` (bucket)             |   ✅   | GET / PUT / DELETE.                                                                                          |
| `?acl` (bucket)                 |   ✅   | Canned + grant-list ACLs. `x-amz-acl` header on PUT bucket.                                                  |
| `?ownershipControls`            |   ✅   | BucketOwnerEnforced / BucketOwnerPreferred / ObjectWriter.                                                   |
| `?publicAccessBlock`            |   ✅   | Stored + enforced on policy / ACL writes.                                                                    |
| `?encryption`                   |   ✅   | Default SSE-S3 (AES256) and SSE-KMS bucket-level config; resolved on PUT-object when no per-request header.  |
| `?object-lock` (config)         |   ✅   | Compliance / Governance modes. Default retention applied on PUT-object.                                      |
| `?logging`                      |   ✅   | Target-bucket buffering; `access-log` worker drains buffered rows into AWS-format log objects.               |
| `?inventory`                    |   ✅   | Configurations stored per (bucket, configID). `inventory` worker writes manifest.json + CSV.gz pairs.       |
| `?notification`                 |   ✅   | Webhook + SQS sinks. DLQ accessible via `GET /?notify-dlq`. Configure via `STRATA_NOTIFY_TARGETS`.            |
| `?replication`                  |   ✅   | Cross-region replication via the `replicator` worker (HTTP PUT to peer Strata).                              |
| `?website`                      |   ✅   | IndexDocument + ErrorDocument + RedirectAllRequestsTo. Routing rules.                                        |
| `?location`                     |   ✅   | Echoes the bucket region.                                                                                    |
| `?accelerate`                   |   ❌   | Not implemented. Transfer Acceleration is an AWS edge-network feature — out of scope.                        |
| `?requestPayment`               |   ❌   | Requester Pays not modelled. See ROADMAP — out of scope.                                                     |
| `?analytics` / `?metrics` / `?intelligent-tiering` | ❌ | Storage-class analytics + Intelligent-Tiering are AWS-side. P3 ROADMAP `Intelligent-Tiering`. |

## Object-level operations

| Operation                            | Status | Notes                                                                                                       |
|--------------------------------------|:------:|-------------------------------------------------------------------------------------------------------------|
| `PutObject`                          |   ✅   | Single-shot + streaming chunked (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`). Per-request checksums (CRC32 / CRC32C / CRC64NVME / SHA1 / SHA256). User-meta echo. `Cache-Control` / `Expires`. |
| `GetObject`                          |   ✅   | Range + conditional headers (RFC 7232). `?versionId=` + `?versionId=null` literal. `ChecksumMode=ENABLED`. |
| `HeadObject`                         |   ✅   | Same shape as `GetObject` minus body.                                                                       |
| `?partNumber=N` GET / HEAD           |   ✅   | Echoes whole-object multipart ETag. Out-of-range → `400 InvalidPart`.                                       |
| `DeleteObject`                       |   ✅   | Versioned + suspended-mode null-replacement.                                                                |
| `DeleteObjects` (POST ?delete)       |   ✅   | Bulk; honours `x-amz-bypass-governance-retention`.                                                          |
| `CopyObject`                         |   ✅   | Same-bucket + cross-bucket. Metadata directive (REPLACE / COPY).                                            |
| `InitiateMultipartUpload`            |   ✅   | User-meta passthrough. Composite checksum algorithm echo on Initiate.                                       |
| `UploadPart`                         |   ✅   | SSE-C, streaming chunk decoder.                                                                             |
| `UploadPartCopy`                     |   ⚠️   | Range parser + special-char URL handling closed; **3 s3-tests fail on GET-side checksum echo divergence** — destination response emits source-side composite checksum, fails boto3 `FlexibleChecksum` validation. P2 ROADMAP `Multipart copy GET-side checksum echo divergence`. |
| `CompleteMultipartUpload`            |   ⚠️   | Validate-then-flip via LWT (cassandra+memory) / pessimistic txn (TiKV). **Rejects duplicate `PartNumber` in Parts list** — strict-ascending guard surfaces `InvalidPartOrder` where AWS dedupes. P2 ROADMAP `Multipart Complete rejects duplicate PartNumber in Parts list`. |
| `AbortMultipartUpload`               |   ✅   |                                                                                                             |
| `ListParts` (GET ?uploadId=)         |   ✅   |                                                                                                             |
| `?tagging` (object)                  |   ✅   | Per-version when versioning is enabled.                                                                     |
| `?retention` (object)                |   ✅   | COMPLIANCE + GOVERNANCE; `x-amz-bypass-governance-retention` honoured for GOVERNANCE.                       |
| `?legal-hold` (object)               |   ✅   |                                                                                                             |
| `?acl` (object)                      |   ✅   | Canned + grant-list.                                                                                        |
| `?attributes` GET                    |   ✅   |                                                                                                             |
| `?restore` POST                      |   ✅   | Lifecycle-aware restore from cold storage class.                                                            |
| Storage classes                      |   ⚠️   | `STANDARD`, `STANDARD_IA`, `GLACIER_IR` round-trip + lifecycle transitions. `GLACIER` / `DEEP_ARCHIVE` accepted as labels but no async-tier semantics. `INTELLIGENT_TIERING` not implemented. |
| Server-side encryption (SSE-S3)      |   ✅   | AES256 with master-key wrap. Master key rotation via `strata-admin rewrap`.                                 |
| Server-side encryption (SSE-KMS)     |   ✅   | `aws:kms` with KMS provider. `x-amz-server-side-encryption-aws-kms-key-id` honoured.                        |
| Server-side encryption (SSE-C)       |   ✅   | Customer-provided key + MD5 verification on every request.                                                  |
| `?select` (Select Object Content)    |   ❌   | P3 ROADMAP `Select / Select Object Content`.                                                                |
| Object Lambda                        |   ❌   | Out of scope (storage layer should not host user code).                                                     |

## IAM / STS surface

`?Action=` requests against the gateway base URL drive the IAM control plane.
The handler is in `internal/s3api/iam.go`.

| Action               | Status | Notes                                                  |
|----------------------|:------:|--------------------------------------------------------|
| `CreateUser`         |   ✅   |                                                        |
| `GetUser`            |   ✅   |                                                        |
| `ListUsers`          |   ✅   |                                                        |
| `DeleteUser`         |   ✅   |                                                        |
| `CreateAccessKey`    |   ✅   |                                                        |
| `ListAccessKeys`     |   ✅   |                                                        |
| `DeleteAccessKey`    |   ✅   | Cached credential invalidation hooked.                  |
| `RotateAccessKey`    |   ✅   |                                                        |
| `AssumeRole` (STS)   |   ✅   | Issues `STSSession{AccessKey, Secret, SessionToken}`. SigV4 verifier honours `SessionToken`. |
| `CreateAccessPoint`  |   ✅   |                                                        |
| `GetAccessPoint`     |   ✅   |                                                        |
| `DeleteAccessPoint`  |   ✅   |                                                        |
| `ListAccessPoints`   |   ✅   |                                                        |

Managed-policy attach / detach is exposed via the **admin API**
(`/admin/v1/iam/users/{name}/policies`), not via SigV4 `?Action=` — managed
policies and policy gating are documented in
{{< ref "/best-practices/web-ui" >}}.

## Auth surfaces

| Surface                                     | Status | Notes                                                                |
|---------------------------------------------|:------:|----------------------------------------------------------------------|
| SigV4 path-style                            |   ✅   | Default. `STRATA_AUTH_MODE` ∈ `required` / `optional` / `""` (off).  |
| SigV4 virtual-hosted style                  |   ✅   | `STRATA_VHOST_PATTERN` (default `*.s3.local`). Auth runs first; vhost rewrite happens AFTER signature verification. |
| SigV4 presigned URLs                        |   ✅   |                                                                      |
| SigV4 streaming chunk decoder               |   ✅   | `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` with chain-HMAC `prevSig` validation; mismatch returns `SignatureDoesNotMatch`. |
| Anonymous (auth=optional or off)            |   ✅   | Bucket policy / ACL gates anonymous list + read.                     |
| STS `AssumeRole`                            |   ✅   | Minimal — single-tier session credentials with default duration.     |
| SigV2                                       |   ❌   | Deprecated 2018; never implemented. 8 `test_headers.py` failures driven by SigV2 / bad-auth shape are accepted gaps. |
| OAuth / SAML SSO / IdP federation           |   ❌   | Self-contained IAM only — no external IdP federation.                |

## Explicitly NOT supported

These surfaces are NOT implemented today. Each row links to the ROADMAP entry
tracking it (or notes "out of scope" if it is intentionally deferred).

- **Cross-account ACLs.** Strata IAM users live in a flat account namespace —
  there is no cross-account principal model.
- **Requester Pays buckets.** Bucket-payment shifting is an AWS-billing
  surface and out of scope.
- **Transfer Acceleration.** AWS edge-network feature; out of scope.
- **Intelligent-Tiering.** P3 ROADMAP `Intelligent-Tiering` — needs hot/cold
  access counters per object.
- **S3 Select / Select Object Content.** P3 ROADMAP
  `Select / Select Object Content`.
- **Object Lambda.** Out of scope (`(out of scope) — Object Lambda` in
  ROADMAP).
- **SigV2.** Deprecated upstream; not on the roadmap.
- **Object Lock COMPLIANCE-violation distinct audit row.** P3 ROADMAP
  `Object Lock COMPLIANCE audit log` — `audit_log` records the denied DELETE
  but does not flag it with a typed retention-violation reason.
- **`aws-chunked-trailer` streaming variant.** Newer aws-cli releases use a
  trailer-based chunked encoding that the bufio-based decoder does not parse.
  Plain `x-amz-content-sha256: <hex>` and `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`
  both work; tracked under `Known latent bugs` in ROADMAP.
- **Multipart copy GET-side checksum echo divergence.** P2 ROADMAP
  `Multipart copy GET-side checksum echo divergence` — 3 of 15 remaining
  s3-tests failures.
- **Multipart Complete duplicate PartNumber.** P2 ROADMAP
  `Multipart Complete rejects duplicate PartNumber in Parts list` — 1 of 15
  remaining s3-tests failures.

## Posture caveats

A few defaults that operators sometimes assume work AWS-style and don't:

- `STRATA_AUTH_MODE=required` (recommended for production) **rejects anonymous
  list before the bucket-policy / ACL gate** — the s3-tests
  `test_bucket_list_objects_anonymous` family fail under this posture by
  design. Use `optional` if you need anonymous reads to flow through to the
  policy gate.
- The SigV4 signing surface includes the original `Host` header. Behind a
  load balancer or ingress controller, you must preserve the upstream `Host`
  (`upstream-vhost: $host` on ingress-nginx; `proxy_set_header Host $host` on
  raw nginx) — see {{< ref "/deploy/kubernetes" >}} and
  {{< ref "/architecture/auth" >}}.
- The headline 91.5% pass rate is on the **executable subset** filter
  (multipart + versioning + listing + object IO). The full s3-tests run
  surfaces additional fixtures around website hosting, CORS, and replication
  that exercise paths the default subset skips. Run
  `S3_TESTS_FILTER=all scripts/s3-tests/run.sh` for the full picture.

## Where to look next

- {{< ref "/get-started" >}} — bring up a gateway and run a few PUT/GET
  round-trips.
- {{< ref "/architecture/auth" >}} — SigV4 internals, presigned URLs, STS.
- {{< ref "/architecture/router" >}} — query-string dispatch shape behind
  the bucket / object tables above.
- {{< ref "/best-practices/monitoring" >}} — what to alert on when surface
  starts misbehaving.
- [`scripts/s3-tests/README.md`](https://github.com/danchupin/strata/blob/main/scripts/s3-tests/README.md)
  — full per-test breakdown + historical baselines.
- [`ROADMAP.md`](https://github.com/danchupin/strata/blob/main/ROADMAP.md) —
  P-item entries for every "not supported" row above.
