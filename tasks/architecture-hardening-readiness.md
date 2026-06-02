# Strata — Architecture-Hardening Readiness Note

**Cycle:** `ralph/architecture-hardening` · **Story:** US-011 (e2e pre-prod
validation walkthrough) · **Date:** 2026-06-02

This is the GO/NO-GO evidence document for the architecture-hardening cycle. The
release decision rests on the **observed** result of the composite walkthrough
below, not on the per-story `passes: true` flags. It plays the same role for
this cycle that `tasks/qa-readiness-report.md` plays for the QA cycle.

**Source of truth = CI on Linux** (FR-4). The author's box is macOS/lima: the
Xcode license is not accepted, so any package pulling cgo (`cmd/strata`,
`internal/serverapp`, `internal/meta/tikv`, `internal/metrics`) fails to build,
and `go run ./cmd/strata server` cannot boot — the live-lab legs (A–E) therefore
run on CI / a real lab, not the laptop. Leg F (data-layer CRC) builds
`CGO_ENABLED=0` and was observed locally.

## How to reproduce

```bash
# Both labs, reshard worker enabled:
STRATA_WORKERS=gc,lifecycle,rebalance,reshard make up-all       && make wait-strata-lab
STRATA_WORKERS=gc,lifecycle,rebalance,reshard make up-cassandra && make wait-cassandra

export STRATA_STATIC_CREDENTIALS=admin:adminpass:owner
export STRATA_RESHARD_ROOT_CRED=<access:secret-with-owner=iam-root>
REQUIRE_LAB=1 make smoke-architecture-hardening
```

Runbook: `docs/site/content/operate/pre-prod-validation.md`.
Composite: `scripts/smoke-architecture-hardening.sh`.

## Result per leg

| Leg | What it proves | Story | Status | Evidence |
|-----|----------------|-------|--------|----------|
| **A** | Per-bucket non-default shard count + online 64→128 reshard under concurrent writes + crash-resume (Cassandra) | US-001/002/003/005 | **PENDING-LAB** | delegates to `smoke-reshard.sh`; green requires the Cassandra lab + reshard worker. Discriminating integration proof already green on the dedicated CI job `integration-cassandra` (`TestCassandraPerBucketShardResolution`, `TestCassandraReshardWorkerMovesRows`). |
| **B** | TiKV reshard parity — immediate-complete no-op, object still readable | US-004 | **PENDING-LAB** | requires TiKV lab; unit proof `internal/reshard` + memory/TiKV no-op contract green per-PR. |
| **C** | DeleteObjects batch (idempotent) + versioned delete → DeleteMarker | US-007 | **PENDING-LAB** | direct handler matrix `internal/s3api/delete_objects_test.go` green per-PR (`CGO_ENABLED=0 go test ./internal/s3api`). |
| **D** | Policy UNION ACL — anon GET denied pre-policy, granted post-policy | US-008 | **PENDING-LAB** | direct gate proof `internal/s3api/{policy_gate,auth_public_access_matrix}_test.go` green per-PR. |
| **E** | Console reshard UI `/admin/v1` supported-gate + queue | US-006 | **PENDING-LAB** | adminapi proof `internal/adminapi/buckets_reshard_test.go` green per-PR; browser pass `web/e2e/reshard-progress.spec.ts` (Playwright, CI). |
| **F** | Plaintext chunk-corruption fail-loud (CRC32C read path) | US-009 | **PASS (observed local)** | `CGO_ENABLED=0 GOWORK=off go test ./internal/data/memory/ ./internal/data/rados/ -run 'CRC\|Checksum\|FailsLoud'` → both packages `ok`. |

`PENDING-LAB` = the leg's behaviour is already covered by a green, discriminating
unit/integration test (red→green proof landed in the cited story); the composite
exercises the **same path against a running gateway**, which needs the labs up.
The composite WARN-skips those legs on a box with no lab and exits 0; with
`REQUIRE_LAB=1` on CI a down lab becomes a hard fail.

## Composite self-check (no lab)

`make smoke-architecture-hardening` on the bare box (no lab, go present):
legs A–E WARN-skip, **leg F PASS**, overall **PASS** (exit 0). Confirms the
harness wiring, skip-degradation, and the corruption proof all execute.

## Verdict

**Conditional GO.** Every behavioural claim in the cycle has a green,
discriminating per-PR or dedicated-CI test (see the per-leg evidence column).
The remaining gate is a single observed `REQUIRE_LAB=1 make
smoke-architecture-hardening` pass with both labs up — the composite is the
mechanism; this note is the record. Flip the PENDING-LAB rows to PASS with the
run output attached once that pass is captured on CI or a staging lab.
