# PRD: Strata "Modern Complete" — P1 + P2 + P3

## Introduction

Strata is a Go-based, S3-compatible object gateway that hit 80.1% on Ceph's `s3-tests` executable subset (141/176, commit `d2b2c02`). Most S3 wire-format breadth is in place: ACL+policy+IAM, multipart, versioning, lifecycle, object lock, CORS, public-access-block, ownership controls, tagging, and storage-only handling for SSE/notifications/replication/website/logging.

Remaining work is no longer about S3 surface area — it is about **real implementations** of features Strata currently fakes (encryption-at-rest, notification publisher, replicator, log delivery, website redirects), **production hardening** orthogonal to S3 (structured logs, metrics, health probes, tracing, audit), **modern AWS-SDK ergonomics** (virtual-hosted URLs, bucket default encryption, per-part GET, composite checksums, versioning null literal), and **enterprise integrations** (SSE-KMS, Inventory, Access Points, multi-cluster RADOS).

This PRD scopes that work in three priority bands. Legacy (SigV2, POST upload, Transfer Acceleration) and AWS-specific niche (S3 Select, Object Lambda, Storage Lens, Multi-Region Access Points) are out of scope.

## Goals

- Ship real implementations for every feature currently in passthrough mode: SSE-S3 (real encryption-at-rest), Notifications (real publisher), Replication (real Strata-to-Strata replicator), Logging (real log delivery), Website (redirect + routing rules).
- Reach operational completeness: JSON structured logs with request-id correlation, full Prometheus metric coverage across all subsystems, k8s-style `/healthz` and `/readyz` probes, optional OpenTelemetry tracing, append-only audit log with 30-day default retention.
- Match modern AWS-SDK clients: virtual-hosted URL routing, bucket default encryption, full per-part GET (`?partNumber=N`) with composite checksum response shape, versioning `"null"` literal semantics for unversioned rows in versioned buckets.
- Cover enterprise integrations: pluggable SSE-KMS, Inventory exports, single-region Access Points, multi-cluster RADOS routing, online per-bucket shard resize.
- Within one band the work parallelises freely (each story is independent); strict ordering only between bands (P1 before P2 before P3).

## User Stories

### Band P1 — production-blockers (must ship before P2)

#### US-001: Pluggable SSE-S3 master-key provider
**Description:** As an operator, I want to configure where the SSE-S3 master key lives — env var, file, or Vault Transit — so production deployments can pick the right secret-management story.

**Acceptance Criteria:**
- [ ] New `internal/crypto/master` package with `Provider` interface (`Resolve(ctx) ([]byte, string, error)`).
- [ ] Built-in providers: `env` (`STRATA_SSE_MASTER_KEY=<hex32>`, default), `file` (`STRATA_SSE_MASTER_KEY_FILE=/path`, watches mtime, hot-reload), `vault` (`STRATA_SSE_MASTER_KEY_VAULT=<addr>:<path>`, periodic refresh).
- [ ] Provider returns a `(key32, keyID)` tuple — `keyID` is wrapped into the per-object DEK so old DEKs can still be unwrapped after rotation.
- [ ] Tests: each provider exercised; missing config → fatal at startup; invalid key length → fatal.
- [ ] `go build ./...` and `go test ./internal/crypto/...` pass.

#### US-002: Real SSE-S3 encryption-at-rest (AES-256-GCM)
**Description:** As a security/compliance team, when I PUT with `x-amz-server-side-encryption: AES256` I want chunk bytes encrypted on the RADOS pool, not stored in plaintext.

**Acceptance Criteria:**
- [ ] `data.Backend.PutChunks` accepts an `Encryption *EncryptionParams`; AES-256-GCM-encrypts each chunk before write when set; IV derived deterministically from `(oid, chunk_index)`.
- [ ] `data.Backend.GetChunks` decrypts on read; mismatched/corrupt chunks → 500 InternalError with structured-log error.
- [ ] Per-object DEK generated per upload, AEAD-wrapped under the master key, persisted in `meta.Object.SSEKey` (additive `objects.sse_key` blob column + `objects.sse_key_id` text column).
- [ ] HEAD/GET echo `x-amz-server-side-encryption: AES256`.
- [ ] Multipart upload supports SSE-S3 across parts (UploadPart inherits the upload's algorithm; Complete reuses the wrapped DEK).
- [ ] Tests: round-trip per object, multipart with SSE-S3, master-key rotation acceptance (US-003), corrupted chunk → 500, plaintext object after PUT without header continues to work.
- [ ] `go test ./...` and `make test-rados` pass.

#### US-003: SSE-S3 master-key rotation
**Description:** As an operator, I need to rotate the master key without re-encrypting every chunk. Old DEKs stay unwrappable via the historical key id.

**Acceptance Criteria:**
- [ ] `STRATA_SSE_MASTER_KEYS` (plural) supports comma-separated `keyID:hex32` list. The first entry is the current "wrap" key; the rest are historical "unwrap-only" keys.
- [ ] Each wrapped DEK carries the keyID it was wrapped with; the unwrap path looks the right master up.
- [ ] `cmd/strata-rewrap` CLI walks `objects` and rewraps DEKs to the current key. Idempotent and resumable from a `rewrap_progress` table.
- [ ] Admin endpoint `POST /?Action=RotateSSEMaster` (gated by `[iam root]`) updates the in-memory marker without restart, picking up new env at next reload.
- [ ] Tests: PUT under key A, rotate to B, GET still works; new PUT uses B; rewrap CLI converts A→B and verifies; mid-rewrap restart resumes.

#### US-004: PutBucketEncryption / GetBucketEncryption / DeleteBucketEncryption
**Description:** As a client, I want a bucket-default encryption policy so PUTs without `x-amz-server-side-encryption` inherit AES256 (and aws:kms once US-019 lands).

**Acceptance Criteria:**
- [ ] `PUT/GET/DELETE /<bucket>?encryption` round-trips the `ServerSideEncryptionConfiguration` XML via existing `setBucketBlob` / `getBucketBlob` helpers.
- [ ] PutObject and InitiateMultipartUpload omitting `x-amz-server-side-encryption` apply the bucket default transparently.
- [ ] DeleteBucketEncryption clears the default; subsequent PUTs without the header store unencrypted.
- [ ] Tests: round-trip XML, default applied on PUT, default cleared, malformed XML → 400.
- [ ] s3-tests `test_*encryption*` in default subset pass.

#### US-005: Notification publisher — Webhook + SQS sink
**Description:** As an event-driven application, I want PutObject/CompleteMultipart/DeleteObject events delivered to an HTTPS webhook or AWS SQS queue when bucket notification configuration matches the request.

**Acceptance Criteria:**
- [ ] `internal/notify` package: `Event` type matching AWS S3 event-message shape (`Records[*]` with `s3.bucket`, `s3.object`, `eventName`, `eventTime`).
- [ ] `Sink` interface; built-in `WebhookSink` (HTTPS POST with HMAC-SHA256 signature of body keyed by per-target shared secret) and `SQSSink` (AWS SDK `SendMessage`).
- [ ] `cmd/strata-notify` worker: leader-elected via `internal/leader`, polls `notify_queue` table (TimeUUID partitioned by hour), fans out to configured sinks with exponential backoff (max 6 retries → DLQ table).
- [ ] PutObject/CompleteMultipart/DeleteObject hooks in `cmd/strata-gateway` enqueue an event row when the bucket has notification config and a rule's Filter (Prefix/Suffix) matches.
- [ ] Tests: webhook sink (httptest), SQS sink (AWS SDK mock), prefix-filter match/miss, retry-then-DLQ, leader handoff.
- [ ] At-least-once delivery; ordering is best-effort (per-object, not global).

#### US-006: Replication runner — Strata-to-Strata
**Description:** As a DR-conscious operator, I want PutBucketReplication's rules to actually mirror writes to a peer Strata cluster's bucket. Today the configuration is stored but no replication happens.

**Acceptance Criteria:**
- [ ] `cmd/strata-replicator` worker: leader-elected, iterates `replication_queue` rows, copies the object to the destination via boto-compatible PUT against the peer's gateway, sets `x-amz-replication-status: COMPLETED` on the source object on success.
- [ ] Server-side hook on PutObject/CompleteMultipart enqueues a task only when at least one rule's Filter (Prefix and/or Tag) matches.
- [ ] Source bucket must have versioning Enabled (AWS rule); reject `PutBucketReplication` otherwise with `InvalidRequest`.
- [ ] Failed replication after 6 retries marks the source as `FAILED` and emits a metric.
- [ ] Destination is constrained to another Strata cluster: target endpoint specified per-rule via `Destination.Bucket=<bucket>` and `Destination.AccessControlTranslation.Owner=<peer-host>:<port>` (Strata-specific reuse of the AWS XML field; documented).
- [ ] Tests: rule match → COMPLETED; no match → no header; destination 5xx → FAILED after retries; with-tag-filter; rejection when source unversioned.
- [ ] s3-tests `test_replication*` in default subset pass for status header.

#### US-007: Real server access log delivery
**Description:** As an audit consumer, I want PutBucketLogging to deliver log objects to the configured target bucket in AWS server-access-log format.

**Acceptance Criteria:**
- [ ] `cmd/strata-access-log` worker: per-source-bucket buffer in `access_log_buffer` Cassandra table, flushes every 5 minutes or 5 MiB whichever first, writes one object per flush to the configured target bucket / prefix.
- [ ] Log format matches AWS: Bucket Owner, Bucket, Time, Remote IP, Requester, Request-ID, Operation, Key, Request-URI, HTTP status, Error Code, Bytes Sent, Object Size, Total Time, Turn-Around Time, Referrer, User-Agent, Version Id (space-separated, dash for missing fields).
- [ ] HTTP middleware in `cmd/strata-gateway` writes one buffer row per request when source bucket has logging enabled.
- [ ] Tests: round-trip via memory backend (logging on bucket A → write to A → fetch from B), format compliance against an AWS sample line.

#### US-008: Structured logging with request-id correlation
**Description:** As an operator, I want JSON logs everywhere with `request_id` propagated through Cassandra and RADOS calls so I can grep one request across the stack.

**Acceptance Criteria:**
- [ ] Replace `log.*` with `log/slog` JSON handler in every `cmd/` and every `internal/` package that logs.
- [ ] HTTP middleware reads `X-Request-Id` from the request or generates a UUID, threads it through `context.Context` via `slog.With("request_id", id)`.
- [ ] Cassandra driver hook logs slow queries (>100 ms) at WARN with the request id and table.
- [ ] RADOS read/write logs OID + duration at DEBUG.
- [ ] Levels controlled by `STRATA_LOG_LEVEL` (default INFO).
- [ ] Tests: a single request produces logs with the same `request_id`; missing header gets a generated UUID.
- [ ] `make smoke` shows JSON log lines per request.

#### US-009: Prometheus metrics — full coverage
**Description:** As an SRE, I want metrics across HTTP, Cassandra, RADOS, lifecycle, GC, replication, notifications.

**Acceptance Criteria:**
- [ ] `internal/metrics` registers:
  - `http_request_duration_seconds{method, path, status}` histogram
  - `cassandra_query_duration_seconds{table, op}` histogram (op ∈ INSERT/SELECT/UPDATE/DELETE/LWT)
  - `rados_op_duration_seconds{pool, op}` histogram (op ∈ put/get/del)
  - `gc_queue_depth{region}` gauge
  - `multipart_active{bucket}` gauge
  - `bucket_bytes{bucket, storage_class}` gauge sampled hourly
  - `lifecycle_tick_total{action, status}` counter (action ∈ transition/expire/abort_mp)
  - `replication_lag_seconds{rule_id}` histogram
  - `notify_delivery_total{sink, status}` counter
- [ ] All worker binaries (`strata-gateway`, `strata-lifecycle`, `strata-gc`, `strata-notify`, `strata-replicator`, `strata-access-log`) expose `/metrics`.
- [ ] Each metric registered exactly once; sample values populate after a smoke run.
- [ ] Grafana dashboard JSON ships under `deploy/grafana/`.
- [ ] Tests: smoke run + scrape `/metrics`, expected metric names present.

#### US-010: Health and readiness endpoints
**Description:** As k8s probes, I need `/healthz` (liveness) and `/readyz` (readiness — Cassandra + RADOS reachable).

**Acceptance Criteria:**
- [ ] `GET /healthz` returns 200 plain text "ok" without dependency checks.
- [ ] `GET /readyz` runs `SELECT 1` on Cassandra and `stat` on a known canary OID in RADOS within a 1s timeout; returns 200 only when both succeed.
- [ ] Both endpoints bypass SigV4 middleware regardless of `STRATA_AUTH_MODE`.
- [ ] Compose ships a `healthcheck:` stanza using `/readyz`.
- [ ] Tests: kill the gateway's Cassandra session → `/readyz` returns 503; same for RADOS; `/healthz` always 200 while the binary is up.

#### US-011: Virtual-hosted-style URL routing
**Description:** As a modern AWS SDK client (default since 2020), I want `https://my-bucket.s3.<region>.example.com/key` to resolve to bucket `my-bucket`.

**Acceptance Criteria:**
- [ ] Router strips `bucket` from `Host` when the host matches `STRATA_VHOST_PATTERN` (default `*.s3.local`, configurable regex).
- [ ] Falls back to path-style when the host doesn't match (no behaviour change for existing callers).
- [ ] SigV4 canonical request uses the **original** Host header for signing; bucket extraction is post-signature so signed virtual-hosted requests verify correctly.
- [ ] Pre-signed URLs work with both addressing styles.
- [ ] Tests: virtual-hosted PUT/GET/HEAD round-trip; path-style still works; signed virtual-hosted matches; mismatched host falls back to path-style.

---

### Band P2 — adoption-blockers (parallelisable within band; require P1)

#### US-012: Audit log
**Description:** As a security auditor, I need an append-only record of every state-changing request retained 30 days by default.

**Acceptance Criteria:**
- [ ] New Cassandra table `audit_log` partitioned by `(bucket, date)`, columns `(event_id timeuuid, ts, principal, action, resource, result, request_id, source_ip)`, TTL = `STRATA_AUDIT_RETENTION` (default 30d).
- [ ] HTTP middleware writes one row per state-changing request after the response is written; reuses request_id from US-008.
- [ ] `GET /?audit&start=...&end=...&principal=...&bucket=...` (gated by `[iam root]`) returns paginated JSON.
- [ ] Tests: every CreateBucket/PutObject/DeleteObject/IAM action emits exactly one row; query API filters on each dimension; expired rows disappear after TTL.

#### US-013: Per-part GET + composite checksum response shape
**Description:** As a multipart-aware client, I want `GET /<bucket>/<key>?partNumber=N` to return only that part and HEAD with `ChecksumMode=ENABLED` to surface the composite checksum.

**Acceptance Criteria:**
- [ ] `meta.Object.PartSizes []int64` (additive Cassandra column or packed into the manifest blob).
- [ ] `?partNumber=N` GET returns the byte range of part N; sets `x-amz-mp-parts-count`, `x-amz-checksum-<algo>` from the per-part stored checksum, `Content-Length` of the part.
- [ ] HEAD with `x-amz-checksum-mode: ENABLED` returns the composite `x-amz-checksum-<algo>` and `x-amz-checksum-type` (COMPOSITE for typical MP, FULL_OBJECT when client requested it).
- [ ] `GetObjectAttributes` populates `ObjectParts` with part sizes + per-part checksums.
- [ ] Tests: round-trip a 3-part multipart, fetch each part by number, verify checksum + size + ContentLength; HEAD with ChecksumMode returns composite.
- [ ] Closes 8 of the remaining s3-tests failures (use_cksum_helper × 5, get_part × 3).

#### US-014: Versioning `"null"` version-id literal
**Description:** As a versioning-aware client, when I write to an unversioned bucket, then enable versioning, the original row should be addressable as `?versionId=null`.

**Acceptance Criteria:**
- [ ] `Versioning=Disabled` PUTs store the row with version_id = `"null"` (sentinel TimeUUID `00000000-0000-0000-0000-000000000000` plus `is_null bool` column on `objects`).
- [ ] After `SetBucketVersioning(Enabled)`, that row remains addressable as `?versionId=null`.
- [ ] Suspended-mode PUT removes the prior `null` version atomically and writes a new one.
- [ ] DELETE in suspended mode follows the same rule.
- [ ] Tests: closes 5 versioning failures (`test_versioning_obj_plain_null_version_*`, `test_versioning_obj_suspend_versions`, `test_versioning_obj_suspended_copy`).
- [ ] Default-on (no feature flag).

#### US-015: Website redirect + routing rules
**Description:** As a static-site host, I want `RedirectAllRequestsTo` and `RoutingRules` on PutBucketWebsite to actually redirect / route, not just round-trip XML.

**Acceptance Criteria:**
- [ ] When `RedirectAllRequestsTo` is set, every GET on the bucket returns 301 to `<protocol>://<host>[/<path>]`.
- [ ] `RoutingRules`: each rule's `Condition` (KeyPrefixEquals / HttpErrorCodeReturnedEquals) matches against the request; `Redirect` (HostName / Protocol / HttpRedirectCode / ReplaceKeyPrefixWith / ReplaceKeyWith) drives the Location header and status.
- [ ] Index serving still works when no redirect rule matches.
- [ ] Tests: index serving baseline; redirect-all-requests; prefix routing rule; error-code routing rule (404 → /404.html).

#### US-016: OpenTelemetry tracing
**Description:** As a debugger of slow requests, I want OTel spans through Gateway → Meta (Cassandra query) → Data (RADOS op), propagated from `traceparent`.

**Acceptance Criteria:**
- [ ] `internal/otel` package; init reads `OTEL_EXPORTER_OTLP_ENDPOINT`; absence → no-op tracer.
- [ ] Spans: `s3.<Operation>`, `meta.cassandra.<table>.<op>`, `data.rados.<op>`.
- [ ] `traceparent` ingress / egress wired through middleware; falls back to creating a root span if absent.
- [ ] Sampling via `STRATA_OTEL_SAMPLE_RATIO` (default 0.01) with always-on for failing requests.
- [ ] Tests: deterministic in-memory exporter captures expected spans for a smoke PUT.
- [ ] Compose ships an optional OTLP collector + Jaeger.

#### US-017: `strata-admin` CLI
**Description:** As an operator, I want a CLI that wraps a small admin API: IAM key rotation, force lifecycle tick, force GC drain, inspect a bucket's shard distribution, rotate SSE master key, force replication retry.

**Acceptance Criteria:**
- [ ] New `cmd/strata-admin` binary with subcommands: `iam create-access-key`, `iam rotate-access-key`, `lifecycle tick`, `gc drain`, `bucket inspect`, `sse rotate`, `replicate retry`.
- [ ] Talks to the gateway over IAM admin endpoints + new `/admin/...` HTTP routes (gated by `[iam root]`).
- [ ] `--json` for machine output; default human output.
- [ ] Tests: each subcommand against an in-memory gateway, asserting request shape + response.

#### US-018: Concurrency / race harness
**Description:** As a maintainer, I want a race harness that hammers the gateway with parallel PUT/DELETE/Complete-multipart/versioning toggles on the same key, asserting Cassandra+RADOS state stays consistent.

**Acceptance Criteria:**
- [ ] New `internal/s3api/race_test.go`: 32 goroutines × 1000 ops each, mixed PUT/DELETE/Multipart-Complete, verifies invariants (`is_latest` count == 1 per key, no orphaned chunks beyond GC queue, version chain monotonic in `mtime`).
- [ ] Run with `-race` in CI.
- [ ] Memory-backend variant runs in `make test`; Cassandra-backed variant runs under `-tags integration`.

---

### Band P3 — nice-to-have (parallelisable; require P1)

#### US-019: SSE-KMS — Encrypt with a KMS-wrapped key
**Description:** As an enterprise tenant, I want `x-amz-server-side-encryption: aws:kms` to wrap the per-object DEK via a KMS-compatible API.

**Acceptance Criteria:**
- [ ] Pluggable `KMSProvider` interface with `WrapDEK` / `UnwrapDEK` / `GenerateDataKey`.
- [ ] Built-in providers: `vault` (HashiCorp Vault Transit), `aws-kms` (AWS SDK), `local-hsm` (test stub).
- [ ] PutObject with `aws:kms` algorithm + `x-amz-server-side-encryption-aws-kms-key-id` resolves the key id, calls GenerateDataKey, wraps DEK, persists wrapped DEK in `obj.SSEKey` (US-002 column).
- [ ] GET unwraps via the same provider; mismatched key id → 403.
- [ ] Tests: round-trip with vault provider; round-trip with aws-kms (mocked); fallback to SSE-S3 when key-id absent.

#### US-020: Inventory exports
**Description:** As a billing/audit pipeline, I want PutBucketInventoryConfiguration to schedule actual exports of the bucket index to a target prefix.

**Acceptance Criteria:**
- [ ] `cmd/strata-inventory` worker: hourly/daily ticks per inventory configuration, walks the source bucket's `objects` rows, writes a manifest object + a data object (CSV first, Parquet phase 2) to the target prefix.
- [ ] Inventory config XML round-trips via `setBucketBlob`.
- [ ] Tests: tick fires, manifest content validates, data object schema matches AWS Inventory CSV header.

#### US-021: Single-region Access Points
**Description:** As a multi-tenant deployment, I want named access points (`arn:aws:s3:region:account:accesspoint/<name>`) with their own policy + alias routing to a single bucket.

**Acceptance Criteria:**
- [ ] New `access_points` table (name PK, bucket ref, policy blob, public_access_block, network_origin).
- [ ] CreateAccessPoint / GetAccessPoint / DeleteAccessPoint via `?Action=CreateAccessPoint` etc.
- [ ] HTTP route accepts `<accesspoint-alias>.s3-accesspoint.<region>.example.com` host; rewrites to underlying bucket; access-point policy evaluated additively to the bucket policy.
- [ ] Tests: round-trip; policy gating via access point; deleted access point returns 404.

#### US-022: ScyllaDB validation
**Description:** As a high-throughput user, I want CI evidence ScyllaDB is a drop-in for Cassandra and a benchmark vs Cassandra on the LWT-heavy hot paths.

**Acceptance Criteria:**
- [ ] CI matrix: testcontainers tests run against both Cassandra 5.0 and ScyllaDB 5.x on a weekly schedule (not every PR).
- [ ] `docs/backends/scylla.md`: deployment notes, raft-LWT semantics confirmation, p50/p95/p99 latency table for CreateBucket / SetBucketVersioning / CompleteMultipartUpload.
- [ ] No code changes required to switch backends (gocql endpoint swap only).

#### US-023: Multi-cluster RADOS routing
**Description:** As a geo-sep cold-tier deployment, I want `data.ChunkRef.Cluster` to actually route to different RADOS clusters with their own keyrings and configs.

**Acceptance Criteria:**
- [ ] `rados.Backend` holds `map[clusterID]*rados.Conn`; per-cluster keyring/config under `STRATA_RADOS_CLUSTERS=<id>:<conf>:<keyring>,...`.
- [ ] PutChunks routing: storage class → cluster (extending the existing `STRATA_RADOS_CLASSES`).
- [ ] GetChunks dispatches by `ChunkRef.Cluster`; missing cluster id → 500 InternalError with structured-log error.
- [ ] Tests: lifecycle transition between hot (local) and cold (remote) cluster works end-to-end; cross-cluster GC still drains.

#### US-024: Online per-bucket shard resize
**Description:** As a bucket grows past tens of millions of objects, fan-out listing becomes expensive; I want an online split flow.

**Acceptance Criteria:**
- [ ] `strata-admin bucket reshard --bucket <name> --target <N>` queues a background rewrite.
- [ ] Rewrite worker copies rows to the new partition layout, LWT-flips the bucket's `shard_count` once done; old rows tombstone-cleaned in a follow-up tick.
- [ ] No downtime: clients reading mid-rewrite see a union of old+new shards via a `shard_count_in_progress` flag on the bucket row.
- [ ] Tests: 1k objects, reshard 32→128, list-objects returns identical set during and after.

#### US-025: Audit cold-tier export
**Description:** As an operator, I want raw audit rows older than 30 days dropped from Cassandra after being exported to a designated bucket as JSON-lines.

**Acceptance Criteria:**
- [ ] `cmd/strata-audit-export` daily-ticked worker.
- [ ] Exports raw rows older than `STRATA_AUDIT_EXPORT_AFTER` (default 30d) to a configured `audit-export` bucket as JSON-lines (one file per day, gzip-compressed).
- [ ] After successful export, deletes from `audit_log`.
- [ ] Tests: 100 rows → tick → 100 lines in target bucket → table partition empty.

#### US-026: Examples directory
**Description:** As a new Strata user, I want copy-paste examples for common workflows: bucket setup, multipart upload, presigned URL, lifecycle, replication, SSE-S3, IAM key rotation.

**Acceptance Criteria:**
- [ ] New `examples/` directory: subdirectories for `aws-cli`, `boto3`, `mc`, `s3cmd`; each has a README and runnable scripts against `make run-memory`.
- [ ] Smoke script `examples/smoke.sh` runs every example end-to-end.
- [ ] Linked from top-level README.

#### US-027: Protobuf manifest
**Description:** Manifest is JSON-encoded in `objects.manifest`. Migrate to protobuf for smaller row size + forward compatibility.

**Acceptance Criteria:**
- [ ] `internal/data/manifest.proto`; generated Go via `buf` with vendored output.
- [ ] Decoder reads both JSON and protobuf transparently; encoder writes protobuf for new objects.
- [ ] Background `manifest-rewriter` job re-encodes existing rows opportunistically (resumable, idempotent).
- [ ] Tests: round-trip both formats; backwards compatibility on a row written in JSON.

## Functional Requirements

- FR-1: Every feature listed in `ROADMAP.md` § "S3 API surface" with status "passthrough" gets a real implementation: SSE-S3 (US-001..003), Notifications (US-005), Replication (US-006), Logging (US-007), Website redirects (US-015).
- FR-2: All command binaries (`cmd/strata-*`) emit JSON logs by default with `request_id` / `worker_id` correlation.
- FR-3: Every worker exposes `/metrics` (Prometheus), `/healthz`, and `/readyz`.
- FR-4: Bucket-default encryption is honoured by every write path (PutObject, InitiateMultipartUpload, CopyObject).
- FR-5: Virtual-hosted-style URL routing works for any host matching `STRATA_VHOST_PATTERN`; path-style continues to work for any other host.
- FR-6: The audit log captures every state-changing request with `(principal, action, resource, result, request_id, source_ip)` and retains rows for 30 days by default.
- FR-7: OpenTelemetry tracing is opt-in via `OTEL_EXPORTER_OTLP_ENDPOINT`; absence does not impact behaviour or perf.
- FR-8: All Cassandra schema migrations stay additive (`ALTER TABLE ... ADD column`); destructive migrations are forbidden.
- FR-9: Within a band, stories may ship in any order. Stories in band P2 may not merge until all P1 stories have merged. Stories in P3 may not merge until all P1 stories have merged (P2 is not a prerequisite for P3).
- FR-10: Replication targets only other Strata clusters (not generic boto-compatible endpoints).
- FR-11: New invasive features (versioning null literal, virtual-hosted, per-part GET, real SSE-S3) ship default-on without feature flags.
- FR-12: All new code lands with unit tests; integration tests for every new worker; race-detector run on the new harness (US-018).

## Non-Goals

The following are explicitly out of scope:

- **Legacy S3 surface**: SigV2 / SigV1, POST upload (browser forms), Transfer Acceleration, Reduced Redundancy Storage.
- **AWS-specific niche features**: S3 Object Lambda, S3 Select, Storage Lens, Multi-Region Access Points, DSSE-KMS (dual-layer), real Glacier-class archival (RestoreObject stays a stub).
- **Cross-region / multi-master replication**. Replication is one-way Strata-to-Strata only (US-006); cross-region needs separate design.
- **Bidirectional replication** between Strata clusters.
- **Replication targets other than Strata**. Generic boto-compatible / AWS S3 / MinIO / Ceph RGW destinations are out of scope; the interface stays simple and Strata-flavoured for now.
- **Browser-based features** beyond what existing CORS supports.
- **Alternative metadata backends** (TiKV, FoundationDB, PostgreSQL+Citus). Cassandra and ScyllaDB only.
- **Changes to public S3 wire format** beyond what existing AWS SDKs expect.
- **No s3-tests numerical pass-rate target** is set for this PRD. Some failing tests may close opportunistically (especially US-013 + US-014 unblock 13), but the goal is feature completeness, not a number.

## Design Considerations

- **Reuse existing patterns aggressively.** The blob-config helpers (`setBucketBlob` / `getBucketBlob` / `deleteBucketBlob`) cover lifecycle / CORS / policy / public-access-block / ownership-controls / encryption-config / notification-config / replication-config / website-config / object-lock-config / tagging — keep using them for any new "bucket has one document of kind X" endpoint.
- **All new workers use `internal/leader`.** It is the only correct way to single-instance a tick-driven daemon against Cassandra.
- **Cassandra schema is append-only.** Add columns via `alterStatements`; never destructive migrations (also encoded as FR-8).
- **`auth.FromContext(ctx).Owner`** is the principal everywhere — feed it through to the audit log, replication source attribution, etc.
- **Tracing and metrics are zero-cost when disabled.** No allocations in hot paths when env vars are unset.
- **No new third-party dependencies** unless absolutely necessary. Stdlib `crypto/aes`, `crypto/cipher`, `log/slog`, `net/http/httptest` cover most of the work. Vault, AWS SDK, OTel SDK are vendored only when first needed.
- **Master-key provider abstraction (US-001) precedes encryption (US-002).** The provider returns a `(key, keyID)` tuple so rotation (US-003) is a property of the storage layer, not a special code path.
- **Audit log partitioning matters.** Partition by `(bucket, date)` so retention drops whole partitions cheaply; rows within a partition are ordered by TimeUUID.

## Technical Considerations

- **Order**: Land P1 fully (US-001..011), then P2 and P3 in parallel — each story within a band is independent. Within P1: US-001 (master-key provider) is a prerequisite for US-002/003/004; US-008 (slog) is a prerequisite for US-012 (audit log) since the audit hook reuses the request_id middleware.
- **Replication is a workload generator** (US-006). Putting it in P1 means `cmd/strata-replicator` will hammer the destination gateway it's replicating into. Test in a two-cluster compose; ensure the destination's auth allows the replicator's credentials.
- **Virtual-hosted (US-011) interacts with SigV4.** Canonicalisation must use the **original** Host header value, not the rewritten path. Easy to break — an integration test against `boto3` with `addressing_style='virtual'` is required.
- **Per-part GET (US-013) requires a manifest schema change.** Old objects without `PartSizes` still need to read normally — the partNumber path returns 400 InvalidArgument when the object has no parts, not the whole object.
- **Versioning null literal (US-014) is invasive.** Touches PutObject / GetObject / DeleteObject / ListObjectVersions / lifecycle / multipart-complete. Default-on; the prior decision to gate behind a flag was reversed.
- **OTel span volume can be high.** Sampling default 0.01 with always-on for failing requests via tail-based sampling.
- **CI cost.** ScyllaDB validation (US-022) doubles testcontainers runtime; runs weekly, not per-PR.
- **Replication targets are constrained to Strata** for this PRD. The `Destination.AccessControlTranslation.Owner` field is overloaded to carry `<peer-host>:<port>` — documented Strata-specific usage. Generic boto-compatible destinations are deferred to a future PRD.
- **SSE-S3 master-key in env** is the default provider for backward compatibility with existing deployments. Operators who need k8s-secret integration pick `file`; those running Vault pick `vault`. All three providers ship in this PRD.
- **Audit log retention** is 30d default, override via `STRATA_AUDIT_RETENTION` (Go duration). Cold-tier export (US-025) lifts data older than `STRATA_AUDIT_EXPORT_AFTER` to a target bucket before TTL deletion.

## Success Metrics

- **Real-implementation rubric** (each is yes/no for "Strata is feature-complete"):
  - Real encryption-at-rest: yes (US-002)
  - Real notification delivery: yes (US-005)
  - Real replication: yes (US-006, Strata-to-Strata)
  - Real log delivery: yes (US-007)
  - Real website redirect/routing: yes (US-015)
- **Operational visibility rubric**:
  - Structured JSON logs with request_id: yes (US-008)
  - Full Prometheus coverage: yes (US-009)
  - k8s-style health probes: yes (US-010)
  - OpenTelemetry tracing: yes (US-016)
  - Audit log: yes (US-012)
- **AWS-SDK ergonomics rubric**:
  - Virtual-hosted URLs: yes (US-011)
  - Bucket default encryption: yes (US-004)
  - Per-part GET + composite checksum: yes (US-013)
  - Versioning null literal: yes (US-014)
- **Performance baseline (post-P1)**:
  - p99 PUT < 50ms for 1MB on hot tier (measured via the histogram from US-009)
  - p99 GET < 30ms for 1MB
  - Throughput ≥ 5k ops/s/gateway on commodity hardware
- **Documentation rubric**:
  - Every P1+P2 feature has a runnable example under `examples/` (US-026 in P3 actually delivers this; bring forward if it slips).
- **No s3-tests numerical target.** Pass rate is expected to climb opportunistically (US-013 closes ~8, US-014 closes ~5) but is not a gating metric.

## Resolved Decisions

These were resolved before implementation began:

- **Vault master-key provider uses a per-Strata-cluster role.** Each cluster authenticates to Vault under its own role-id / secret-id pair (`STRATA_SSE_VAULT_ROLE_ID`, `STRATA_SSE_VAULT_SECRET_ID`). Reason: blast-radius isolation — compromise of one cluster does not grant access to other clusters' wrapped keys. App-role-shared-across-clusters was rejected.
- **Replication exposes a queue-depth metric.** `replication_queue_depth{rule_id}` gauge, sampled every 30s by the replicator worker (one `SELECT COUNT(*)` on the queue partition). Cheap and necessary for backlog alerting — the lag histogram only tells us about successful deliveries, not backlog.
- **Audit `?audit` read API does NOT support presigned URLs.** Header auth via `[iam root]` only. Reason: presigned URLs leak through proxy/server logs and have a long lifetime; audit data is sensitive enough that the operational simplicity is not worth the leak risk. Operators who want to share audit slices export them via the admin CLI (US-017) as local JSON.
- **`PartSizes` is a dedicated Cassandra column** (`objects.part_sizes list<int>`), not packed into the `manifest` blob. Reason: HEAD/GET with `?partNumber=N` is on the hot path and needs a single-column read without unmarshalling the 4–64 KiB manifest blob. Tradeoff: minor duplication (chunk sizes are derivable from manifest); revisit during US-027 (protobuf manifest) and consider folding then.
- **Website routing rules support Prefix + HttpErrorCode only — no regex.** Matches the AWS core spec; the AWS regex extension is non-standard, varies across RGW versions, and brings interpretation surprises. Operators who need richer routing place an nginx/Caddy in front.
- **`STRATA_VHOST_PATTERN` is a comma-separated list of patterns; first match wins.** Reason: a single Strata cluster commonly serves both production (`*.s3.example.com`) and staging (`*.s3-dev.example.com`) host suffixes. Comma-separated list is easier to write and read than a single regex with alternation.
