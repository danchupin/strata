---
title: 'TiKV-default lab'
weight: 50
description: 'Flip the lab compose default from Cassandra-backed single canonical strata to TiKV-backed 2-replica + nginx LB; Cassandra moves under --profile cassandra.'
---

# Migrating to the TiKV-default 2-replica lab

`docker compose up -d` used to bring up a single Cassandra-backed
`strata` service on `:9999` plus a `ceph` + `ceph-b` multi-cluster
RADOS pair. TiKV was opt-in via `--profile tikv` (`strata-tikv`) or
`--profile lab-tikv` / `--profile lab-tikv-3` for 2- and 3-replica
labs, and a parallel 3-replica Cassandra lab lived under
`--profile lab-cassandra-3`. Four distinct profile shapes plus the
always-on bare default — each rarely-used, each one a maintenance
surface to keep green across CI + smoke + bench scripts.

This cycle (`ralph/tikv-default-lab`, US-001..US-005) collapses that
matrix. The bare `docker compose up -d` is now the **TiKV-default
2-replica lab**: PD + TiKV for metadata, ceph + ceph-b as the
multi-cluster RADOS pair, two strata replicas (`strata-a` :10001 /
`strata-b` :10002) sharing the `strata-jwt-shared` volume behind an
nginx LB on `:9999`. The Cassandra-backed shape becomes opt-in via
`make up-cassandra` (= `docker compose --profile cassandra up -d`),
which adds `cassandra` + a single Cassandra-backed `strata-cassandra`
replica on `:9998` on top of the bare default.

This guide is the operator-facing checklist. There is no schema
migration, no data migration, and no Go code change — the move is
purely a compose-shape + Makefile-target flip plus the doc /
script / CI sweep that follows from it.

## What was removed

| Removed                                       | Reason                                                                                       |
| --------------------------------------------- | -------------------------------------------------------------------------------------------- |
| `strata` (Cassandra-backed, :9999)            | Renamed under `--profile cassandra` as `strata-cassandra` on `:9998`. The bare-default slot is now TiKV-backed. |
| `strata-tikv` (single TiKV replica, profile `tikv`) | Redundant — bare default IS TiKV-backed.                                              |
| `strata-tikv-c` (lab-tikv-3 3rd replica)      | Lab-tikv-3 retired. 3-replica bench parked as P3 ROADMAP follow-up.                          |
| `strata-cass-a/b/c` (lab-cassandra-3)         | Retired; 3-replica Cassandra lab not justifying its maintenance cost.                        |
| `strata-lb-nginx-cass` (lab-cassandra-3 LB)   | Retired with `strata-cass-*`.                                                                |
| `--profile tikv`                              | Retired — bare default IS TiKV.                                                              |
| `--profile lab-tikv` / `--profile lab-tikv-3` | Retired — bare default is the 2-replica lab; 3-replica parked as P3.                         |
| `--profile lab-cassandra-3`                   | Retired with the 3-replica Cassandra shape.                                                  |
| `make up-tikv` / `make up-lab-tikv` / `make up-lab-tikv-3` / `make up-lab-cassandra-3` | Replaced by bare `make up` / `make up-all` (TiKV-default) + `make up-cassandra`. |
| `make smoke-tikv` / `make smoke-signed-tikv`  | Redundant — `make smoke` / `make smoke-signed` target the bare default (now TiKV).           |
| `make smoke-multi-cluster`                    | Multi-cluster IS the default now.                                                            |
| `make wait-strata-tikv` / `make wait-strata-lab-cassandra` | Replaced by `make wait-strata-{a,b,lb-nginx}` + combined `make wait-strata-lab`. |
| `scripts/smoke-compose-collapse.sh`           | Superseded by US-005's `scripts/smoke-tikv-default-lab.sh`.                                  |

## What replaced the dual-profile matrix

The bare-default `docker compose up -d` brings up exactly these
no-profile services:

```
pd, tikv, ceph, ceph-b, strata-a, strata-b, strata-lb-nginx,
prometheus, grafana
```

The two strata replicas share the `strata-jwt-shared` volume and carry
distinct `STRATA_NODE_ID` env values so a session JWT issued by
`strata-a` validates on `strata-b` under the round-robin nginx LB.
Both attach to the `default` (ceph) and `cephb` (ceph-b) RADOS
clusters via:

```yaml
STRATA_RADOS_CLUSTERS: default:/etc/ceph-a/ceph.conf:/etc/ceph-a/ceph.client.admin.keyring,cephb:/etc/ceph-b/ceph.conf:/etc/ceph-b/ceph.client.admin.keyring
```

`make up-cassandra` (= `docker compose --profile cassandra up -d`)
additionally brings up `cassandra` + `strata-cassandra` (port 9998)
on top of the bare default. The Cassandra-backed strata mirrors the
strata-a env shape (multi-cluster, admin creds preset) with
`STRATA_META_BACKEND=cassandra` + `STRATA_CASSANDRA_HOSTS=cassandra:9042`.

## Operator recipes

### Bare-default TiKV bring-up

```bash
docker compose up -d                 # full default: pd, tikv, ceph, ceph-b, strata-a/b, LB, monitoring
make wait-tikv                        # HTTP probe on PD's /pd/api/v1/health
make wait-ceph
make wait-strata-lab                  # strata-a (:10001) + strata-b (:10002) + LB (:9999)
make smoke                            # PUT/GET round-trip against the nginx LB at :9999
```

### Cassandra-backed regression lab side-by-side

```bash
make up-cassandra                     # bare default + cassandra + strata-cassandra (:9998)
make wait-cassandra
bash scripts/smoke.sh http://127.0.0.1:9998
```

`:9999` (LB → TiKV) and `:9998` (Cassandra-backed strata) serve in
parallel against independent metadata state. Each backend keeps its
own bucket index; the buckets visible at one endpoint are not visible
at the other.

### Single-cluster env override

The single-cluster override recipe is unchanged from the previous
compose collapse — `STRATA_RADOS_CLUSTERS` at runtime against bare
`docker compose up -d`:

```bash
STRATA_RADOS_CLUSTERS=default:/etc/ceph-a/ceph.conf:/etc/ceph-a/ceph.client.admin.keyring \
  docker compose up -d strata-a strata-b strata-lb-nginx
```

## Port-change warning

The Cassandra-backed strata **moved from `:9999` to `:9998`**:

| Backend | Pre-cycle port | Post-cycle port |
| ------- | -------------- | --------------- |
| Cassandra-backed strata | `:9999` (bare default) | `:9998` (`--profile cassandra`, service `strata-cassandra`) |
| TiKV-backed strata      | `:9999` was unused by TiKV at bare default; `:9998` was `strata-tikv` under `--profile tikv` | `:9999` (nginx LB → `strata-a` + `strata-b`) |
| Direct-replica access   | n/a                    | `:10001` (`strata-a`) and `:10002` (`strata-b`) |

Operator curl scripts targeting `:9999` for the **Cassandra-backed**
gateway MUST update to `:9998` after bringing the Cassandra profile up
with `make up-cassandra`. Scripts targeting `:9999` generically (e.g.
the official `make smoke` against the bare default) continue to work
unchanged — they now hit the nginx LB → TiKV-backed strata-a / strata-b
pair.

## Makefile target breaking changes

`make wait-cassandra` no longer succeeds against the bare default. The
`cassandra` service is now profile-gated under `--profile cassandra`,
so `docker compose ps cassandra` returns nothing without the matching
profile flag, and the until-loop in `wait-cassandra` hangs forever.
The Makefile's `wait-cassandra` target itself was updated to pass
`--profile cassandra` so it works once the cassandra service is up,
but **operator scripts and CI workflows that chained `make up &&
make wait-cassandra`** must switch:

| Pre-cycle invocation                               | Replacement                                                              |
| -------------------------------------------------- | ------------------------------------------------------------------------ |
| `make up && make wait-cassandra` (TiKV path)       | `make up && make wait-tikv` (PD HTTP health on `:2379/pd/api/v1/health`) |
| `make up && make wait-cassandra` (Cassandra path)  | `make up-cassandra && make wait-cassandra`                               |
| `make up-tikv && make wait-strata-tikv`            | `make up && make wait-strata-lab`                                        |
| `make up-lab-tikv && make wait-strata-lab`         | `make up && make wait-strata-lab`                                        |
| `make up-lab-cassandra-3 && make wait-strata-lab-cassandra` | RETIRED — no replacement (3-replica Cassandra lab parked).      |
| `make smoke-tikv` / `make smoke-signed-tikv`       | `make smoke` / `make smoke-signed` (bare-default IS TiKV).               |

The full list of retired `make` targets is in the "What was removed"
table above. Sweep your CI workflows + smoke scripts for the
pre-cycle names; the US-005 grep gate enforces zero matches outside
the archive / docs-public exception set.

## Compose-default rebalance cadence change

The retired Cassandra-backed `strata` service shipped explicit
compose-level rebalance defaults:

```yaml
STRATA_REBALANCE_INTERVAL: 30s
STRATA_REBALANCE_RATE_MB_S: 100
```

The new bare-default strata-a / strata-b services **do NOT carry these
envs** — the gateway's built-in defaults apply instead: `1h` interval,
`100 MB/s` rate (per the `Background workers` section of `CLAUDE.md`).
This is intentional: the 30-second tick was a lab-debug convenience that
generated noisy logs in steady-state operation. The new default
matches what `internal/rebalance/worker.go` advertises in `--help`.

Operators relying on the 30-second rebalance tick (e.g. for fast drain
convergence during a manual operator demo) must set the env explicitly
at compose-up time:

```bash
STRATA_REBALANCE_INTERVAL=30s docker compose up -d strata-a strata-b
```

The rate-limit knob (`STRATA_REBALANCE_RATE_MB_S=100`) is unchanged
between the two defaults; it does not need to be set explicitly.

## Cassandra meta-backend remains first-class in code

**This is a lab compose change, not a backend deprecation.** Everything
about the Cassandra meta backend remains first-class:

- `internal/meta/cassandra/**` code is unchanged.
- The `internal/meta/storetest/contract.go` shared contract suite still
  runs against Cassandra in `internal/meta/cassandra/store_integration_test.go`
  (build tag `integration`) on every PR.
- `make test-integration` (testcontainers Cassandra; needs Docker)
  is preserved verbatim — the build tag, the timeouts, the matrix.
- Cassandra parity with TiKV in
  [the meta-backend comparison benchmarks]({{< ref "/architecture/benchmarks/meta-backend-comparison" >}})
  is maintained.
- The `--profile cassandra` lab path (`make up-cassandra`) exists
  precisely so operator regression smoke can be run against the
  Cassandra-backed gateway side-by-side with the TiKV-backed bare
  default.

The cycle is a **deployment shape flip**, motivated by TiKV's
structural wins on `ListObjects` (native ordered range scan vs
Cassandra's 64-way fan-out + heap-merge) + multi-leader scaling
(per-shard pessimistic-txn leases) being the better fit for the
default operator-facing experience. Operators choosing Cassandra /
Scylla for production stay on the same code path; only the bare
lab default flipped.

## Follow-up — 3-replica TiKV bench

The `lab-tikv-3` profile's primary use case was the `SHARDS=3`
rebalance-multi bench (`scripts/bench-rebalance-multi.sh`). With the
3-replica lab retired, the bench's `SHARDS=3` scenario now SKIPs
with an explicit stdout message after running the `SHARDS=1` +
`SHARDS=2` baselines. The 3-replica bench is parked as a new P3
ROADMAP entry **"Restore 3-replica TiKV bench (SHARDS=3
rebalance-multi)"**. Operators who genuinely need the SHARDS=3
data point can spin a one-off third replica via:

```bash
docker compose run --rm -p 10003:9000 \
  -e STRATA_NODE_ID=strata-c \
  -e STRATA_WORKERS=gc,lifecycle,rebalance \
  strata-a
```

…for the bench duration, then teardown. This is the higher-complexity
alternative path documented in the cycle PRD.

## End-to-end smoke

The full structural cutover is gated by
`scripts/smoke-tikv-default-lab.sh` (`make smoke-tikv-default-lab`).
See the script header for the four-scenario contract (bare-default
bring-up + round-robin LB assertion + drain evacuate convergence;
Cassandra profile side-by-side; single-cluster env override on the
TiKV default; residue grep gate against retired profile / service
names).

## See also

- [Compose collapse]({{< ref "/architecture/migrations/compose-collapse" >}})
  — the prior compose cycle that this one builds on (single canonical
  strata service; legacy single-cluster + features sidecars removed).
- [TiKV operator guide]({{< ref "/architecture/backends/tikv" >}}) —
  TiKV gotchas, sizing, env vars, capability matrix.
- [Placement + rebalance]({{< ref "/best-practices/placement-rebalance" >}})
  — multi-cluster drain workflow + multi-leader scaling shape.
- [Meta-backend comparison benchmarks]({{< ref "/architecture/benchmarks/meta-backend-comparison" >}})
  — headline numbers comparing the two backends at parity.
- ROADMAP entries closed in this cycle: `P3 — TiKV-default 2-replica
  lab` (added + closed atomically per CLAUDE.md "discovering a new
  gap" rule).
- ROADMAP entry parked: `P3 — Restore 3-replica TiKV bench (SHARDS=3
  rebalance-multi)`.
