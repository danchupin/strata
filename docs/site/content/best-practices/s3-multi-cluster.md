---
title: 'S3 multi-cluster routing'
weight: 70
description: 'Env-driven multi-cluster S3 data backend — STRATA_S3_CLUSTERS / STRATA_S3_CLASSES JSON shape, credentials_ref discriminator, per-class bucket routing, rolling-restart workflow for cluster add / remove, fail-fast credential validation at boot.'
---

# S3 multi-cluster routing

The S3 data backend supports routing per storage class to a distinct
`(cluster, bucket)` pair. Two envs hold the full config — `STRATA_S3_CLUSTERS`
(JSON array of bucket-less cluster specs) and `STRATA_S3_CLASSES` (JSON object
mapping storage class names to `{cluster, bucket}` tuples). Adding or removing
a cluster requires a gateway restart; multi-replica deployments hide
per-instance downtime via rolling restart.

This page is the operator guide. For the conceptual S3 backend overview see
[S3 data backend]({{< ref "/architecture/backends/s3" >}}).

## Env shape

### `STRATA_S3_CLUSTERS` — JSON array

Each entry describes one S3 endpoint; buckets live on the class entries, not
here. Two classes may share one cluster but route to different buckets.

```json
[
  {
    "id": "primary",
    "endpoint": "https://s3.us-east-1.amazonaws.com",
    "region": "us-east-1",
    "force_path_style": false,
    "part_size": 16777216,
    "upload_concurrency": 4,
    "max_retries": 5,
    "op_timeout_secs": 30,
    "sse_mode": "passthrough",
    "sse_kms_key_id": "",
    "credentials": {"type": "chain"}
  },
  {
    "id": "cold-eu",
    "endpoint": "https://s3.eu-west-1.amazonaws.com",
    "region": "eu-west-1",
    "credentials": {"type": "env", "ref": "COLD_EU_AK:COLD_EU_SK"}
  }
]
```

Field summary:

| Field                | Required | Notes                                                          |
| -------------------- | -------- | -------------------------------------------------------------- |
| `id`                 | yes      | Cluster identifier. Referenced from `STRATA_S3_CLASSES`. Must be unique. |
| `endpoint`           | yes      | `https://host[:port]` for AWS / MinIO / Ceph RGW / Garage / Wasabi / B2-S3. |
| `region`             | yes      | AWS region name or any string for non-AWS endpoints.           |
| `force_path_style`   | no       | `true` for MinIO / Ceph RGW. Defaults to `false` (virtual-hosted style). |
| `part_size`          | no       | Multipart part size in bytes. Defaults to 16 MiB.              |
| `upload_concurrency` | no       | Concurrent multipart part uploads. Defaults to 4.              |
| `max_retries`        | no       | SDK-level retry budget. Defaults to 5.                         |
| `op_timeout_secs`    | no       | Per-op SDK timeout. Defaults to 30 s.                          |
| `sse_mode`           | no       | `passthrough` (default), `AES256`, or `aws:kms`.               |
| `sse_kms_key_id`     | no       | KMS key id (alias / ARN) when `sse_mode == "aws:kms"`.         |
| `credentials`        | yes      | `CredentialsRef` envelope — see next section.                  |

### `STRATA_S3_CLASSES` — JSON object

Each storage class maps to one `(cluster, bucket)` pair. Both `cluster` and
`bucket` are REQUIRED — there is no `DefaultCluster` fallback.

```json
{
  "STANDARD":  {"cluster": "primary",  "bucket": "hot-tier"},
  "STANDARD_IA": {"cluster": "primary", "bucket": "warm-tier"},
  "GLACIER":   {"cluster": "cold-eu",  "bucket": "glacier-archive"}
}
```

Validation runs at boot. Failures bubble up from `s3.New` — the gateway
refuses to start with a descriptive error in `journalctl` / `kubectl logs`:

- duplicate cluster `id` → error
- empty `id` / `endpoint` / `region` → error
- unknown `credentials.type` → error
- class references unknown cluster id → error
- empty `cluster` / `bucket` on a class → error
- `credentials.type == "env"` and named env vars are not set → error
- `credentials.type == "file"` and file path does not exist → error

## `credentials_ref` discriminator

Credentials are never stored plaintext in the cluster spec. The
`credentials` envelope is a discriminator with three shapes:

| `type`   | `ref` field                  | Resolution                                                                                       |
| -------- | ---------------------------- | ------------------------------------------------------------------------------------------------ |
| `chain`  | empty                        | AWS SDK default credential chain — env (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`), `~/.aws/credentials`, IRSA web-identity, EC2 instance metadata. |
| `env`    | `"ACCESS_KEY_VAR:SECRET_KEY_VAR"` | Read the two named env vars at SDK client-build time. Boot rejects if either is unset.        |
| `file`   | `"/path/to/credentials[:profile]"` | `awsconfig.LoadSharedConfigProfile` with parsed `path:profile`; profile defaults to `default`. |

Examples:

```json
{"type": "chain"}
{"type": "env",  "ref": "PRIMARY_AK:PRIMARY_SK"}
{"type": "file", "ref": "/etc/strata/creds:primary"}
```

The `chain` resolver is intentionally NOT probed at boot — IMDS / IRSA
round-trips at startup break in environments where the chain is resolved
lazily by the SDK. The `env` and `file` resolvers ARE probed at boot
(env-var presence check + `os.Stat` respectively). Plan accordingly: an
IRSA-mis-bound EKS pod surfaces the error at first connect, not at boot.

## Per-class routing

Every data-plane method (`Put` / `Get` / `Delete` / `Head` / `Copy` / `List` /
`PutChunks` / `GetChunks` / `Presign` / multipart) resolves the target
cluster + bucket via the class on the request:

1. Lifecycle worker sets `Object.StorageClass = "GLACIER"`.
2. Gateway calls `Backend.Delete(m)` where `m.Class = "GLACIER"`.
3. `Backend.resolveClass("GLACIER")` returns `(cluster=cold-eu, bucket=glacier-archive)`.
4. `Backend.connFor("cold-eu")` lazy-builds the SDK client + uploader on
   first use (cached for the lifetime of the process).
5. The DELETE is dispatched against `cold-eu`'s endpoint, bucket
   `glacier-archive`.

Per-cluster SSE knobs (`sse_mode`, `sse_kms_key_id`) come from the cluster
spec — not from a top-level setting. Two classes routed to the same cluster
share SSE config.

## Bootstrap workflow

Set both envs in your systemd unit / Kubernetes ConfigMap and start the
gateway:

```bash
export STRATA_DATA_BACKEND=s3
export STRATA_S3_CLUSTERS='[{"id":"primary","endpoint":"https://s3.us-east-1.amazonaws.com","region":"us-east-1","credentials":{"type":"chain"}}]'
export STRATA_S3_CLASSES='{"STANDARD":{"cluster":"primary","bucket":"strata-hot"}}'
strata server
```

If you forget `STRATA_S3_CLASSES`, the gateway refuses to start with:

```
config: STRATA_S3_CLASSES required when STRATA_DATA_BACKEND=s3
```

If a class references a cluster id not present in `STRATA_S3_CLUSTERS`,
the gateway refuses to start with:

```
class "GLACIER": cluster "cold-eu" not in STRATA_S3_CLUSTERS
```

## Adding or removing a cluster

The cluster set is restart-only by design — a prior dynamic cluster
registry attempt (`ralph/dynamic-clusters`) was reverted on 2026-05-11
because static env config is simpler operationally + tests are easier
when config is static. Add a cluster by:

1. Update the env-source (systemd unit / k8s ConfigMap / Helm values) on
   one replica.
2. Restart that replica — load balancer drains it during the restart,
   so no client-visible downtime.
3. Repeat for each replica in turn (rolling restart).
4. Repoint a storage class to the new cluster by updating
   `STRATA_S3_CLASSES` and rolling-restarting again.

Removing a cluster follows the inverse order: first re-route every class
away from it, then drop the cluster entry, rolling-restart each time. Data
already stored on the retired cluster is **not migrated** by this cycle
— chunk-side rebalance is tracked separately by the P2 "Per-bucket
placement policy + cross-cluster rebalance worker" entry on `ROADMAP.md`.

## What's NOT supported (yet)

- **Dynamic registry / admin API.** Cluster set is env-only. (Decision
  2026-05-11 — `ralph/dynamic-clusters` cycle reverted; static env config
  is simpler operationally + tests are easier when config is static.)
- **Chunk-side data migration between clusters.** When you re-route a
  class, existing objects keep their `BackendRef` pointing at the old
  cluster. The class-routed delete path keys off `m.Class`, but reads
  follow `BackendRef`. Rebalance worker is a separate P2 follow-up.
- **KMS-fetched cluster credentials.** P3 follow-up. Use the AWS-side
  IRSA / EC2 instance metadata path via `{"type":"chain"}` instead.
- **Per-class SSE.** SSE config is per-cluster. If two classes need
  different SSE keys, put them on separate clusters even if they share
  an endpoint.

## See also

- [S3 data backend]({{< ref "/architecture/backends/s3" >}}) — conceptual
  overview, capability matrix, backend bucket configuration.
- [Multi-replica deploy]({{< ref "/deploy/multi-replica" >}}) — rolling
  restart sequencing.
