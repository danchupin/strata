# PRD: gc + lifecycle worker concurrency (Phase 1 — internal parallelism)

## Introduction

Both `internal/gc/worker.go::drainCount` and `internal/lifecycle/worker.go` walk their work queues with a single goroutine and a `for _, e := range entries { … }` body that blocks on a sequential RADOS round-trip + meta-store ack on every iteration. At ~10 ms RADOS delete + ~5 ms meta ack per chunk, a single gc worker tops out around 50–200 chunks/s; lifecycle saturates around 100–500 objects/s under similar latencies. For prod-scale churn (e.g. 10k object PUTs/s × ~4 chunks each = 40k chunks/s deletion), the queue grows linearly forever and the cluster goes underwater within hours.

**Phase 1 (this cycle)** lifts the per-worker throughput cap by ~32× via a bounded `errgroup` inside the existing single-leader worker — no change to leader-election semantics. Phase 2 (separate future cycle, queued in `ROADMAP.md`) adds sharded leader-election so multiple replicas process disjoint slices in parallel. We do Phase 1 first because it's the simpler win and the bench harness it lands gives Phase 2 quantified targets.

## Goals

- **`STRATA_GC_CONCURRENCY`** env (default 1, max 256) — bounded parallelism inside the existing gc worker. `errgroup` with `SetLimit(N)`. Each goroutine handles one GCEntry's `rados.Delete` + `meta.AckGCEntry`.
- **`STRATA_LIFECYCLE_CONCURRENCY`** env (default 1, max 256) — same shape inside lifecycle worker for object expiration / transition.
- **Bench harness `cmd/strata-bench-gc`** + **`cmd/strata-bench-lifecycle`** — populate N synthetic entries, drain them, report `chunks_per_second` / `objects_per_second` to stdout (JSON) + Prometheus `strata_gc_bench_throughput` gauge.
- **`docs/benchmarks/gc-lifecycle.md`** — measured throughput curve at concurrency = 1 / 4 / 16 / 64 / 256 against the lab-tikv stack.
- **ROADMAP P1 entry flipped Done at cycle close**, with the measured numbers cited.

## User Stories

### US-001: STRATA_GC_CONCURRENCY — bounded errgroup inside gc.Worker.drainCount
**Description:** As an operator running gc against a high-churn cluster, I want to parallelise chunk deletes inside the single elected gc leader so that the throughput cap rises ~32× without any change to leader-election semantics.

**Acceptance Criteria:**
- [ ] `gc.Worker` gains a `Concurrency int` field; zero/negative falls back to 1 (current sequential behaviour).
- [ ] `cmd/strata/workers/gc.go` reads `STRATA_GC_CONCURRENCY` at Build time (sibling of existing `STRATA_GC_INTERVAL` / `STRATA_GC_BATCH_SIZE`); parses to int; clamps to `[1, 256]`; passes to `gc.Worker.Concurrency`.
- [ ] `gc.Worker.drainCount` replaces the inner `for _, e := range entries { … }` block with `errgroup.WithContext(ctx)` + `eg.SetLimit(w.Concurrency)`. Each iteration `eg.Go(func() error { … delete + ack … })`. Errors are logged (same as today) but do NOT cancel siblings — the loop drains as much as possible per tick. `processed` counter mutated under `atomic.Int64` so the return value is correct.
- [ ] On per-entry failure (delete or ack), behaviour matches today: log warn + continue. Do NOT fail the batch.
- [ ] Existing `internal/gc/worker_test.go` keeps its sequential semantics (Concurrency=0/1 path); add `TestWorker_DrainConcurrency` that drives 1k entries with Concurrency=32 against the in-memory `data.Backend` + `meta.Store` and asserts wall-clock < 4× the per-entry latency × 1k / 32.
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: STRATA_LIFECYCLE_CONCURRENCY — bounded errgroup inside lifecycle.Worker
**Description:** As an operator running lifecycle against a bucket with millions of objects in the expiration / transition window, I want to parallelise the per-object actions inside the single elected lifecycle leader for the same reason as US-001.

**Acceptance Criteria:**
- [ ] `lifecycle.Worker` gains a `Concurrency int` field; zero/negative falls back to 1.
- [ ] `cmd/strata/workers/lifecycle.go` reads `STRATA_LIFECYCLE_CONCURRENCY` env, parses, clamps to `[1, 256]`, passes through.
- [ ] The inner per-object loop in `lifecycle.Worker.processBucket` (or whichever helper holds the `for _, o := range res.Objects { … }` block) wraps the body in `errgroup` + `SetLimit(Concurrency)`. Per-version + per-noncurrent-version inner loops too — they all do meta + data round-trips.
- [ ] CAS conflict path stays the same: `Store.SetObjectStorage` returning `applied=false` discards the freshly-written tier-2 chunks via the GC queue (existing behaviour). Concurrent siblings see different `expectedClass` snapshots — the LWT enforces correctness regardless of order.
- [ ] AbortIncompleteMultipartUpload sweep parallelises too (same shape, errgroup over `for _, u := range uploads`).
- [ ] Existing `internal/lifecycle/worker_test.go` keeps its sequential semantics; add `TestWorker_TransitionConcurrency` exercising the 32-concurrent transition path against in-memory backends.
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: Bench harness — cmd/strata-bench-gc + cmd/strata-bench-lifecycle
**Description:** As a maintainer, I want a one-shot benchmark harness that quantifies the throughput curve for both workers across concurrency levels so the Phase 1 win is documented and Phase 2 targets are quantified.

**Acceptance Criteria:**
- [ ] New `cmd/strata-bench-gc/main.go`: connects to a configured `meta.Store` + `data.Backend` (via `STRATA_*` envs, same shape as `strata server`); pre-seeds N synthetic GCEntry rows (default N=10000, override via `--entries=`); runs the gc worker once with `--concurrency=N` (default 1); reports `{entries, concurrency, elapsed_ms, throughput_per_sec}` as a single JSON line on stdout and as a `strata_gc_bench_throughput{concurrency="N"}` Prometheus gauge published to the configured push gateway (`STRATA_PROM_PUSHGATEWAY` env, optional).
- [ ] Same shape `cmd/strata-bench-lifecycle/main.go`: pre-seeds a bucket with N objects with a 1-day expiration rule + an Mtime in the past; runs lifecycle once with the configured concurrency; reports as above.
- [ ] Both bench commands clean up state at exit (`defer` block deletes seeded entries / objects) so the same lab can run benchmarks repeatedly without disk leak.
- [ ] `Makefile` adds `bench-gc` + `bench-lifecycle` targets that run the bench against `up-lab-tikv` for `--concurrency=1,4,16,64,256` (one process per concurrency level) and `tee` the JSON-line output to `bench-gc-results.jsonl` / `bench-lifecycle-results.jsonl`.
- [ ] Bench results land in `docs/benchmarks/gc-lifecycle.md` (US-005 — separate doc story).
- [ ] Typecheck passes
- [ ] Tests pass (the bench commands themselves don't have unit tests — they ARE the tests; add a smoke test ensuring they exit 0 against in-memory backends with --entries=10).

### US-004: Run the bench against lab-tikv + capture throughput curve
**Description:** As a maintainer, I want the actual numbers measured on the canonical lab-tikv stack so the Phase 1 win is concrete (not a hypothetical 32× speedup).

**Acceptance Criteria:**
- [ ] Run `make up-lab-tikv && make wait-strata-lab && make bench-gc && make bench-lifecycle` from a fresh checkout.
- [ ] Capture the `bench-gc-results.jsonl` + `bench-lifecycle-results.jsonl` outputs in `docs/benchmarks/gc-lifecycle.md` as a Markdown table:
      ```
      | concurrency | gc chunks/s | lifecycle objects/s |
      |-------------|-------------|---------------------|
      | 1           | …           | …                   |
      | 4           | …           | …                   |
      | …           | …           | …                   |
      ```
- [ ] Document the bottleneck observed at each tier: meta ack at low concurrency, RADOS contention at high, OR Cassandra/TiKV LWT throughput at very high.
- [ ] Identify the "knee" — concurrency above which per-additional-goroutine yield is < 10 % of the previous step. That's the recommended `STRATA_*_CONCURRENCY` default in production.
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: docs + ROADMAP close-flip + cycle merge
**Description:** As an operator, I want the new envs documented and the ROADMAP entry closed so the cycle is fully wrapped.

**Acceptance Criteria:**
- [ ] New `docs/benchmarks/gc-lifecycle.md` covers the harness command, methodology, results table from US-004, recommended concurrency defaults, and the tradeoff for very-high concurrency (memory + connection pressure on the meta backend).
- [ ] `docs/operators.md` (or whichever exists) documents `STRATA_GC_CONCURRENCY` + `STRATA_LIFECYCLE_CONCURRENCY` envs alongside the existing `STRATA_GC_INTERVAL` / etc.
- [ ] ROADMAP P1 entry (`gc / lifecycle workers serialise inside a single goroutine`) flipped Done with one-line summary citing the measured numbers + commit SHA. Phase 2 (sharded leader-election) gets a NEW P1 entry under `## Scalability & performance` — surfaced + ready for the next cycle prep.
- [ ] **Cycle-end merge**: ff-only merge `ralph/gc-lifecycle-scale` into `main`, push origin/main; `archive_cycle` snapshots `prd.json + progress.txt` under `scripts/ralph/archive/2026-MM-DD-gc-lifecycle-scale-complete/` on `<promise>COMPLETE</promise>`. Mirror the multi-replica-cluster close shape (commit `0dee924`).
- [ ] Markdown PRD `tasks/prd-gc-lifecycle-scale.md` REMOVED in close-flip commit per CLAUDE.md PRD lifecycle rule.
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- **FR-1**: `STRATA_GC_CONCURRENCY` env, default 1, parsed as int, clamped to [1, 256], read at gc worker Build time.
- **FR-2**: `STRATA_LIFECYCLE_CONCURRENCY` env, same shape.
- **FR-3**: gc worker uses `golang.org/x/sync/errgroup` with `SetLimit(N)` inside `drainCount`. Per-entry errors logged but do not cancel siblings; `processed` counter under `atomic.Int64`.
- **FR-4**: lifecycle worker uses errgroup with `SetLimit(N)` inside the per-object / per-version / per-multipart-upload loops.
- **FR-5**: Both bench commands accept `--entries=N --concurrency=N --backend=tikv|cassandra|memory --data=rados|memory|s3` flags; pre-seed state, run drain, report JSON line on stdout + optional pushgateway.
- **FR-6**: `make bench-gc` / `make bench-lifecycle` targets run the bench harness across concurrency tiers and `tee` JSON to results files.
- **FR-7**: Default concurrency in `cmd/strata server` deployment stays 1 (no behaviour change for existing operators); bumping is opt-in via env.

## Non-Goals

- **No sharded leader-election in this cycle.** Phase 2 territory; new gc-leader-{0..N-1} keys + per-bucket lifecycle leases land in a separate ralph cycle, queued in ROADMAP.
- **No new `meta.Store` interface methods.** All concurrency lives inside the worker; the queue read API (`ListGCEntries`, etc.) stays single-call.
- **No backpressure on enqueue.** If the cluster's GC queue still grows faster than the parallelised drain, that's a Phase 2 problem (sharding) — not solved by tweaking concurrency higher than the meta backend can accept LWT writes.
- **No automatic concurrency tuning.** Operator picks via env; no adaptive ramp-up.
- **No bench against full-replica HA setup.** Single replica + lab-tikv is sufficient to measure per-worker speedup; multi-replica throughput comes with sharding (Phase 2).
- **No replication / notify / access-log / audit-export workers in this cycle.** Those have different bottlenecks (network egress for replication, queue drain for notify) and warrant separate analysis.

## Design Considerations

- **errgroup + SetLimit** is the canonical Go idiom for bounded parallelism. `golang.org/x/sync/errgroup` is already in `go.mod` (used by `internal/s3api/multipart.go`). No new dep.
- **Per-entry log on failure, not whole-batch fail.** Matches today's behaviour. The whole point is "drain as much as possible per tick" — abandoning the batch on the first failure halves throughput when one OSD flakes.
- **`atomic.Int64` for the `processed` counter.** Cleaner than `sync.Mutex` for a single counter; matches Go idiom for atomic increments.
- **Concurrency cap = 256.** Above that the meta backend's LWT throughput (Cassandra ~1k/s per node, TiKV ~5k/s per region) becomes the bottleneck — pumping more goroutines at it just queues. Phase 2 sharding is the right move at that scale.
- **Bench harness is one-shot, not a long-running service.** No leader-election, no heartbeat, no `/readyz` — runs to completion + exits. This is testing scaffolding, not a worker.

## Technical Considerations

- **`gc.Worker.drainCount` already returns the count.** Wrap the inner loop in errgroup; outer per-batch logic (paginate `ListGCEntries` until short batch) stays unchanged. Test path: in-memory `data.Backend` + `meta.Store` give synchronous deletes — concurrency win is measurable but smaller; against RADOS/TiKV the round-trip latency dominates and the speedup approaches Concurrency × (within meta backend's LWT cap).
- **Lifecycle worker has nested loops** (bucket → rule → object → version). Apply errgroup at the per-object level (most leaf-y); the per-bucket loop stays sequential because cross-bucket parallelism is Phase 2 territory (per-bucket sharded leaders) and applying errgroup at the bucket level would pin the elected leader as a wider bottleneck.
- **Per-entry failures don't cancel siblings.** errgroup's default behaviour DOES cancel the parent ctx on first error. Use `errgroup.Group` with the ctx returned BUT capture errors per-goroutine into a `slog.Logger.Warn` line + return nil from the goroutine — the group never sees an error, so the ctx is never cancelled. (This is the standard "log + continue" pattern.)
- **Bench harness pushgateway hop is optional.** Only push if `STRATA_PROM_PUSHGATEWAY` is set; otherwise stdout JSON is the only output. CI doesn't have a pushgateway and shouldn't need one.
- **Lab-tikv stack** (TiKV + RADOS) is the canonical bench target. Cassandra path probably has different LWT throughput but the same shape; future work — bench against `up-all` profile too.
- **Worker supervisor restart semantics** (CLAUDE.md): a panic in the parallelised inner goroutine MUST NOT escape — wrap each `eg.Go` body in `defer recover() { … log + return nil }` so a single bad entry doesn't crash the worker. The supervisor's panic backoff (1s → 5s → 30s → 2m) is for top-level panics only; per-entry panics are noise.

## Success Metrics

- gc throughput at concurrency=64 measures ≥ 16× the concurrency=1 baseline against lab-tikv (RADOS round-trip dominates; theoretical upper bound 64×, but meta ack contention will eat some).
- lifecycle throughput at concurrency=64 measures ≥ 16× the concurrency=1 baseline.
- The "knee" of the curve (where additional concurrency yields < 10 % per-step improvement) lands at 32–64 for both workers; that becomes the recommended production default in `docs/benchmarks/gc-lifecycle.md`.
- ROADMAP P1 entry flipped Done with the actual numbers cited.
- Operators can opt in via a single env flip without rebuilding the binary.

## Open Questions — RESOLVED before cycle launch

- **Default concurrency value** — RESOLVED: stays at 1. The cycle's win is the operator can opt in; changing the default is a behavioural change that needs more bench coverage (Cassandra path, multi-replica HA) before flipping. Phase 2 cycle revisits.
- **`atomic.Int64` vs sync.Mutex** — RESOLVED: atomic. Counter is the only shared state; lock contention scales linearly with goroutine count and there's no compound state.
- **errgroup ctx cancellation behaviour** — RESOLVED: log-and-continue pattern (return nil from each goroutine; capture error in a logger line). The operator wants drain progress, not first-error abort.
- **Bench against Cassandra path** — RESOLVED: future cycle. Lab-tikv is the canonical bench target this cycle; Cassandra LWT throughput differs but the speedup-curve shape is similar.
- **Per-bucket lifecycle parallelism** — RESOLVED: Phase 2. Within-worker parallelism (this cycle) targets the per-object axis; per-bucket shards across replicas (Phase 2) target the per-bucket axis. Independent improvements.
