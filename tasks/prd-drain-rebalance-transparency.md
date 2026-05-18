# PRD: Drain + rebalance transparency — config endpoints, live ETA, bandwidth visibility

## Introduction

Two parked + freshly-surfaced operator-transparency gaps merged into one cycle:

1. **Awaiting GC chip ships with static tooltip** (parked from `ralph/drain-progress-physical` US-002): the `<DrainProgressBar>` Awaiting GC state renders `"Physical delete completes after STRATA_GC_GRACE elapses (~5m default) plus the next gc worker tick."` — copy that does not adapt to a deploy's actual GC tunables. Operators with non-default `STRATA_GC_GRACE` / `_INTERVAL` / `_BATCH_SIZE` / `_CONCURRENCY` / `_SHARDS` get prose that doesn't match their cadence.

2. **Rebalance bandwidth is invisible to operators** (newly surfaced 2026-05-18 in operator walkthrough): `STRATA_REBALANCE_RATE_MB_S` (100 MB/s default, per-replica token bucket; chunk move costs `chunkSize × 2 tokens`) is configurable but operators have no UI / endpoint to see what's actually set. Concern: an operator running a drain has no way to confirm "migration won't saturate my network and won't impact user traffic." Compounds with the just-changed `STRATA_REBALANCE_INTERVAL` default (5m) — `make up-all` lab override is 30s — different cadences depending on environment, and operators need to see which one is active.

Fix is unified: two parallel read-only admin endpoints (`/admin/v1/gc-config`, `/admin/v1/rebalance-config`) expose the resolved tunables; `<DrainProgressBar>` consumes both to render per-deploy ETA + live bandwidth utilisation; Cluster Overview gains a rebalance config card; operator runbook gets a bandwidth-tuning section with concrete network-share calculations.

Both endpoints follow the same shape (read-only, env-static, audit-stamped, returns int seconds + int values). Shared admin handler infrastructure.

## Goals

- Two new admin endpoints:
  - `GET /admin/v1/gc-config` → `{grace_seconds, interval_seconds, batch_size, concurrency, shards}`
  - `GET /admin/v1/rebalance-config` → `{interval_seconds, rate_mb_s, inflight, shards, replicas_count}` (replicas_count derived from `cluster_nodes` heartbeat for the running tier)
- Both endpoints read-only, env-static, audit-stamped, returns flat JSON.
- `<DrainProgressBar>` consumes gc-config to render a live `~Nm` ETA on the Awaiting GC chip (replacing the static tooltip).
- `<DrainProgressBar>` consumes rebalance-config + Prometheus rate of `strata_rebalance_bytes_moved_total{to=<cluster>}` to render a live bandwidth indicator + ETA on the Migrating chip.
- New Cluster Overview rebalance card surfaces `per-replica rate / aggregate / effective forward / interval / inflight / shards / live 1m bandwidth`, with tooltip explaining the rate-limit semantics (`chunk move costs chunkSize × 2 tokens; aggregate ≈ replicas × rate_mb_s; effective forward ≈ aggregate / 2`).
- New operator runbook section `Rebalance bandwidth tuning` documents the rate-limit knob, the token-bucket math, and example settings for `1 GbE / 10 GbE / 25 GbE / 100 GbE` operator networks so the operator can quickly pick a non-saturating value.
- Smoke harness asserts both endpoints respond + the formulas track reality.
- The static tooltip behavior is preserved as graceful fallback when either admin endpoint errors / 500s.

## User Journey (pre-cycle walkthrough)

Happy path — drain with adequate operator visibility:

1. Operator opens Cluster Overview. Rebalance card renders: `Per-replica rate: 100 MB/s | Aggregate (×2 replicas): ~200 MB/s | Effective forward: ~100 MB/s | Interval: 30s | Inflight: 4 | Shards: 1`. Tooltip on the metrics explains the chunk move costs chunkSize × 2 tokens.
2. Operator clicks Drain → evacuate on cephb. `<ConfirmDrainModal>` opens with the existing impact analysis. Submits.
3. `<DrainProgressBar>` mounts; fetches `/admin/v1/gc-config` + `/admin/v1/rebalance-config` once (TanStack `staleTime: Infinity`).
4. Migration starts. Migrating chip renders: `Migrating: 100 chunks remaining · ~4 MB/s observed · ~6m ETA`. Observed bandwidth derived from `rate(strata_rebalance_bytes_moved_total{to="cephb"}[1m])`. ETA = `(remaining_chunks × chunk_size) / observed_rate` (or, if observed_rate is 0/cold, falls back to formula using rebalance-config rate_mb_s / 2 as effective forward estimate).
5. Migration completes → Awaiting GC cleanup chip: `Awaiting GC cleanup: 100 chunks awaiting physical delete (~5m ETA)`. ETA computed from gc-config: `eta_min = ceil(grace_seconds/60) + ceil(gc_queue_pending / (batch_size × shards) × (interval_seconds/60))`. Cap at `~24h+` on misconfiguration.
6. Tooltip on the chip enumerates the inputs: `ETA computed from current GC queue depth (100), STRATA_GC_GRACE (5m), STRATA_GC_INTERVAL (5m), STRATA_GC_BATCH_SIZE (100), STRATA_GC_SHARDS (1).`
7. GC drains → green Ready chip. Operator deregisters.

Operator network-share concern walkthrough:

1. Operator on a 1 GbE network worries about migration impact. Reads runbook `Rebalance bandwidth tuning`. Table example: `1 GbE = 125 MB/s capacity. To keep user traffic at 80% of pipe, cap rebalance aggregate at 25 MB/s → per-replica STRATA_REBALANCE_RATE_MB_S=12 (with 2 replicas).`
2. Operator sets env on next `make up-all`. Restarts. Cluster Overview rebalance card shows the new rate live. Drain starts; bandwidth indicator confirms ~6 MB/s effective forward, leaving plenty of headroom.

Edge cases:

- **Either admin endpoint errors** (transient): UI falls back to pre-cycle behavior for that side (static GC tooltip / no rebalance card OR cached values). Counter `strata_admin_config_endpoint_errors_total{endpoint}` increments. Operator notices via existing metrics dashboards.
- **PromQL endpoint unavailable** (existing `strata_prometheus_url` plumbing has a graceful-degrade path): rebalance card omits the live bandwidth row; ETA on Migrating chip uses formula fallback.
- **Operator changes env mid-drain via gateway restart**: gateway re-reads env at boot → new admin endpoint snapshot. TanStack `staleTime: Infinity` cache persists across browser session, so operator must hard-reload to see the new values. Documented.
- **`STRATA_GC_GRACE=0` (misconfig)**: ETA = 0 + queue work time; small but real. No cap hit. Documented as "values that exit the safe band are not capped — operator sees the math and self-diagnoses."
- **`STRATA_GC_GRACE=999h` (misconfig)**: ETA hits `~24h+` cap.
- **`STRATA_REBALANCE_RATE_MB_S=0`** (would div-by-zero in ETA formula): denominator clamped to 1; ETA hits `~24h+` cap.
- **Multi-replica deploy (>2)**: rebalance card's `Aggregate` = `replicas_count × per_replica_rate`. `replicas_count` derived from `cluster_nodes` heartbeat live (so adding a replica updates the card on its next 10s heartbeat tick).

## User Stories

### US-001: Two new admin endpoints — `/admin/v1/gc-config` + `/admin/v1/rebalance-config`

**Description:** As a developer, I need the gateway to expose its resolved GC + rebalance tunables as read-only admin endpoints so the UI can render them.

**Acceptance Criteria:**
- [ ] New handler file `internal/adminapi/config_endpoints.go` (or fold into existing admin handler file if codebase convention warrants — verify): both endpoints share infra.
- [ ] `GET /admin/v1/gc-config` returns JSON `{grace_seconds: int, interval_seconds: int, batch_size: int, concurrency: int, shards: int}`. Audit-stamped `admin:GetGCConfig`.
- [ ] `GET /admin/v1/rebalance-config` returns JSON `{interval_seconds: int, rate_mb_s: int, inflight: int, shards: int, replicas_count: int}`. Audit-stamped `admin:GetRebalanceConfig`. `replicas_count` derived from `meta.Store.ListClusterNodes(ctx)` filtered to live heartbeats (within `2 × HEARTBEAT_INTERVAL` of now) — NOT static.
- [ ] Both endpoints read-only: no PUT/PATCH/POST counterpart. Env stays source of truth; gateway restart updates the snapshot.
- [ ] GC values sourced from the resolved process env captured at boot. Pull from `cmd/strata/workers/gc.go::clampShards` neighbors + cache as a `GCConfig` struct on `adminapi.Server`. Wire through `adminapi.Config` for unit-test injection.
- [ ] Rebalance values sourced from `cmd/strata/workers/rebalance.go::buildRebalance` env reads. Same wiring pattern as gc-config: `RebalanceConfig` struct on `adminapi.Server`.
- [ ] Response uses int seconds (not Go duration strings) for cross-tool friendliness — UI does `seconds / 60` for minutes.
- [ ] OpenAPI spec at `internal/adminapi/openapi.yaml` (if exists — verify) gains both endpoint definitions.
- [ ] Counter `strata_admin_config_endpoint_errors_total{endpoint}` (`endpoint ∈ {gc-config, rebalance-config}`) increments on internal error (e.g. cluster_nodes scan fails).
- [ ] Unit tests: both handlers return injected configs verbatim; degenerate (zero) values are handled (200 with zeros); rebalance-config replicas_count reflects mocked cluster_nodes list.
- [ ] Integration test against memory backend: `GET /admin/v1/gc-config` returns boot-time defaults; `GET /admin/v1/rebalance-config` returns 30s/100/4/1/1 (compose lab default values).
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: `<DrainProgressBar>` live ETA on Awaiting GC chip + live bandwidth on Migrating chip

**Description:** As an operator, I want both drain states to show a precise per-deploy ETA + live bandwidth so I know when the drain will complete and whether it's saturating my network.

**Acceptance Criteria:**
- [ ] `<DrainProgressBar>` fetches `/admin/v1/gc-config` + `/admin/v1/rebalance-config` via two TanStack Query keys (`gc-config`, `rebalance-config`), both `staleTime: Infinity` (env static).
- [ ] **Awaiting GC chip live ETA** (replaces static tooltip from prior cycle): `eta_min = ceil(grace_seconds / 60) + ceil(gc_queue_pending / (batch_size × shards × 1) × (interval_seconds / 60))`. Denominator components clamped to 1 if zero (degenerate). Cap at `~24h+` if computed > 24h.
- [ ] Awaiting GC chip label flips from static `"Awaiting GC cleanup: X chunks awaiting physical delete"` to `"Awaiting GC cleanup: X chunks awaiting physical delete (~N m ETA)"` (or `~Nh Mm` if N ≥ 60).
- [ ] Awaiting GC tooltip changes from static to: `"ETA computed from current GC queue depth (X chunks), STRATA_GC_GRACE (Y), STRATA_GC_INTERVAL (Z), STRATA_GC_BATCH_SIZE (B), STRATA_GC_SHARDS (S)."` (values interpolated).
- [ ] **Migrating chip live bandwidth**: render `"Migrating: X chunks remaining · ~Y MB/s observed · ~Zm ETA"`. Y = `rate(strata_rebalance_bytes_moved_total{to=<cluster_id>}[1m])` via existing `STRATA_PROMETHEUS_URL` plumbing (already used by `/admin/v1/clusters/{id}/rebalance-progress` per placement-rebalance Web UI cycle). Z = `(remaining_chunks × chunk_size) / observed_rate` if observed_rate > 0; else fallback to `(remaining_chunks × chunk_size) / (rebalance_config.rate_mb_s × replicas_count / 2)` using rebalance-config (effective forward estimate).
- [ ] Migrating chip tooltip enumerates inputs: `"ETA from observed bandwidth (Y MB/s on cluster <id>) over remaining manifest chunks (X). Configured rate cap per replica: <rate_mb_s> MB/s."`
- [ ] When either query is loading: render pre-cycle static behavior for that chip (manifest-only Migrating + static-copy Awaiting GC). Graceful fallback.
- [ ] When either query errors (404/500): same pre-cycle fallback. Counter on the gateway side already tracks `strata_admin_config_endpoint_errors_total`. UI doesn't surface error count to operator (would be noise; existing metrics dashboards cover it).
- [ ] When PromQL `rate(strata_rebalance_bytes_moved_total{...}[1m])` returns null / 0 (cold start): Migrating chip uses formula fallback for ETA + the observed bandwidth row reads `~0 MB/s observed (cold start)`.
- [ ] When `rebalance-config.rate_mb_s` is 0 (clamp) or `replicas_count` is 0: ETA cap fires; chip reads `~24h+ ETA`.
- [ ] Playwright spec extends `web/e2e/drain-progress.spec.ts`: mocks responses for `/admin/v1/gc-config` + `/admin/v1/rebalance-config` + Prometheus query; asserts correct ETA + bandwidth + tooltip text for Migrating + Awaiting GC + error fallbacks.
- [ ] Verify in browser: drain cephb in lab; observe live ETA + bandwidth + chip transitions through all 3 states (Migrating → Awaiting GC → Ready).
- [ ] `pnpm run build` succeeds
- [ ] Typecheck passes

### US-003: Cluster Overview rebalance config card + live bandwidth indicator

**Description:** As an operator, I want the Cluster Overview to surface the rebalance tunables + the current bandwidth utilisation so I can confirm at a glance that migration is configured safely.

**Acceptance Criteria:**
- [ ] New card on Cluster Overview page (under the existing cluster_nodes / leader heartbeats area): `<RebalanceConfigCard>` reads `/admin/v1/rebalance-config` + Prometheus rate of `strata_rebalance_bytes_moved_total` (sum across clusters).
- [ ] Card renders four rows:
  - `Per-replica rate: <rate_mb_s> MB/s`
  - `Aggregate (× <replicas_count> replicas): ~<rate_mb_s × replicas_count> MB/s`
  - `Effective forward: ~<aggregate / 2> MB/s`
  - `Cadence: every <interval_seconds>s · Inflight: <inflight> · Shards: <shards>`
- [ ] Live bandwidth row below the config rows: `Observed last 1m: ~<rate> MB/s · ~<chunks> chunks/sec`. Hidden if PromQL endpoint unavailable.
- [ ] Tooltip on the "Aggregate / Effective forward" cells explains the chunkSize × 2 tokens math: `Each chunk move consumes chunkSize × 2 tokens from the per-replica bucket (read + write). Aggregate ceiling assumes all replicas migrating concurrently; effective forward halves the aggregate to account for the read+write split.`
- [ ] Card has a "Tune" link/button → opens `docs/site/content/best-practices/placement-rebalance.md#bandwidth-tuning` in a new tab (cross-link to runbook).
- [ ] TanStack key `rebalance-config` shared with `<DrainProgressBar>` from US-002 (single fetch per app session).
- [ ] When `/admin/v1/rebalance-config` returns 404 (legacy gateway not exposing endpoint): card hidden; no error toast (operator UX preserved).
- [ ] Playwright spec `web/e2e/cluster-overview-rebalance.spec.ts` (new): mocks rebalance-config + PromQL; asserts card renders with correct values + tooltip text + Tune link target.
- [ ] Verify in browser: lab default values render in card; tooltip displays correctly.
- [ ] `pnpm run build` succeeds
- [ ] Typecheck passes

### US-004: Operator runbook `Rebalance bandwidth tuning` section

**Description:** As an operator, I need documented guidance on how to pick a non-saturating `STRATA_REBALANCE_RATE_MB_S` for my network so migration doesn't impact user traffic.

**Acceptance Criteria:**
- [ ] New section `## Bandwidth tuning` in `docs/site/content/best-practices/placement-rebalance.md` (anchored as `#bandwidth-tuning` for the Cluster Overview card's Tune link from US-003).
- [ ] Section opens with the token-bucket model: `Each replica's rebalance worker has a token bucket sized at STRATA_REBALANCE_RATE_MB_S MB/s. A chunk move consumes chunkSize × 2 tokens (read from source + write to target). With N replicas, the aggregate ceiling is N × rate_mb_s; the effective forward (net new data on the target cluster) is approximately aggregate / 2.`
- [ ] Network share calculator table:
  - `1 GbE (125 MB/s capacity)`: safe rate per replica ≈ 12 MB/s (24 MB/s aggregate / 12 MB/s effective forward = ~10% of pipe)
  - `10 GbE (1.25 GB/s capacity)`: safe rate per replica ≈ 100 MB/s (200 MB/s aggregate / 100 MB/s effective forward = ~8% of pipe)
  - `25 GbE (3.1 GB/s capacity)`: safe rate per replica ≈ 300 MB/s
  - `100 GbE (12.5 GB/s capacity)`: safe rate per replica ≈ 1000 MB/s
- [ ] Two concrete tuning workflows documented:
  - **Low-bandwidth lab/dev** (1 GbE): `STRATA_REBALANCE_RATE_MB_S=10 make up-all`
  - **Production 10 GbE deploy**: `STRATA_REBALANCE_RATE_MB_S=100` in k8s manifest (matches gateway default)
- [ ] Reference to live observability: `Cluster Overview > Rebalance card surfaces the configured rate + observed 1m bandwidth so you can verify the cap is taking effect.`
- [ ] Reference to the new admin endpoint: `curl http://gateway:9999/admin/v1/rebalance-config` for scripted health-check shape.
- [ ] Cross-link from the existing `Multi-leader scaling` subsection in the same doc.
- [ ] Migration note `docs/site/content/architecture/migrations/drain-progress-physical.md` (from prior cycle) gains a one-line update: `Live ETA + rebalance bandwidth visibility shipped as a follow-up in ralph/drain-rebalance-transparency.`
- [ ] OpenAPI spec / admin-api docs (e.g. `docs/site/content/architecture/admin-api.md` if exists) document both new endpoints.
- [ ] `make docs-build` passes
- [ ] Typecheck passes

### US-005: Smoke + ROADMAP close-flip + PRD removal

**Description:** As an operator and as a future-maintainer, I need a smoke that exercises both endpoints + the formulas against a non-default deploy + documented migration + the ROADMAP entries flipped.

**Acceptance Criteria:**
- [ ] Extend `scripts/smoke-drain-progress-ui.sh` (from prior cycle): the harness already configures `STRATA_REBALANCE_RATE_MB_S=1` + `STRATA_GC_GRACE=60s` for smoke-time isolation. New steps:
  - After reaching Awaiting GC state, call `GET /admin/v1/gc-config`; assert response contains `grace_seconds: 60` (matches smoke-time override).
  - Compute `eta_min` from the gc-config formula + the polled `gc_queue_pending`; assert ETA is within a sane band (`0 ≤ eta_min ≤ 30`); assert formula output matches expected within ±1 min.
  - Call `GET /admin/v1/rebalance-config`; assert response contains `rate_mb_s: 1` (smoke override) + `replicas_count: 2` (default lab has strata-a + strata-b).
  - During the Migrating state, sample Prometheus query `rate(strata_rebalance_bytes_moved_total{to="cephb"}[1m])` directly; assert > 0 MB/s (migration is actively moving data).
- [ ] OR alternatively (at implementer's discretion): a separate `scripts/smoke-drain-rebalance-transparency.sh` that does ONLY the new endpoint + Prom-rate assertions, leaving the existing smoke unchanged. Pick the cleaner path; one or the other.
- [ ] `make smoke-drain-progress-ui` (or new `make smoke-drain-rebalance-transparency`) Makefile target wraps the script.
- [ ] `ROADMAP.md` close-flip — TWO entries:
  - The existing parked P3 `Precise drain-progress ETA from gateway GC tunables` → `~~**P3 — Precise drain-progress ETA from gateway GC tunables.**~~ — **Done.** <one-line summary>. (commit \`<pending>\`)`.
  - A new P3 entry added + closed at the same commit `~~**P3 — Rebalance bandwidth visibility on Cluster Overview + drain progress.**~~ — **Done.** <one-line summary>. (commit \`<pending>\`)` (operator-requested live; not pre-roadmapped per CLAUDE.md `Discovering a new gap` rule).
- [ ] Closing SHA backfilled on main in a follow-up commit.
- [ ] `tasks/prd-drain-rebalance-transparency.md` REMOVED in the same commit (CLAUDE.md PRD lifecycle rule).
- [ ] `make docs-build`, `make vet`, `make test`, `pnpm run build`, smoke harness all green.
- [ ] Typecheck passes

## Functional Requirements

- FR-1: New `GET /admin/v1/gc-config` admin endpoint returns resolved GC tunables.
- FR-2: New `GET /admin/v1/rebalance-config` admin endpoint returns resolved rebalance tunables + live replicas_count from heartbeats.
- FR-3: Both endpoints read-only; audit-stamped; env-static (gateway restart updates snapshot).
- FR-4: `<DrainProgressBar>` Awaiting GC chip renders live `~Nm` ETA from gc-config + `gc_queue_pending`.
- FR-5: `<DrainProgressBar>` Migrating chip renders live `~Y MB/s observed · ~Zm ETA` from Prom rate + rebalance-config fallback.
- FR-6: Cluster Overview gains `<RebalanceConfigCard>` showing per-replica rate / aggregate / effective forward / cadence + live 1m bandwidth row.
- FR-7: Operator runbook documents bandwidth tuning with network-share calculator table.
- FR-8: All UI surfaces gracefully fall back to pre-cycle static behavior when endpoints / Prom are unavailable.
- FR-9: Smoke harness asserts formula tracks reality with non-default tunables.

## State Truth Tables

### `<DrainProgressBar>` chip rendering

| State | Inputs available | Chip label format |
|---|---|---|
| Migrating | rebalance-config OK, Prom OK, rate > 0 | `Migrating: X chunks remaining · ~Y MB/s observed · ~Zm ETA` |
| Migrating | rebalance-config OK, Prom OK, rate = 0 (cold) | `Migrating: X chunks remaining · ~0 MB/s observed (cold start) · ~Zm ETA (formula)` |
| Migrating | rebalance-config OK, Prom unavailable | `Migrating: X chunks remaining · ~Zm ETA (estimated)` |
| Migrating | rebalance-config error/loading | `Migrating: X chunks remaining` (pre-cycle behavior) |
| Awaiting GC | gc-config OK | `Awaiting GC cleanup: X chunks awaiting physical delete (~N m ETA)` |
| Awaiting GC | gc-config error/loading | `Awaiting GC cleanup: X chunks awaiting physical delete` (pre-cycle behavior, static tooltip) |
| Ready | n/a | `✓ Ready to deregister` (existing green chip) |

### Endpoint response shapes

| Endpoint | Field | Type | Source |
|---|---|---|---|
| `/admin/v1/gc-config` | `grace_seconds` | int | env `STRATA_GC_GRACE` |
| | `interval_seconds` | int | env `STRATA_GC_INTERVAL` |
| | `batch_size` | int | env `STRATA_GC_BATCH_SIZE` |
| | `concurrency` | int | env `STRATA_GC_CONCURRENCY` |
| | `shards` | int | env `STRATA_GC_SHARDS` |
| `/admin/v1/rebalance-config` | `interval_seconds` | int | env `STRATA_REBALANCE_INTERVAL` |
| | `rate_mb_s` | int | env `STRATA_REBALANCE_RATE_MB_S` |
| | `inflight` | int | env `STRATA_REBALANCE_INFLIGHT` |
| | `shards` | int | env `STRATA_REBALANCE_SHARDS` |
| | `replicas_count` | int | live count of `cluster_nodes` heartbeats |

## Cache Invalidation Ledger

- TanStack `gc-config` query: `staleTime: Infinity`. Invalidated only by hard reload / explicit `invalidateQueries`.
- TanStack `rebalance-config` query: `staleTime: Infinity` (same shape — env-static at boot). `replicas_count` field is mildly time-varying (replica joins/leaves), but the operator UX doesn't need sub-30s freshness on a count display; hard reload covers the rare topology change.
- No new in-process server-side caches.
- Existing `ClusterStatsCache` + `placement.DrainCache` unaffected.

## Safety Claims Preconditions

- **Claim: ETA + bandwidth are observability, not safety.**
  - Precondition: `deregister_ready` chip still derives ONLY from existing US-006 safety gate (`total_chunks==0 && gc_queue_pending==0 && no_open_multipart`). Neither ETA nor observed bandwidth gates anything. **Test**: existing US-006 contract tests pass unchanged.
- **Claim: both endpoints are read-only and cannot mutate gateway state.**
  - Precondition: no PUT / PATCH / POST handlers on either path. **Test**: route table inspection (two new GET routes, no other methods).
- **Claim: formula degenerate inputs handled gracefully.**
  - Precondition: denominators clamped to 1; ETA capped at 24h. **Test**: unit tests in UI test suite assert ETA finite (not Infinity / NaN) for clamped inputs.
- **Claim: rebalance card hides itself when endpoint unavailable** (legacy gateway compatibility).
  - Precondition: `/admin/v1/rebalance-config` 404 → card hidden. **Test**: Playwright spec asserts card not rendered on mock 404.

## Downstream Consumer Grep

- `gc_queue_pending` — already shipped in prior cycle; reused.
- `strata_rebalance_bytes_moved_total` — existing Prom counter (per CLAUDE.md). No code change needed.
- New endpoints `/admin/v1/gc-config`, `/admin/v1/rebalance-config` — no prior consumers (greenfield).
- New audit actions `admin:GetGCConfig`, `admin:GetRebalanceConfig` — no prior consumers.
- New metric `strata_admin_config_endpoint_errors_total` — new; documented in metrics catalog.
- `cluster_nodes` heartbeat list (used for `replicas_count`): existing `meta.Store.ListClusterNodes` consumed by `/admin/v1/diagnostics/node` (per CLAUDE.md) — same call site shape, no contract change.

## Worst-Case Thought Exercise

What if the operator changes env mid-drain via gateway restart?

- Restart → boot reloads env → new admin endpoint snapshots. TanStack `staleTime: Infinity` cache persists across the gateway's brief downtime, so first poll after recovery would still see OLD config. Operator hard-reload fixes it. Acceptable.

What if a replica returns stale rebalance-config (different from sibling replicas)?

- Canonical lab compose ensures all replicas share env. Diverging env across replicas is explicitly non-supported. If operator violates, ETA may be inconsistent across browser tabs hitting different replicas. Not a safety issue.

What if a gateway has rebalance worker disabled (`STRATA_WORKERS` doesn't include `rebalance`)?

- `/admin/v1/rebalance-config` still returns env values (parsed at boot regardless of registered workers). UI renders the card. Migration never happens → Migrating chip stays at `Migrating: X chunks remaining · ~0 MB/s observed (worker idle) · ~24h+ ETA`. Operator notices, enables worker.

What if Prometheus is down (`STRATA_PROMETHEUS_URL` unreachable)?

- Existing `rebalance-progress` admin endpoint already has the graceful-degrade pattern (returns `metrics_available: false`). Reuse: rebalance card hides live bandwidth row; Migrating chip falls back to formula ETA.

What if operator runs the lab harness for one drain, then drains a second cluster?

- Each drain runs independently; rebalance-config doesn't change. UI handles per-cluster `<DrainProgressBar>` instances independently. No cross-talk.

What if migration completes in <60s (small object set; before Prom 1m rate window settles)?

- Prom rate query returns 0 or small numbers for fresh windows. UI's observed bandwidth row shows the cold-start fallback. Migrating chip flips to Awaiting GC quickly. Operator sees the bandwidth indicator briefly; ETA was approximate; this is fine.

## Non-Goals

- Operator-mutable tunables via admin API (PUT /admin/v1/gc-config or rebalance-config). Env stays the source of truth.
- Per-cluster rebalance tunables. Single global config.
- Historical accuracy tracking (was-predicted vs actually-took). Out of scope.
- Adaptive ETA based on observed throughput. YAGNI — static formula sufficient.
- Bandwidth shaping at the network layer (tc / iptables). Operator-set rate_mb_s is the only knob.
- Per-network-interface bandwidth visibility (kernel-level NIC stats). Operator uses node-exporter / Grafana for that; orthogonal.
- Adding rate field to the existing `/admin/v1/clusters/{id}/rebalance-progress` response — already populated; the new card on Cluster Overview is a separate surface.

## Open Questions

- **TanStack `staleTime: Infinity` granularity**: `replicas_count` from heartbeat is slightly time-varying. Could move that field to a sub-30s refresh while keeping the rest static. Decision: keep simple (single TanStack key, staleTime Infinity, hard-reload to pick up replica changes). Revisit if operator complaints surface.
- **PromQL rate window**: `[1m]` chosen for responsiveness. Could be `[30s]` for faster cold-start, but smaller windows have higher noise on low-traffic deploys. Decision: 1m.

## Technical Considerations

- `GCConfig` + `RebalanceConfig` structs live in `internal/adminapi/config_endpoints.go`. Both wired through `adminapi.Config` for unit-test injection.
- Boot-time env resolution paths already in `cmd/strata/workers/{gc,rebalance}.go::clampShards/clampDuration/clampInt`. The new endpoints pull from the resolved values, don't re-parse.
- TanStack Query `staleTime: Infinity` is a stable pattern in the codebase for boot-time-static values.
- Formula precision: integer minutes. Sub-minute precision adds UI noise; minutes is the right grain.
- ETA rendering: `~Nh Mm` style when N ≥ 60; otherwise `~N m`.
- The smoke harness already uses `STRATA_REBALANCE_RATE_MB_S=1` + `STRATA_GC_GRACE=60s` for prior cycle's smoke. Reuse those exact env overrides; the new assertions just add GET /admin/v1/{gc,rebalance}-config + Prom-rate sample steps.
- `replicas_count` derivation: `meta.Store.ListClusterNodes(ctx)` filtered to `last_heartbeat > now() - 2 × STRATA_HEARTBEAT_INTERVAL` (default 10s × 2 = 20s window). Existing helper if available; else inline filter.
- The "Tune" link on the rebalance card opens `docs/site/content/best-practices/placement-rebalance.md#bandwidth-tuning` in a new tab. URL constructed via existing docs-link helper if one exists; else `target="_blank" rel="noopener"` on a plain anchor.

## Success Metrics

- `make smoke-drain-progress-ui` (or new `make smoke-drain-rebalance-transparency`) passes end-to-end.
- Operator can see configured rate + observed bandwidth + ETA on a fresh drain without consulting docs.
- Operator can dial `STRATA_REBALANCE_RATE_MB_S` to a non-saturating value based on the runbook table.
- Existing US-006 drain-cleanup + drain-progress-physical safety gates unchanged.
- ROADMAP P3 entries (1 parked + 1 new) closed.
