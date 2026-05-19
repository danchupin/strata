---
title: 'S3 API operations'
weight: 30
---

<!--
Maintainer note: this table is hand-maintained. When adding a new handler
in internal/s3api/, append a row here in the same PR. A lint test
(internal/s3api/docs_reference_test.go) AST-parses the dispatch functions
ServeHTTP / handleBucket / handleObject / handleBucketInventory in
internal/s3api/, collects every *Server method called directly from those
bodies, and asserts each one either:

  1. appears in this markdown — matched by backtick'd function name OR by
     a `Handler file:line` cell of the form `internal/s3api/<file>.go:<line>`
     that points at the func declaration, OR
  2. carries a `// docs:skip` line comment immediately above the func
     declaration (tolerated as intentionally-internal — non-S3 helpers
     like auth gates, quota checks, website routing).

The same test catches orphan rows: any `internal/s3api/<file>.go:<line>`
cell that does NOT resolve to a *Server method body fails the build
(handler renamed or removed without updating the row).

Row sourcing: walk the dispatch arms in `internal/s3api/server.go` +
`inventory.go`. Each route arm becomes one row. `Handler file:line` points
at the route arm in `server.go` OR the leaf handler in
`internal/s3api/<feature>.go`.
-->

# S3 API operations

_This page is the operator + SDK-author index. The router is in
[`internal/s3api/server.go`](https://github.com/danchupin/strata/blob/main/internal/s3api/server.go) —
`ServeHTTP` → `handleBucket` (bucket-scope) → `handleObject` (object-scope).
Leaf handlers live in `internal/s3api/<feature>.go`. AWS divergence column
is empty when the operation is full-compat; one-liner otherwise._

The "Shipped in" column references the ROADMAP / PRD that landed the
operation when known. Pre-roadmap MVP surface (most rows) shows `—`.

## Bucket lifecycle

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `CreateBucket` | `internal/s3api/server.go:422` | — | `x-amz-bucket-object-lock-enabled: true` flips the bucket-level Object Lock setting on creation. `x-amz-acl` canned ACL header honoured. Idempotent for same-owner re-`PUT` (returns 200 + `Location`). |
| `DeleteBucket` | `internal/s3api/server.go:470` | — | 409 `BucketNotEmpty` when objects (incl. delete markers) remain. Operator force-empty available via `POST /admin/v1/buckets/{name}/force-empty`. |
| `HeadBucket` | `internal/s3api/server.go:476` | — | `x-amz-bucket-region` echoed from the bucket row (default `default`). |
| `ListBuckets` | `internal/s3api/server.go:198` | — | Anonymous principal returns empty `<Buckets>` element rather than 403. |
| `GetBucketLocation` | `internal/s3api/location.go:42` | — | Returns `<LocationConstraint>` of the bucket's stored region; empty for `default` region (matches AWS for `us-east-1`). |

## Object lifecycle

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `PutObject` | `internal/s3api/server.go:887` (→ `putObject` server.go:918) | — | `If-Match` / `If-None-Match` evaluated pre-write. `x-amz-storage-class` falls back to bucket `DefaultClass`. Quota check before body read; SSE-S3/KMS/SSE-C wrap on `body` reader. |
| `GetObject` | `internal/s3api/server.go:898` (→ `getObject` server.go:1144) | — | `partNumber=N` on non-multipart object returns whole body when `N=1`, `InvalidPart` otherwise (s3-tests parity). Range GET suppresses `x-amz-checksum-*` (boto3 1.36+ FlexibleChecksum mismatch). |
| `HeadObject` | `internal/s3api/server.go:903` (same `getObject` with `body=false`) | — | Same code path as GetObject; `partNumber=N` returns part metadata + composite multipart ETag. |
| `DeleteObject` | `internal/s3api/server.go:912` (→ `deleteObject` server.go:1452) | — | Suspended versioning: unversioned DELETE replaces null version. MFA-Delete enforced when `?versionId=` set + bucket has MfaDelete enabled. Object Lock `GOVERNANCE` bypassable with `x-amz-bypass-governance-retention: true`. |
| `DeleteObjects` (POST `?delete`) | `internal/s3api/delete_objects.go:44` | — | Quiet mode honoured. Per-key Object Lock checks; failures returned in `<Errors>` element, partial-success preserved. |
| `CopyObject` | `internal/s3api/copy_object.go:53` | — | Triggered by `x-amz-copy-source` on `PUT`. `x-amz-metadata-directive=REPLACE` swaps user metadata; `COPY` (default) preserves. SSE-C source key required when source is SSE-C-encrypted. |
| `RestoreObject` (POST `?restore`) | `internal/s3api/restore.go:21` | — | Transition target restored to STANDARD per request body `<Days>`; sync restore (no async polling — restore completes in-handler). |

## Object metadata

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `GetObjectAttributes` (GET `?attributes`) | `internal/s3api/object_attributes.go:65` | — | `x-amz-object-attributes` header drives which subfields render (ETag, Checksum, ObjectParts, StorageClass, ObjectSize). |
| `GetObjectAcl` | `internal/s3api/acl.go:238` | — | Returns owner + per-grantee `<Grant>` list; canned ACLs translated to explicit grants. |
| `PutObjectAcl` | `internal/s3api/acl.go:255` | — | `x-amz-acl` canned header OR XML body; mixed input returns 400 `MalformedACLError`. |
| `GetObjectTagging` | `internal/s3api/tagging.go:37` | — | Empty `<TagSet/>` when no tags (200, not 404). |
| `PutObjectTagging` | `internal/s3api/tagging.go:15` | — | Replaces full TagSet — no merge semantics. |
| `DeleteObjectTagging` | `internal/s3api/tagging.go:50` | — | Idempotent — 204 even when no tags existed. |

## Multipart upload

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `CreateMultipartUpload` (POST `?uploads`) | `internal/s3api/server.go:784` (→ `initiateMultipart` multipart.go:40) | — | SSE headers persisted on the multipart session; per-part SSE-C key required on every UploadPart (AWS parity). `BackendUploadID` opaque handle binds the session to the originating cluster (drain-survival, US-004 drain-followup). |
| `UploadPart` (PUT `?uploadId=&partNumber=`) | `internal/s3api/server.go:851` (→ `uploadPart` multipart.go:177) | — | `Content-MD5` accepted but not validated against streaming body (P2 ROADMAP gap). Part-checksum headers (`x-amz-checksum-*`) persisted per part for Complete verification. |
| `UploadPartCopy` (PUT `?uploadId=&partNumber=` + `x-amz-copy-source`) | `internal/s3api/multipart.go:311` (dispatched from `uploadPart` at multipart.go:189) | — | `x-amz-copy-source-range: bytes=lo-hi` honoured with strict `lo<=hi<size` parse. |
| `CompleteMultipartUpload` (POST `?uploadId=`) | `internal/s3api/server.go:858` (→ `completeMultipart` multipart.go:448) | — | LWT `IF status='uploading' → completing` blocks concurrent retries with `NoSuchUpload`. Composite ETag = `md5(concat(part-md5s))-<count>`. Per-part `x-amz-checksum-*` aggregated into object-level `ChecksumType=COMPOSITE`. |
| `AbortMultipartUpload` (DELETE `?uploadId=`) | `internal/s3api/server.go:864` (→ `abortMultipart` multipart.go:733) | — | Uploaded parts queue into the GC worker via `enqueueChunks`. Idempotent — 204 even when upload already aborted. |
| `ListParts` (GET `?uploadId=`) | `internal/s3api/server.go:870` (→ `listParts` multipart.go:748) | — | Pagination via `part-number-marker` + `max-parts`. |
| `ListMultipartUploads` (GET `?uploads`) | `internal/s3api/server.go:387` (→ `listMultipartUploads` multipart.go:779) | — | Pagination via `key-marker` + `upload-id-marker`. |

## Listing

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `ListObjects` (v1, GET no `list-type`) | `internal/s3api/server.go:484` (→ `listObjects` server.go:497) | — | Sharded fan-out across `STRATA_BUCKET_SHARDS` partitions (Cassandra) OR ordered range scan (TiKV via `RangeScanStore`). `marker` is the literal last-key from prior page. |
| `ListObjectsV2` (GET `list-type=2`) | `internal/s3api/server.go:484` (same `listObjects`, `v2=true`) | — | `ContinuationToken` is opaque base64-URL JSON (`listV2Token`) — clients carrying older literal-marker tokens still resume via decode fallback (US-006). |
| `ListObjectVersions` (GET `?versions`) | `internal/s3api/versioning.go:95` | — | Heap-merge of `(key ASC, version_id DESC)` across shards. Delete markers interleaved per AWS shape. |

## Versioning

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `GetBucketVersioning` | `internal/s3api/versioning.go:13` | — | `Suspended` rows persist `IsNull=true` on subsequent PUTs (null-version sentinel). |
| `PutBucketVersioning` | `internal/s3api/versioning.go:26` | — | LWT `IF EXISTS` guarantees Paxos coherence on the next quorum read. MFA-Delete toggle requires `x-amz-mfa` header. |

## ACL / Policy

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `GetBucketAcl` | `internal/s3api/acl.go:194` | — | |
| `PutBucketAcl` | `internal/s3api/acl.go:208` | — | Canned ACL `x-amz-acl` OR XML body; mixed input rejected. |
| `GetBucketPolicy` | `internal/s3api/policy.go:35` | — | 404 `NoSuchBucketPolicy` when unset (AWS parity). |
| `PutBucketPolicy` | `internal/s3api/policy.go:12` | — | JSON body validated for required `Version` + `Statement` fields. |
| `DeleteBucketPolicy` | `internal/s3api/policy.go:55` | — | Idempotent. |
| `GetPublicAccessBlock` | `internal/s3api/public_access_block.go:43` | — | |
| `PutPublicAccessBlock` | `internal/s3api/public_access_block.go:20` | — | |
| `DeletePublicAccessBlock` | `internal/s3api/public_access_block.go:63` | — | |
| `GetBucketOwnershipControls` | `internal/s3api/ownership.go:53` | — | |
| `PutBucketOwnershipControls` | `internal/s3api/ownership.go:22` | — | |
| `DeleteBucketOwnershipControls` | `internal/s3api/ownership.go:73` | — | |

## CORS

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `GetBucketCors` | `internal/s3api/cors.go:103` | — | |
| `PutBucketCors` | `internal/s3api/cors.go:47` | — | |
| `DeleteBucketCors` | `internal/s3api/cors.go:209` | — | |
| `CORS Preflight` (OPTIONS) | `internal/s3api/cors.go:233` | — | Dispatched on `OPTIONS` to bucket OR object scope; non-S3 RPC (browser-only). |

## Encryption (SSE-S3 / SSE-KMS / SSE-C)

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `GetBucketEncryption` | `internal/s3api/encryption.go:147` | — | |
| `PutBucketEncryption` | `internal/s3api/encryption.go:109` | — | Default `<ApplyServerSideEncryptionByDefault>` resolved at every PUT when client omits SSE headers. |
| `DeleteBucketEncryption` | `internal/s3api/encryption.go:167` | — | |

## Replication

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `GetBucketReplication` | `internal/s3api/replication.go:100` | — | |
| `PutBucketReplication` | `internal/s3api/replication.go:59` | — | Replicator worker (`STRATA_WORKERS=replicator`) drains `replication_queue` via HTTP PUT to peer Strata; one rule per bucket. |
| `DeleteBucketReplication` | `internal/s3api/replication.go:120` | — | |

## Tagging

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `GetBucketTagging` | `internal/s3api/tagging.go:105` | — | 404 `NoSuchTagSet` when unset. |
| `PutBucketTagging` | `internal/s3api/tagging.go:58` | — | Replace-all semantics. |
| `DeleteBucketTagging` | `internal/s3api/tagging.go:125` | — | |

## Lifecycle

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `GetBucketLifecycleConfiguration` | `internal/s3api/lifecycle.go:91` | — | |
| `PutBucketLifecycleConfiguration` | `internal/s3api/lifecycle.go:14` | — | Lifecycle worker (`STRATA_WORKERS=lifecycle`) executes rules. CAS on `SetObjectStorage` — concurrent client write wins, worker discards tier-2 chunks via GC queue. |
| `DeleteBucketLifecycleConfiguration` | `internal/s3api/lifecycle.go:111` | — | |

## Object Lock

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `GetObjectLockConfiguration` | `internal/s3api/objectlock.go:72` | — | |
| `PutObjectLockConfiguration` | `internal/s3api/objectlock.go:26` | — | Bucket-level Object Lock must be enabled at CreateBucket (`x-amz-bucket-object-lock-enabled: true`) or via this op. |
| `GetObjectRetention` | `internal/s3api/objectlock.go:156` | — | |
| `PutObjectRetention` | `internal/s3api/objectlock.go:125` | — | `GOVERNANCE` downgrades require `x-amz-bypass-governance-retention: true`. |
| `GetObjectLegalHold` | `internal/s3api/objectlock.go:188` | — | |
| `PutObjectLegalHold` | `internal/s3api/objectlock.go:169` | — | |

## Inventory

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `GetBucketInventoryConfiguration` (GET `?inventory&id=`) | `internal/s3api/inventory.go:144` | — | |
| `PutBucketInventoryConfiguration` (PUT `?inventory&id=`) | `internal/s3api/inventory.go:77` | — | URL `id` must match `<Id>` element; otherwise 400 `InvalidArgument`. Inventory worker (`STRATA_WORKERS=inventory`) writes manifest.json + CSV.gz pairs. |
| `DeleteBucketInventoryConfiguration` (DELETE `?inventory&id=`) | `internal/s3api/inventory.go:164` | — | |
| `ListBucketInventoryConfigurations` (GET `?inventory` no id) | `internal/s3api/inventory.go:185` | — | |

## Notification

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `GetBucketNotificationConfiguration` | `internal/s3api/notification.go:91` | — | |
| `PutBucketNotificationConfiguration` | `internal/s3api/notification.go:56` | — | Notify worker (`STRATA_WORKERS=notify`) drains `notify_queue` to webhook / SQS sinks per `STRATA_NOTIFY_TARGETS`. |

## Logging

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `GetBucketLogging` | `internal/s3api/logging.go:71` | — | |
| `PutBucketLogging` | `internal/s3api/logging.go:26` | — | Access-log worker (`STRATA_WORKERS=access-log`) drains `access_log_buffer` and writes AWS-format log objects into the configured target bucket. |

## Website

| Operation | Handler file:line | Shipped in | AWS gotchas |
|---|---|---|---|
| `GetBucketWebsite` | `internal/s3api/website.go:106` | — | |
| `PutBucketWebsite` | `internal/s3api/website.go:57` | — | Index + error documents persisted; GET on the bucket root serves `index.html` when configured. |
| `DeleteBucketWebsite` | `internal/s3api/website.go:126` | — | |

## Non-S3 RPC surface (routed alongside S3)

| Operation | Handler file:line | Notes |
|---|---|---|
| IAM `?Action=` queries (CreateUser, AssumeRole, …) | `internal/s3api/iam.go:62` | Routed on bucket-less requests when `extractIAMAction(r)` matches. Full action set: see the [Admin API surface]({{< ref "/reference/admin-api" >}}) page's IAM section. |
| Audit query (GET `?audit`) | `internal/s3api/audit_query.go:41` | Strata extension — operator read of the `audit_log` table. |
| Notification DLQ query (GET `?notify-dlq`) | `internal/s3api/notification_dlq.go:43` | Strata extension — operator read of notify-worker DLQ rows. |

## Out of scope this cycle

The following AWS S3 surface is intentionally not implemented or not documented here:

- **Analytics**, **Metrics**, **Intelligent-Tiering**, **Accelerate** — analytics
  surface not implemented (no row in `handleBucket`).
- **MultiRegionAccessPoint**, **MultipartUploadV2**, **Select** — not implemented.
- **PutObjectLockConfiguration on a bucket without `x-amz-bucket-object-lock-enabled`** —
  returns the AWS-spec `InvalidBucketState` shape.
