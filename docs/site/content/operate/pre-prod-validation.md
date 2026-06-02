---
title: 'Pre-prod validation walkthrough'
weight: 60
description: 'One e2e pass across both labs (TiKV + Cassandra) exercising sharding/reshard, DeleteObjects, policy-UNION-ACL, the console reshard UI, and chunk-corruption fail-loud — the GO/NO-GO evidence run.'
---

# Pre-prod validation walkthrough

This is the release-owner runbook: a **single documented pass** that brings
up both production labs and exercises the whole hardened object path so the
GO/NO-GO decision rests on observed behaviour, not story-by-story claims. It
is the operator-facing companion to the per-feature smokes — it composes them
into one walkthrough and records a pass/fail-per-leg readiness note.

The composite is `scripts/smoke-architecture-hardening.sh`, wired as
`make smoke-architecture-hardening`. Each leg is independent: it WARN-skips
when its lab is down and only fails the run on a real assertion failure. Set
`REQUIRE_LAB=1` to promote a down lab to a hard fail (use this in CI where the
labs are guaranteed up).

## What it exercises

| Leg | Proves | Backend |
|-----|--------|---------|
| **A** | Per-bucket **non-default shard count** + online **64→128 reshard under concurrent writes** + crash-resume | Cassandra (`:9998`) |
| **B** | Reshard **parity** — immediate-complete no-op, object still readable | TiKV (`:9999`) |
| **C** | **DeleteObjects** batch (idempotent, missing key = success row) + versioned delete → `DeleteMarker` | either lab |
| **D** | **Policy UNION ACL** — anonymous GET denied pre-policy, granted by an explicit `s3:GetObject` Allow to `Principal:"*"` | either lab |
| **E** | **Console reshard UI** — `/admin/v1` reports `supported=true` (Cassandra) / `false` (TiKV, UI-disabled) and queues a job | both labs |
| **F** | **Plaintext chunk-corruption fail-loud** — a flipped byte surfaces `ErrChecksumMismatch` on the CRC32C read path, never silent-truncate | data layer (no lab needed) |

Leg A reuses `scripts/smoke-reshard.sh` verbatim (no fork). Leg F runs the
`internal/data/{memory,rados}` CRC verification tests directly
(`CGO_ENABLED=0` — those packages build without librados): a live gateway
exposes **no byte-flip hook by design** (that would be a data-integrity hole),
so the corruption is injected at the backend test seam and the read path is
asserted to fail loud.

## Bring up the labs

The reshard worker **must** be enabled on each gateway or the reshard legs
hang (the job stays queued):

```bash
# TiKV-default lab on :9999
STRATA_WORKERS=gc,lifecycle,rebalance,reshard make up-all && make wait-strata-lab

# Cassandra-profile lab on :9998
STRATA_WORKERS=gc,lifecycle,rebalance,reshard make up-cassandra && make wait-cassandra
```

## Run the walkthrough

```bash
export STRATA_STATIC_CREDENTIALS=admin:adminpass:owner      # same value the gateway booted with
export STRATA_RESHARD_ROOT_CRED=<access:secret>             # owner == iam-root, for /admin/bucket/*
make smoke-architecture-hardening
```

Env knobs (all optional): `TIKV_BASE`, `CASS_BASE`, `SMOKE_AH_TARGET`
(reshard target, default 128), `SMOKE_AH_JOB_GRACE`, `WAIT_GRACE`,
`REQUIRE_LAB`.

Leg D needs the lab's default `STRATA_AUTH_MODE=optional` so an anonymous
request reaches the authorization gate — the leg first asserts the anon GET is
**denied** before the policy, so a lab that doesn't gate object reads is
flagged inconclusive rather than passing vacuously.

## Browser pass

The console reshard UI is also covered by the Playwright spec
`web/e2e/reshard-progress.spec.ts` (trigger → in-progress → complete on a
supported backend, plus the TiKV disabled state). That spec runs in CI — its
`webServer` boots `go run ./cmd/strata server`, which cgo-blocks on a
macOS/lima box, so the browser leg is **CI-only** locally; `npx playwright
test --list` validates the spec parses without booting the gateway.

## Capture the outcome

Record the pass/fail-per-leg result in
[`tasks/architecture-hardening-readiness.md`](https://github.com/danchupin/strata/blob/main/tasks/architecture-hardening-readiness.md)
— that note is the GO/NO-GO evidence document for the cycle, the same role
`tasks/qa-readiness-report.md` plays for the QA cycle.
