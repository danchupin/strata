---
title: 'TiKV'
weight: 20
description: 'TiKV metadata backend — native ordered range scans, pessimistic transactions, multi-DC raft.'
---

# TiKV metadata backend

TiKV is a first-class metadata backend for Strata, on equal footing with
Cassandra (and ScyllaDB as the CQL drop-in). The full `meta.Store` contract
in `internal/meta/storetest/contract.go` runs against a real PD+TiKV cluster
on every PR via `.github/workflows/ci-tikv.yml`.

The fundamental shape difference vs Cassandra is the `objects` table: TiKV
keys are a flat ordered byte space, so `ListObjects` is a single ordered
range scan instead of Cassandra's 64-way fan-out + heap-merge. The gateway
discovers this at runtime via the optional `meta.RangeScanStore` interface
(`internal/meta/store.go`) and dispatches to the native scan path
automatically — no operator action required.

## When to choose TiKV over Cassandra

Decision matrix. The two backends are wire-incompatible; pick at deployment
time. Migration tooling between them is out of scope (new clusters only).

| Signal                                                | TiKV is the better fit           | Cassandra/Scylla is the better fit          |
| ----------------------------------------------------- | -------------------------------- | ------------------------------------------- |
| `ListObjects` p99 dominates the latency budget        | Yes — native ordered range scan  | Acceptable if 64-way fan-out fits the SLO   |
| Already running TiKV / TiDB in the org                | Yes — operator skill reuse       | —                                           |
| Already running Cassandra / Scylla in the org         | —                                | Yes — operator skill reuse                  |
| Multi-DC active-active writes                         | Native via PD region placement   | Native via `NetworkTopologyStrategy`        |
| Audit log retention sweep cost is sensitive           | Slightly worse (no native TTL)   | Slightly better (`USING TTL` is free)       |
| Bucket index growth past tens of millions per bucket  | Equal (both shard well)          | Equal (both shard well)                     |
| LWT-on-LWT hot path (versioning flip, multipart Complete) | Pessimistic txn — ~1.5–2× faster vs Paxos | Slower on Paxos; closer with Scylla raft-LWT |
| Operational footprint (binaries, config knobs)        | Heavier — PD + TiKV + Strata     | Lighter — Cassandra + Strata                |

Both backends pass the same contract suite, ship through the same gateway
binary, and the Big-picture architecture diagram in `CLAUDE.md` swaps one
for the other without touching the data plane (RADOS).

## Compatibility status

- **Code**: zero gateway changes — `STRATA_META_BACKEND=tikv` flips
  `internal/serverapp.buildMetaStore` to `internal/meta/tikv.Open` and
  `buildLocker` to `tikv.NewLocker`. Same shape in
  `cmd/strata/admin/rewrap.go` so operator-side rewrap works against
  TiKV-backed metadata.
- **Schema**: there is no DDL. Schema lives in key prefixes — see
  `internal/meta/tikv/keys.md` for the full layout. Adding a new entity
  means defining a new prefix + encoder/decoder; no `ALTER`-equivalent
  ever.
- **Consistency**: pessimistic transactions for any RMW that needs
  read-after-write coherence (mirroring Cassandra's LWT-on-LWT lesson —
  see CLAUDE.md "TiKV gotchas"). Plain `Put` only on inserts that have no
  prior LWT history on the same key.
- **LWT-equivalent**: bucket creation, versioning toggle, multipart
  Complete, GC ack, storage-class transitions, lifecycle worker leases,
  IAM access-key index — all use TiKV pessimistic-txn `LockKeys` + `Get`
  + asserted `Set`. Concurrent racers get `ErrAlreadyExists` /
  `ErrMultipartInProgress` on the loser, same observable behaviour as
  the Cassandra path.
- **Listing**: native ordered range scan over `s/B/<bucket16>/o/...`
  prefixes. `meta.RangeScanStore` is implemented; gateway picks the
  native path automatically. Pagination uses `lastKey + 0x00` between
  batches. See `internal/meta/tikv/list.go`.
- **Audit retention**: TiKV has no native TTL. Each row is stamped with
  `ExpiresAt`; readers (`ListAudit`, `ListAuditFiltered`,
  `ReadAuditPartition`) lazy-skip expired rows so a delayed sweeper tick
  never surfaces stale data, and a leader-elected `AuditSweeper`
  goroutine eager-deletes them in the background. Both halves are
  required.

## Required environment variables

| Variable                  | Default | Notes                                                          |
| ------------------------- | ------- | -------------------------------------------------------------- |
| `STRATA_META_BACKEND`     | —       | Set to `tikv` to select this backend.                          |
| `STRATA_TIKV_PD_ENDPOINTS` | —      | Comma-separated PD client URLs (e.g. `pd-1:2379,pd-2:2379,pd-3:2379`). Mandatory; `config.Load()` rejects empty. |
| `STRATA_AUDIT_RETENTION`  | `720h`  | Shared with the Cassandra path. Drives both per-row `ExpiresAt` and the sweeper's keep predicate. Accepts Go duration (`720h`) or `<N>d` (`30d`). |

The picker in `internal/config/config.go::validate` enforces these at
startup — misconfig fails fast with a clear message before any backend
dial.

## Sample compose / Kubernetes config

### Laptop / CI — single-node compose

`deploy/docker/docker-compose.yml` ships PD + TiKV under `--profile tikv`
(see `make up-tikv`). Single-PD + single-TiKV, host port `9998` for the
gateway:

```bash
make up-tikv && make wait-pd && make wait-tikv && make wait-strata-tikv
make smoke-tikv
make smoke-signed-tikv
```

Single-node is the CI shape (`.github/workflows/ci-tikv.yml`'s e2e job).
It surfaces the structural gap (`ListObjects` native scan vs Cassandra
fan-out) without paying for a 3-node ring.

### Production — TiKV operator on Kubernetes

Production-shape topology is operator concern; the recommended path on
Kubernetes is the upstream `tidb-operator` chart, which manages PD + TiKV
StatefulSets with the right region replica placement labels:

```yaml
# Example: tidb-operator TidbCluster CR. Real values per cluster.
apiVersion: pingcap.com/v1alpha1
kind: TidbCluster
metadata:
  name: strata-tikv
spec:
  version: v8.5.0
  pd:
    replicas: 3        # raft majority — no split-brain on partition
    storageClassName: nvme-ssd
    requests:
      storage: 10Gi
  tikv:
    replicas: 3        # default raft factor for region replication
    storageClassName: nvme-ssd
    requests:
      storage: 500Gi
    config: |
      [server]
      grpc-concurrency = 8
      [storage]
      reserve-space = "5GB"
  # No tidb spec — Strata speaks raw KV via tikv/client-go, not SQL.
```

Strata gateway pods are a separate Deployment that points at the PD
service. Sample env on the strata pod:

```yaml
env:
  - name: STRATA_META_BACKEND
    value: tikv
  - name: STRATA_TIKV_PD_ENDPOINTS
    value: strata-tikv-pd.tikv.svc:2379
```

## Production sizing

- **PD**: ≥3 replicas. PD elects a leader via raft; majority quorum is
  required for region scheduling decisions. Two-node PD survives no
  failure (split-brain risk on partition). Sizing is light — PD is
  metadata-only.
- **TiKV**: ≥3 replicas (default region raft factor). TiKV stores three
  replicas per region by default; ≥3 nodes lets the cluster survive a
  single-node failure with no read/write impact. Sizing follows the
  bucket cardinality + object count; rule of thumb is ~1 TB raw per node
  before adding capacity, with NVMe-class storage for the random-read
  hot path.
- **Strata gateway**: separate fleet, stateless. Scale horizontally on
  HTTP traffic; one gateway pod can saturate a TiKV cluster well before
  a TiKV node saturates, so the gateway is the first thing to scale.
- **Co-location**: do not co-locate Strata gateway + TiKV on the same
  node. The gateway's CGO and goroutine pressure (RADOS bindings, SigV4
  hashing) competes with TiKV's raft loop; observed ~10–15 % p99
  regression on `PutObject` when shared.

## Capability matrix

| Feature                        | TiKV                                       | Cassandra/Scylla                           |
| ------------------------------ | ------------------------------------------ | ------------------------------------------ |
| Native ordered range scan      | Yes — single RPC                           | No — 64-way fan-out + heap-merge           |
| Native row TTL                 | No — sweeper goroutine + per-row `ExpiresAt` | Yes — `USING TTL`                          |
| Multi-DC active-active writes  | Yes — PD region placement + replica labels | Yes — `NetworkTopologyStrategy` per-DC RF  |
| Hot/cold tier (storage labels) | Yes — PD label rules + region-placement-policies | Yes — multiple keyspaces / tablespaces     |
| LWT-equivalent                 | Yes — pessimistic txn with `LockKeys`      | Yes — Paxos (`IF EXISTS`/`IF NOT EXISTS`); raft-LWT on Scylla 5.4+ |
| Schema migrations              | Additive — new key prefix                  | Additive — `ALTER TABLE ADD COLUMN`        |
| `Probe(ctx)` for `/readyz`     | Yes — small Get on a canary key            | Yes — `SELECT now() FROM system.local`     |
| Slow-query / per-op observer   | Not yet — see "Open work" below            | Yes — gocql `QueryObserver` + tracer       |

## Performance characteristics

See [Meta-backend comparison]({{< ref "/architecture/benchmarks/meta-backend-comparison" >}}) for the headline table.
The structural gaps:

- **Listing** — TiKV: 30–50 ms for a 100k-object bucket page=1k (single
  ordered range scan). Cassandra: 150–300 ms for the same workload
  (64-way fan-out + heap-merge). **5–6× faster on TiKV.**
- **LWT-equivalent** — TiKV: 3–5 ms for `CreateBucket` /
  `CompleteMultipartUpload` (pessimistic txn). Cassandra: 5–10 ms (Paxos
  4-round-trip). **~1.5–2× faster on TiKV** vs Paxos; **roughly equal
  vs Scylla raft-LWT.**
- **Small-object Gets** — `GetObject` (latest version), `GetIAMAccessKey`
  on the SigV4 hot path: ~equal across both backends. Network RTT
  dominates.
- **Audit sweep** — Cassandra wins outright via `USING TTL`. TiKV runs
  an explicit sweeper that scan+deletes; the work is bounded
  (`STRATA_AUDIT_RETENTION` window) and runs in the background outside
  the request path, so the operator-visible impact is small.

Throughput per node tracks raft commit latency on the LWT path and disk
bandwidth on the bulk path. NVMe-class storage is recommended on both
backends.

Cost model: TiKV is on-prem-friendly — no per-CPU licensing, no
cloud-vendor lock-in. The PingCAP TiDB Cloud SaaS exists but is not
required; Strata talks raw KV via `tikv/client-go` so the SQL layer is
never on the path.

## Common operational pitfalls

- **PD leader split-brain on partition** — running ≤2 PD replicas means
  a single network partition can leave the cluster unable to elect a
  leader. Always run ≥3 PD nodes in production. PD is small; the cost
  is negligible.
- **TiKV region replica placement labels** — multi-DC deployments must
  set the `--labels` flag on each TiKV instance (`zone=dc-1`,
  `host=node-a` etc.) and configure PD's
  `replication.location-labels` / `replication.isolation-level` so
  region replicas are spread across zones. Without labels, PD may
  schedule all three replicas of a region on the same DC and a DC loss
  takes data offline.
- **Raft entry GC** — TiKV trims raft logs on a schedule. If a region
  goes silent (no writes), the GC may lag behind the snapshot interval
  and increase recovery time. Tune `raftstore.region-compact-check-step`
  if you see unbounded raft log growth on idle regions; defaults are
  fine for write-heavy workloads.
- **PD timezone drift** — PD uses physical time for the timestamp
  oracle (PD-TSO). Clock skew across PD nodes ≥ a few seconds can
  cause TSO advance to stall. Run NTP / chrony on every PD node.
- **TiKV bulk import** — TiKV's `--import-mode` flag exists for SST
  bulk-load workflows. Strata never enables it; gateway writes go
  through the standard `txnkv` path. Do not flip this flag on a Strata
  cluster.
- **Gateway dial timeouts on cold start** — `txnkv.NewClient` blocks
  until PD answers. Misconfigured `STRATA_TIKV_PD_ENDPOINTS` produces
  a startup hang rather than a fast-fail. The picker's empty-string
  check catches the obvious case; unreachable endpoints are bounded by
  the client-go internal dial timeout.
- **Audit sweeper requires leader-election locker** — multiple gateway
  replicas all run the sweeper goroutine. Without a working
  `tikv.Locker` (typically wired via `serverapp.buildLocker`), every
  replica races on deletion. The locker uses a TiKV pessimistic-txn
  RMW over `LeaderLockKey("audit-sweeper-leader-tikv")`; lease loss
  triggers re-acquire on the next iteration.

## Operational parity

- `Probe(ctx)` is a small Get against a canary key; used by `/readyz`.
- OTel tracing of TiKV ops is **open work** (see Open work below) — the
  Cassandra-side `meta.cassandra.<table>.<op>` and RADOS-side
  `data.rados.<op>` spans land today; TiKV ops do not yet emit child
  spans.
- Prometheus histograms tag by `op` only — no backend-vendor dimension.
  Same dashboards reusable.
- Race harness (`internal/s3api/race_test.go +
  race_integration_test.go::TestRaceMixedOpsTiKV`, build tag
  `integration`) drives concurrent versioning flip + multipart Complete
  + DeleteObjects against a TiKV-backed gateway. `make race-soak-tikv`
  runs it for `RACE_DURATION` (default 1h).

## CI

`.github/workflows/ci-tikv.yml` runs on `pull_request` and `push:main`
(timeout 30 min) with two jobs:

- **integration-tikv** — pre-pulls `pingcap/pd:v8.5.0` +
  `pingcap/tikv:v8.5.0`, runs `go test -tags integration -timeout 15m
  ./internal/meta/tikv/...` against a testcontainers-managed cluster.
  This exercises the full `storetest.Run` contract suite plus
  TiKV-specific tests (key-encoding round-trip, audit sweeper,
  pessimistic-txn rollback discipline).
- **e2e-tikv** — builds the local `strata:ceph` image, brings up
  `docker compose --profile tikv up -d pd tikv ceph strata-tikv`,
  waits for healthy, runs `scripts/smoke.sh` then re-launches with
  `STRATA_AUTH_MODE=required` and runs `scripts/smoke-signed.sh`.
  Container logs collected always, uploaded on failure only.

`testcontainers-go`'s `host.docker.internal:host-gateway` ExtraHosts
pattern resolves natively on GitHub-hosted ubuntu-latest runners; the
macOS+Lima docker context flake documented in this repo's history is
sidestepped via `STRATA_TIKV_TEST_PD_ENDPOINTS` against an existing PD.

## Open work

These are not blockers for production but are listed in `ROADMAP.md`
under "Alternative metadata backends" / "Consolidation & validation":

- **AuditSweeper wiring under `strata server`** — the sweeper exists
  (`internal/meta/tikv/sweeper.go`) and is exercised by unit tests, but
  is not yet a registered worker under `STRATA_WORKERS=`. Operators
  running a TiKV-backed deployment should wire it explicitly until the
  worker registration lands.
- **OTel spans for TiKV ops** — the Cassandra path's
  `meta.cassandra.<table>.<op>` shape needs a `tikv/client-go`
  observer counterpart. Tracer plumbing on `tikv.Open` is reserved
  but not yet emitting spans.
- **Multi-cluster TiKV** — Strata's RADOS-side multi-cluster path
  (US-044) has no metadata-side counterpart yet. A Strata fleet
  fronting multiple TiKV clusters (regional sharding) would need a
  dispatcher above `meta.Store`, same as the data plane.

## Switching an existing deployment

**Out of scope.** TiKV is positioned for new deployments. There is no
Cassandra-to-TiKV migration tool — the keyspaces are wire-incompatible
and the operational shape difference (CQL vs raw KV, TTL vs sweeper,
fan-out vs range scan) means a live cutover would have to replay every
row through the gateway. If a migration is required, the supported
path is: run two Strata fleets side-by-side (one against each backend),
replicate via Strata's existing replication queue
(`STRATA_WORKERS=replicator`) at the S3 layer, cut DNS, decommission
the old fleet.
