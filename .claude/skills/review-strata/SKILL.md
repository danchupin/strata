---
name: review-strata
description: Orchestrate Strata's domain reviewer subagents over a change — correctness (foreground) + tests/s3-compat/errors/simplifier (parallel), severity-aggregated. Use PROACTIVELY before committing a non-trivial change, and whenever the user asks to review a diff, branch, or PR. Args optional: a subset of aspects (e.g. "correctness tests"), or a PR number / branch / paths.
---

# Review Strata

Structured, in-session code review for Strata. Dispatches the domain reviewer
subagents in `.claude/agents/` and aggregates their findings by severity. Modelled
on CockroachDB's `review-crdb`, tuned to this repo.

This is the **entry point** for review — the `.claude/agents/strata-*` reviewers are
not auto-invoked. `/code-review ultra` is the separate cloud whole-branch review.

## 1. Determine scope

From the args, in priority order:
- **Explicit paths / a PR number / a branch** in args → review that. For a PR, use
  `gh pr diff <n>` (and `gh pr view <n>` for context).
- **No scope arg** → review the current change: `git diff main...HEAD` plus any
  uncommitted changes (`git diff` + `git status --short`). If on `main` with a clean
  tree, ask the user what to review.

Collect the changed files and the diff once; pass them to every agent so they don't
re-derive scope. Note the scope you chose in the output.

## 2. Select aspects

Default = all applicable. A leading args token that names aspects runs only those
(e.g. `/review-strata correctness tests`). Aspect → agent, with conditional gating:

| Aspect | Agent | Run when |
|--------|-------|----------|
| correctness | `strata-correctness-reviewer` | **always** (foreground) |
| tests | `strata-test-reviewer` | any `*_test.go` changed, or new logic lacks tests |
| s3-compat | `strata-s3-compat-reviewer` | `internal/s3api/` or S3 surface changed |
| errors | `strata-error-reviewer` | error paths / sentinels / `errors.go` changed |
| simplifier | `strata-simplifier` | only on larger changes, or when asked |

If an aspect isn't applicable to the diff, skip it and SAY SO in the output
("skipped: s3-compat — no s3api changes"). Never silently omit.

## 3. Dispatch

- Run `strata-correctness-reviewer` in the **foreground** (it reads the most context
  and is the gating lens).
- Launch the other selected agents in the **background** (`Agent` with
  `run_in_background: true`), one per aspect, in a single message so they run
  concurrently. Rely on the completion notifications — do NOT poll in a tight loop.
- Pass each agent: the scope/diff, the changed-file list, and a pointer to
  `.claude/rules/` + `CLAUDE.md` for the standing conventions.
- **Failure handling:** if an agent errors, retry it once. If it still fails, report
  it as "aspect skipped: <agent> — <reason>" — never drop it silently.

## 4. Aggregate

Collect all findings and present a single report:

- **Blocking** — must fix (data loss/corruption, broken invariant, deadlock/leak,
  silent data-plane failure). Each with `file:line`, the failure scenario, the fix
  direction, and which agent found it.
- **Suggestions** — should fix.
- **Nits** — only if the user asked for a thorough pass (prefix `nit:`); otherwise
  omit.
- **Strengths / verified** — what the reviewers confirmed correct (which invariants
  hold). A clean review is a valid result.

Dedupe findings that multiple agents surface (report once, note the agents). Order
blocking-first. End with a one-line verdict: ship / fix-blocking-first / needs-tests.

## 5. Standing context (remind every agent)

Strata is **pre-launch** — no users/data, hard cutovers fine. Do NOT raise
backwards-compat, rolling-upgrade / cluster-version, or migration-backfill concerns.
The deliberate `s3-tests` gaps (SigV2, boto3 prefix-decode, anonymous-list) stay
deliberate. CI (Linux) is the source of truth for anything that doesn't reproduce
locally.

## Optional: post to a PR

If reviewing a GitHub PR and the user asks to post, batch all findings into ONE
review via `gh api` with inline comments tagged by severity
(`review-strata(blocking)`, `review-strata(suggestion)`). Default is to print the
report in-session, not post.
