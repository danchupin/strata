# PRD: Drain transparency + drain/evacuate split (always-strict)

## Introduction

The `ralph/drain-lifecycle` cycle shipped drain primitives with an opt-in `STRATA_DRAIN_STRICT` env + soft fail-open fallback semantics + a single drain action that always triggered migration. Operator walkthrough exposed five gaps:

1. **Toggleable strict mode is confusing.** Drain should be a hard semantic guarantee, not an opt-in.
2. **Drain blocks are silent.** Operator can drain a cluster and walk away assuming chunks will migrate; reality is buckets with single-cluster policy or no policy at all get stuck forever with no UI surface flagging it.
3. **One drain mode conflates two operator intents.** "Stop writes for a maintenance window" is a different workflow from "Fully evacuate a cluster to deregister it" — both currently routed through the same admin action.
4. **In-flight uploads break.** Drain currently has no graceful contract for open multipart sessions on the draining cluster.
5. **No pre-drain analysis or bulk policy fix surface.** Operator must inspect each bucket individually to discover what will/won't migrate.

This PRD fixes all five: hard-wired always-strict semantics (env removed), explicit drain/evacuate mode split, pre-drain impact analysis with bulk policy fix dialog, graceful in-flight multipart handling, and a redesigned progress bar that categorizes stuck chunks with click-through actions.

Closes new ROADMAP P3 *Drain transparency + drain/evacuate split (always-strict)* (Web UI section). Adds remediation note to the prior closed P3 *Drain strict mode for PUT routing fallback*: env removed in this cycle.

## Goals

- Hard-wired always-strict drain — no `STRATA_DRAIN_STRICT` env, no `drain_strict` admin field, no "strict" UI chip
- 4-state cluster machine `{live, draining_readonly, evacuating, removed}` replacing the current 3-state machine
- `POST /admin/v1/clusters/{id}/drain {mode: "readonly"|"evacuate"}` action splits the two operator workflows
- New `GET /admin/v1/clusters/{id}/drain-impact` pre-evacuate analysis endpoint with categorized chunk counts + per-bucket suggested policies
- `<ConfirmDrainModal>` mode picker (radio) + impact analysis for evacuate mode + Submit disabled when stuck>0
- New `<BulkPlacementFixDialog>` lets operator fix N affected buckets in one workflow (per-bucket suggestion picker + "Apply uniform to all" toggle)
- `<DrainProgressBar>` shows categorized counts (Migrating / Stuck-single-policy / Stuck-no-policy) + mode label + click-through drawer
- In-flight multipart sessions on draining cluster complete normally (UploadPart + Complete/Abort routed via stored multipart handle cluster id, bypassing placement picker)
- Three-scenario smoke + Playwright e2e covering: stop-writes drain (maintenance), full evacuate (decommission), upgrade readonly→evacuating

## User Stories

### US-001: State machine `{live, draining_readonly, evacuating, removed}` + admin API mode picker
**Description:** As an operator, I want two distinct drain modes — stop-writes-only for maintenance and full evacuate for decommission — so the action I take matches my actual intent.

**Acceptance Criteria:**
- [ ] `cluster_state` meta wire shape gains `mode string` field alongside `state string`; legal combinations: `state=live mode=""`, `state=draining_readonly mode="readonly"`, `state=evacuating mode="evacuate"`, `state=removed mode=""`
- [ ] Migration: existing `state="draining"` rows are rewritten to `state="evacuating" mode="evacuate"` on first read (preserves current behavior — option 1A from clarifying)
- [ ] Memory + Cassandra + TiKV backends implement the new shape; contract test `caseClusterStateModes` in `internal/meta/storetest/contract.go` exercises every transition
- [ ] `POST /admin/v1/clusters/{id}/drain` accepts JSON body `{mode: "readonly"|"evacuate"}` (no default — operator MUST pick); 400 BadRequest if `mode` missing or not in enum
- [ ] Transitions enforced: `live → draining_readonly` (mode=readonly), `live → evacuating` (mode=evacuate), `draining_readonly → evacuating` (mode=evacuate, "upgrade" path), `draining_readonly → live` (undrain), `evacuating → live` (undrain — migration stops, no rollback per 2A)
- [ ] Invalid transitions return 409 Conflict with `code=InvalidTransition` and `current_state`+`requested_mode` fields
- [ ] `POST /admin/v1/clusters/{id}/undrain` works from both `draining_readonly` and `evacuating` states; flips to `live`; moved-chunks stay on target cluster (no reverse migration)
- [ ] `GET /admin/v1/clusters` response: `state` field carries the 4-state value; new `mode` field present when state != live/removed
- [ ] `GET /admin/v1/clusters/{id}/drain-progress` returns `mode` field alongside existing fields
- [ ] OpenAPI spec at `internal/adminapi/openapi.yaml` updated
- [ ] Audit row stamped with action `admin:DrainCluster` + resource `cluster:<id>` + body `{mode}` for traceability
- [ ] Unit test: every legal + illegal transition pair
- [ ] Integration test: existing `state="draining"` row migrates to `state="evacuating" mode="evacuate"` on first read in all three backends
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: Worker scan categorization (evacuating only) + ProgressTracker
**Description:** As a developer, I want the rebalance worker to categorize chunks on draining clusters into migratable / stuck-single-policy / stuck-no-policy so the operator UI can show what's blocking drain.

**Acceptance Criteria:**
- [ ] `internal/rebalance.ProgressSnapshot` gains three fields (replacing flat `Chunks` int): `MigratableChunks int`, `StuckSinglePolicyChunks int`, `StuckNoPolicyChunks int`; total `Chunks()` derived; existing fields kept
- [ ] Worker scan loop (`internal/rebalance/worker.go::scanDistribution`) classifies each chunk on a draining cluster:
  - `MigratableChunks` — bucket has Placement policy AND `PickClusterExcluding(policy, draining)` returns a non-empty cluster id
  - `StuckSinglePolicyChunks` — bucket has Placement policy AND `PickClusterExcluding(policy, draining)` returns "" (every cluster in policy is draining)
  - `StuckNoPolicyChunks` — bucket has Placement == nil; class-env routing brought the chunk here; worker has no target
- [ ] Per-bucket breakdown stored: `progressCache[clusterID].byBucket map[string]BucketScanCategory` with `{Category: "migratable"|"stuck_single_policy"|"stuck_no_policy", ChunkCount, BytesUsed}`
- [ ] Worker scan happens ONLY when state=evacuating; readonly state skips the scan (no migration triggered → no progress data needed)
- [ ] `/drain-progress` endpoint returns the three counts + total (sum); `eta_seconds` computed against `MigratableChunks` only (stuck chunks never converge to 0)
- [ ] `deregister_ready = state=evacuating AND total_chunks == 0` (all three categories at 0)
- [ ] Unit test: scan a fixture bucket with each category configuration; assert counters land in the correct bucket
- [ ] Integration test (multi-cluster lab): drain cephb with mixed buckets → progress endpoint reports correct per-category breakdown
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: `/drain-impact` pre-evacuate analysis endpoint
**Description:** As an operator, I want a preview of what would happen if I evacuate a cluster — categorized chunk counts plus suggested policy updates per affected bucket — so I can fix stuck buckets before committing.

**Acceptance Criteria:**
- [ ] New admin endpoint `GET /admin/v1/clusters/{id}/drain-impact` returns JSON `{cluster_id, current_state, migratable_chunks, stuck_single_policy_chunks, stuck_no_policy_chunks, total_chunks, by_bucket: [{name, current_policy: map[string]int|null, category: string, chunk_count: int, bytes_used: int, suggested_policies: [{label, policy}]}], last_scan_at}`
- [ ] Callable when state ∈ {live, draining_readonly} (preview before evacuate transition); 409 Conflict when state ∈ {evacuating, removed} (use `/drain-progress` instead)
- [ ] If state=live, runs synchronous one-off scan via the rebalance worker (worker isn't normally scanning live clusters); 5-minute cache to dedup concurrent admin requests
- [ ] If state=draining_readonly, returns cached scan from progressCache if available (worker scans periodically while readonly even though it doesn't migrate); cache miss → triggers synchronous scan
- [ ] `suggested_policies` per bucket — two choices (option 4A):
  - `{label: "Add all live clusters (uniform)", policy: {<current keys with weight 0 for draining>:0, <every live cluster>:1}}`
  - `{label: "Replace draining with <id>", policy: {<each live cluster>: existing weight from draining}}` — generates one entry per live cluster (operator picks)
- [ ] For `stuck_no_policy` buckets (no current_policy), `current_policy: null` and suggested_policies offer `{label: "Set initial policy: live clusters uniform", policy: {<each live>: 1}}` + per-live-cluster single-target options
- [ ] OpenAPI spec updated
- [ ] Audit row stamped with `admin:GetClusterDrainImpact`
- [ ] Pagination via `?limit=N&offset=M` (default limit=100) on `by_bucket` list; sort: stuck_single_policy first, then stuck_no_policy, then migratable; within category descending by chunk_count
- [ ] Unit test: stub progressCache + meta.ListBuckets → endpoint produces expected categorized output + suggested policies
- [ ] Integration test (multi-cluster lab): set up the three demo buckets (split / cephb / no-policy), call `/drain-impact` on cephb → assert categorized counts match physical chunk distribution
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: ConfirmDrainModal redesign — mode picker + preflight + block-if-stuck
**Description:** As an operator, I want the drain modal to make me pick a mode, show me the impact if I'm evacuating, and refuse to submit if any chunks would be stuck — so I can never accidentally leave data behind.

**Acceptance Criteria:**
- [ ] `web/src/components/storage/ConfirmDrainModal.tsx` rewritten: top section is a radio mode picker with two options — "Stop new writes (maintenance)" (selected by default) and "Full evacuate (decommission)" — each with a brief description
- [ ] When mode=readonly: modal skips impact analysis; submit button enabled after typed-confirm input matches cluster id
- [ ] When mode=evacuate: modal fetches `/drain-impact` on mode flip; renders a categorized impact section: "X migratable · Y stuck single-policy · Z stuck no-policy"
- [ ] Stuck row(s) render with amber background + "Fix N buckets" CTA button opening `<BulkPlacementFixDialog>` (US-005)
- [ ] Submit button text: "Drain (X chunks will migrate)" when stuck=0; "Drain blocked — fix N stuck buckets" when stuck>0; DISABLED while stuck>0
- [ ] Mode flip refetches impact; bulk-fix completion refetches impact; counts update live
- [ ] Existing typed-confirm input persists (operator types cluster id to enable Drain even after stuck=0)
- [ ] Cancel button closes modal without side effects
- [ ] When operator is upgrading from `draining_readonly` (not first-time drain), modal title reads "Upgrade to evacuate" and the readonly radio is hidden (only evacuate option visible — undrain is the way back to live)
- [ ] Bundle size delta ≤ 5 KiB gzipped
- [ ] Typecheck passes
- [ ] Verify in browser using dev-browser skill

### US-005: BulkPlacementFixDialog — per-bucket suggestion picker + Apply-uniform-to-all
**Description:** As an operator, I want to fix many affected buckets in one workflow without clicking through to each BucketDetail individually.

**Acceptance Criteria:**
- [ ] New component `web/src/components/storage/BulkPlacementFixDialog.tsx`
- [ ] Opens from "Fix N buckets" CTA in `<ConfirmDrainModal>` (US-004); receives the `by_bucket` list from the parent's cached `/drain-impact` response
- [ ] Renders multi-select bucket table with columns: checkbox, bucket name, current policy summary, category badge (amber stuck / green migratable), suggested-policy dropdown (per bucket — operator picks from the suggested_policies offered)
- [ ] Suggested-policy dropdown default: the first suggested policy from the API (uniform live clusters)
- [ ] Top-of-dialog "Apply uniform to all selected" toggle: when on, all checked buckets get the same policy (operator picks ONE suggestion from a dialog-level dropdown which then overrides individual per-bucket picks)
- [ ] "Select all" / "Deselect all" buttons
- [ ] "Apply" button: issues `PUT /admin/v1/buckets/{name}/placement` for every selected bucket with the chosen policy; bulk progress (e.g. "3 / 5 done"); failures don't block remaining buckets — collected and toasted at the end
- [ ] On all-success or partial-success-with-some-applied, dialog closes; parent modal refetches `/drain-impact`
- [ ] Cancel button closes dialog without applying anything
- [ ] Bundle size delta ≤ 4 KiB gzipped
- [ ] Unit test: rendering with mixed-category buckets + apply-to-all toggle behavior
- [ ] Typecheck passes
- [ ] Verify in browser using dev-browser skill

### US-006: DrainProgressBar redesign — categorized + mode label + drill-down drawer
**Description:** As an operator, I want the progress bar on an evacuating cluster to break down what's migrating vs what's stuck so I always know whether I need to take action.

**Acceptance Criteria:**
- [ ] `web/src/components/storage/DrainProgressBar.tsx` rewritten: renders three sub-counters with color (green Migrating / amber Stuck single-policy / amber Stuck no-policy); total badge sums them
- [ ] Mode label rendered above the counters: "Evacuating" (red text) or "Stop-writes" (orange text); cluster card state badge mirrors
- [ ] When mode=readonly: bar collapses to a single chip "Stop-writes drain — no migration; reads/deletes/in-flight multipart continue. Undrain to resume writes."
- [ ] Stuck counters are clickable → open `<StuckBucketsDrawer>` with the per-bucket breakdown from `/drain-progress` (returns the same `by_bucket` shape as `/drain-impact`); each row has "Edit policy" link to BucketDetail Placement tab in a new tab
- [ ] ETA renders only when migratable > 0 (no ETA when all remaining chunks are stuck)
- [ ] Deregister-ready green chip renders when `state=evacuating AND total_chunks == 0` (all three categories at 0); chip text: "✓ Ready to deregister (env edit + restart)"
- [ ] When `state=draining_readonly`: the bar is replaced by an "Upgrade to evacuate" button that opens `<ConfirmDrainModal>` in upgrade mode (US-004); plus a small "Undrain" button next to it
- [ ] Polling 30s shared query key
- [ ] Bundle size delta ≤ 4 KiB gzipped
- [ ] Typecheck passes
- [ ] Verify in browser using dev-browser skill

### US-007: Always-strict refactor + remove STRATA_DRAIN_STRICT env + in-flight multipart graceful handling
**Description:** As an operator, I want drain to be unconditionally strict and never break my in-flight multipart sessions so I have a hard semantic guarantee.

**Acceptance Criteria:**
- [ ] `STRATA_DRAIN_STRICT` env reference removed from `internal/data/rados/Config`, `internal/data/s3/Config`, `internal/config/config.go`, `internal/serverapp/serverapp.go`, all tests, all docs
- [ ] `drain_strict: bool` field removed from `GET /admin/v1/clusters` JSON response + OpenAPI spec
- [ ] "strict" chip removed from `web/src/components/storage/ClustersSubsection.tsx` cluster cards
- [ ] `RADOS Backend.PutChunks` (and S3 symmetric) ALWAYS refuses fallback to `state ∈ {draining_readonly, evacuating}` clusters → `data.ErrDrainRefused`; no env gate
- [ ] HTTP 503 wire shape unchanged: `<Code>DrainRefused</Code>`, body explains "cluster is draining", `Retry-After: 300`
- [ ] Counter relabeled: `strata_putchunks_refused_total{reason="drain_refused",cluster}` (was `reason="drain_strict"`) — hard switch per option 3A, dashboards must update
- [ ] In-flight multipart graceful contract: open multipart sessions store the cluster id in the multipart handle (`cluster\x00bucket\x00key\x00uploadID`); UploadPart routes via the stored cluster id, BYPASSES `placement.PickClusterExcluding`; Complete/Abort similarly bypass; this means an in-flight multipart finishes on the draining cluster even though new PUTs are refused
- [ ] New Multipart Init AFTER drain goes through `PickClusterExcluding` → excludes draining → picks alternative or returns "" → 503 if no fallback
- [ ] Integration test: open multipart on `cephb` (Init + UploadPart x2) → drain cephb (mode=evacuate) → UploadPart third part → expect 200 OK (graceful) → Complete → expect 200 OK → object readable from cephb
- [ ] Integration test: open multipart on cephb → drain cephb → Abort → expect 200 OK + parts cleaned on cephb
- [ ] Integration test: NEW Multipart Init after drain on bucket with no alternative cluster in policy → expect 503 DrainRefused
- [ ] Unit test: env loader fails fast or logs WARN+ignore if legacy `STRATA_DRAIN_STRICT` env still set (option 4A — fail-fast preferred since no prod deploys)
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-008: Smoke + 3-scenario walkthrough + docs + ROADMAP close-flip + PRD removal
**Description:** As an operator and as a future-maintainer, I need the new drain semantics validated end-to-end (per `feedback_cycle_end_to_end.md`) against the lab stand and documented so I trust the cycle is actually done.

**Acceptance Criteria:**
- [ ] `scripts/smoke-drain-transparency.sh` covers three walkthrough scenarios end-to-end against `docker compose --profile multi-cluster`:
  - **Scenario A (stop-writes drain):** create bucket with `Placement={cephb:1,default:1}`; PUT 5 objects; POST drain cephb `{mode:"readonly"}` → assert state=draining_readonly mode=readonly; new PUT to cephb-routed key → 503 DrainRefused; GET existing object → 200; in-flight multipart Init+Upload+Complete on cephb mid-drain → 200; POST undrain → state=live; new PUT → 200
  - **Scenario B (full evacuate):** create three buckets (`demo-split` `{cephb:1,default:1}`, `demo-cephb` `{cephb:1}`, `demo-residual` no policy with stale chunks on cephb); GET /drain-impact on cephb → categorized counts (5 migratable, 5 stuck_single_policy, 2 stuck_no_policy); attempt drain `{mode:"evacuate"}` → 409 Conflict with stuck breakdown; PUT `demo-cephb` placement `{cephb:1,default:1}` + PUT `demo-residual` placement `{default:1}`; retry drain `{mode:"evacuate"}` → 200; wait for worker tick; assert chunks_remaining drops to 0; assert deregister_ready=true
  - **Scenario C (upgrade readonly→evacuate):** start at `state=draining_readonly` from scenario A; POST drain `{mode:"evacuate"}` (state transition); /drain-impact returns stuck breakdown; fix + retry → 200 → migration runs to completion
- [ ] `make smoke-drain-transparency` Makefile target wraps the script
- [ ] Playwright spec `web/e2e/drain-transparency.spec.ts` covers UI half of scenarios A + B + C: clicks mode picker + observes impact analysis + opens BulkPlacementFixDialog + applies bulk fix + watches DrainProgressBar categorized counts + sees Ready-to-deregister chip
- [ ] `docs/site/content/best-practices/placement-rebalance.md` "Drain lifecycle" section rewritten: clear separation between "Stop-writes mode (maintenance)" and "Evacuate mode (decommission)"; walkthrough table for each scenario; pre-drain checklist updated to reference impact analysis UI
- [ ] `docs/site/content/best-practices/web-ui.md` capability matrix updated: BulkPlacementFixDialog row added; strict chip row removed (chip removed in US-007)
- [ ] Project root `CLAUDE.md` "Background workers" section updated: rebalance bullet describes scan happens only for state=evacuating
- [ ] `ROADMAP.md` close-flip new P3 "Drain transparency + drain/evacuate split (always-strict)" → `~~**P3 — ...**~~ — **Done.** <one-line summary>. (commit \`<pending>\`)`
- [ ] `ROADMAP.md` adds follow-up note to the closed P3 "Drain strict mode for PUT routing fallback": "The opt-in `STRATA_DRAIN_STRICT` env shipped here was removed in `ralph/drain-transparency` (commit `<pending>`) — drain is now unconditionally strict; strict-chip + admin field also removed."
- [ ] `tasks/prd-drain-transparency.md` REMOVED in the same commit (PRD lifecycle rule)
- [ ] `make docs-build` succeeds
- [ ] `make vet` succeeds
- [ ] `make test` succeeds
- [ ] `pnpm run build` succeeds
- [ ] `make smoke-drain-transparency` succeeds against the running multi-cluster lab
- [ ] Manual screenshots of each scenario's key UI states captured + embedded in operator runbook
- [ ] Typecheck passes

## Functional Requirements

- **FR-1:** `cluster_state` schema gains `mode string` alongside `state string`; 4-state machine `{live, draining_readonly, evacuating, removed}` with mode `""|"readonly"|"evacuate"|""` respectively
- **FR-2:** `POST /admin/v1/clusters/{id}/drain {mode: "readonly"|"evacuate"}` — mode required; transitions enforced
- **FR-3:** `POST /admin/v1/clusters/{id}/undrain` flips back to live from both draining states; no chunk rollback
- **FR-4:** Migration: existing `state="draining"` rows rewritten to `state="evacuating" mode="evacuate"` on first read
- **FR-5:** Worker scan + ProgressTracker categorization (migratable / stuck_single_policy / stuck_no_policy) — only when state=evacuating
- **FR-6:** `GET /admin/v1/clusters/{id}/drain-impact` returns categorized counts + per-bucket breakdown + suggested_policies (two choices: uniform live + replace-with-single-live)
- **FR-7:** `ConfirmDrainModal` mode picker; evacuate mode runs preflight via `/drain-impact`; Submit disabled while stuck>0
- **FR-8:** `BulkPlacementFixDialog` multi-select bucket fix; per-bucket suggestion picker + Apply-uniform-to-all toggle; bulk PUT placement
- **FR-9:** `DrainProgressBar` categorized counters + mode label + stuck-buckets drawer drill-down
- **FR-10:** `STRATA_DRAIN_STRICT` env, `drain_strict` admin field, and "strict" UI chip removed
- **FR-11:** PutChunks fallback to draining cluster always 503 DrainRefused; counter metric renamed `reason="drain_refused"`
- **FR-12:** In-flight multipart sessions on draining cluster complete normally (UploadPart + Complete + Abort routed via stored multipart handle cluster id)
- **FR-13:** 3-scenario smoke + Playwright e2e verify end-to-end
- **FR-14:** ROADMAP entries updated: new P3 close-flip + follow-up note on prior closed P3

## Non-Goals

- **No drain scheduling.** Drain happens immediately; "drain at 02:00 UTC" out of scope.
- **No bulk drain.** Drain one cluster at a time via admin API.
- **No reverse migration on undrain.** Moved chunks stay on target cluster; undrain just stops further planning (option 2A).
- **No legacy env back-compat.** `STRATA_DRAIN_STRICT` removed cleanly; fail-fast if still set (no prod deploys to migrate per user statement).
- **No custom suggested policy.** Suggested policies are derived from live cluster set; operator picks from the offered options or edits via BucketDetail Placement tab manually.
- **No state-machine extensibility hook.** 4-state machine is closed; new modes require a fresh PRD.
- **No multipart abort enforcement on drain.** In-flight multipart sessions are allowed to complete on the draining cluster — graceful, not stop-the-world.
- **No mandatory dual-cluster minimum.** Operator can drain the only cluster in a single-cluster deployment; everything will be stuck and drain proceeds in readonly mode happily.

## Design Considerations

- **Mode picker UI:** radio buttons, not dropdown — both modes need to be visible side-by-side with descriptions
- **Stuck row color:** amber matches the existing `<PolicyDrainWarning>` chip palette
- **Per-bucket suggestion dropdown** in BulkPlacementFixDialog reuses the existing `<Select>` primitive
- **State machine migration** is read-time, not boot-time: avoids a separate migration step; existing draining rows pick up the new shape lazily
- **`POST /drain` body required** (no default mode) — forces operator intent every time; CLI scripts must adapt
- **Drawer drill-down** from DrainProgressBar reuses the existing `<BucketReferencesDrawer>` shape (placement-ui US-006) but populated from `/drain-progress` per-bucket data
- **Existing closed strict P3 entry**: keep the entry's `~~strikethrough~~ Done` mark but append a paragraph with the env-removal note — this preserves the audit trail of the wrong-shape decision and its remediation

## Technical Considerations

- **In-flight multipart routing**: critical — UploadPart MUST NOT consult placement.PickClusterExcluding. It MUST read the cluster id from the multipart handle directly. Verify both RADOS and S3 multipart paths.
- **State machine migration on read** (FR-4): cleanest place is in `meta.Store.GetClusterState` — when reading legacy `state="draining"` row, transparently convert to `state="evacuating" mode="evacuate"` AND write back. Avoids stale clusters on next read.
- **Synchronous scan on `/drain-impact` for state=live**: triggers a one-shot bucket+manifest scan in the admin handler thread (worker isn't scanning live clusters). For large clusters this could be slow; document as O(buckets × objects) cost. 5-minute cache deduplicates concurrent UI loads.
- **Suggested policy edge cases**: if there's only ONE cluster total (no alternative), suggested_policies is empty array; UI must handle "no fix available" + advise operator to register another cluster first.
- **Counter rename**: existing dashboards must update `reason="drain_strict"` → `reason="drain_refused"`. Operator runbook documents the rename in a "Breaking changes" section.
- **`POST /undrain` from evacuating mid-migration**: documented as "rollback stops planning; already-moved chunks stay on target". Doc must be explicit — surprising operator who expects reversal.
- **Audit row** on every drain mode change carries the mode in the body for forensic clarity.

## Success Metrics

- Operator can categorize chunk impact before evacuate via `/drain-impact` in one click (modal mode flip)
- Bulk policy fix for N affected buckets completes in one operator flow (no per-bucket navigation)
- In-flight multipart sessions never fail mid-drain — integration test asserts 200 OK on UploadPart + Complete against a draining cluster
- 3-scenario smoke runs green against the multi-cluster lab in ≤ 2 minutes per scenario
- Zero `STRATA_DRAIN_STRICT` env references remain in code, docs, or compose files after the cycle merges

## Open Questions

- Should the "Upgrade to evacuate" path on a draining_readonly cluster require re-typing the cluster id in the confirm modal? (Recommended: yes — re-confirm because evacuate is the heavier action; consistent with the safety pattern.)
- Should `/drain-impact` results be cached per-cluster across admin requests for live clusters? (Recommended: yes, 5-min TTL; reduces O(buckets × objects) scan cost.)
- Should the BulkPlacementFixDialog let operator de-select buckets they don't want to touch? (Recommended: yes — checkboxes default to all-selected; operator can opt-out individual buckets if they want to handle some manually.)
