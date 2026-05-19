---
title: 'Rebalance scaling'
weight: 20
description: 'Phase 2 multi-leader fan-out for the rebalance worker. STRATA_REBALANCE_SHARDS=N drives per-shard `rebalance-leader-<i>` leases so N replicas chew through drains in parallel.'
---

# Rebalance worker scaling benchmark (Phase 2)

Closes ROADMAP P2 _"Rebalance worker not sharded — single goroutine
bottleneck on large deploys"_. Mirrors the gc + lifecycle Phase 2 shape
(see [GC + Lifecycle scaling]({{< ref "/architecture/benchmarks/gc-lifecycle" >}}#phase-2--multi-leader-us-006)
for the same fan-out pattern measured against gc and lifecycle workers).

The single-leader rebalance worker scans every draining bucket per tick on
one goroutine. Phase 2 (`STRATA_REBALANCE_SHARDS=N`) shards that scan by
`fnv32a(bucketID) % N`; each replica races for one of the
`rebalance-leader-0..N-1` leases and processes only its own shard's bucket
subset. `STRATA_REBALANCE_SHARDS=1` reproduces Phase 1 byte-for-byte.

## Harness

```bash
# Bare-default TiKV lab (post-ralph/tikv-default-lab): PD + TiKV +
# ceph + ceph-b + strata-a + strata-b + strata-lb-nginx + prometheus +
# grafana via `docker compose up -d`. Both replicas attach to both
# RADOS clusters via STRATA_RADOS_CLUSTERS so `Placement={default:1, cephb:1}`
# policies have a second target for the rebalance worker to migrate to.
make up && make wait-strata-lab

# Default sweep: 1000 buckets × 10 chunks (10k chunks), 32 KiB each.
# Override BENCH_BUCKETS / BENCH_CHUNKS_PER_BUCKET / BENCH_OBJECT_SIZE_KB
# for smaller laptop runs (e.g. 50 / 4 / 4 finishes in a couple of minutes).
make bench-rebalance-multi
```

The make target wraps `scripts/bench-rebalance-multi.sh`. The script
orchestrates two passes (`SHARDS=1` baseline + `SHARDS=2` fan-out), seeds
buckets via aws-cli, drains `default` evacuate mode, polls
`/admin/v1/clusters/default/drain-progress` until `chunks_on_cluster=0`,
and prints a per-pass JSON line plus an aggregate ratio + verdict. A
third `SHARDS=3` leg auto-runs when the bench-3replica profile is up —
the script detects `strata-c` via `docker compose ps -q strata-c`. When
strata-c is absent the leg SKIPs (operator bring-up: `docker compose
--profile bench-3replica up -d strata-c` — see the [3-replica TiKV
(post-restore)](#3-replica-tikv-post-restore) section below). The
restart between passes runs through `BENCH_RESTART_HOOK` (default:
docker compose force-recreate of strata-a + strata-b with
`STRATA_REBALANCE_SHARDS=$SHARDS` exported); the 3-replica leg uses
`BENCH_RESTART_HOOK_3REPLICA` which adds the bench-3replica profile +
strata-c to the recreate set. Override either hook to drive a k8s
rollout or systemd restart on bare-metal labs.

Results land in `bench-rebalance-multi-results.jsonl` (one line per pass):

```json
{"shards":1,"buckets":1000,"chunks_per_bucket":10,"object_kb":32,"elapsed_s":612}
{"shards":2,"buckets":1000,"chunks_per_bucket":10,"object_kb":32,"elapsed_s":312}
```

## Methodology

- **Stack:** bare-default TiKV-default 2-replica lab (PD + TiKV + ceph +
  ceph-b + strata-a + strata-b behind nginx LB on host port 9999). Both
  replicas attach to both RADOS clusters so `Placement={default:1, cephb:1}`
  policies have a second target for the rebalance worker to migrate to.
  PD + TiKV are single-instance for the laptop / CI shape; production
  needs PD ≥3 + TiKV ≥3 for raft majority (see [TiKV
  backend]({{< ref "/architecture/backends/tikv" >}})).
- **Hardware:** macOS host, lima Docker context. Numbers are
  **directional** — absolute throughput depends on RADOS pool sizing,
  network topology, and per-replica gateway concurrency. The shape of
  the curve (relative speedup between shards=1 and shards=3) is the
  load-bearing signal.
- **Workload:** N=1000 buckets, each with 10 chunks (10k chunks total)
  on cluster `default`. Each bucket carries
  `Placement={default:1, cephb:1}` `mode=weighted`, so every chunk is
  classified as `migratable` on drain — no `stuck_single_policy` or
  `stuck_no_policy` noise.
- **Drain trigger:** `POST /admin/v1/clusters/default/drain
  {"mode":"evacuate"}`; wall-clock measured from the response time to
  the first `/drain-progress` poll that returns `chunks_on_cluster=0`.
- **Restart between passes:** the strata replicas are force-recreated
  with `STRATA_REBALANCE_SHARDS=$SHARDS` baked into their env. Seeded
  buckets are torn down between passes so the second drain measures the
  same workload, not residue.

## Expected production multiplier

The bench's pass / fail verdict thresholds:

| Ratio (shards=3 ÷ shards=1) | Verdict | Reason |
|----------------------------:|:--------|:-------|
| ≤ 40 % | `SPEEDUP_OK` (exit 0) | ≥2.5× speedup — Phase 2 fan-out working as designed. Linear-ish on disjoint shards, minus the per-replica RADOS write-bandwidth contention (read+write share the same token-bucket — see [STRATA_REBALANCE_RATE_MB_S]({{< ref "/best-practices/placement-rebalance" >}}#rate-limiting)). |
| 40 – 70 % | `SPEEDUP_PARTIAL` (exit 0) | Some speedup, but below the 2.5× target. Common causes: target cluster's RADOS pool is the bottleneck (all 3 replicas write to the same OSDs), STRATA_REBALANCE_INFLIGHT too low per replica, single-PD region heat on the meta side. |
| > 70 % | `SPEEDUP_FAILED` (exit 1, regression) | shards=3 within 30 % of shards=1 — the fan-out bought nothing. Look for: shards collapsed onto one replica (check `/admin/v1/cluster/nodes` `leader_for` — should show one `rebalance-leader-<i>` per replica), draining bucket subset all hashes to one shard (unlucky FNV-1a distribution at small N), or a shared meta-side mutex serialising the per-shard work. |

The `SPEEDUP_OK` threshold is set at 2.5× rather than 3× because each
replica also competes for the same target cluster's RADOS write
bandwidth — drain throughput is fundamentally write-bound on the target
side once the source-side scan parallelises. Operators with multiple
target clusters (e.g. `Placement={default:1, cephb:1, cephc:1}` and
draining `default`) should see closer to 3× because the write fan-out
spreads across more pools.

## Cap shape — when does Phase 2 stop scaling?

The rebalance worker fan-out parallelises cleanly up to the point where
one of these saturates first:

1. **Target cluster RADOS pool write throughput** — every shard writes
   to the chosen target cluster (per-bucket Placement). With one target
   cluster and N source shards, the target pool sees N× write load. At
   `STRATA_REBALANCE_RATE_MB_S=100` (default) and 3 shards, the target
   sustains ~300 MB/s; the single-MON / 3-OSD lab tops out somewhere
   below that.
2. **Source cluster RADOS read throughput** — same shape on the read
   side. The per-replica token-bucket is shared between read + write
   (1 chunk move = chunkSize × 2 tokens), so a single replica caps at
   `RATE_MB_S` regardless of `INFLIGHT`.
3. **TiKV region heat on the buckets table** — `ListBucketsShard`
   issues one prefix scan per shard. At very high N the scans converge
   on overlapping regions; TiKV's read-only scans rarely contend
   meaningfully, but heavy `SetObjectStorage` CAS on chunk manifests
   can stress the same regions. **Knee:** ~32–64 shards on a 3-node
   TiKV cluster before region splits stabilise.
4. **Bucket count vs shard count** — `fnv32a(bucketID) % N`. With
   fewer buckets than shards, hash collisions waste replicas.
   **Knee:** speedup plateaus at `min(N, distinct-bucket-count)`. A
   3-replica deploy with 3 draining buckets ≈ Phase 1 throughput
   because each bucket pins to one shard.

The **per-bucket scan** is no longer the bottleneck post-US-001..US-003 —
each shard scans an independent subset and aggregates into the shared
`ProgressTracker` via per-shard slots. The next ceiling is the RADOS
read+write round-trip on the per-chunk copy phase.

## Operator-facing recommendation

- **3-replica deploy:** `STRATA_REBALANCE_SHARDS=3`,
  `STRATA_REBALANCE_INFLIGHT=4` (default; raise to 8 or 16 if RADOS
  pool has headroom), `STRATA_REBALANCE_RATE_MB_S=100` (default; raise
  per-cluster bandwidth budget allows). Aggregate ceiling ≈ 2.5–3×
  Phase 1 per-replica cap on disjoint workloads.
- **N-replica deploy in general:** scale `STRATA_REBALANCE_SHARDS` to
  match replica count up to ~64 (TiKV region-heat band). Going beyond
  replica count is a no-op (extra leases unfilled); going below leaves
  replicas without rebalance work (they still process gc + lifecycle if
  configured).
- **Single-replica fallback:** `STRATA_REBALANCE_SHARDS=1` (default)
  reproduces Phase 1 behaviour byte-for-byte. The existing
  `smoke-drain-lifecycle.sh` and `smoke-drain-transparency.sh` continue
  to pass under this default — Phase 2 is opt-in via env.
- **Folded heartbeat:** all shards held by one replica fold into
  exactly one `leader_for=rebalance` chip on `/admin/v1/cluster/nodes`.
  Operators verifying the distribution should look at
  `/admin/v1/cluster/nodes` per-replica `leader_for` lists — each
  replica should carry exactly one of `rebalance-leader-{0,1,2}`.

## Caveats

- The bench measures **end-to-end drain wall-clock** — not per-chunk
  cost. It captures the realistic operator workflow but mixes shard
  fan-out gain with RADOS pool saturation. For pure meta-side
  worker fan-out cost, run the gc + lifecycle Phase 2 bench instead
  (the worker code-path overhead is the same shape).
- Real-world drain workloads have heterogeneous bucket sizes; the
  benchmark seeds uniform 10-chunk buckets so the per-shard work is
  evenly distributed. Production runs will show shard-imbalance noise
  proportional to the bucket-size distribution variance.
- Single-PD + single-TiKV lab — production multi-node TiKV will shift
  the region-heat knee right.
- Cross-host lease lottery — in the bare-default 2-replica stack both
  replicas race for `rebalance-leader-{0,1}` through the same
  TiKV-backed locker. Steady-state ownership rotation under churn
  (one replica restarted, leases redistributed) is exercised by
  scenario D of `scripts/smoke-rebalance-scale.sh` (US-005), not by
  this bench. The 3-replica `rebalance-leader-{0,1,2}` race is
  available via the [3-replica TiKV (post-restore)](#3-replica-tikv-post-restore)
  bench leg below.

## 3-replica TiKV (post-restore)

Restored by `ralph/dx-lab` US-002. The bench's `SHARDS=3` leg is opt-in
via a new compose profile, `bench-3replica`, which layers a third
strata gateway (`strata-c`, host port `10003:9000`,
`STRATA_NODE_ID=strata-c`) on top of the bare-default 2-replica lab.
The third replica joins the same TiKV-backed locker pool so it races
for one of the `rebalance-leader-{0,1,2}` leases — no nginx LB change
is required because the rebalance worker shard fan-out runs over TiKV
leases, independent of HTTP routing (`strata-c` simply offers a third
goroutine to claim a shard lease; the bench's seed-buckets + drain-
trigger + drain-progress polls still go through the nginx LB on port
9999 against `strata-a` / `strata-b`).

### Bring-up

```bash
# Baseline lab.
docker compose -f deploy/docker/docker-compose.yml up -d
# Third replica (opt-in).
docker compose -f deploy/docker/docker-compose.yml --profile bench-3replica up -d strata-c
# Wait for strata-c /readyz on 10003 before driving the bench.
until curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:10003/readyz | grep -q 200; do sleep 2; done
make bench-rebalance-multi
# Tear-down (cleans up bench-3replica profile too).
make down
```

`scripts/bench-rebalance-multi.sh` auto-detects `strata-c` via
`docker compose ps -q strata-c`. When present, the bench appends a
third pass with `STRATA_REBALANCE_SHARDS=3` and prints
`elapsed_s` + ratio vs the SHARDS=1 baseline. When absent, the bench
prints a SKIP line referencing the resource cap (3 strata replicas + 2
ceph clusters + pd + tikv on a single laptop / lima Docker host can
exceed the 8–12 GiB memory budget typical for that shape) and exits on
the SHARDS=1 / SHARDS=2 verdict alone.

### Numbers

This subsection is the baseline placeholder for the first three-replica
capture. Local-run capture from this restore cycle was deferred — the
canonical Ralph DX/lab box was a macOS + lima Docker context where the
3-replica bench cap (~12 GiB combined for strata-c + 2 ceph memstores +
pd + tikv + prometheus + grafana) exceeds the default lima memory
budget. The bench-3replica profile + the script's auto-detected
third-pass leg are wired in and exercised under bash syntax + `make
docs-build` gates so operators with headroom can capture
representative numbers on demand.

When you do capture numbers, the expected shape is:

| Replicas | Shards | Elapsed | Ratio vs baseline | Notes |
|---------:|-------:|--------:|------------------:|:------|
| 1 | 1 | T1 (baseline) | 100 % | Phase 1 byte-for-byte |
| 2 | 2 | ~ 0.4 × T1 | ≤ 40 % | bare-default lab (SPEEDUP_OK threshold) |
| 3 | 3 | ~ 0.3 × T1 | 25 – 35 % | linear-ish on disjoint shards, modulo RADOS target-pool write contention |

The 3-replica ratio target sits closer to 30 % (3× speedup) rather
than the strict 1/3 because per-replica RADOS write bandwidth shares
the target-cluster pool (see [Cap shape](#cap-shape--when-does-phase-2-stop-scaling)
above — point 1, target-cluster RADOS write throughput).
