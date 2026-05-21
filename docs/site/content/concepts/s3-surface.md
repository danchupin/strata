---
title: 'S3 surface'
weight: 10
description: 'Which S3 operations Strata speaks and how they map to the gateway.'
---

# S3 surface

Strata speaks the AWS S3 REST API over HTTP. The goal is compatibility with
the Amazon S3 client ecosystem — the same `aws s3` / `aws s3api` / `mc`
commands and the same SDK calls work against Strata with only the endpoint
URL changed. Compatibility is measured against Ceph's upstream `s3-tests`
suite; the running pass rate lives on the
[S3 Compatibility]({{< relref "/s3-compatibility/" >}}) page.

The rest of this page introduces each major operation family. None of the
details below depend on which metadata or data backend you pick — the S3
surface is identical across backends.

## Buckets

`CreateBucket`, `HeadBucket`, `ListBuckets`, `DeleteBucket`, plus the
configuration subresources `?cors`, `?policy`, `?lifecycle`, `?versioning`,
`?tagging`, `?acl`, `?logging`, `?notification`, `?replication`,
`?encryption`, `?object-lock`, `?public-access-block`, `?ownership-controls`,
`?accelerate`, and `?inventory`. Bucket names follow S3 rules: lowercase,
DNS-safe, 3–63 characters.

Buckets are tenant-isolated by SigV4 identity. The owner of a bucket sees it
in `ListBuckets`; other principals need an explicit grant via bucket policy
or ACL.

## Objects

`PutObject`, `GetObject`, `HeadObject`, `DeleteObject`, `DeleteObjects`,
`CopyObject`, plus `?tagging`, `?acl`, `?legal-hold`, `?retention`, and
`?torrent` subresources. Range reads (`Range: bytes=…`) and conditional
requests (`If-Match`, `If-None-Match`, `If-Modified-Since`,
`If-Unmodified-Since`) are honored on both the read and write paths.

Object bytes are split into 4 MiB chunks behind the scenes. The split is an
implementation detail — clients see a single object with a single ETag and a
single content stream.

## Multipart

`CreateMultipartUpload`, `UploadPart`, `UploadPartCopy`, `ListParts`,
`ListMultipartUploads`, `CompleteMultipartUpload`, `AbortMultipartUpload`.
Multipart uploads are the standard S3 path for objects larger than a few
hundred megabytes; clients call them transparently via the `aws s3 cp` /
SDK transfer manager.

## Versioning

`PutBucketVersioning` flips the bucket between unversioned, versioning, and
versioning-suspended states. Once versioning is enabled, `PutObject` adds a
new version each time the same key is written and `DeleteObject` writes a
delete marker rather than tombstoning the row. `?versionId=<id>` and
`?versionId=null` resolve a specific version on read. `ListObjectVersions`
returns the version history.

## Access control

SigV4 signing is required for authenticated requests; presigned URLs are
supported for time-bounded delegation. The chunked streaming variant of
SigV4 (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`) is implemented with chain HMAC
validation per chunk.

Authorization is layered: bucket policy, bucket ACL, and IAM policies all
contribute, and a request is allowed only when at least one source grants
the action and no source denies it. The gateway records every state-changing
request (PUT / POST / DELETE) in an audit log table.

## Lifecycle and encryption

`PutBucketLifecycleConfiguration` schedules transitions and expirations; the
lifecycle worker applies the rules in the background. SSE-S3, SSE-C, and
SSE-KMS are all supported; the master key for SSE-S3 / SSE-KMS is rotated
in place via `strata admin rewrap`.

## Replication

`PutBucketReplication` configures cross-bucket asynchronous replication.
The replicator worker streams writes to peer Strata endpoints over HTTP
PUT, applying the configured filter and storage class.

## Where to read next

- [Multi-cluster routing]({{< relref "/concepts/multi-cluster" >}}) — how a bucket is bound to one or more data clusters.
- [Workers]({{< relref "/concepts/workers" >}}) — the background loops behind lifecycle, replication, and audit export.
- [S3 Compatibility]({{< relref "/s3-compatibility/" >}}) — the supported / unsupported matrix.
