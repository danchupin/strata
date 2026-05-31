# Test discipline

Adapted from CockroachDB's `.claude/rules/{go-test,table-driven-test}.md` and
`skip-test-with-issue` skill — kept only what fits Strata (Go stdlib `testing`,
ROADMAP instead of a GitHub-issue tracker, our own harnesses).

## Never weaken an assertion to go green

A failing test has usually found a **real bug in the production code**, not a
problem with the test. When a test goes red:

1. **Read the production source first.** Decide whether the test caught a genuine
   defect.
2. **Fix the source** if there's a bug there. Fix the **test** only if the test
   logic itself was wrong. If unsure, surface it — don't guess.
3. **Never** loosen, delete, or `t.Skip` an assertion just to make the suite
   pass. That buries the signal.

This is the codebase's standing "don't mask bugs" rule, made explicit. US-002 of
the QA cycle is the canonical example: a red conditional-request test exposed a
real RFC 7232 precedence bug in `conditional.go` — the fix went into the source,
not the test.

## Bug fixes need red/green proof

When fixing a bug, the change MUST include a test that **fails before** the fix
and **passes after**. Observe both states (run the test against the unpatched
source, then the patched). Note it in the commit / `progress.txt` so reviewers
can trust the test actually exercises the fix.

## Every `t.Skip` cites a tracker + a specific reason

No bare or vague skips. Each `t.Skip`/`t.Skipf` must state **why** and point at a
durable record:

- A **ROADMAP P-item** for a known gap/bug (Strata tracks in `ROADMAP.md`, not
  GitHub issues): `t.Skip("dedup not implemented yet — ROADMAP P2 content-addressed dedup")`.
- Or a **concrete env condition** for integration-only tests:
  `t.Skip("ceph not reachable; integration-only")`, `t.Skipf("set STRATA_SCYLLA_TEST=1")`.

**Re-validate skips when you touch the area.** If the stated condition no longer
holds, remove the skip. A stale guard is a coverage hole that reads as "covered":
e.g. `sse_object_test.go:207` skipped corruption "because memory lacks
`CorruptFirstChunk`" — but memory has had it since `backend.go:148`; the skip was
silently dropping a real assertion.

## Table-driven canons

- **Inputs first, then `expected`-prefixed fields.** Field order = inputs at top,
  verification fields below, every output field named `expected*` so a reader tells
  inputs from outputs at a glance.
- **One concern per case.** Don't pack normal + edge + special into a single row.
- **Name = intent, not data.** `"suffix range past object size"`, not
  `"bytes=-99"` or `"test3"`. The name alone should say what the case proves.
- **Only specify fields the case exercises.** Leave irrelevant struct fields at
  zero — noise hides the one value under test.
- **Cases are independent.** No state carried across rows.

## Assertions

- Prefer `require.*` (halts on failure) for preconditions and anything that makes
  later asserts meaningless. Use `assert.*` only when you deliberately want several
  independent failures surfaced in one run.
- Compare values, not bare booleans: `require.Equal(t, want, got)` /
  `require.ErrorIs(t, err, ErrSignatureInvalid)` so the failure output shows the
  actual value, not just "false".

## Reuse harnesses — don't fork

No parallel test harnesses (FR-2 no-duplication). Extend the nearest existing one:

- `internal/s3api/testutil_test.go` — `newHarness(t)`, `h.doString`, `h.mustStatus`.
- `internal/meta/storetest` — `Run(t, factory)` contract, run on TiKV + memory.
- `internal/racetest` — `workload.go` + `tracker.go` (RecordIntent before Complete
  for the multipart etag) for concurrency scenarios under `-race`.

## CI is the source of truth

Many failure modes (cgo/Xcode locally, gosec cephimpl, Cassandra Paxos
starvation, SSE-KMS, drain/rebalance) don't reproduce on a local macOS/lima box.
"Passes locally" is necessary but not sufficient — verify on the CI (Linux) run.
