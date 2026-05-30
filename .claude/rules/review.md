# Review discipline

Adapted from CockroachDB's `review-crdb` skill + reviewer agents — kept the
severity-gating and aspect-decomposition, dropped the CRDB-specific concerns
(cluster-version gating, redactability) that don't apply to a pre-launch gateway.

## Severity-gate the findings — signal over noise

Report findings in three tiers, and **suppress the bottom tier unless asked**:

- **blocking** — must fix. Correctness, data loss/corruption, security, a broken
  invariant. The only tier that should ever block a merge.
- **suggestion** — should fix. Real but lower-impact: a missing test, an awkward
  abstraction, a leak on an error path.
- **nit** — style/wording/preference. Do NOT emit these unless the user asks for a
  thorough pass; if you do, prefix with `nit:`.

Don't drown a real blocking issue under ten nits. If there are no high-confidence
issues, say so plainly and state what you verified — a clean review is a result,
not a failure to find something.

## Never silently omit an aspect

If a review aspect couldn't run (tool failed, area out of scope, file unreadable),
**name it**: "skipped: <aspect> — <reason>". A silent omission reads as "reviewed
and clean" when it wasn't.

## Strata's high-value correctness lenses

These are where this codebase actually breaks — weight them first:

- **LWT / CAS read-after-write coherence** — a plain `Put`/`UPDATE` after an LWT
  insert leaves Paxos/txn state stale (Cassandra LWT-on-LWT, TiKV pessimistic-txn).
  Any RMW that's later read at quorum must itself be LWT / pessimistic-txn.
- **Drain / stop-write invariant** — PUT into a draining/evacuating cluster must
  503 `DrainRefused`; reads/deletes/HEAD/in-flight-multipart keep working.
- **GC / refcount** — chunk deleted exactly once; dual-write to `_by_cluster`
  lookup tables stays in lockstep; no double-delete, no leak.
- **Concurrency** — mutex discipline, goroutine lifecycle, channel sizing, early
  returns that leak a txn lock (`txn.Rollback()` on non-error early return).
- **Resource leaks** — `Close()`/`defer` on every path, including error paths.
- **Auth boundary** — SigV4/presigned/streaming chain-HMAC rejects every tamper
  class; the secure default (`required`) denies anonymous.

## Aspect decomposition (deep reviews)

For a thorough multi-agent review, split into specialized lenses rather than one
generalist pass: **correctness/concurrency** (foreground — reads the most
context), plus **tests**, **errors/sentinels**, **conventions** in parallel.
Each finding gets a severity tag. This mirrors `/code-review ultra`'s cloud
multi-agent shape; for a local pass, fan out subagents per lens.

## Pre-launch — skip these concerns entirely

Strata has no users/data; hard cutovers are fine. Do **not** raise:
backwards-compat shims, rolling-upgrade / cluster-version gating, migration
backfills, or "this breaks existing deployments". Flagging them here is noise.
