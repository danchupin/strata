---
title: 'STRATA_* environment variables'
weight: 10
---

<!--
Source of truth: grep the codebase via `grep -rhoE 'STRATA_[A-Z_][A-Z0-9_]+' cmd/strata/ internal/ | sort -u`.
Cross-reference defaults + clamp ranges at the consuming call sites (koanf envMap,
worker `*FromEnv` helpers, `os.Getenv(...)`). Update this page when adding a new env var.
-->

# `STRATA_*` environment variables

_Source of truth: grep the codebase via `grep -rhoE 'STRATA_[A-Z_][A-Z0-9_]+' cmd/strata/ internal/`._
_Update this page when adding a new env var — the [reference index]({{< ref "/reference" >}})
links here as the operator-facing tuning manual._

Variables are grouped by the layer that consumes them. CLI flags on
`strata server` (e.g. `--listen`, `--vhost-pattern`, `--log-level`) override
the matching env var.

## Gateway (core HTTP)

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_LISTEN` | `:9000` | host:port | HTTP listen address. CLI: `--listen`. |
| `STRATA_REGION` | `strata-local` | string | Default region tag advertised by the gateway and stamped on bucket-create. |
| `STRATA_DATA_BACKEND` | `memory` | `memory \| rados \| s3` | Data-backend selector. Required at boot. |
| `STRATA_META_BACKEND` | `memory` | `memory \| cassandra \| tikv` | Meta-backend selector. Required at boot. |
| `STRATA_BUCKET_SHARDS` | `64` | positive int | Per-bucket default shard count for the `objects` table; see [sharded objects]({{< ref "/architecture/sharding" >}}). |
| `STRATA_SHUTDOWN_WAIT` | `10s` | Go duration | Graceful-shutdown drain window before `http.Server.Close`. |
| `STRATA_VHOST_PATTERN` | `*.s3.local` | comma-separated `*.<suffix>`; `-` to disable | Virtual-hosted-style routing. CLI: `--vhost-pattern`. |
| `STRATA_LOG_LEVEL` | `INFO` | `DEBUG \| INFO \| WARN \| ERROR` | slog handler level. CLI: `--log-level`. |
| `STRATA_NODE_ID` | hostname-derived | string | Replica identity, stamped on heartbeats + leader leases. |
| `STRATA_WORKERS` | empty | comma list | Workers to run on this replica (`gc,lifecycle,...`). CLI: `--workers=`. Unknown names exit 2 at startup. |
| `STRATA_CONFIG_FILE` | empty | path | Optional TOML config; env vars + CLI flags layer on top. |
| `STRATA_VERSION` | empty | string | Version label overridden at build time; surfaced via `/version` + admin metadata. |
| `STRATA_CLUSTER_NAME` | empty | string | Logical cluster name surfaced to the admin console. |
| `STRATA_PROMETHEUS_URL` | empty | URL | PromQL endpoint for hot-bucket / lag / metrics dashboards. Unset → admin reports `metrics_available=false`. |
| `STRATA_PROM_PUSHGATEWAY` | empty | URL | Pushgateway target for `strata admin bench-*` throughput gauges. |
| `STRATA_AUDIT_RETENTION` | `720h` (30d) | Go duration or `<N>d` | Row TTL on `audit_log`. See [audit log retention]({{< ref "/best-practices/quotas-billing" >}}). |
| `STRATA_MANIFEST_FORMAT` | `proto` | `proto \| json` | Write-format for `objects.manifest`. Read path sniffs both. |
| `STRATA_MFA_SECRETS` | empty | `serial:base32,...` | Optional TOTP secrets for MFA Delete; see [auth]({{< ref "/architecture/auth" >}}). |
| `STRATA_STORAGE_HEALTH_OVERRIDE` | empty | `healthy \| degraded \| down` | Admin-console health override for canary deploys. |
| `STRATA_ADMIN_ENDPOINT` | `http://localhost:9000` | URL | `strata admin <subcmd>` client target. |
| `STRATA_ADMIN_PRINCIPAL` | empty | string | Test harness — populates `X-Test-Principal` on admin CLI calls. |
| `STRATA_BUCKETSTATS_INTERVAL` | unset → `1h` | Go duration | Bucketstats sampler cadence. Sub-second values accepted for e2e. |
| `STRATA_BUCKETSTATS_TOPN` | `100` | positive int | Top-N cap for the per-shard distribution sampler. |

## Meta backend — Cassandra / ScyllaDB

ScyllaDB uses the same gocql client; no code-level distinction.

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_CASSANDRA_HOSTS` | `127.0.0.1` | comma-separated host list | Contact points. |
| `STRATA_CASSANDRA_KEYSPACE` | `strata` | string | Keyspace name; created at boot if missing. |
| `STRATA_CASSANDRA_DC` | `datacenter1` | string | Local datacenter for `LOCAL_QUORUM` routing. |
| `STRATA_CASSANDRA_REPLICATION` | `{'class': 'SimpleStrategy', 'replication_factor': '1'}` | CQL replication strategy | Used only when the keyspace is created by strata. |
| `STRATA_CASSANDRA_USER` | empty | string | Auth user. |
| `STRATA_CASSANDRA_PASSWORD` | empty | string | Auth password. |
| `STRATA_CASSANDRA_TIMEOUT` | `10s` | Go duration | Per-query timeout. |
| `STRATA_CASSANDRA_SLOW_MS` | `100` | positive int (ms) | Slow-query WARN threshold for the gocql query observer. |

## Meta backend — TiKV

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_TIKV_PD_ENDPOINTS` | empty | comma-separated PD addrs | Required when `STRATA_META_BACKEND=tikv`. |
| `STRATA_GC_DUAL_WRITE` | `on` | `on \| off` | Dual-write the legacy + denormalised GC tables. Flipped off after a migration cycle drains the legacy queue. Applies to Cassandra + TiKV. |

## Data backend — RADOS

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_RADOS_CONF` | `/etc/ceph/ceph.conf` | path | `ceph.conf` for the implicit `default` cluster. |
| `STRATA_RADOS_USER` | `admin` | short user id | `client.<id>` resolved via `go-ceph.NewConnWithUser`. |
| `STRATA_RADOS_KEYRING` | empty | path | Keyring override (defaults to `[client.<user>]` in `ceph.conf`). |
| `STRATA_RADOS_POOL` | `strata.rgw.buckets.data` | pool name | Default data pool. |
| `STRATA_RADOS_NAMESPACE` | empty | string | Optional RADOS namespace prefix. |
| `STRATA_RADOS_CLASSES` | empty | `<class>=<pool>,...` | Per-storage-class pool override map. |
| `STRATA_RADOS_CLUSTERS` | empty | `<id>:<conf-path>:<keyring-path>,...` | Multi-cluster connection specs. The implicit `default` cluster uses the single-cluster envs above. |
| `STRATA_RADOS_PUT_CONCURRENCY` | `32` | `[1, 256]` | Parallel chunk-PUT bound. See [parallel chunks tuning]({{< ref "/architecture/benchmarks/parallel-chunks" >}}#tuning-knobs). |
| `STRATA_RADOS_GET_PREFETCH` | `4` | `[1, 64]` | GET-path prefetch depth. See [parallel chunks tuning]({{< ref "/architecture/benchmarks/parallel-chunks" >}}#tuning-knobs). |
| `STRATA_RADOS_HEALTH_OID` | `strata-readyz-canary` | RADOS OID | Canary object stat'd by `/readyz`. Internal — debug only. |

## Data backend — S3 pass-through

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_S3_CLUSTERS` | empty | JSON array of `S3ClusterSpec` | Required when `STRATA_DATA_BACKEND=s3`. See [S3 multi-cluster routing]({{< ref "/best-practices/s3-multi-cluster" >}}#strata_s3_clusters--json-array). |
| `STRATA_S3_CLASSES` | empty | JSON object of `ClassSpec` | Required when `STRATA_DATA_BACKEND=s3`. See [S3 multi-cluster routing]({{< ref "/best-practices/s3-multi-cluster" >}}#strata_s3_classes--json-object). |

## Auth + admin console

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_AUTH_MODE` | `off` | `off \| disabled \| required \| optional` | SigV4 enforcement mode. |
| `STRATA_STATIC_CREDENTIALS` | empty | `ak:sk,...` | Static credential store (bootstrap before IAM is populated). |
| `STRATA_CONSOLE_JWT_SECRET` | empty | hex 32 bytes | Admin-console session signing key. Empty → ephemeral 32-byte secret (sessions invalidate on restart). |
| `STRATA_JWT_SECRET_FILE` | `/etc/strata/jwt-secret` | path | On-disk JWT secret read at boot. Lower precedence than `STRATA_CONSOLE_JWT_SECRET`. |
| `STRATA_JWT_SHARED` | empty | path | Shared JWT secret mount for cross-replica session validation (multi-replica labs). |
| `STRATA_CONSOLE_THEME_DEFAULT` | `system` | `system \| light \| dark` | Admin-console default theme. |

## SSE + KMS + master keys

Precedence inside `crypto/master`: `STRATA_SSE_MASTER_KEYS` > `STRATA_SSE_MASTER_KEY_VAULT` > `STRATA_SSE_MASTER_KEY_FILE` > `STRATA_SSE_MASTER_KEY`.

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_SSE_MASTER_KEYS` | empty | `<id>:<hex64>,...` | Rotation provider (active = first). Required for `strata admin rewrap`. |
| `STRATA_SSE_MASTER_KEY` | empty | hex 64 | Single static master key. |
| `STRATA_SSE_MASTER_KEY_ID` | empty | string | Identifier paired with the static key. |
| `STRATA_SSE_MASTER_KEY_FILE` | empty | path | File-backed master key with mtime hot-reload. |
| `STRATA_SSE_MASTER_KEY_VAULT` | empty | `<addr>:<transit-export-path>` | Vault Transit-export provider. |
| `STRATA_SSE_VAULT_ROLE_ID` | empty | string | AppRole role id (shared with `STRATA_KMS_VAULT_*`). |
| `STRATA_SSE_VAULT_SECRET_ID` | empty | string | AppRole secret id (shared with `STRATA_KMS_VAULT_*`). |
| `STRATA_KMS_AWS_REGION` | empty | AWS region | Enables AWS KMS provider; SDK client factory must also be wired. |
| `STRATA_KMS_LOCAL_HSM_SEED` | empty | hex 32 | Deterministic local HSM stand-in for tests. Internal — debug only. |
| `STRATA_KMS_VAULT_ADDR` | empty | URL | Vault Transit endpoint. |
| `STRATA_KMS_VAULT_PATH` | empty | string | Vault Transit mount path (e.g. `transit`). |

## GC worker (`--workers=gc`)

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_GC_INTERVAL` | `30s` | Go duration | Tick cadence. |
| `STRATA_GC_GRACE` | `5m` | Go duration | Grace before a queued chunk is eligible for delete. |
| `STRATA_GC_BATCH_SIZE` | `0` (backend default) | non-negative int | Per-tick batch cap; `0` uses the meta backend's preferred batch size. |
| `STRATA_GC_CONCURRENCY` | `1` | positive int | Per-shard delete workers. |
| `STRATA_GC_SHARDS` | `1` | `[1, 1024]` | Fan-out shard count (also drives lifecycle leader-replica selection). Out-of-range → clamped + WARN at boot. |
| `STRATA_GC_METRICS_LISTEN` | `:9100` | host:port | Legacy GC metrics listener (subsumed by the main gateway exporter). |

## Lifecycle worker (`--workers=lifecycle`)

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_LIFECYCLE_INTERVAL` | `60s` | Go duration | Tick cadence. |
| `STRATA_LIFECYCLE_UNIT` | `day` | `day \| hour` (test fixtures may inject other values) | Granularity for `Days`/`Date` evaluation. |
| `STRATA_LIFECYCLE_CONCURRENCY` | `1` | positive int | Per-tick transition/expire workers. |
| `STRATA_LIFECYCLE_METRICS_LISTEN` | `:9101` | host:port | Legacy lifecycle metrics listener. |

## Rebalance worker (`--workers=rebalance`)

See [Placement & rebalance]({{< ref "/best-practices/placement-rebalance" >}}#bandwidth-tuning).

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_REBALANCE_INTERVAL` | `5m` | `[1m, 24h]` | Tick cadence; out-of-range → clamped. |
| `STRATA_REBALANCE_RATE_MB_S` | `100` | `[1, 10000]` | Token-bucket budget (read+write share the same bucket). |
| `STRATA_REBALANCE_INFLIGHT` | `4` | `[1, 64]` | Per-Move errgroup bound. |
| `STRATA_REBALANCE_SHARDS` | `1` | `[1, 1024]` | Per-shard leader-elected fan-out count. |

## Notify worker (`--workers=notify`)

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_NOTIFY_TARGETS` | empty | `type:arn=<url>\|<secret>,...` | Required when worker is enabled. |
| `STRATA_NOTIFY_INTERVAL` | `5s` | Go duration | Poll cadence. |
| `STRATA_NOTIFY_MAX_RETRIES` | `6` | non-negative int | Retry cap before DLQ. |
| `STRATA_NOTIFY_BACKOFF_BASE` | `1s` | Go duration | Exponential-backoff base. |
| `STRATA_NOTIFY_POLL_LIMIT` | `100` | positive int | Per-poll row cap. |

## Replicator worker (`--workers=replicator`)

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_REPLICATOR_INTERVAL` | `5s` | Go duration | Poll cadence. |
| `STRATA_REPLICATOR_MAX_RETRIES` | `6` | non-negative int | Retry cap before DLQ. |
| `STRATA_REPLICATOR_BACKOFF_BASE` | `1s` | Go duration | Exponential-backoff base. |
| `STRATA_REPLICATOR_POLL_LIMIT` | `100` | positive int | Per-poll row cap. |
| `STRATA_REPLICATOR_HTTP_TIMEOUT` | `30s` | Go duration | Per-request peer HTTP timeout. |
| `STRATA_REPLICATOR_PEER_SCHEME` | `https` | `http \| https` | Peer URL scheme. |

## Access-log worker (`--workers=access-log`)

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_ACCESS_LOG_INTERVAL` | `5m` | Go duration | Flush cadence. |
| `STRATA_ACCESS_LOG_MAX_FLUSH_BYTES` | `5242880` (5 MiB) | positive int64 | Per-flush bytes cap. |
| `STRATA_ACCESS_LOG_POLL_LIMIT` | `10000` | positive int | Per-poll row cap. |

## Inventory worker (`--workers=inventory`)

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_INVENTORY_INTERVAL` | `5m` | Go duration | Tick cadence. |
| `STRATA_INVENTORY_REGION` | `deps.Region` fallback | string | Region tag for target-bucket writes. |

## Audit-export worker (`--workers=audit-export`)

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_AUDIT_EXPORT_BUCKET` | empty | bucket name | Required when worker is enabled. |
| `STRATA_AUDIT_EXPORT_PREFIX` | empty | object-key prefix | Optional path prefix inside the export bucket. |
| `STRATA_AUDIT_EXPORT_AFTER` | `720h` (30d) | Go duration | Drain partitions older than this. |
| `STRATA_AUDIT_EXPORT_INTERVAL` | `24h` | Go duration | Tick cadence. |

## Manifest-rewriter worker (`--workers=manifest-rewriter`)

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_MANIFEST_REWRITER_INTERVAL` | `24h` | Go duration | Tick cadence. |
| `STRATA_MANIFEST_REWRITER_BATCH_LIMIT` | `500` | positive int | Per-tick rewrite cap. |
| `STRATA_MANIFEST_REWRITER_DRY_RUN` | `false` | `true \| false` | Skip the write phase; log diffs only. |

## Quota-reconcile worker (`--workers=quota-reconcile`)

See [Quotas + billing]({{< ref "/best-practices/quotas-billing" >}}#drift-reconcile-workersquota-reconcile).

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_QUOTA_RECONCILE_INTERVAL` | `6h` | Go duration | Tick cadence. |

## Usage-rollup worker (`--workers=usage-rollup`)

See [Quotas + billing]({{< ref "/best-practices/quotas-billing" >}}#usage-rollup-workersusage-rollup).

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_USAGE_ROLLUP_INTERVAL` | `24h` | Go duration | Tick cadence. |
| `STRATA_USAGE_ROLLUP_AT` | `00:00` (UTC) | `HH:MM` | Anchor time for the daily rollup. |

## Tracing (OpenTelemetry)

See [Tracing best-practices]({{< ref "/best-practices/tracing" >}}).

| Variable | Default | Range | Notes |
|---|---|---|---|
| `STRATA_OTEL_SAMPLE_RATIO` | `0.01` | `[0, 1]` | Tail sampler ratio; failing spans always export. |
| `STRATA_OTEL_RINGBUF` | `on` | `on \| off` | In-process span ring buffer (powers `/admin/v1/diagnostics/trace`). |
| `STRATA_OTEL_RINGBUF_BYTES` | `4194304` (4 MiB) | positive int | Ring-buffer byte budget. |

The standard W3C `OTEL_EXPORTER_OTLP_ENDPOINT` controls OTLP/HTTP export (unset disables export entirely).

## Retired / legacy

| Variable | Status | Notes |
|---|---|---|
| `STRATA_DRAIN_STRICT` | Retired | US-007 made drain unconditionally strict. WARN-logged at boot if set; ignored. |
| `STRATA_S3_BACKEND_*` (`_ENDPOINT`, `_REGION`, `_BUCKET`, `_ACCESS_KEY`, `_SECRET_KEY`, `_FORCE_PATH_STYLE`, `_PART_SIZE`, `_UPLOAD_CONCURRENCY`, `_MAX_RETRIES`, `_OP_TIMEOUT_SECS`, `_MULTIPART_TIMEOUT_SECS`, `_SSE_MODE`, `_SSE_KMS_KEY_ID`) | Retired in `ralph/s3-multi-cluster` | Replaced by `STRATA_S3_CLUSTERS` + `STRATA_S3_CLASSES`. Boot fails loud if the legacy envs are still set without the JSON replacements. |

## Test-only

| Variable | Notes |
|---|---|
| `STRATA_TIKV_TEST_PD_ENDPOINTS` | Operator-provided PD endpoints for TiKV integration tests (bypasses testcontainers). |
| `STRATA_SCYLLA_TEST` / `STRATA_SCYLLA_IMAGE` | Gates + image override for the ScyllaDB contract suite. |
| `STRATA_TEST_AK` / `STRATA_TEST_SK` | Static AK/SK pair consumed by the S3 multi-cluster contract suite via `CredentialsEnv`. |
| `STRATA_TEST_CEPH_CONF` / `STRATA_TEST_CEPH_POOL` / `STRATA_TEST_CEPH_CLASSES` | RADOS integration-test cluster wiring. |
| `STRATA_TEST_REBALANCE_SRC_POOL` / `STRATA_TEST_REBALANCE_TGT_POOL` | RADOS rebalance mover integration-test pools. |
