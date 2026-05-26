---
title: 'STRATA_* environment variables'
weight: 10
---

<!--
Maintainer note: source of truth for new entries is the codebase. Grep with
`grep -rhoE 'STRATA_[A-Z_][A-Z0-9_]+' cmd/strata/ internal/ | sort -u` and
cross-reference defaults + clamp ranges at the consuming call sites (koanf
envMap, worker `*FromEnv` helpers, `os.Getenv(...)`). Update this page when
adding a new env var.

The `TOML key` column mirrors `envMap` in `internal/config/config.go`.
Exempt env vars (bootstrap, test-only, retired, build metadata) show `—`.
The drift-lint test `internal/config/env_toml_parity_test.go` fails the
build if a STRATA_* var is added without wiring through Config + envMap +
this page, so keep all three in lockstep.
-->

# `STRATA_*` environment variables

Every `STRATA_*` knob, grouped by the layer that consumes it. CLI flags on
`strata server` (`--listen`, `--vhost-pattern`, `--log-level`, `--workers=`)
override the matching env var.

This is the operator-facing tuning manual; see the
[reference index]({{< ref "/reference" >}}) for the rest of the reference
material.

## Gateway (core HTTP)

See [Concepts — S3 surface]({{< ref "/concepts/s3-surface" >}}) for what the
gateway speaks and [Architecture — router]({{< ref "/architecture/router" >}})
for the dispatch shape.

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_LISTEN` | `:9000` | host:port | HTTP listen address. CLI: `--listen`. | `listen` |
| `STRATA_REGION` | `strata-local` | string | Default region tag advertised by the gateway and stamped on bucket-create. | `region` |
| `STRATA_DATA_BACKEND` | `memory` | `memory \| rados \| s3` | Data-backend selector. Required at boot. | `data_backend` |
| `STRATA_META_BACKEND` | `memory` | `memory \| cassandra \| tikv` | Meta-backend selector. Required at boot. | `meta_backend` |
| `STRATA_BUCKET_SHARDS` | `64` | positive int | Per-bucket default shard count for the `objects` table; see [sharded objects]({{< ref "/architecture/sharding" >}}). | `default_bucket_shards` |
| `STRATA_SHUTDOWN_WAIT` | `10s` | Go duration | Graceful-shutdown drain window before `http.Server.Close`. | `shutdown_wait` |
| `STRATA_HTTP_READ_HEADER_TIMEOUT` | `10s` | Go duration ≥ 0 | Slowloris-safe ceiling on header receipt. `0` = disabled (net/http semantic). | `http.read_header_timeout` |
| `STRATA_HTTP_READ_TIMEOUT` | `60s` | Go duration ≥ 0 | Header + body receipt ceiling. `0` = disabled. | `http.read_timeout` |
| `STRATA_HTTP_WRITE_TIMEOUT` | `30m` | Go duration in `[0, 24h]` | Response-write ceiling. 30m default ≈ 2.8 MB/s minimum on a 5 GiB body (cellular safe). `0` = disabled. | `http.write_timeout` |
| `STRATA_HTTP_IDLE_TIMEOUT` | `120s` | Go duration ≥ 0 | Keep-alive idle ceiling. `0` = disabled. | `http.idle_timeout` |
| `STRATA_HTTP_MAX_HEADER_BYTES` | `1048576` (1 MiB) | int in `[0, 16777216]` | Header byte cap per request. `0` = net/http default (1 MiB). | `http.max_header_bytes` |
| `STRATA_TLS_CERT_FILE` | empty | path | PEM certificate (server + optional intermediates) for the built-in TLS listener. Empty → plain HTTP. Must be set together with `STRATA_TLS_KEY_FILE`. | `tls.cert_file` |
| `STRATA_TLS_KEY_FILE` | empty | path | PEM private key matching `STRATA_TLS_CERT_FILE`. | `tls.key_file` |
| `STRATA_TLS_MIN_VERSION` | `TLS1.2` | `TLS1.2 \| TLS1.3` | Minimum negotiated TLS protocol version. | `tls.min_version` |
| `STRATA_TLS_CIPHER_PROFILE` | `mozilla-modern` | `mozilla-modern \| mozilla-intermediate \| go-default` | TLS 1.2 cipher suite selection. `mozilla-modern` pins TLS 1.3 AEAD suites only (TLS 1.2 clients rejected). Informational on TLS 1.3 connections per RFC 8446. | `tls.cipher_profile` |
| `STRATA_TLS_CERT_DIR` | empty | path | SNI multi-cert directory (US-003). Walked for `*.crt` + matching `*.key` pairs; cert dispatched per-handshake via `tls.Config.GetCertificate`. Mutually exclusive with `STRATA_TLS_CERT_FILE`. | `tls.cert_dir` |
| `STRATA_TLS_CLIENT_CA_FILE` | empty | path | PEM CA bundle for client-cert verification. When set, the gateway requires mTLS (`ClientAuth=RequireAndVerifyClientCert`). | `tls.client_ca_file` |
| `STRATA_TLS_RELOAD_INTERVAL` | `60s` | Go duration in `[10s, 1h]` or `0` | Periodic re-stat fallback for fsnotify drops + k8s ConfigMap atomic-symlink swaps. `0` disables (fsnotify-only). | `tls.reload_interval` |
| `STRATA_VHOST_PATTERN` | `*.s3.local` | comma-separated `*.<suffix>`; `-` to disable | Virtual-hosted-style routing. CLI: `--vhost-pattern`. | `vhost.pattern` |
| `STRATA_LOG_LEVEL` | `INFO` | `DEBUG \| INFO \| WARN \| ERROR` | slog handler level. CLI: `--log-level`. | `logging.level` |
| `STRATA_LOG_FORMAT` | `json` | `json \| text` | slog handler format. | `logging.format` |
| `STRATA_NODE_ID` | hostname-derived | string | Replica identity, stamped on heartbeats + leader leases. | `node.id` |
| `STRATA_WORKERS` | empty | comma list | Workers to run on this replica (`gc,lifecycle,...`). CLI: `--workers=`. Unknown names exit 2 at startup. | `workers.enabled` |
| `STRATA_CONFIG_FILE` | empty | path | Optional TOML config; env vars + CLI flags layer on top. | — |
| `STRATA_VERSION` | empty | string | Version label overridden at build time; surfaced via `/version` + admin metadata. | — |
| `STRATA_CLUSTER_NAME` | empty | string | Logical cluster name surfaced to the admin console. | `cluster.name` |
| `STRATA_PROMETHEUS_URL` | empty | URL | PromQL endpoint for hot-bucket / lag / metrics dashboards. Unset → admin reports `metrics_available=false`. | `prometheus.url` |
| `STRATA_PROM_PUSHGATEWAY` | empty | URL | Pushgateway target for `strata admin bench-*` throughput gauges. | — |
| `STRATA_AUDIT_RETENTION` | `720h` (30d) | Go duration or `<N>d` | Row TTL on `audit_log`. See [audit log retention]({{< ref "/best-practices/quotas-billing" >}}). | `audit_log.retention` |
| `STRATA_MANIFEST_FORMAT` | `proto` | `proto \| json` | Write-format for `objects.manifest`. Read path sniffs both. | `manifest.format` |
| `STRATA_MFA_SECRETS` | empty | `serial:base32,...` | Optional TOTP secrets for MFA Delete; see [auth]({{< ref "/architecture/auth" >}}). | `mfa.secrets` |
| `STRATA_STORAGE_HEALTH_OVERRIDE` | empty | `healthy \| degraded \| down` | Admin-console health override for canary deploys. | — |
| `STRATA_ADMIN_ENDPOINT` | `http://localhost:9000` | URL | `strata admin <subcmd>` client target. | — |
| `STRATA_ADMIN_PRINCIPAL` | empty | string | Test harness — populates `X-Test-Principal` on admin CLI calls. | — |
| `STRATA_BUCKETSTATS_INTERVAL` | unset → `1h` | Go duration | Bucketstats sampler cadence. Sub-second values accepted for e2e. | `bucket_stats.interval` |
| `STRATA_BUCKETSTATS_TOPN` | `100` | positive int | Top-N cap for the per-shard distribution sampler. | `bucket_stats.top_n` |

## Meta backend — Cassandra / ScyllaDB

ScyllaDB is a CQL-compatible drop-in for Cassandra; the same envs apply.
See [Concepts — workers]({{< ref "/concepts/workers" >}}) and
[Architecture — meta-store]({{< ref "/architecture/meta-store" >}}) for
the meta-backend contract.

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_CASSANDRA_HOSTS` | `127.0.0.1` | comma-separated host list | Contact points. | `cassandra.hosts` |
| `STRATA_CASSANDRA_KEYSPACE` | `strata` | string | Keyspace name; created at boot if missing. | `cassandra.keyspace` |
| `STRATA_CASSANDRA_DC` | `datacenter1` | string | Local datacenter for `LOCAL_QUORUM` routing. | `cassandra.local_dc` |
| `STRATA_CASSANDRA_REPLICATION` | `{'class': 'SimpleStrategy', 'replication_factor': '1'}` | CQL replication strategy | Used only when the keyspace is created by strata. | `cassandra.replication` |
| `STRATA_CASSANDRA_USER` | empty | string | Auth user. | `cassandra.username` |
| `STRATA_CASSANDRA_PASSWORD` | empty | string | Auth password. | `cassandra.password` |
| `STRATA_CASSANDRA_TIMEOUT` | `10s` | Go duration | Per-query timeout. | `cassandra.timeout` |
| `STRATA_CASSANDRA_SLOW_MS` | `100` | positive int (ms) | Slow-query WARN threshold for the Cassandra query observer. | `cassandra.slow_ms` |
| `STRATA_CASSANDRA_TLS_CA_FILE` | empty | path | PEM CA bundle for server-cert verification. Empty → system root pool (when any TLS field is set) or plain-TCP (when all TLS fields unset). | `cassandra.tls.ca_file` |
| `STRATA_CASSANDRA_TLS_CERT_FILE` | empty | path | PEM client certificate for mutual TLS. Must be paired with `STRATA_CASSANDRA_TLS_KEY_FILE`. | `cassandra.tls.cert_file` |
| `STRATA_CASSANDRA_TLS_KEY_FILE` | empty | path | PEM private key matching `STRATA_CASSANDRA_TLS_CERT_FILE`. | `cassandra.tls.key_file` |
| `STRATA_CASSANDRA_TLS_SKIP_VERIFY` | `false` | bool | Disables server-cert verification (sets `tls.Config.InsecureSkipVerify` + `gocql.SslOptions.EnableHostVerification=false`). Bumps `strata_backend_tls_skip_verify{backend="cassandra"}=1` and logs a WARN at boot. Never set in production. | `cassandra.tls.skip_verify` |

## Meta backend — TiKV

See [Architecture — backends/TiKV]({{< ref "/architecture/backends/tikv" >}})
for the meta-backend contract + range-scan short-circuit.

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_TIKV_PD_ENDPOINTS` | empty | comma-separated PD addrs | Required when `STRATA_META_BACKEND=tikv`. | `tikv.pd_endpoints` |
| `STRATA_TIKV_TLS_CA_FILE` | empty | path | PEM CA bundle for server-cert verification. Required when any other `STRATA_TIKV_TLS_*` knob is set (tikv-client-go's `Security.ToTLSConfig` short-circuits on empty `ClusterSSLCA`). Empty all-four → plain-gRPC. | `tikv.tls.ca_file` |
| `STRATA_TIKV_TLS_CERT_FILE` | empty | path | PEM client certificate for mutual TLS. Must be paired with `STRATA_TIKV_TLS_KEY_FILE`. | `tikv.tls.cert_file` |
| `STRATA_TIKV_TLS_KEY_FILE` | empty | path | PEM private key matching `STRATA_TIKV_TLS_CERT_FILE`. | `tikv.tls.key_file` |
| `STRATA_TIKV_TLS_SKIP_VERIFY` | `false` | bool | Disables server-cert verification on the PD HTTP control plane. Bumps `strata_backend_tls_skip_verify{backend="tikv"}=1` and logs a WARN at boot. Never set in production. | `tikv.tls.skip_verify` |
| `STRATA_GC_DUAL_WRITE` | `on` | `on \| off` | Dual-write the legacy + denormalised GC tables. Flipped off after a migration cycle drains the legacy queue. Applies to Cassandra + TiKV. | `workers.gc.dual_write` |

## Data backend — RADOS

See [Architecture — data-backend]({{< ref "/architecture/data-backend" >}}).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_RADOS_CONF` | `/etc/ceph/ceph.conf` | path | `ceph.conf` for the implicit `default` cluster. | `rados.config_file` |
| `STRATA_RADOS_USER` | `admin` | short user id | `client.<id>` resolved via `go-ceph.NewConnWithUser`. | `rados.user` |
| `STRATA_RADOS_KEYRING` | empty | path | Keyring override (defaults to `[client.<user>]` in `ceph.conf`). | `rados.keyring` |
| `STRATA_RADOS_POOL` | `strata.rgw.buckets.data` | pool name | Default data pool. | `rados.pool` |
| `STRATA_RADOS_NAMESPACE` | empty | string | Optional RADOS namespace prefix. | `rados.namespace` |
| `STRATA_RADOS_CLASSES` | empty | `<class>=<pool>,...` | Per-storage-class pool override map. | `rados.classes` |
| `STRATA_RADOS_CLUSTERS` | empty | `<id>:<conf-path>:<keyring-path>,...` | Multi-cluster connection specs. The implicit `default` cluster uses the single-cluster envs above. | `rados.clusters` |
| `STRATA_RADOS_PUT_CONCURRENCY` | `32` | `[1, 256]` | Parallel chunk-PUT bound. See [parallel chunks tuning]({{< ref "/architecture/benchmarks/parallel-chunks" >}}#tuning-knobs). | `rados.put_concurrency` |
| `STRATA_RADOS_GET_PREFETCH` | `4` | `[1, 64]` | GET-path prefetch depth. See [parallel chunks tuning]({{< ref "/architecture/benchmarks/parallel-chunks" >}}#tuning-knobs). | `rados.get_prefetch` |
| `STRATA_RADOS_HEALTH_OID` | `strata-readyz-canary` | RADOS OID | Canary object stat'd by `/readyz`. Internal — debug only. | `rados.health_oid` |
| `STRATA_RADOS_POOL_SIZE` | `1` | `[1, 32]` | Per-cluster connection-pool depth. | `rados.pool_size` |
| `STRATA_RADOS_BATCH_OPS` | `false` | `true \| false` | Toggle WriteOp/ReadOp batched helpers. | `rados.batch_ops` |

## Data backend — S3 pass-through

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_S3_CLUSTERS` | empty | JSON array of `S3ClusterSpec` | Required when `STRATA_DATA_BACKEND=s3`. See [S3 multi-cluster routing]({{< ref "/best-practices/s3-multi-cluster" >}}#strata_s3_clusters--json-array). | `s3.clusters` |
| `STRATA_S3_CLASSES` | empty | JSON object of `ClassSpec` | Required when `STRATA_DATA_BACKEND=s3`. See [S3 multi-cluster routing]({{< ref "/best-practices/s3-multi-cluster" >}}#strata_s3_classes--json-object). | `s3.classes` |
| `STRATA_S3_TLS_CA_FILE` | empty | PEM path | S3-upstream mTLS — PEM-encoded CA bundle for server-cert validation. Global default; per-cluster `tls.ca_file` on `STRATA_S3_CLUSTERS` overrides outright. | `s3.tls.ca_file` |
| `STRATA_S3_TLS_CERT_FILE` | empty | PEM path | S3-upstream mTLS — PEM-encoded client certificate. Paired with `STRATA_S3_TLS_KEY_FILE`; half-pair rejected at boot. | `s3.tls.cert_file` |
| `STRATA_S3_TLS_KEY_FILE` | empty | PEM path | S3-upstream mTLS — PEM-encoded client private key. Paired with `STRATA_S3_TLS_CERT_FILE`. | `s3.tls.key_file` |
| `STRATA_S3_TLS_SKIP_VERIFY` | `false` | bool | Disable server-cert validation on the S3-upstream client. Bumps `strata_backend_tls_skip_verify{backend="s3",cluster=<id>}=1` for every cluster that resolves to this bundle. Never set in production. | `s3.tls.skip_verify` |

## Auth + admin console

See [Architecture — auth]({{< ref "/architecture/auth" >}}) and
[Best practices — operator console]({{< ref "/best-practices/web-ui" >}}).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_AUTH_MODE` | `off` | `off \| disabled \| required \| optional` | SigV4 enforcement mode. | `auth.mode` |
| `STRATA_STATIC_CREDENTIALS` | empty | `ak:sk,...` | Static credential store (bootstrap before IAM is populated). | `auth.static_credentials` |
| `STRATA_STS_DURATION` | `1h` | Go duration ∈ [`15m`, `12h`] | Default `AssumeRole` TTL when the client omits `DurationSeconds`. Out-of-range values clamp + WARN. | `auth.sts_duration` |
| `STRATA_CONSOLE_JWT_SECRET` | empty | hex 32 bytes | Admin-console session signing key. Empty → ephemeral 32-byte secret (sessions invalidate on restart). | `console.jwt_secret` |
| `STRATA_JWT_SECRET_FILE` | `/etc/strata/jwt-secret` | path | On-disk JWT secret read at boot. Lower precedence than `STRATA_CONSOLE_JWT_SECRET`. | `jwt.secret_file` |
| `STRATA_JWT_SHARED` | empty | path | Shared JWT secret mount for cross-replica session validation (multi-replica labs). | `jwt.shared_file` |
| `STRATA_CONSOLE_THEME_DEFAULT` | `system` | `system \| light \| dark` | Admin-console default theme. | `console.theme_default` |

## SSE + KMS + master keys

See [Best practices — compliance]({{< ref "/best-practices/compliance" >}})
for the operator workflow and [Architecture — auth]({{< ref "/architecture/auth" >}})
for the SSE wrap + KMS provider contract.

Precedence inside the master-key resolver: `STRATA_SSE_MASTER_KEYS` >
`STRATA_SSE_MASTER_KEY_VAULT` > `STRATA_SSE_MASTER_KEY_FILE` >
`STRATA_SSE_MASTER_KEY`.

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_SSE_MASTER_KEYS` | empty | `<id>:<hex64>,...` | Rotation provider (active = first). Required for `strata admin rewrap`. | `sse.master_keys` |
| `STRATA_SSE_MASTER_KEY` | empty | hex 64 | Single static master key. | `sse.master_key` |
| `STRATA_SSE_MASTER_KEY_ID` | empty | string | Identifier paired with the static key. | `sse.master_key_id` |
| `STRATA_SSE_MASTER_KEY_FILE` | empty | path | File-backed master key with mtime hot-reload. | `sse.master_key_file` |
| `STRATA_SSE_MASTER_KEY_VAULT` | empty | `<addr>:<transit-export-path>` | Vault Transit-export provider. | `sse.master_key_vault` |
| `STRATA_SSE_VAULT_ROLE_ID` | empty | string | AppRole role id (shared with `STRATA_KMS_VAULT_*`). | `kms.vault.role_id` |
| `STRATA_SSE_VAULT_SECRET_ID` | empty | string | AppRole secret id (shared with `STRATA_KMS_VAULT_*`). | `kms.vault.secret_id` |
| `STRATA_KMS_ADAPTER` | empty | `vault \| aws \| local_hsm` | Explicit SSE-KMS provider; empty falls back to auto-precedence (vault > aws > local_hsm). | `kms.adapter` |
| `STRATA_KMS_AWS_REGION` | empty | AWS region | Enables AWS KMS provider; SDK client factory must also be wired. | `kms.aws.region` |
| `STRATA_KMS_AWS_ENDPOINT` | empty | URL | Optional custom AWS KMS endpoint (LocalStack/moto). | `kms.aws.endpoint` |
| `STRATA_KMS_AWS_ROLE_ARN` | empty | ARN | Optional STS assume-role for KMS client credentials. | `kms.aws.role_arn` |
| `STRATA_KMS_LOCAL_HSM_SEED` | empty | hex 32 | Deterministic local HSM stand-in for tests. Internal — debug only. | `kms.local_hsm.seed` |
| `STRATA_KMS_VAULT_ADDR` | empty | URL | Vault Transit endpoint. | `kms.vault.address` |
| `STRATA_KMS_VAULT_PATH` | empty | string | Vault Transit mount path (e.g. `transit`). | `kms.vault.mount` |
| `STRATA_KMS_VAULT_TOKEN` | empty | string | Optional static Vault token; alternative to AppRole. | `kms.vault.token` |
| `STRATA_DEK_CACHE_TTL` | `5m` | Go duration ∈ [`30s`, `1h`] | TTL for the per-bucket signing-key DEK cache on the SigV4 hot path (US-001 auth-dx-trailer-lima). Plaintext DEK is zeroed via `subtle.ConstantTimeCopy` on eviction. Out-of-range values clamp + WARN. | `kms.dek_cache_ttl` |
| `STRATA_KEY_MAX_AGE` | `2160h` (90d) | Go duration ∈ [`24h`, `8760h`] | Max age for a per-bucket signing key before the SigV4 path rejects requests with `401 KeyExpired` (US-002 auth-dx-trailer-lima). Operator must `POST /admin/v1/buckets/{name}/signing-key/rotate` to recover. Default 90 days matches PCI-DSS / SOX rotation policy; out-of-range values clamp + WARN. | `auth.key_max_age` |
| `STRATA_KMS_DEFAULT_KEY_ID` | empty | KMS CMK handle | Default CMK applied by `POST /signing-key/rotate` when the operator omits `key_id` (US-002). Empty falls back to the bucket name (works for AWS KMS aliases + Vault Transit). | `kms.default_key_id` |

## GC worker (`--workers=gc`)

See [Best practices — GC & lifecycle tuning]({{< ref "/best-practices/gc-lifecycle-tuning" >}}).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_GC_INTERVAL` | `30s` | Go duration | Tick cadence. | `workers.gc.interval` |
| `STRATA_GC_GRACE` | `5m` | Go duration | Grace before a queued chunk is eligible for delete. | `workers.gc.grace` |
| `STRATA_GC_BATCH_SIZE` | `0` (backend default) | non-negative int | Per-tick batch cap; `0` uses the meta backend's preferred batch size. | `workers.gc.batch_size` |
| `STRATA_GC_CONCURRENCY` | `1` | positive int | Per-shard delete workers. | `workers.gc.concurrency` |
| `STRATA_GC_SHARDS` | `1` | `[1, 1024]` | Fan-out shard count (also drives lifecycle leader-replica selection). Out-of-range → clamped + WARN at boot. | `workers.gc.shards` |
| `STRATA_GC_METRICS_LISTEN` | `:9100` | host:port | Legacy GC metrics listener (subsumed by the main gateway exporter). | `workers.gc.metrics_listen` |

## Lifecycle worker (`--workers=lifecycle`)

See [Best practices — GC & lifecycle tuning]({{< ref "/best-practices/gc-lifecycle-tuning" >}}).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_LIFECYCLE_INTERVAL` | `60s` | Go duration | Tick cadence. | `workers.lifecycle.interval` |
| `STRATA_LIFECYCLE_UNIT` | `day` | `day \| hour` (test fixtures may inject other values) | Granularity for `Days`/`Date` evaluation. | `workers.lifecycle.unit` |
| `STRATA_LIFECYCLE_CONCURRENCY` | `1` | positive int | Per-tick transition/expire workers. | `workers.lifecycle.concurrency` |
| `STRATA_LIFECYCLE_METRICS_LISTEN` | `:9101` | host:port | Legacy lifecycle metrics listener. | `workers.lifecycle.metrics_listen` |

## Rebalance worker (`--workers=rebalance`)

See [Placement & rebalance]({{< ref "/best-practices/placement-rebalance" >}}#bandwidth-tuning).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_REBALANCE_INTERVAL` | `5m` | `[1m, 24h]` | Tick cadence; out-of-range → clamped. | `workers.rebalance.interval` |
| `STRATA_REBALANCE_RATE_MB_S` | `100` | `[1, 10000]` | Token-bucket budget (read+write share the same bucket). | `workers.rebalance.rate_mb_s` |
| `STRATA_REBALANCE_INFLIGHT` | `4` | `[1, 64]` | Per-Move errgroup bound. | `workers.rebalance.inflight` |
| `STRATA_REBALANCE_SHARDS` | `1` | `[1, 1024]` | Per-shard leader-elected fan-out count. | `workers.rebalance.shards` |

## Notify worker (`--workers=notify`)

See [Concepts — workers]({{< ref "/concepts/workers" >}}).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_NOTIFY_TARGETS` | empty | `type:arn=<url>\|<secret>,...` | Required when worker is enabled. | `workers.notify.targets` |
| `STRATA_NOTIFY_INTERVAL` | `5s` | Go duration | Poll cadence. | `workers.notify.interval` |
| `STRATA_NOTIFY_MAX_RETRIES` | `6` | non-negative int | Retry cap before DLQ. | `workers.notify.max_retries` |
| `STRATA_NOTIFY_BACKOFF_BASE` | `1s` | Go duration | Exponential-backoff base. | `workers.notify.backoff_base` |
| `STRATA_NOTIFY_POLL_LIMIT` | `100` | positive int | Per-poll row cap. | `workers.notify.poll_limit` |

## Replicator worker (`--workers=replicator`)

See [Concepts — workers]({{< ref "/concepts/workers" >}}).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_REPLICATOR_INTERVAL` | `5s` | Go duration | Poll cadence. | `workers.replicator.interval` |
| `STRATA_REPLICATOR_MAX_RETRIES` | `6` | non-negative int | Retry cap before DLQ. | `workers.replicator.max_retries` |
| `STRATA_REPLICATOR_BACKOFF_BASE` | `1s` | Go duration | Exponential-backoff base. | `workers.replicator.backoff_base` |
| `STRATA_REPLICATOR_POLL_LIMIT` | `100` | positive int | Per-poll row cap. | `workers.replicator.poll_limit` |
| `STRATA_REPLICATOR_HTTP_TIMEOUT` | `30s` | Go duration | Per-request peer HTTP timeout. | `workers.replicator.http_timeout` |
| `STRATA_REPLICATOR_PEER_SCHEME` | `https` | `http \| https` | Peer URL scheme. | `workers.replicator.peer_scheme` |

## Access-log worker (`--workers=access-log`)

See [Concepts — workers]({{< ref "/concepts/workers" >}}).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_ACCESS_LOG_INTERVAL` | `5m` | Go duration | Flush cadence. | `workers.access_log.interval` |
| `STRATA_ACCESS_LOG_MAX_FLUSH_BYTES` | `5242880` (5 MiB) | positive int64 | Per-flush bytes cap. | `workers.access_log.max_flush_bytes` |
| `STRATA_ACCESS_LOG_POLL_LIMIT` | `10000` | positive int | Per-poll row cap. | `workers.access_log.poll_limit` |

## Inventory worker (`--workers=inventory`)

See [Concepts — workers]({{< ref "/concepts/workers" >}}).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_INVENTORY_INTERVAL` | `5m` | Go duration | Tick cadence. | `workers.inventory.interval` |
| `STRATA_INVENTORY_REGION` | `deps.Region` fallback | string | Region tag for target-bucket writes. | `workers.inventory.region` |

## Audit-export worker (`--workers=audit-export`)

See [Best practices — compliance]({{< ref "/best-practices/compliance" >}}).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_AUDIT_EXPORT_BUCKET` | empty | bucket name | Required when worker is enabled. | `workers.audit_export.bucket` |
| `STRATA_AUDIT_EXPORT_PREFIX` | empty | object-key prefix | Optional path prefix inside the export bucket. | `workers.audit_export.prefix` |
| `STRATA_AUDIT_EXPORT_AFTER` | `720h` (30d) | Go duration | Drain partitions older than this. | `workers.audit_export.after` |
| `STRATA_AUDIT_EXPORT_INTERVAL` | `24h` | Go duration | Tick cadence. | `workers.audit_export.interval` |

## Manifest-rewriter worker (`--workers=manifest-rewriter`)

See [Concepts — workers]({{< ref "/concepts/workers" >}}).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_MANIFEST_REWRITER_INTERVAL` | `24h` | Go duration | Tick cadence. | `workers.manifest_rewriter.interval` |
| `STRATA_MANIFEST_REWRITER_BATCH_LIMIT` | `500` | positive int | Per-tick rewrite cap. | `workers.manifest_rewriter.batch_limit` |
| `STRATA_MANIFEST_REWRITER_DRY_RUN` | `false` | `true \| false` | Skip the write phase; log diffs only. | `workers.manifest_rewriter.dry_run` |

## Quota-reconcile worker (`--workers=quota-reconcile`)

See [Quotas + billing]({{< ref "/best-practices/quotas-billing" >}}#drift-reconcile-workersquota-reconcile).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_QUOTA_RECONCILE_INTERVAL` | `6h` | Go duration | Tick cadence. | `workers.quota_reconcile.interval` |

## Usage-rollup worker (`--workers=usage-rollup`)

See [Quotas + billing]({{< ref "/best-practices/quotas-billing" >}}#usage-rollup-workersusage-rollup).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_USAGE_ROLLUP_INTERVAL` | `24h` | Go duration | Tick cadence. | `workers.usage_rollup.interval` |
| `STRATA_USAGE_ROLLUP_AT` | `00:00` (UTC) | `HH:MM` | Anchor time for the daily rollup. | `workers.usage_rollup.at` |
| `STRATA_USAGE_ROLLUP_SAMPLES_PER_DAY` | `24` | positive int | Samples emitted per UTC day per (bucket, storage_class). | `workers.usage_rollup.samples_per_day` |

## Tracing (OpenTelemetry)

See [Tracing best-practices]({{< ref "/best-practices/tracing" >}}).

| Variable | Default | Range | Notes | TOML key |
|---|---|---|---|---|
| `STRATA_OTEL_EXPORTER_ENDPOINT` | empty | URL | Overrides `OTEL_EXPORTER_OTLP_ENDPOINT`. Empty + `ringbuf=false` → no-op tracer provider. | `otel.endpoint` |
| `STRATA_OTEL_SAMPLE_RATIO` | `0.01` | `[0, 1]` | Tail sampler ratio; failing spans always export. | `otel.sample_ratio` |
| `STRATA_OTEL_RINGBUF` | `on` | `on \| off` | In-process span ring buffer (powers `/admin/v1/diagnostics/trace`). | `otel.ringbuf` |
| `STRATA_OTEL_RINGBUF_BYTES` | `4194304` (4 MiB) | `[1 MiB, 1 GiB]` | Ring-buffer byte budget. | `otel.ringbuf_bytes` |

The standard W3C `OTEL_EXPORTER_OTLP_ENDPOINT` controls OTLP/HTTP export
(unset disables export entirely). `STRATA_OTEL_EXPORTER_ENDPOINT` /
`[otel].endpoint` take precedence when set.

## Retired / legacy

| Variable | Status | Notes | TOML key |
|---|---|---|---|
| `STRATA_DRAIN_STRICT` | Retired | US-007 made drain unconditionally strict. WARN-logged at boot if set; ignored. | — |
| `STRATA_S3_BACKEND_*` (`_ENDPOINT`, `_REGION`, `_BUCKET`, `_ACCESS_KEY`, `_SECRET_KEY`, `_FORCE_PATH_STYLE`, `_PART_SIZE`, `_UPLOAD_CONCURRENCY`, `_MAX_RETRIES`, `_OP_TIMEOUT_SECS`, `_MULTIPART_TIMEOUT_SECS`, `_SSE_MODE`, `_SSE_KMS_KEY_ID`) | Retired in `ralph/s3-multi-cluster` | Replaced by `STRATA_S3_CLUSTERS` + `STRATA_S3_CLASSES`. Boot fails loud if the legacy envs are still set without the JSON replacements. | — |

## Test-only

| Variable | Notes | TOML key |
|---|---|---|
| `STRATA_TIKV_TEST_PD_ENDPOINTS` | Operator-provided PD endpoints for TiKV integration tests (bypasses testcontainers). | — |
| `STRATA_SCYLLA_TEST` / `STRATA_SCYLLA_IMAGE` | Gates + image override for the ScyllaDB contract suite. | — |
| `STRATA_TEST_AK` / `STRATA_TEST_SK` | Static AK/SK pair consumed by the S3 multi-cluster contract suite via `CredentialsEnv`. | — |
| `STRATA_TEST_CEPH_CONF` / `STRATA_TEST_CEPH_POOL` / `STRATA_TEST_CEPH_CLASSES` | RADOS integration-test cluster wiring. | — |
| `STRATA_TEST_REBALANCE_SRC_POOL` / `STRATA_TEST_REBALANCE_TGT_POOL` | RADOS rebalance mover integration-test pools. | — |
