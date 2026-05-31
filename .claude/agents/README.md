# Strata review agents

Project-specific reviewer subagents, modelled on CockroachDB's `.claude/agents`
but tuned to Strata's domain (S3 gateway over distributed TiKV/Cassandra metadata
and RADOS/S3 data). They encode the gotchas from `CLAUDE.md` and the discipline in
`.claude/rules/` so a review catches the bugs that actually bite this codebase.

> Invoked via the `/review-strata` orchestrator skill (see
> `.claude/skills/review-strata`), which CI's claude-review runs as a second pass
> alongside the generic code-review.

## The set

| Agent | Lens | When |
|-------|------|------|
| `strata-correctness-reviewer` | LWT/CAS coherence, txn-lock leaks, drain stop-write, placement, GC dual-write, concurrency, resource leaks | **Always** (foreground — reads the most context) |
| `strata-test-reviewer` | behavioral coverage, red/green proof, no weakened asserts, skip-with-tracker, table-driven canons, harness reuse | When tests change |
| `strata-s3-compat-reviewer` | status codes, error-XML shape, conditional/range/SSE headers, query-router, vhost, s3-tests parity | When an S3 endpoint/handler changes |
| `strata-error-reviewer` | sentinel discipline, sentinel↔APIError lockstep, best-effort vs request-failing, data-plane fail-loud | When error paths change |
| `strata-simplifier` | dead code, over-abstraction, helper reuse, single-binary invariant | Optional, on larger changes |

## Severity convention (all agents)

- **blocking** — must fix: data loss/corruption, a broken invariant, a deadlock/leak,
  a silent data-plane failure.
- **suggestion** — should fix: real but lower-impact.
- **nit** — style/preference; suppressed unless explicitly asked.

Agents report only findings they are highly confident are real, and never silently
omit an aspect — if a lens couldn't run, it says "skipped: <reason>". A clean review
states what was verified.

## How to run

- Via the `Agent` tool with `subagent_type` / `agentType` set to the agent name,
  one per lens. For a deep pass, run `strata-correctness-reviewer` in the foreground
  and fan the others out in parallel (mirrors the `review-crdb` orchestration).
- `/code-review ultra` remains the cloud multi-agent review for a whole branch/PR;
  these local agents are for targeted, in-session review.

## Standing context for every agent

Strata is **pre-launch** — no users/data, hard cutovers are fine. Do NOT raise
backwards-compat, rolling-upgrade / cluster-version gating, or migration-backfill
concerns; they are noise here. Read `CLAUDE.md` (gotchas) and `.claude/rules/`
(test + review discipline) before reviewing.
