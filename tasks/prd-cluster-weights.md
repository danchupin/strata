# PRD: Default-routing weights + `pending` cluster state (safe gradual cluster activation)

## Introduction

`ralph/drain-transparency` closed the 4-state cluster machine and made drain semantics transparent + always-strict. Two operator-UX gaps remain:

1. **No default-routing weights.** A bucket without `meta.Bucket.Placement` policy falls back to `spec.Cluster` from the class env — a single cluster regardless of how many are registered. In symmetric multi-cluster deploys this stays imbalanced unless the operator sets per-bucket Placement on every bucket. There is no global default-policy mechanism.
2. **No safe new-cluster activation gate.** Adding a cluster to `STRATA_RADOS_CLUSTERS` env makes it eligible for writes immediately after the rolling restart finishes. The operator can't validate the cluster first, can't gradually ramp traffic (10% → 25% → 50% → 100%), and gets no UI gate.

This PRD adds a 5th state `pending` + a per-cluster `weight int (0..100)` field. Together they enable a clean cluster lifecycle: **register → pending → activate at weight 10 → ramp gradually → live at weight 100 → drain → evacuate → remove**.

Closes ROADMAP P2 *Default-routing weights + `pending` cluster state (safe gradual cluster activation)* under Correctness & consistency section.

## Goals

- 5-state cluster machine `{pending, live, draining_readonly, evacuating, removed}` (extends drain-transparency's 4-state)
- Per-cluster `weight int (0..100)` field — meaningful only when state=live
- New clusters auto-init as `state=pending weight=0` at boot via env-vs-meta reconcile
- Existing live clusters with chunks already on them auto-detected at boot → state=live weight=100 (backwards compat — zero operator action required)
- Default routing for buckets without `Placement` policy synthesises `{<each-live-cluster>: <its-weight>}` policy → proportional spread
- Bucket explicit `Placement` policy always overrides cluster weights (two weight layers never combine)
- `POST /admin/v1/clusters/{id}/activate {weight}` flips pending → live with initial weight; typed-confirm modal in UI
- `PUT /admin/v1/clusters/{id}/weight {weight}` adjusts weight while live; UI slider with debounced PUT
- `pending` / `draining_readonly` / `evacuating` clusters excluded from default-routing weight wheel
- `weight=0 + state=live` legal — no new default-routed chunks, reads + explicit policies still work

## User Journey Walkthrough

Pre-cycle walkthrough per `feedback_cycle_end_to_end.md`. Operator end-to-end scenario: add a new RADOS cluster `cephc` to the multi-cluster lab and ramp it up gradually. Every step has a concrete UI/API/log surface; gaps go in scope.

| # | Operator action | Surface | Backing story |
|---|-----------------|---------|---------------|
| 1 | Edit `STRATA_RADOS_CLUSTERS` env, add `cephc:/etc/ceph-c/ceph.conf` | env file edit | (out-of-band) |
| 2 | Rolling restart strata replicas | docker compose restart | (out-of-band) |
| 3 | Gateway boot detects new cluster id in env → creates `cluster_state` row `state=pending weight=0` | log INFO `"cluster auto-init"` | **US-001** |
| 4 | Open `/console/storage` | Storage page | (existing) |
| 5 | See cephc card with badge "Pending — not receiving writes" + "Activate" button | `<ClustersSubsection>` | **US-003** |
| 6 | Click Activate | `<ActivateClusterModal>` opens | **US-003** |
| 7 | Modal: initial-weight slider 0..100 (default 10) + paired numeric input + warning explainer + typed-confirm input | modal body | **US-003** |
| 8 | Type "cephc" + slider stays at 10 + click Submit | typed-confirm armed | **US-003** |
| 9 | POST `/admin/v1/clusters/cephc/activate {weight: 10}` → 200 | admin API | **US-001** |
| 10 | Card flips to live; weight=10 displayed; "Activate" button replaced by inline slider | re-render | **US-003** + **US-004** |
| 11 | PUT objects via nil-policy buckets — new chunks start landing on cephc ~10% of the time | log `rebalance plan` shows actual distribution | **US-002** |
| 12 | Operator monitors load via Grafana / Prometheus over an hour | (out-of-band) | — |
| 13 | Drag cephc card slider 10 → 25; debounced PUT after 500ms | PUT `/weight {weight:25}` | **US-004** |
| 14 | Next nil-policy PUTs spread ~25% to cephc | (existing routing) | **US-002** |
| 15 | Repeat ramp: 25 → 50 → 100 | (each step a PUT + re-route) | **US-004** |
| 16 | Cluster fully integrated; default routing among 3 live clusters proportional to weights | log / metrics | **US-002** |

Negative paths covered:
- Operator activates cephc with weight=0 → state=live but weight=0; no new default-routed writes; explicit bucket Placement still routes to it (verified in smoke US-005 scenario C)
- Operator tries Activate on cluster in state=live → 409 Conflict (US-001 acceptance)
- Operator tries PUT /weight on pending cluster → 409 Conflict (must Activate first)
- All live clusters have weight=0 + nil-policy bucket → PickCluster returns "" → fallback to class env `spec.Cluster`
- Existing live clusters (chunks via bucket_stats) auto-detected at boot → state=live weight=100 (zero operator action — preserves existing routing semantics)

## User Stories

### US-001: 5-state machine + cluster_weight field + admin API endpoints
**Description:** As a developer, I need the meta layer + admin API to support the new `pending` state and per-cluster weight so the UI + routing can rely on them.

**Acceptance Criteria:**
- [ ] `cluster_state` row schema gains `weight int (0..100)` field alongside existing `state` + `mode`
- [ ] `meta.Store` interface: `SetClusterState(ctx, id, state, mode, weight) error`; `GetClusterState` returns `ClusterStateRow{State, Mode, Weight}`; `ListClusterStates` returns the same map
- [ ] Memory + Cassandra + TiKV backends implement the new shape in lockstep; new contract test `caseClusterStateWeights` in `internal/meta/storetest/contract.go` exercises legal combos + weight range validation + state-mode-weight transitions
- [ ] Boot-time reconcile in `internal/serverapp/serverapp.go::Run`: gateway compares `STRATA_RADOS_CLUSTERS` env vs `cluster_state` rows. For each cluster in env without a row:
  - If `bucket_stats` sum references this cluster (any bucket has chunks here) → auto-create state=live weight=100 (existing-live detection)
  - Otherwise → auto-create state=pending weight=0
  - Log INFO `"cluster auto-init"` with `cluster_id`, `state`, `weight`, `reason` ("existing-live" or "new-pending")
- [ ] New admin endpoint `POST /admin/v1/clusters/{id}/activate {weight: int}` — pending → live; 409 Conflict if state != pending; 400 if weight not in [0, 100]
- [ ] New admin endpoint `PUT /admin/v1/clusters/{id}/weight {weight: int}` — adjust weight while state=live; 409 Conflict if state != live; 400 if weight not in [0, 100]
- [ ] `GET /admin/v1/clusters` response carries `weight int` per cluster (0 when not live)
- [ ] `GET /admin/v1/clusters/{id}/drain-progress` includes `weight` (for symmetry; not used by drain logic)
- [ ] OpenAPI spec at `internal/adminapi/openapi.yaml` updated
- [ ] Audit row stamped via `s3api.SetAuditOverride(ctx, "admin:ActivateCluster"|"admin:UpdateClusterWeight", "cluster:<id>", ...)` with body `{weight}` for traceability
- [ ] Unit tests: boot-time reconcile fixture covers (a) existing-live detection via bucket_stats, (b) new-pending init, (c) idempotent re-run (no duplicate writes)
- [ ] Integration test (multi-cluster lab via testcontainers or compose): wipe `cluster_state` row for one cluster → restart gateway → assert auto-init to expected state
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: Default-routing synthesised policy in `placement.PickCluster`
**Description:** As a developer, I need the placement picker to consult cluster weights when the bucket has no explicit Placement policy so default routing actually spreads across all live clusters.

**Acceptance Criteria:**
- [ ] New helper `placement.DefaultPolicy(clusterStates map[string]meta.ClusterStateRow) map[string]int` returns `{<each-live-cluster-with-weight>0>: <weight>}` excluding pending/draining_readonly/evacuating/removed states
- [ ] `placement.PickClusterExcluding` extended (or new sibling): when `policy == nil` AND `spec.Cluster` from class env is empty (no `@cluster` pin) → use synthesised default policy from cluster weights
- [ ] Class env `@cluster` suffix still wins: when `spec.Cluster != ""` (explicit pin in class env) → that cluster is used regardless of weights, bypassing default-routing synthesis
- [ ] Bucket `Placement` policy still wins over cluster weights: when `bucket.Placement != nil` → policy = bucket.Placement, cluster.weight ignored for this bucket
- [ ] All live clusters with weight=0 → DefaultPolicy returns empty map → PickCluster returns "" → caller falls back to class env spec.Cluster (or 503 if strict-refuse path)
- [ ] `internal/data/rados/Backend.PutChunks` (and S3 symmetric) calls extended picker with cluster-state map injected via `data.WithClusterStates(ctx, ...)` (or similar context-threading mechanism)
- [ ] Cluster-state map sourced from in-process cache (refreshed every 30s via existing DrainCache pattern OR new ClusterStateCache); cache invalidates immediately on activate/weight admin endpoint calls so operator changes are reflected within seconds
- [ ] Unit test: 3 simulated clusters with weights {c1:10, c2:30, c3:60} + nil bucket policy → 1000 PUT keys distribute ~100/300/600 (5% tolerance on each)
- [ ] Unit test: cluster c2 pending → PickCluster never returns c2 even if policy lists it
- [ ] Unit test: bucket policy `{c1:1, c2:1}` + cluster.weight `{c1:100, c2:50}` → 50/50 split (bucket policy wins, cluster.weight ignored)
- [ ] Integration test (multi-cluster lab): set weights {default:50, cephb:50} + nil-policy bucket → PUT 100 chunks → assert ~50/50 distribution via `rados ls | wc -l` on each cluster
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: UI — Pending cluster card variant + Activate modal with typed-confirm + initial-weight slider
**Description:** As an operator, I want a pending cluster card with an Activate button that opens a typed-confirm modal with an initial-weight slider so I can safely bring a new cluster into the routing rotation.

**Acceptance Criteria:**
- [ ] `web/src/components/storage/ClustersSubsection.tsx` renders pending-state cards distinct from live: badge "Pending — not receiving writes" (gray palette), no Drain button, only an "Activate" button + "Show affected buckets" link (existing — still useful to see what would route here once active)
- [ ] New component `web/src/components/storage/ActivateClusterModal.tsx` opens on Activate click
- [ ] Modal body contains: cluster id label, weight slider 0..100 (default 10), paired numeric `<Input type="number">` two-way bound, warning explainer ("Activating this cluster will start routing a share of new bucket PUTs without explicit Placement policy. Initial share = weight × 100% / total-live-weights. Adjust later via the cluster card slider."), typed-confirm input matching the existing `<ConfirmDrainModal>` pattern
- [ ] Submit DISABLED until typed value === cluster.id (case-sensitive, same convention as ConfirmDrainModal)
- [ ] Submit calls `POST /admin/v1/clusters/{id}/activate {weight: <slider>}`; on 200 → success toast + refetch shared `clusters` TanStack query; card transitions to live state with the chosen weight
- [ ] On 409 (already-live race) → error toast; modal stays open so operator can dismiss
- [ ] Slider + numeric input clamp to [0, 100]; non-numeric blocked
- [ ] Reuse existing `<Card>`, `<Badge>`, `<Modal>`, `<Slider>`, `<Input>`, `<Button>`, `<Toast>` primitives
- [ ] Bundle size delta ≤ 4 KiB gzipped
- [ ] Typecheck passes
- [ ] Verify in browser using dev-browser skill

### US-004: UI — Live cluster card weight slider + debounced PUT + weight=0 chip
**Description:** As an operator, I want a slider on each live cluster card so I can adjust its routing weight without re-opening modals — and a visual cue when weight=0 so I know that cluster is silent.

**Acceptance Criteria:**
- [ ] `<ClustersSubsection>` live cluster cards expose an inline weight slider 0..100 + paired `<Input type="number">` two-way bound (numeric input on the right of slider)
- [ ] Drag/edit debounces 500ms then issues `PUT /admin/v1/clusters/{id}/weight {weight: <value>}`; rapid drags coalesce to the final value
- [ ] Optimistic UI: slider position + numeric input update immediately on drag; on 4xx response → revert + error toast with API message
- [ ] Tooltip on slider: "Share in default uniform routing for buckets without explicit Placement policy. weight=0 means no default-routed writes; reads + explicit policies still work."
- [ ] When `weight === 0` (still state=live) → render small muted chip next to slider: "weight=0 — no default-routed writes"
- [ ] On 409 Conflict (state changed mid-edit, e.g. operator drained) → revert slider + warning toast "Cannot update weight: cluster is no longer in live state"
- [ ] Unit test (jest/vitest as used in repo): mock fetch + simulate slider drag → assert one PUT after 500ms with final value; rapid drags collapse to single PUT
- [ ] Bundle size delta ≤ 3 KiB gzipped
- [ ] Typecheck passes
- [ ] Verify in browser using dev-browser skill

### US-005: End-to-end smoke + docs + ROADMAP close-flip + PRD removal
**Description:** As an operator and as a future-maintainer, I need the new cluster lifecycle validated end-to-end against the multi-cluster lab + documented so I trust the cycle is actually done.

**Acceptance Criteria:**
- [ ] `scripts/smoke-cluster-weights.sh` covers four walkthrough scenarios end-to-end against `docker compose --profile multi-cluster`
- [ ] Scenario A (new cluster activation): wipe `cluster_state` row for `cephb` directly via Cassandra/TiKV query → restart strata-multi → assert /clusters reports cephb state=pending weight=0; POST /clusters/cephb/activate {weight:10}; assert state=live weight=10; PUT 1000 chunks via a nil-policy bucket; assert ~100 chunks landed on cephb via `docker exec strata-cephb rados ls | wc -l` (5% tolerance); PUT /clusters/cephb/weight {weight:50}; PUT 1000 more chunks; assert next batch distribution ~50/50
- [ ] Scenario B (existing-live auto-detect at boot): plant N chunks on default cluster via direct rados PUT pre-restart; wipe `cluster_state` for default; restart strata-multi; assert auto-init sets default to state=live weight=100 (because bucket_stats reference) rather than pending
- [ ] Scenario C (explicit policy overrides weights): create bucket with `Placement={default:1, cephb:1}`; set cluster.weights `{default:100, cephb:10}`; PUT 200 chunks; assert ~100/100 distribution (bucket policy wins — 50/50 ratio from policy, NOT 100/10 from weights)
- [ ] Scenario D (pending excluded from default routing): wipe cluster_state for cephb → pending; PUT 100 chunks via nil-policy bucket; assert 0 chunks on cephb; activate cephb weight=10; PUT 100 more; assert ~10 land on cephb
- [ ] Script EXITS NON-ZERO on any assertion failure; per-scenario log lines so failing scenario is obvious
- [ ] `make smoke-cluster-weights` Makefile target wraps the script
- [ ] Playwright spec `web/e2e/cluster-weights.spec.ts` covers UI half: navigate to /storage → assert pending card with Activate button → open modal → drag slider to 25 → typed-confirm → Submit → assert card flips to live + weight chip shows 25 → drag inline slider on live card → 500ms debounce → PUT verified via page.route assertion → weight=0 case renders muted chip
- [ ] `docs/site/content/best-practices/placement-rebalance.md` gains new "Cluster lifecycle (register → activate → ramp)" section: full walkthrough table + the two-weight-layer explanation (bucket Placement vs cluster.weight) + tips on choosing initial weight (10% for production new-cluster, 100% for trusted clone)
- [ ] `docs/site/content/best-practices/web-ui.md` capability matrix gains rows for: Pending card variant, Activate modal, cluster weight slider, weight=0 chip
- [ ] Project root `CLAUDE.md` "Cluster state machine" section (add if absent) describes the 5-state machine + per-cluster weight semantics + the two-weight-layer rule
- [ ] `ROADMAP.md` close-flip the P2 entry → `~~**P2 — Default-routing weights + ...**~~ — **Done.** <one-line summary>. (commit \`<pending>\`)`; SHA backfilled in follow-up commit on main
- [ ] `tasks/prd-cluster-weights.md` REMOVED in the same commit (PRD lifecycle rule)
- [ ] Manual screenshots: pending card + Activate modal + live card with weight=25 + weight=0 chip — referenced in operator runbook
- [ ] `make docs-build` succeeds
- [ ] `make vet` succeeds
- [ ] `make test` succeeds
- [ ] `pnpm run build` succeeds
- [ ] `make smoke-cluster-weights` succeeds against the running multi-cluster lab
- [ ] Typecheck passes

## Functional Requirements

- **FR-1:** `cluster_state` row gains `weight int (0..100)` field; legal: any value in range, validated server-side
- **FR-2:** 5th state `pending` legal in the state machine; transitions: `(no row) → pending`, `pending → live` (via activate), `live → live` (weight update)
- **FR-3:** Boot-time reconcile auto-creates pending rows for new clusters in env + auto-detects existing-live via bucket_stats
- **FR-4:** Admin API `POST /clusters/{id}/activate {weight}` with audit + 409 if not pending
- **FR-5:** Admin API `PUT /clusters/{id}/weight {weight}` with audit + 409 if not live
- **FR-6:** `GET /clusters` response includes `weight` per cluster
- **FR-7:** `placement.PickCluster` synthesises default policy `{<each-live-with-weight>0>: <weight>}` when `bucket.Placement == nil` AND class env `spec.Cluster` is empty
- **FR-8:** Class env `@cluster` suffix preserved as explicit per-class pin (overrides default-routing)
- **FR-9:** Bucket explicit `Placement` policy always overrides cluster weights (two weight layers never combine)
- **FR-10:** Pending / draining_readonly / evacuating clusters excluded from default-routing synthesis regardless of weight
- **FR-11:** UI Pending card variant + Activate modal with typed-confirm + initial-weight slider
- **FR-12:** UI Live card inline weight slider with 500ms debounced PUT + weight=0 chip
- **FR-13:** 4-scenario smoke (new-cluster activation, existing-live auto-detect, explicit-policy override, pending excluded) verifies end-to-end
- **FR-14:** Playwright e2e covers UI half (pending card → activate → slider → debounce)
- **FR-15:** Operator runbook documents the cluster lifecycle + two-weight-layer rule

## Non-Goals

- **No automatic weight ramp-up timer.** Operator manually steps 10→25→50→100; auto-incremental ramp (with cooldown windows) is a future P3 if anyone asks.
- **No weight-history audit trail beyond audit rows.** Audit rows capture each weight change; no in-meta history table or rollback feature.
- **No combined two-layer weights.** Bucket Placement vs cluster.weight stay separate; multiplying / adding them out of scope (operator mental model would suffer).
- **No suggested initial weight from cluster fill telemetry.** Smart-default ("set this cluster's initial weight based on its free-space ratio") is out — operator sets the value.
- **No bulk activate.** One cluster at a time via admin API + UI; activating 5 clusters at once via single endpoint out of scope.
- **No automatic state transition timer.** Pending stays pending until operator activates; no "auto-activate after 24h" timer (operator must affirm).
- **No backwards-compat with absent `weight` field.** Going forward every cluster_state row carries weight; the absent-field default at decode is 0, NOT 100 — old rows must be migrated explicitly.
- **No new env knobs.** Operator workflow is admin-API driven; we do not add `STRATA_CLUSTER_DEFAULT_WEIGHT` or similar env.

## Design Considerations

- **Typed-confirm in Activate modal** matches existing `<ConfirmDrainModal>` precedent — same UI primitive, same case-sensitive match, same "type cluster id to arm submit" pattern.
- **Slider component**: reuse the existing `<Slider>` primitive used in BucketDetail Placement tab; paired numeric input mirrors the same pattern.
- **Pending card palette**: gray badge differs from live (green) / draining (orange) / evacuating (red) — visually distinct so operator can scan the page.
- **Boot reconcile log line** `"cluster auto-init"` should include reason field so operators can grep logs for "where did this weight come from".
- **Cluster-state cache** lives in-process (refresh 30s + invalidate-on-write); same pattern as the existing `placement.DrainCache` from drain-transparency cycle. Probably worth merging into a single `ClusterStateCache` that supersedes DrainCache.

## Technical Considerations

- **Cluster-state cache invalidation on weight PUT**: critical — without immediate invalidation operators see lag between admin endpoint success and routing change. PUT handler should call `cache.Invalidate(clusterID)` synchronously before returning 200.
- **Boot reconcile cost**: O(clusters in env). For typical deploys (<32 clusters) this is microseconds even on cold cassandra. No cache here — runs once at startup.
- **`bucket_stats` reference detection** for existing-live auto-detect: a cluster is "existing-live" if ANY bucket has chunks on it via the manifest layer. Cheap signal: walk `bucket_stats` once at boot, observe per-cluster sum > 0 in the BackendRef cluster field. Cassandra has the answer in O(buckets). For lab + smoke we plant a known chunk first to trigger the auto-live branch.
- **Two-weight-layer enforcement**: critical that `bucket.Placement != nil` short-circuits BEFORE consulting cluster weights. Otherwise tests get confused — bucket policy `{c1:1}` + cluster.weight `{c1:0}` should still route to c1 (operator said "this bucket goes here, period"). Order matters in PickCluster.
- **Migration on upgrade**: existing deploys WILL have cluster_state rows from drain-transparency cycle without `weight` field. Decoder default = 0 (NOT 100) per FR explicit rule. Boot-reconcile detects "row exists but weight==0 AND state==live" → treats as legacy-migrated, sets weight=100 if `bucket_stats` reference > 0 (or leave at 0 if not). Document this carefully.
- **Smoke scenario A "wipe cluster_state row"**: how to simulate "new cluster appears"? Direct meta wipe via cassandra cqlsh: `DELETE FROM cluster_state WHERE cluster_id='cephb';` then restart strata-multi. TiKV path uses `tikv-cli` (or admin endpoint dev-mode). Avoid adding a 3rd ceph container to compose just for this scenario — wipe + restart is cheaper.

## Success Metrics

- Operator can register a new cluster + activate it gradually (10% → 100%) entirely from the console + admin API (no env edit after restart, no SQL)
- Default-routing distribution among 3 live clusters with weights {10, 30, 60} → matches within 5% tolerance over 1000 PUTs
- Bucket explicit Placement policy ALWAYS overrides cluster weights (smoke scenario C verifies)
- Pending cluster receives 0 default-routed chunks (smoke scenario D verifies)
- 4-scenario smoke + Playwright UI e2e runs green against the multi-cluster lab in ≤ 3 minutes per scenario
- Zero `STRATA_CLUSTER_DEFAULT_WEIGHT`-style env knobs introduced; everything is admin-API driven

## Open Questions

- Should `pending` cluster appear in `<BucketReferencesDrawer>` of an evacuate impact analysis? Recommendation: yes — operator may want to see "if I'd just activated this, here's what would shift". But out of scope for this cycle; defer.
- Should the cluster weight slider on the live card be replaced by a step-button group ("10 | 25 | 50 | 100") for ergonomic ramping? Recommendation: slider + numeric input together (current scope) — operator can pick any value, no preset bias.
- Should boot-reconcile log a one-line summary like `"cluster reconcile: <N> auto-pending, <M> existing-live"`? Yes — helpful in startup logs; include in US-001 acceptance as a bonus.
