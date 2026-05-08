---
title: 'Meta-backend comparison'
weight: 20
description: 'Hot-path latency + throughput for Cassandra, TiKV, and the in-tree memory reference.'
---

# Meta-backend benchmarks: TiKV vs Cassandra

This page captures hot-path latency / throughput numbers for Strata's two
production metadata backends (Cassandra and TiKV) and the in-tree memory
reference. The numbers are operator-runnable on a single laptop docker
stack via the harness in `internal/meta/storetest/bench.go`.

The headline operations (US-018):

| Op                          | Why it matters                                         |
| --------------------------- | ------------------------------------------------------ |
| `CreateBucket`              | LWT-equivalent create-if-not-exists                    |
| `GetObject`                 | Single Get / latest-version range-scan                 |
| `ListObjects` 100k page=1k  | Listing throughput on a real-world bucket              |
| `CompleteMultipartUpload`   | LWT-equivalent status flip (`uploading→completing`)    |
| `GetIAMAccessKey`           | SigV4 verifier — runs on every request                 |
| `AuditAppend`               | Audit log write amplification                          |
| `AuditSweepPartition`       | Periodic retention sweep (TiKV emulates Cassandra TTL) |
| `RangeScanObjects` 100k     | TiKV-only: native ordered scan vs Cassandra fan-out    |

## Rig

Single-laptop docker stack. Both backends run as 3-node clusters so LWT
+ raft latency floors are realistic; for sandbox runs a single-node stack
is also supported via the standard `make up` / `make up-tikv` targets.

| Component | Image / version          | Layout                       |
| --------- | ------------------------ | ---------------------------- |
| Cassandra | `cassandra:5.0`          | 3-node ring, LOCAL_QUORUM    |
| TiKV      | `pingcap/tikv:v8.5.0`    | 3-replica region (PD ≥ 1)    |
| PD        | `pingcap/pd:v8.5.0`      | 1-node sandbox / 3-node prod |
| Strata    | `strata:ceph` local      | gateway + harness binary     |

Reproducing the published 3-node shape locally is operator concern — the
default `deploy/docker/docker-compose.yml` ships single-node profiles for
both backends; multi-node is a swarm/k8s deployment topic outside this
doc. Single-node still surfaces the structural gap (TiKV's single
ordered scan vs Cassandra's 64-way fan-out).

## Harness

`internal/meta/storetest/bench.go` exports `Bench(b, newStore, opts)` —
one function exercises every sub-benchmark above against any
`meta.Store`. Memory, Cassandra, and TiKV all consume it from their own
`*_bench[_integration]_test.go` files.

```
internal/meta/storetest/bench_test.go                     -> BenchmarkMemoryStore
internal/meta/cassandra/store_bench_integration_test.go   -> BenchmarkCassandraStore
internal/meta/tikv/store_bench_integration_test.go        -> BenchmarkTiKVStore
```

`BenchOptions`:

| Field          | Default | Notes                                           |
| -------------- | ------- | ----------------------------------------------- |
| `Concurrency`  | 50      | parallel writers per sub-benchmark              |
| `ListSize`     | 100_000 | objects pre-populated for ListObjects benches   |
| `PageSize`     | 1_000   | ListObjects page size                           |
| `AuditPreload` | 10_000  | audit rows pre-aged for AuditSweepPartition     |

`-short` shrinks `ListSize` and `AuditPreload` to 1k each so smoke runs
finish in seconds; drop it for the published numbers.

## Methodology

- 60 s warmup is implicit — the standard Go bench loop ramps `b.N`
  upward, calling the function under test at increasing iteration
  counts. The first low-N pass exercises caches and JIT-equivalents
  before the timed pass.
- 5 min measurement window per sub-benchmark: `-benchtime=5m`.
- 50 concurrent writers via the harness's own `runParallel(b, conc, ...)`
  helper (pins goroutine count to `BenchOptions.Concurrency` instead of
  `b.RunParallel`'s GOMAXPROCS-multiplied shape so the writer count is
  stable across runner sizes).
- Setup cost (bucket-create, 100k pre-populate, 10k audit pre-age)
  happens before `b.ResetTimer()` so it does not leak into the
  measurement.
- Every sub-bench runs against a fresh `meta.Store`; the underlying
  cluster (Cassandra / TiKV) is shared but each store gets a fresh
  keyspace / namespace.

## Reproducing

Memory baseline (no docker, ~30 s):

```bash
go test -bench=. -benchtime=5s ./internal/meta/storetest/...
```

Cassandra (3-node compose, 5 min × 8 sub-benches ≈ 40 min):

```bash
make up && make wait-cassandra
go test -tags integration -bench=BenchmarkCassandraStore -benchtime=5m \
    -timeout=60m ./internal/meta/cassandra/...
```

TiKV (compose stack via `make up-tikv`, similar duration):

```bash
make up-tikv && make wait-pd && make wait-tikv
STRATA_TIKV_TEST_PD_ENDPOINTS=127.0.0.1:2379 \
go test -tags integration -bench=BenchmarkTiKVStore -benchtime=5m \
    -timeout=60m ./internal/meta/tikv/...
```

Smoke variant (drops `ListSize` to 1k, `AuditPreload` to 1k):

```bash
go test -bench=. -benchtime=10s -short ./internal/meta/storetest/...
```

## Numbers

### Memory baseline (in-tree)

Apple M3 Pro, single-process, `go test -bench=. -benchtime=2s`:

| Op                             | ns/op    | ops/s     |
| ------------------------------ | -------- | --------- |
| `CreateBucket`                 | 1 348    | 740 k     |
| `GetObject`                    | 138      | 7.2 M     |
| `ListObjects_100k`             | 1.93 ms  | 520       |
| `CompleteMultipartUpload`      | 2 337    | 430 k     |
| `GetIAMAccessKey`              | 107      | 9.4 M     |
| `AuditAppend`                  | 528      | 1.9 M     |
| `AuditSweepPartition` (10k)    | 2.36 ms  | 420       |
| `RangeScanObjects_100k`        | 2.16 ms  | 460       |

The memory numbers floor what a network-backed backend can hit — they
expose contention on a process-local map under `sync.RWMutex`, nothing
more. Network round-trips dominate everything below.

### Cassandra vs TiKV (operator-measured)

Reproduce on a 3-node stack of each backend using the commands above and
file the numbers in this table on the same SHA. The expected shape from
the architectural design (US-005, US-018):

| Op                           | Cassandra (target)  | TiKV (target)      | TiKV/Cassandra |
| ---------------------------- | ------------------- | ------------------ | -------------- |
| `CreateBucket`               | 5–10 ms (LWT Paxos) | 3–5 ms (pessim. txn) | ~1.5–2× faster |
| `GetObject` (latest)         | 1–2 ms              | 1–2 ms             | ~equal         |
| `ListObjects` 100k page=1k   | 150–300 ms          | 30–50 ms           | **5–6× faster** |
| `CompleteMultipartUpload`    | 5–10 ms (LWT Paxos) | 3–5 ms (pessim. txn) | ~1.5–2× faster |
| `GetIAMAccessKey`            | 0.5–1 ms            | 0.5–1 ms           | ~equal         |
| `AuditAppend`                | 1–2 ms              | 1–2 ms             | ~equal         |
| `AuditSweepPartition` (10k)  | 0 (USING TTL)       | 200–400 ms (sweeper) | Cassandra wins (no work) |

The structural takeaway is in `ListObjects`: Cassandra's `objects` table
is partitioned by `(bucket_id, shard)` (default 64 shards), so listing
fans out 64 concurrent partition scans + heap-merges by clustering
order. TiKV's range scan against a single ordered key prefix issues one
RPC and streams results — no fan-out, no merge. The 5–6× headline
follows from this; small-object hot paths (`GetObject`, `GetIAMAccessKey`)
are dominated by network RTT and look comparable.

The audit sweeper line is the only place Cassandra wins outright:
`USING TTL` lets the storage engine drop expired rows during compaction
with no application-side work, while TiKV has no native TTL and Strata's
audit sweeper has to enumerate + delete partitions explicitly. Mostly
moot — both run in the background outside the request path.

## How to update

When closing a perf-impacting story, re-run both backends with
`-benchtime=5m` and update the operator-measured table with absolute
numbers + the SHA of the closing commit. Drop the "(target)" tag once
real numbers replace expected ranges.

If a sub-benchmark needs a new shape (e.g. SSE-encrypted GET, KMS
rewrap), add it to `internal/meta/storetest/bench.go` so memory +
Cassandra + TiKV all pick it up at once. Per-backend bench files
should stay thin wrappers.
