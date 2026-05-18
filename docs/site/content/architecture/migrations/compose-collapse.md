---
title: 'Compose collapse'
weight: 30
description: 'Single-strata compose: collapsed parallel single-cluster + multi-cluster services into one canonical multi-cluster service.'
---

# Migrating to the single-strata compose shape

`docker compose up -d` used to bring up two strata services in parallel on
the same Cassandra metadata: a single-cluster `strata` (port 9999, knew
only the `default` RADOS cluster) and a profile-gated `strata-multi`
(port 9998, knew `default` + `cephb`). Both raced for the **same**
global worker leases (`gc-leader-N`, `rebalance-leader-N`,
`lifecycle-leader-<bucketID>`, etc.) on Cassandra. Whichever container
started first won — and when the single-cluster winner popped a GC
entry for `cephb`, deletion failed forever (no cephb connection), the
queue piled up, `deregister_ready` stuck `false`, and drain wedged.

The fix is structural: there is now exactly **one** strata service in
the canonical compose stack. The single-cluster `strata` service block,
the `strata-features` sidecar, and the `multi-cluster` profile gate are
removed. The surviving service is named `strata`, listens on `:9999`,
attaches to BOTH `default` + `cephb` RADOS clusters by default, and is
always-on (no profile flag). Single-cluster smoke is now an env override
at runtime, not a separate compose service.

This guide is the operator-facing checklist for upgrading an existing
lab. There is no schema migration, no data migration, and no env-var
rename — the move is purely a compose-shape change.

## What was removed

| Removed                              | Reason                                                                                       |
| ------------------------------------ | -------------------------------------------------------------------------------------------- |
| `strata` (single-cluster) service    | Raced with `strata-multi` for the same Cassandra worker leases — root cause of the wedge.    |
| `strata-multi` service               | Renamed to `strata`; multi-cluster is now the canonical default shape.                       |
| `strata-features` service            | Folded into the single `strata` via `STRATA_WORKERS=` opt-in (feature workers as needed).    |
| `multi-cluster` compose profile      | No service references it — multi-cluster is now always-on.                                   |
| `features` compose profile           | Renamed to `webhook-trap` (the only remaining purpose was the CI artifact trap).             |
| Host port `9998`                     | Bare strata now exclusively at `:9999`. `strata-tikv` still uses `:9998` (independent shape).|

## What replaced the dual-service shape

```yaml
# deploy/docker/docker-compose.yml — single canonical strata service.
services:
  strata:
    container_name: strata
    image: strata:ceph
    depends_on:
      cassandra: { condition: service_healthy }
      ceph:      { condition: service_healthy }
      ceph-b:    { condition: service_healthy }
    environment:
      STRATA_WORKERS: ${STRATA_WORKERS:-gc,lifecycle,rebalance}
      STRATA_RADOS_CLUSTERS: ${STRATA_RADOS_CLUSTERS:-default:/etc/ceph-a/ceph.conf:/etc/ceph-a/ceph.client.admin.keyring,cephb:/etc/ceph-b/ceph.conf:/etc/ceph-b/ceph.client.admin.keyring}
      # … (full env in deploy/docker/docker-compose.yml)
    ports:
      - "9999:9000"
    volumes:
      - strata-ceph-etc:/etc/ceph:ro
      - strata-ceph-etc:/etc/ceph-a:ro
      - strata-cephb-etc:/etc/ceph-b:ro
```

`ceph-b` lost its `multi-cluster` profile gate and is now an
always-on dependency of `strata`. `make up-all` (which runs
`docker compose up -d`) brings up the multi-cluster stack with no
flag.

## Operator recipes

### Multi-cluster smoke (canonical default)

```bash
docker compose up -d                 # cassandra + ceph + ceph-b + strata
make wait-cassandra && make wait-ceph
make smoke                           # round-trip a PUT/GET against :9999
```

### Single-cluster smoke (env-override only)

There is **no separate compose service** for single-cluster smokes any
more. Override `STRATA_RADOS_CLUSTERS` at the host before bringing up
the bare strata service:

```bash
STRATA_RADOS_CLUSTERS=default:/etc/ceph-a/ceph.conf:/etc/ceph-a/ceph.client.admin.keyring \
  docker compose up -d strata
```

The gateway boots with one RADOS connection. Note that the
`cluster_state` row for `cephb` (if persisted from a prior multi-cluster
run) remains — Strata's reconcile path treats absence-from-env as
"leave the row alone" (no auto-removal). Operators inspecting the
single-cluster smoke should look at the strata container log for
exactly one cluster connection entry rather than relying on the
admin `clusters` listing.

### Feature workers (notify / replicator / access-log / inventory / audit-export)

The `strata-features` sidecar is gone. Opt into feature workers via
`STRATA_WORKERS=` on the single `strata` container:

```bash
STRATA_WORKERS=gc,lifecycle,rebalance,notify,replicator,access-log,inventory,audit-export \
  docker compose up -d strata
```

Lease distribution is unchanged — each worker leader-elects on its own
lease keyed by name (`<worker>-leader[-<shard>]`); a single replica
runs all elected leaders, additional replicas stand by.

### Multi-replica HA validation — `lab-cassandra-3` profile

The new `lab-cassandra-3` profile mirrors `lab-tikv-3` on the Cassandra
metadata backend: 3 named strata replicas (`strata-cass-a/b/c`,
container_name preserved) sharing one Cassandra, fronted by an nginx LB
on host port `10000`. Used to exercise multi-leader worker-lease
distribution (`gc-leader-N`, `rebalance-leader-N`,
`lifecycle-leader-<bucketID>`).

```bash
docker compose stop strata               # avoid bare strata racing the lab
make up-lab-cassandra-3                  # cassandra + ceph + ceph-b + 3 replicas + LB
```

The default `strata` service at `:9999` stays online unless explicitly
stopped — leaving both up reintroduces the lease race the compose
collapse removed. Always `docker compose stop strata` before bringing
the lab up.

| Profile                    | Replicas (strata)                       | Host ports        | Use case                            |
| -------------------------- | --------------------------------------- | ----------------- | ----------------------------------- |
| _(no profile)_             | 1 (`strata`)                            | 9999              | Canonical bare bring-up             |
| `lab-tikv`                 | 2 (`strata-tikv-a/b`)                   | 9999 via nginx    | TiKV-backed 2-replica lab           |
| `lab-tikv-3`               | 3 (`strata-tikv-a/b/c`)                 | 9999 via nginx    | TiKV-backed 3-replica lab           |
| `lab-cassandra-3` _(new)_  | 3 (`strata-cass-a/b/c`)                 | 10000 via nginx   | Cassandra-backed 3-replica lab      |

Per-replica host ports `10001` / `10002` / `10003` poke individual
replicas; the LB at `10000` round-robins.

## End-to-end smoke

The full structural cutover is gated by
`scripts/smoke-compose-collapse.sh`
(`make smoke-compose-collapse`). Four scenarios:

- **A — bare bring-up:** assert canonical 4-service stack
  (`cassandra + ceph + ceph-b + strata`), assert no legacy
  `strata-multi` / `strata-features` containers, PUT 10 objects via
  `:9999` on a split-placement bucket and round-trip GET them, drain
  `cephb` evacuate, assert `deregister_ready=true` within
  `DRAIN_TIMEOUT_S`.
- **B — single-cluster env-override visibility:** assert
  `/admin/v1/clusters` lists the canonical two clusters, PUT + GET
  round-trip on a default-pinned bucket. The env-override path is a
  documented operator recipe (no separate compose service to test).
- **C — `lab-cassandra-3` multi-replica:** assert the 3 cassandra-backed
  replicas + LB running with bare strata stopped, PUT 30 objects via
  the LB at `:10000`, inspect `worker_locks` via `cqlsh` and assert
  ≥2 of 3 replicas hold ≥1 `gc-leader-*` / `rebalance-leader-*`
  lease (distribution sanity), drain `cephb` evacuate via the LB,
  assert `deregister_ready=true`.
- **D — residue grep gate:** assert zero matches for `strata-multi` /
  `--profile multi-cluster` / `--profile features` / `strata-features`
  outside the documented exception set (Hugo build output, frozen
  Ralph cycle snapshots, ROADMAP close-flip narratives, this smoke
  script itself). `:9998` filtered through the strata-tikv exception
  subset (TiKV-backed gateway legitimately uses 9998).

Scenarios A + B + C skip with exit 77 when the relevant compose
stack is not reachable after `WAIT_GRACE` seconds. Set `REQUIRE_LAB=1`
to convert the skip into a hard fail. Scenario D always runs.

## Follow-up — Scope worker leases per-cluster

The compose collapse removes the **trigger** for the lease race (one
strata service = no parallel containers to fight). The **structural**
fix — scoping worker leases per-cluster (`gc-leader-<cluster>-N`,
`rebalance-leader-<cluster>-N`) — is tracked as a P3 follow-up on
the roadmap. Per-cluster scoping would prevent the wrong worker from
winning a lease for a cluster it doesn't service if multi-strata is
ever reintroduced (e.g. one strata per tenant or per region sharing
one Cassandra).

## See also

- [Placement + rebalance]({{< ref "/best-practices/placement-rebalance" >}})
  — multi-cluster drain workflow + multi-leader scaling.
- [Binary consolidation]({{< ref "/architecture/migrations/binary-consolidation" >}})
  — the single-`strata`-binary cutover that precedes this one.
- [Workers]({{< ref "/architecture/workers" >}}) — registration shape,
  supervisor lifecycle, leader-election semantics.
- ROADMAP entry: `P2 — Compose default + multi-cluster profiles race
  for worker leases on shared Cassandra` (closed in this cycle).
