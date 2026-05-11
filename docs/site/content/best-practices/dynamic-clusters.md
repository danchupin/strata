---
title: 'Dynamic clusters'
weight: 45
description: 'RADOS cluster catalogue persisted in meta.Store, registered/de-registered via the admin API, hot-reloaded by every gateway replica — no restart for zero-downtime cluster add.'
---

# Dynamic clusters

The RADOS cluster set is persisted in `meta.Store` (the `cluster_registry`
table) and reconciled in-process by every gateway replica every
`STRATA_CLUSTER_REGISTRY_INTERVAL` (default 30 s, range `[5 s, 5 m]`).
Adding a new RADOS cluster is a single `POST /admin/v1/storage/clusters`
call — no gateway restart, no env-file edit, no rolling deploy.

The legacy `STRATA_RADOS_CLUSTERS` env path is fully retired. The
registry is the single source of truth.

## Catalogue shape

`ClusterRegistryEntry` lives in `internal/meta/store.go`:

| Field | Type | Notes |
|---|---|---|
| `ID` | `string` | Matches `[a-z0-9-]{1,64}`. Stable across the entry's lifetime; used by `ClassSpec.Cluster` to route per-storage-class writes. |
| `Backend` | `string` | `"rados"` or `"s3"`. The rados watcher filters non-rados rows; S3-backend consumer is a P2 follow-up. |
| `Spec` | `[]byte` | Opaque JSON. Decoded by the consuming backend — registry does not validate spec internals. |
| `CreatedAt` / `UpdatedAt` | `time.Time` | Server-stamped on every write. |
| `Version` | `int64` | Monotonic CAS counter. `PutCluster` is reject-on-stale (`ErrClusterVersionMismatch`); idempotent retries with the latest version succeed. |

For RADOS, the `Spec` decodes into `data.rados.ClusterSpec`:

```json
{
  "config_file": "/etc/ceph/ceph.conf",
  "user": "client.admin",
  "keyring": "/etc/ceph/ceph.client.admin.keyring",
  "pool": "strata.rgw.buckets.data",
  "namespace": ""
}
```

Pool + namespace are the per-cluster defaults; per-storage-class pools
still override via `[rados] classes` (see [Per-storage-class routing](#per-storage-class-routing)).

## Bootstrap workflow

The gateway starts with an empty registry — admin API + meta layer are
fully functional, but RADOS traffic fails fast with `unknown cluster`
until the first POST. There is **no env-seed fallback**, by design (one
source of truth, no two-source confusion at boot).

Operator's first task after `strata server` is up:

```bash
curl -sS -X POST http://localhost:8080/admin/v1/storage/clusters \
  -H 'Content-Type: application/json' \
  -b "$(cat strata-session.cookie)" \
  -d '{
    "id": "default",
    "backend": "rados",
    "spec": {
      "config_file": "/etc/ceph/ceph.conf",
      "user": "client.admin",
      "keyring": "/etc/ceph/ceph.client.admin.keyring",
      "pool": "strata.rgw.buckets.data"
    }
  }'
```

Within one poll interval (`STRATA_CLUSTER_REGISTRY_INTERVAL`, default 30 s)
every replica's watcher reconciles the new row into its in-memory map.
The next chunk PUT lazy-dials the cluster — no synchronous handshake on
the admin POST.

The smoke harness (`scripts/s3-tests/run.sh`) registers the test cluster
this way against a fresh stack; mirror the same shape in your CI bootstrap.

## Admin API

Three endpoints under `/admin/v1/storage/clusters`. OpenAPI contract:
`internal/adminapi/openapi.yaml`. Handlers stamp the audit log via
`s3api.SetAuditOverride` with `action="admin:{List,Create,Delete}Cluster"`
and `resource="cluster:<id>"`.

| Endpoint | Body | Response |
|---|---|---|
| `GET /admin/v1/storage/clusters` | — | `200 [{id, backend, spec, created_at, updated_at}]` sorted by id asc. |
| `POST /admin/v1/storage/clusters` | `{id, backend, spec}` | `201 ClusterEntry` on insert; `400` on validation; `409 ClusterAlreadyExists`. |
| `DELETE /admin/v1/storage/clusters/{id}` | — | `204` on drop; `404 NoSuchCluster`; `409 ClusterReferenced {referenced_by: ["STANDARD", "COLD"]}` if a storage class still routes at this cluster. |

POST validation is format-only — no probe-dial:

- `id` matches `[a-z0-9-]{1,64}`
- `backend ∈ {rados, s3}`
- `spec` is non-empty, well-formed JSON

Use the admin web console (`/console/`) for the same flow if you prefer
clicks over curl. The console reads the registry through the same
endpoints.

### List

```bash
curl -sS http://localhost:8080/admin/v1/storage/clusters \
  -b "$(cat strata-session.cookie)" | jq
```

### Register

```bash
curl -sS -X POST http://localhost:8080/admin/v1/storage/clusters \
  -H 'Content-Type: application/json' \
  -b "$(cat strata-session.cookie)" \
  -d '{"id":"cold","backend":"rados","spec":{"config_file":"/etc/ceph/cold.conf","user":"client.cold","keyring":"/etc/ceph/cold.keyring","pool":"strata.rgw.cold.data"}}'
```

### De-register (with drain semantics)

```bash
curl -sS -X DELETE http://localhost:8080/admin/v1/storage/clusters/cold \
  -b "$(cat strata-session.cookie)"
```

If any storage class still routes here (`ClassSpec.Cluster=="cold"`),
the handler refuses with `409 ClusterReferenced` and names the offending
classes:

```json
{
  "code": "ClusterReferenced",
  "message": "Cluster still referenced by one or more storage classes",
  "referenced_by": ["COLD", "GLACIER"]
}
```

Re-map the classes (edit `[rados] classes` in `app.toml`, restart that
replica, or stage the change across a rolling deploy), then retry the
DELETE. Note: **the handler does not chase chunk-level references** —
manifests still pointing at the cluster's pool are the operator's
responsibility. A `rebalance` worker that copies chunks across clusters
is filed as a P2 follow-up; until then, drain via re-mapping classes +
waiting for lifecycle / GC to roll over.

## Watcher behaviour

`internal/data/rados/registry_watcher.go` runs in every replica — there
is no leader. Set-diff is idempotent so concurrent reconciliations
converge safely.

| Event | Watcher action |
|---|---|
| **Add** | Merge spec into `Backend.clusters` under `b.mu`. `connFor(ctx, id)` lazy-dials on next traffic. |
| **Remove** | Close cached `*rados.Conn` + all `<id>|<pool>` ioctxes under `b.mu`, remove from map. In-flight ops keep their snapshot reference; new ops fail with `unknown cluster`. |
| **Update** | Same id, bumped Version → replace + re-dial on next traffic (treated as remove + add). |

Initial sync runs synchronously in `rados.New(cfg)` (5 s timeout) so the
gateway boot has a populated map before traffic flows. If the registry
is empty at boot, the gateway starts cleanly — admin API + meta layer
work; RADOS traffic fails fast until the first POST.

## Per-storage-class routing

`[rados] classes` in `app.toml` is unchanged. Each `ClassSpec.Cluster`
names a registry id; the gateway resolves the id against the live
`Backend.clusters` map at request time:

```toml
[rados.classes.STANDARD]
cluster   = "default"
pool      = "strata.rgw.buckets.data"

[rados.classes.COLD]
cluster   = "cold"
pool      = "strata.rgw.cold.data"
namespace = ""
```

A class whose `cluster` does not (yet) match a registry id surfaces
`unknown cluster` on traffic. Best practice: POST the registry entry
first, then redeploy the class config. A per-class admin write API is a
P3 follow-up — today the class map is static config.

## Audit + observability

| Signal | Where | Use |
|---|---|---|
| `strata_cluster_registry_changes_total{op="add"|"remove"|"update"}` | Prometheus default registry | Alert on unexpected churn (drift, drift-correction loops). |
| `audit_log` rows | `meta.Store.ListAudit` | Every Create / Delete admin call stamps `action=admin:{Create,Delete}Cluster`, `resource=cluster:<id>`. |
| `request_id`-bound logs | `internal/logging` JSON output | Watcher tick lines tagged `component=registry-watcher`. |

See [Monitoring]({{< ref "/best-practices/monitoring" >}}) for the global
alert set + audit-log retention.

## Common pitfalls

- **POSTing before the gateway is up.** `/admin/v1/*` is only available
  once the gateway listens on the admin port. Bootstrap scripts should
  wait on `/healthz` first.
- **Pool referenced by a class but never created on the RADOS side.**
  Format-only validation cannot catch this — chunk PUT errors out at
  traffic time with `pool not found`. Pre-create pools via
  `ceph osd pool create` on the cluster you register.
- **Deleting a cluster while a manifest still references its OIDs.**
  The DELETE-when-referenced check is class-level only — it does NOT
  scan manifests. Old objects whose chunks live on the dropped cluster
  surface ENOENT on GET. Drain via re-mapping classes + waiting for
  lifecycle / GC, or hold the entry until a future `rebalance` worker
  migrates the chunks.
- **Re-using an id with a different backend.** Update-in-place via
  `PutCluster` bumps `Version`; the watcher treats it as remove + add
  on the next tick. To swap a `rados` id for an `s3` id, DELETE then
  POST — keeps the audit trail clean.

## See also

- [Storage status]({{< ref "/architecture/storage" >}}) — Meta + Data
  tab surfacing health of each registered cluster.
- [Monitoring]({{< ref "/best-practices/monitoring" >}}) — alert set,
  audit-log shape.
- [Architecture — Data backend]({{< ref "/architecture/data-backend" >}})
  — `connFor` lazy-dial + ioctx cache.
- `internal/adminapi/openapi.yaml` — canonical Admin-API contract for
  `/storage/clusters` (and everything else).
