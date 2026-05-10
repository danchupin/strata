---
title: 'Binary consolidation'
weight: 10
description: 'Migrating from the eleven `cmd/*` binaries to `strata` + `strata-admin`.'
---

# Migrating to the unified `strata` binary

Strata used to ship eleven `cmd/*` binaries — one gateway plus a long tail of
single-purpose worker daemons. The codebase now ships exactly two:

- `strata` — the gateway plus every long-running background worker, selected
  at startup via `STRATA_WORKERS=` (or `--workers=`).
- `strata-admin` — the operator CLI for one-shot maintenance tasks.

This guide is the operator-facing checklist for upgrading an existing deploy.
There is no schema migration, no data migration, and no env-var rename — the
move is purely a packaging change.

## Old binary → new invocation

| Old binary                        | New invocation                                    |
| --------------------------------- | ------------------------------------------------- |
| `cmd/strata-gateway`              | `strata server`                                   |
| `cmd/strata-gc`                   | `strata server --workers=gc`                      |
| `cmd/strata-lifecycle`            | `strata server --workers=lifecycle`               |
| `cmd/strata-notify`               | `strata server --workers=notify`                  |
| `cmd/strata-replicator`           | `strata server --workers=replicator`              |
| `cmd/strata-access-log`           | `strata server --workers=access-log`              |
| `cmd/strata-inventory`            | `strata server --workers=inventory`               |
| `cmd/strata-audit-export`         | `strata server --workers=audit-export`            |
| `cmd/strata-manifest-rewriter`    | `strata server --workers=manifest-rewriter`       |
| `cmd/strata-rewrap`               | `strata-admin rewrap`                             |

`STRATA_WORKERS` accepts a comma-separated list, deduplicated and validated at
startup. Unknown names exit with code 2 before any backend is built. `--workers=`
takes precedence over the env var. An empty list runs the gateway only.

Multiple workers in the same process is supported and intended — each worker
holds its own leader-election lease (`<name>-leader`) keyed by name, so two
replicas configured with `STRATA_WORKERS=gc,lifecycle` will only run one active
gc and one active lifecycle between them.

## Env-var renames

**None.** Every per-worker `STRATA_*` knob keeps its old spelling:

- `STRATA_GC_INTERVAL`, `STRATA_GC_GRACE`, `STRATA_GC_BATCH_SIZE`,
  `STRATA_GC_CONCURRENCY` (Phase 1 fan-out, default `1`, max `256` —
  see [GC + Lifecycle scaling]({{< ref "/architecture/benchmarks/gc-lifecycle" >}})),
  `STRATA_GC_SHARDS` (Phase 2 multi-leader, default `1`, range `[1, 1024]` —
  shards the gc-leader lease space across replicas; see
  [GC + lifecycle Phase 2]({{< ref "/architecture/migrations/gc-lifecycle-phase-2" >}}) for the cutover runbook).
  Lifecycle inherits the same value via the per-bucket distribution gate;
  there is no separate `STRATA_LIFECYCLE_SHARDS`.
- `STRATA_GC_DUAL_WRITE` (Phase 2 cutover, default `on`) — controls
  dual-writes to the legacy gc queue (Cassandra `gc_queue` /
  TiKV `gc/<region>/<oid>`) alongside the v2 partition / prefix. Operator
  flips to `off` after the legacy data has drained. See
  [GC + lifecycle Phase 2]({{< ref "/architecture/migrations/gc-lifecycle-phase-2" >}}).
- `STRATA_LIFECYCLE_INTERVAL`, `STRATA_LIFECYCLE_UNIT`,
  `STRATA_LIFECYCLE_CONCURRENCY` (same shape as
  `STRATA_GC_CONCURRENCY`)
- `STRATA_NOTIFY_TARGETS`, `STRATA_NOTIFY_*`
- `STRATA_REPLICATOR_*`
- `STRATA_ACCESS_LOG_*`
- `STRATA_INVENTORY_*`
- `STRATA_AUDIT_EXPORT_*`, `STRATA_AUDIT_RETENTION`
- `STRATA_MANIFEST_REWRITER_INTERVAL`

The same applies to gateway-scoped envs (`STRATA_LISTEN`, `STRATA_AUTH_MODE`,
`STRATA_VHOST_PATTERN`, `STRATA_LOG_LEVEL`, all `STRATA_CASSANDRA_*` and
`STRATA_RADOS_*` settings, `OTEL_EXPORTER_OTLP_ENDPOINT`). Set them on the one
container as you used to set them across the per-worker containers.

The four gateway-scoped envs have matching cross-cutting flags on `strata
server` — `--listen`, `--auth-mode`, `--vhost-pattern`, `--log-level`. Flag
overrides env which overrides defaults.

## Kubernetes manifest delta

Before — N Deployments, one per worker:

```yaml
# strata-gateway.yaml
kind: Deployment
metadata: { name: strata-gateway }
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: strata-gateway
          image: strata:<sha>
          command: ["/usr/local/bin/strata-gateway"]
---
# strata-gc.yaml
kind: Deployment
metadata: { name: strata-gc }
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: strata-gc
          image: strata:<sha>
          command: ["/usr/local/bin/strata-gc"]
---
# strata-lifecycle.yaml ... strata-notify.yaml ... strata-replicator.yaml ...
# (one Deployment per worker, each with its own command, each shipping the
# same image)
```

After — one Deployment, workers selected by env:

```yaml
kind: Deployment
metadata: { name: strata }
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: strata
          image: strata:<sha>
          # ENTRYPOINT is /usr/local/bin/strata, default CMD is ["server"].
          # No command override is needed unless you want explicit flags.
          env:
            - name: STRATA_WORKERS
              value: "gc,lifecycle"
            - name: STRATA_NOTIFY_TARGETS
              value: "https://hooks.internal/strata"
          ports:
            - containerPort: 9000
          readinessProbe:
            httpGet: { path: /readyz, port: 9000 }
          livenessProbe:
            httpGet: { path: /healthz, port: 9000 }
```

Notes:

- `replicas: 3` is fine even for the worker subset — each worker leader-elects
  on its own lease, so only one replica is the active gc / lifecycle / etc.
  at a time. The other replicas serve gateway traffic and stand by to take
  over the lease on shutdown or pod eviction.
- Splitting a "feature" replica with the wider worker set (notify, replicator,
  access-log, inventory, audit-export) onto a second Deployment is supported
  and is the shape used by the default `docker compose` stack:

  ```yaml
  kind: Deployment
  metadata: { name: strata-features }
  spec:
    replicas: 1
    template:
      spec:
        containers:
          - name: strata
            image: strata:<sha>
            env:
              - name: STRATA_WORKERS
                value: "notify,replicator,access-log,inventory,audit-export"
  ```

  The leases are independent of the Deployment grouping — running `gc` on
  both Deployments would still elect a single leader.
- Delete the per-worker Deployments (`strata-gc`, `strata-lifecycle`,
  `strata-notify`, `strata-replicator`, `strata-access-log`,
  `strata-inventory`, `strata-audit-export`, `strata-manifest-rewriter`,
  `strata-rewrap`) once the consolidated Deployment is healthy. The
  `strata-rewrap` Deployment in particular should not survive — rewrap is
  now a one-shot Job.

The `strata-admin rewrap` workflow becomes a Kubernetes `Job`:

```yaml
kind: Job
metadata: { name: strata-rewrap-2026-04 }
spec:
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: strata-admin
          image: strata:<sha>
          command: ["/usr/local/bin/strata-admin", "rewrap"]
          args: ["--target-key-id", "alias/strata-2026"]
          env:
            - name: STRATA_CONFIG_FILE
              value: /etc/strata/strata.toml
```

Resumption is automatic — `strata-admin rewrap` reads `rewrap_progress` rows
from the metadata store and skips already-rewrapped objects. Re-running the
Job is safe.

## Dashboards and alerts

**Metric names did not change.** The Prometheus surface is identical to what
the per-binary deploy exposed:

- `strata_gc_*` (queue depth, enqueue/processed counters)
- `strata_lifecycle_*` (tick total, transitions/expirations)
- `strata_notify_*` (delivery total)
- `strata_replicator_*`, `strata_access_log_*`, `strata_inventory_*`,
  `strata_audit_export_*`, `strata_manifest_rewriter_*`
- Gateway HTTP histograms / counters

Existing Grafana panels and Prometheus alerts continue to work without edits.

**One new metric** is exposed by the unified binary:

- `strata_worker_panic_total{worker="<name>"}` — counter, increments when a
  worker goroutine panics and the supervisor catches it. Alert on a non-zero
  rate per worker; a sustained increase means the worker is panic-restarting
  on its exponential backoff (1s → 5s → 30s → 2m). The gateway and sibling
  workers are unaffected, but the worker itself is making no progress until
  the panic stops reproducing.

Suggested alert (Prometheus):

```yaml
- alert: StrataWorkerPanicLoop
  expr: increase(strata_worker_panic_total[10m]) > 3
  for: 10m
  labels: { severity: warning }
  annotations:
    summary: "strata worker {{ $labels.worker }} is panic-restarting"
    description: |
      Worker {{ $labels.worker }} caught >3 panics in 10m. Inspect the pod
      logs for the stack trace and the last released lease.
```

If your scrape configuration discovered targets by per-worker Deployment
name (e.g. `kubernetes_sd_configs` filtered on `app=strata-gc`), replace
the filter with the consolidated Deployment label (`app=strata`) plus a
`STRATA_WORKERS` annotation if you want to differentiate by enabled worker
set. Worker identity is carried in the `worker=` metric label, not the
target identity, so a single scrape job is sufficient.

## Pre-cutover checklist

1. Build the unified image (`make docker-build` or your own pipeline).
2. Roll the gateway Deployment first with `STRATA_WORKERS=` empty (gateway
   only). Confirm `/readyz` and S3 traffic.
3. Add `STRATA_WORKERS=gc,lifecycle` to one replica (or a sibling
   Deployment), confirm the per-worker metrics are non-zero and the leader
   lease shows in the metadata store.
4. Repeat for the feature workers (notify / replicator / access-log /
   inventory / audit-export / manifest-rewriter) on a second replica or
   sibling Deployment.
5. Delete the legacy per-binary Deployments and their associated Services
   (none of them exposed traffic; they were daemons-only).
6. Run `strata-admin rewrap` as a Job the next time you rotate the SSE
   master key — the legacy `strata-rewrap` Deployment is no longer needed.

## Non-Goals

The two-binary rule (`strata` + `strata-admin`) covers the production
artifacts that ship in the runtime image. Developer / CI tools live
under `cmd/` too but are explicitly out of scope for the consolidation
goal:

- **`cmd/strata-racecheck`** — the duration-bounded race-harness driver
  used by `make race-soak` and the nightly CI workflow. It is built from
  the standard `make build` target so operators can package it
  alongside `strata` for soak runs, but it is not a daemon, has no
  `/readyz`, and is never deployed as part of a production stack. It
  does not count as a third production binary.

When adding a similar developer/CI tool in the future, document the
exception here so the rule stays explicit and reviewers don't have to
re-derive intent from `cmd/` directory contents.
