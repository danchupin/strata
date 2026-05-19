---
title: 'ADR-0004: One leader lease per background worker'
weight: 4
---

# ADR-0004: One leader lease per background worker

## Status

Accepted — April 2026

(Reconsideration tracked under
[ROADMAP `## Consolidation & validation`](https://github.com/danchupin/strata/blob/main/ROADMAP.md#consolidation--validation).
If the supervisor collapse lands, this ADR will be marked `Superseded
by ADR-XXXX`.)

## Context

`strata server` ships roughly ten background workers — `gc`,
`lifecycle`, `replicator`, `notify`, `access-log`, `inventory`,
`audit-export`, `manifest-rewriter`, `rebalance`,
`quota-reconcile`, `usage-rollup`. Each of them must run on at most
one replica at a time: GC must not double-delete, lifecycle must
not duplicate transitions, etc. Some shape of singleton election is
required regardless of the deploy footprint.

The alternative shapes considered:

- **Single "control" worker per replica, internally fanning out to
  every sub-worker.** One global leader lease selects the active
  control replica; sub-worker scheduling lives inside the elected
  process.
- **One leader lease per worker, independently elected.** Each
  worker on each replica races for its own `<name>-leader` lease
  via `leader.Session`; the supervisor merely supervises the
  goroutines and recovers panics.

The first is simpler operationally (one chip to watch, one
heartbeat) but tightly couples worker lifetimes: a panic in one
worker pulls the control process down, lease-loss flips every
worker at once, and one replica owns all background load.

## Decision

Each worker is leader-elected separately on a `<name>-leader` lease
via `leader.Session`. `workers.Supervisor` (in
`cmd/strata/workers/`) spawns one goroutine per requested worker;
each goroutine acquires its own lease, builds the runner under a
supervised context, runs it, releases on exit, and recovers from
panics with exponential backoff (1s → 5s → 30s → 2m, reset after 5
min healthy). Sharded fan-out workers (gc, rebalance) declare
`SkipLease: true` and own their per-shard `<name>-leader-<i>`
leases internally; the supervisor still owns panic recovery and
backoff, only the outer lease is skipped.

## Consequences

- **Fault isolation.** A panic in `notify` does not affect `gc` or
  the gateway. Lease loss restarts the affected worker only —
  sibling workers and the HTTP listener keep serving.
- **Per-worker scaling knob.** `STRATA_WORKERS=gc,lifecycle` runs
  exactly those two workers on a replica; another replica can opt
  into `STRATA_WORKERS=notify,access-log`. Operators tune the load
  shape per replica without forcing a homogeneous fleet. The
  gateway-only deploy is `STRATA_WORKERS=` (empty).
- **Independent observability per worker.** The supervisor emits an
  `acquired` / `released` heartbeat per worker
  (`leader_for=<name>`), `strata_worker_panic_total{worker=<name>,
  shard=<i>}` exposes panic rates per worker, and OTel iteration
  spans (`worker.<name>.tick`) carry `strata.worker=<name>` for
  filtering.
- **Higher Cassandra LWT churn.** Each lease is held via LWT-backed
  heartbeat; ten leases × N replicas multiplies the heartbeat
  traffic compared to a single global lease. In practice the
  heartbeat cadence is generous enough (seconds, not
  milliseconds) that the churn is invisible against the data-plane
  write rate — but it is a real cost that the consolidation
  reconsideration takes seriously.
- **Reconsideration path.** A future supervisor that owns a single
  `strata-control-leader` lease and dispatches to internally
  scheduled sub-workers is on the table. Migration would (a) flip
  the `Build` constructors to expose tick functions instead of
  long-lived runners, (b) replace the per-worker leases with one
  global lease and a deterministic in-process scheduler, and (c)
  retain `STRATA_WORKERS=` as the enable mask. This ADR's
  `Superseded by` line is reserved for that change.
