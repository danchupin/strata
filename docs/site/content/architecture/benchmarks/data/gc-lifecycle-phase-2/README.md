# Phase 2 — gc + lifecycle multi-leader bench artifacts

JSONL captured by the bench harness in **in-process simulation mode**:
both meta and data backends are pure-memory; the multi-shard / multi-
replica race happens inside one `strata-admin` process via the `--shards`
(gc) / `--replicas` (lifecycle) flags wired in US-006.

## Files

| File | What | How to regenerate |
|------|------|-------------------|
| `sim-bench-gc-shards1.jsonl` | gc bench, single-leader (Phase 1 shape), N=50000, c∈{1,4,16,64,256} | `for c in 1 4 16 64 256; do STRATA_META_BACKEND=memory STRATA_DATA_BACKEND=memory ./bin/strata-admin bench-gc --entries=50000 --concurrency=$c --shards=1; done \| jq -c .` |
| `sim-bench-gc-shards3.jsonl` | gc bench, 3-shard (Phase 2 shape), N=50000 | …`--shards=3` |
| `sim-bench-lifecycle-replicas1.jsonl` | lifecycle bench, single-replica (Phase 1), N=10000, 1 bucket | `for c in …; do … bench-lifecycle --objects=10000 --concurrency=$c --replicas=1 --buckets=1; done` |
| `sim-bench-lifecycle-replicas3.jsonl` | lifecycle bench, 3-replica (Phase 2), N=10000, 9 buckets | …`--replicas=3 --buckets=9` |

## Caveats

- Memory meta + memory data put the worker on a fast path with effectively
  zero per-op latency. Concurrency speedup that the production lab observes
  on TiKV (real pessimistic-txn round-trips) does NOT manifest here — the
  in-process Go RWMutex is the bottleneck. Treat these numbers as a noise
  floor that pins the **shape of multi-leader coordination overhead**, not
  as a forecast of production throughput.
- For the canonical Phase 2 numbers operators should rerun
  `make bench-gc-multi` / `make bench-lifecycle-multi` against the
  3-replica lab brought up via `make up-lab-tikv-3` (see
  `docs/site/content/architecture/benchmarks/gc-lifecycle.md`'s "Phase 2 — multi-leader" section).
- Every JSON object on a line is one bench level (one `(--concurrency,
  --shards|--replicas)` combination). Schema matches `cmd/strata-admin/
  bench_common.go::benchResult`.
