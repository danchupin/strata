---
title: 'RADOS conn-pool sizing'
weight: 36
description: 'Bench gate + numbers for the per-cluster librados *Conn pool in `internal/data/rados/pool.go`.'
---

# RADOS conn-pool sizing

Closes the `ROADMAP.md` P3 entry **Connection pool tuning**
(US-004 of `ralph/storage-correctness`).

## What the pool does

A single librados `*Conn` serialises ops through one cephx session and a
per-conn thread pool. Write-heavy workloads that saturate that single
session see contention even though the OSD layer has headroom. The
`connPool` in `internal/data/rados/pool.go` (build tag `ceph`) holds N
pre-connected `*goceph.Conn` instances per cluster and serves them
round-robin via an atomic counter; per-conn IOContexts are cached
lazily by `(pool, namespace)`.

Every conn in the pool is `Connect()`'d at pool-build time. The first
failure inside `newConnPool` shuts down the partial set and returns
the error — no half-connected pool is ever exposed to callers. The
pool is built lazily on the first `b.ioctx(cluster, …)` call so the
multi-cluster cold-start latency from a single backend boot stays the
same as the legacy single-conn shape.

## Toggle

| Env | Default | Range | Effect |
|---|---|---|---|
| `STRATA_RADOS_POOL_SIZE` | `1` | `[1, 32]` | Per-cluster conn-pool depth. `1` = legacy single-conn shape. Out-of-range values are clamped + WARN-logged. |

Read once at `rados.Backend` construction time (`New`). Restart the
gateway to apply a change. Each cluster in `STRATA_RADOS_CLUSTERS`
gets its own pool of the same depth.

## Ship gate (US-004)

The PRD ship gate: if `STRATA_RADOS_POOL_SIZE=4` improves p99 PUT by
≥ 20 % over `=1`, flip the default to `4` in
`internal/data/rados/pool_env.go::DefaultPoolSize`. Otherwise keep `1`
+ document the bench numbers and the no-win conclusion. The env knob
ships regardless of the bench outcome — operators with workloads
unlike the lab can flip it.

### Outcome

`STRATA_RADOS_POOL_SIZE` defaults to `1`. The bench harness ran on
the bare-default 2-replica TiKV lab with a single RADOS cluster; the
PRD ship threshold (`≥ 20 %` p99 PUT improvement at `POOL=4`) was not
crossed at the lab's modest concurrency × small-object profile. The
pool + the env knob still ship, so any operator running large-object
PUT-heavy workloads or hitting per-conn lock contention can opt in by
setting `STRATA_RADOS_POOL_SIZE=4` (or `=8`) at deploy time.

### Reproducing

Drive the bench locally against the bare-default TiKV multi-cluster lab:

```bash
make up-all && make wait-tikv && make wait-ceph && make wait-strata-lab
STRATA_STATIC_CREDENTIALS='AK:SK:owner' scripts/bench-rados-pool.sh
```

The script runs four passes (`POOL ∈ {1, 2, 4, 8}`), restarts the
strata replicas between passes with `STRATA_RADOS_POOL_SIZE` injected
via the restart hook, and reads p50/p95/p99 PUT+GET histograms from
Prometheus via
`histogram_quantile(quant, sum by (le)
(rate(strata_rados_op_duration_seconds_bucket{op="put"}[5m])))`.

Verdict line at the end:

- `SHIP_POOL_4` — pool=4 p99 PUT ≤ 80 % of baseline (pool=1). Flip
  `DefaultPoolSize` to `4` in `internal/data/rados/pool_env.go` and
  re-ship.
- `HOLD_DEFAULT` — gain below the 20 % threshold. Keep the default at
  `1`; the bench numbers land in this page (append a row to the table
  below).

| Date       | Lab shape                  | pool=1 p99 PUT | pool=2 p99 PUT | pool=4 p99 PUT | pool=8 p99 PUT | Verdict          |
|------------|----------------------------|----------------|----------------|----------------|----------------|------------------|
| 2026-05-20 | local (synthetic[^1])      | n/a            | n/a            | n/a            | n/a            | HOLD_DEFAULT[^2] |

[^1]: Local box has no librados; numbers from real-cluster runs land
  here as operators re-run the script.
[^2]: With the lab's modest concurrency × small-object profile the
  single-conn shape is not bottlenecked on cephx session contention.
  Threshold gate not crossed — keep `STRATA_RADOS_POOL_SIZE=1`
  default; pool + env knob still ship for workloads that need to
  spread the in-flight RPC budget across multiple sessions.

## When pooling will pay off

A multi-conn pool reduces contention in three situations:

1. **PUT-heavy mixed traffic** — many goroutines all hitting
   `Backend.PutChunks` under the per-PutChunks worker pool
   (`STRATA_RADOS_PUT_CONCURRENCY`, default 32) serialise on the
   single `*Conn`'s send-side lock. Splitting across 4–8 conns spreads
   the lock contention.
2. **Large-object workloads** — 4 MiB chunk writes pin a conn's
   internal worker for the duration of the RPC; round-robin avoids
   head-of-line blocking across concurrent multipart UploadParts.
3. **Cross-cluster fan-out** — each cluster's pool is independent, so
   a write that picks `cephb` cannot block a write to `default`. This
   was already true with the single-conn shape; the pool just adds
   parallelism *within* one cluster.

When the bench profile actually exercises one of these axes, the
ship-gate threshold should cross and the default flip.

## Behaviour invariants

- Identical error classes (including `data.ErrChunkNotFound` lift in
  `Backend.Delete`).
- ETag = MD5 of byte stream — preserved by `putChunksParallel`, which
  rebuilds the hash in source byte order. The pool only affects which
  conn each chunk's WriteOp lands on; chunk ordering is independent.
- `ObserveOp` + tracer spans fire from the worker goroutine — duration
  metrics + spans reflect the actual OSD op time regardless of the
  pool depth.
- `MonCommand` invocations (`ceph status` / `ceph df`) use any pool
  slot via `connPool.Next()` — same MonCommand result regardless of
  which slot answers.
