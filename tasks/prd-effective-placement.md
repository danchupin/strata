# PRD: Effective-policy fallback to cluster weights

## Introduction

drain UX currently forces operator to edit per-bucket Placement via `<BulkPlacementFixDialog>` when buckets have `Placement={X:1}` only and X is draining. But cluster weights already define global default routing — silly to force per-bucket edits when cluster weights can serve as fallback target. Operator design critique surfaced post drain-followup merge.

This cycle adds `placement.EffectivePolicy` helper that tries bucket Placement first (excluding draining), falls back to synthesised cluster-weights policy. Per-bucket `PlacementMode="strict"` opt-in preserves explicit-pin semantic for compliance-sensitive buckets (data-sovereignty, replication design). Default `"weighted"` mode maximizes operator-friendliness.

Closes ROADMAP P2 *Effective-policy fallback to cluster weights — eliminate forced Placement edit on drain* (added in commit `aa83664`).

## Goals

- New `bucket.PlacementMode` field (`"weighted"` default | `"strict"`) persisted across all 3 meta backends
- `placement.EffectivePolicy(bucket.Placement, bucket.PlacementMode, cluster.weights, clusterStates) map[string]int` helper
- PUT chunks routing path + rebalance worker classifier use EffectivePolicy
- `stuck_single_policy` category in `/drain-impact` fires ONLY for strict-flagged buckets
- `<BulkPlacementFixDialog>` filters to strict-flagged stuck buckets only
- BucketDetail Placement tab gains "Strict placement" toggle with explainer tooltip
- 9-step smoke + Playwright validation against multi-cluster lab
- ROADMAP P2 entry close-flipped

## State Truth Tables

### EffectivePolicy decision matrix

| bucket.Placement | mode | clusters in policy live? | cluster.weights live? | Returns | Rebalance classifier |
|------------------|------|--------------------------|----------------------|---------|----------------------|
| nil/empty | — | n/a | yes | cluster.weights | migratable (when chunk on draining cluster, target = weights) |
| nil/empty | — | n/a | no (all drained) | empty | stuck_no_policy |
| non-empty | weighted | yes (some live) | — | bucket.Placement (excluding draining) | migratable |
| non-empty | weighted | no (all draining) | yes | cluster.weights (synthesised fallback) | migratable |
| non-empty | weighted | no | no | empty | stuck_no_policy |
| non-empty | strict | yes (some live) | — | bucket.Placement (excluding draining) | migratable |
| non-empty | strict | no (all draining) | — | empty (no fallback) | stuck_single_policy |

### UI BulkPlacementFixDialog content

| Stuck bucket count | mode breakdown | Dialog renders |
|-------------------|----------------|----------------|
| 0 | n/a | hidden (no Fix CTA) |
| N>0 | all weighted | hidden — auto-resolved via cluster weights |
| N>0 | some strict | dialog opens with ONLY strict buckets; CTA "Fix N compliance-locked buckets" |

## Cache Invalidation Ledger

| Cache | TTL | Invalidation triggers |
|-------|-----|----------------------|
| `placement.DrainCache` (existing) | 30s | POST /clusters/{id}/drain, POST /clusters/{id}/undrain, POST /clusters/{id}/activate |
| `drainImpactCache` (existing) | 5min | PUT /buckets/{name}/placement (includes mode change), DELETE /buckets/{name}/placement, DELETE /buckets/{name} |

No new caches introduced this cycle. Existing drainImpactCache already invalidates on placement PUT — that PUT now carries the new `mode` field, so cache stays correct.

## Safety Claims Preconditions

| UI claim | Preconditions | Verified by story |
|----------|---------------|-------------------|
| "Weighted bucket auto-fallback on drain" | bucket.PlacementMode == "weighted" AND at least one cluster.weight live | US-002 EffectivePolicy logic + US-003 worker classifier |
| "Strict bucket blocks drain" | bucket.PlacementMode == "strict" AND all bucket.Placement entries drained | US-003 classifier marks stuck_single_policy |
| "stuck_no_policy fires only when ALL clusters drained" | EffectivePolicy returned empty AND mode == "weighted" AND cluster-weights returns empty | US-003 |

## User Journey Walkthrough

Pre-cycle walkthrough per `feedback_cycle_end_to_end.md`. Operator scenario: multi-cluster lab with mixed bucket policies + drain cephb.

| # | Action | Surface | Story |
|---|--------|---------|-------|
| 1 | Operator runs lab with 4 buckets: A (`{default:1,cephb:1}`); B (`{cephb:1}` weighted); C (`{cephb:1}` strict); D (no Placement) | meta seeded | (test fixture) |
| 2 | Cluster weights `{default:50, cephb:50}` set | existing UI | (existing) |
| 3 | Operator clicks Drain on cephb card | `<ConfirmDrainModal>` opens | (existing) |
| 4 | Modal fetches /drain-impact in evacuate mode | preflight API | (existing — invalidates cache from US-002 of drain-followup) |
| 5 | /drain-impact returns: migratable=N (A+B+D), stuck_single_policy=1 (C), stuck_no_policy=0 | server categorizer using EffectivePolicy | **US-002 + US-003** |
| 6 | Modal renders "1 bucket needs explicit Placement edit (compliance)" + Fix CTA | UI updates | **US-005** |
| 7 | Operator clicks "Fix 1 compliance-locked bucket" → BulkPlacementFixDialog opens with ONLY bucket-C | filtered list | **US-005** |
| 8 | Dialog offers C three options: (a) keep strict + edit Placement; (b) flip to weighted; (c) keep strict + acknowledge stuck | per-bucket actions | **US-005** |
| 9 | Operator picks "Flip to weighted" → PUT placement updates mode=weighted; impact cache invalidated; modal refetches → stuck=0 | mode toggle | **US-001 + US-004** |
| 10 | Drain Submit unlocked; operator types "cephb" + Submit → state=evacuating | (existing) | (existing) |
| 11 | Rebalance worker plans moves for A, B, C, D all using EffectivePolicy → all migrate to default | worker logic | **US-003** |
| 12 | After drain completes, BucketDetail header for C shows muted "strict OFF" indicator (was on, now flipped) | BucketDetail surfacing | **US-004** |

Negative paths:
- All clusters drained → EffectivePolicy returns empty for all buckets → stuck_no_policy populated → 503 DrainRefused on writes (strict-mode global, from drain-cleanup)
- Strict bucket with operator-chose-to-keep-stuck → stuck_single_policy persists → drain Submit stays blocked → operator must edit Placement manually OR accept drain stays open
- PUT to weighted bucket while its only-cluster is draining → routes silently to cluster-weights target (no 503, no warning); operator informed via audit log

## User Stories

### US-001: `bucket.PlacementMode` field + meta wire shape + admin API
**Description:** As a developer, I need a per-bucket `PlacementMode` field so the rebalance + PUT-routing paths can distinguish strict (compliance) buckets from weighted (auto-fallback) buckets.

**Acceptance Criteria:**
- [ ] Add `PlacementMode string` field to `meta.Bucket` with `json:"placement_mode,omitempty"` tag; legal values `""` (== "weighted" per default), `"weighted"`, `"strict"`
- [ ] Validation helper `meta.ValidatePlacementMode(s string) error` accepts empty/weighted/strict; rejects others with `ErrInvalidPlacementMode`
- [ ] Admin API `PUT /admin/v1/buckets/{name}/placement` body extended with `mode: "weighted"|"strict"` (optional — absence = "weighted")
- [ ] `GET /admin/v1/buckets/{name}/placement` response carries `mode` field always (computed: empty/NULL → "weighted")
- [ ] `meta.Store` SetBucketPlacement signature extended OR a new `SetBucketPlacementMode` triple — caller's choice; prefer single PUT with combined payload for atomicity
- [ ] Memory + Cassandra + TiKV implement in lockstep:
  - Cassandra: `ALTER TABLE buckets ADD placement_mode text` (idempotent via existing `alterStatements` helper that swallows "column already exists")
  - TiKV: extend the Bucket JSON blob encoding with the new field (schema-additive — old blobs decode mode as "")
  - Memory: extend the in-memory struct field
- [ ] New contract test `casePlacementMode` in `internal/meta/storetest/contract.go` exercises read/write of all three values across all backends + backwards-compat (rows without the column read as "weighted")
- [ ] Audit row stamped via `s3api.SetAuditOverride(ctx, "admin:UpdateBucketPlacementMode", "bucket:<name>", ...)` when mode changes
- [ ] OpenAPI spec at `internal/adminapi/openapi.yaml` updated: PUT placement body schema gains `mode` field with enum constraint
- [ ] Unit tests on the validation path
- [ ] `go vet ./...` passes; typecheck passes; tests pass

### US-002: `placement.EffectivePolicy` helper
**Description:** As a developer, I need a single helper that resolves the effective routing policy from `(bucket.Placement, bucket.PlacementMode, cluster.weights, clusterStates)` so both PUT routing AND rebalance worker classifier share the same logic.

**Acceptance Criteria:**
- [ ] New helper `placement.EffectivePolicy(bucketPolicy map[string]int, mode string, clusterWeights map[string]int, clusterStates map[string]meta.ClusterStateRow) map[string]int` in `internal/data/placement/router.go` (or new file)
- [ ] Logic:
  - Compute `liveBucketPolicy` = `bucketPolicy` with entries removed where `clusterStates[id].State ∈ {draining_readonly, evacuating, pending, removed}` (any state ≠ live)
  - If `len(liveBucketPolicy) > 0` → return liveBucketPolicy (bucket Placement still has at least one live target)
  - If `len(liveBucketPolicy) == 0` AND `mode == "strict"` → return empty (caller falls back to class env spec.Cluster; strict-refuse semantic preserved)
  - If `len(liveBucketPolicy) == 0` AND `mode != "strict"` (i.e. "" or "weighted") → return synthesised `cluster.weights` filtered to live + weight>0 (matches existing `placement.DefaultPolicy` semantic from cluster-weights cycle)
  - If both empty → return empty (genuine no-target case)
- [ ] Treat `mode == ""` as equivalent to `"weighted"` (backwards-compat default for legacy buckets)
- [ ] Unit tests cover all branches: nil bucketPolicy + weighted (returns cluster.weights); nil bucketPolicy + strict (treats as weighted — strict requires explicit policy); bucketPolicy with all live; bucketPolicy with mixed live/draining; bucketPolicy all draining + weighted (fallback to weights); bucketPolicy all draining + strict (returns empty); all clusters drained both ways
- [ ] Distribution test: `bucket.Placement={X:1}`, X draining, mode=weighted, `cluster.weights={Y:50,Z:50}` + 1000 PUTs → ~50/50 split on Y/Z (5% tolerance)
- [ ] Distribution test: same setup but `mode=strict` → all 1000 PUTs return empty → caller observes 503 path
- [ ] `go vet ./...` passes; typecheck passes; tests pass

### US-003: PutChunks routing + rebalance worker classifier use `EffectivePolicy`
**Description:** As a developer, I need PutChunks AND the rebalance worker scan to share the EffectivePolicy semantic so categorization matches actual routing behavior.

**Acceptance Criteria:**
- [ ] `internal/data/rados/Backend.PutChunks` (and S3 symmetric path) replaces the direct `placement.PickClusterExcluding` call with a flow that:
  - Reads `bucket.PlacementMode` from ctx (new `data.WithPlacementMode(ctx, mode)` helper) OR from bucket struct passed alongside Placement
  - Resolves cluster weights + states from in-process cache
  - Calls `EffectivePolicy(bucket.Placement, mode, weights, states)` → uses returned map as the picker input
  - When returned map is empty → falls back to `spec.Cluster` (class env) → may strict-refuse 503
- [ ] Rebalance worker `internal/rebalance/worker.go::scanDistribution` replaces direct `PickClusterExcluding` with `EffectivePolicy` lookup
- [ ] Classifier updated:
  - **migratable** = `EffectivePolicy` non-empty AND `pickedTarget != chunk.Cluster`
  - **stuck_single_policy** = `EffectivePolicy` empty AND `mode == "strict"` AND `bucket.Placement` references only draining clusters
  - **stuck_no_policy** = `EffectivePolicy` empty AND (`mode == "weighted"` OR mode == "") AND ALL live clusters drained (genuinely no target)
  - Anything else not stuck — covered by migratable
- [ ] `/drain-impact` response `stuck_single_policy_chunks` count now decreases significantly in deploys with default mode (weighted) — verified by integration test fixture
- [ ] Suggested policies in `by_bucket` entries for stuck_single_policy: explicit 2 options — (a) flip mode to weighted (with new `placement_mode_override: "weighted"` field on the suggestion), (b) replace with live cluster
- [ ] Suggested policies for stuck_no_policy: existing "set initial uniform-live" still emitted
- [ ] Unit tests: bucket fixtures covering all 7 truth-table rows; classifier outputs match
- [ ] Integration test (multi-cluster lab): seed buckets A/B/C/D from the walkthrough; drain cephb; assert /drain-impact reports stuck_single_policy=1 (bucket-C only), migratable includes A+B+D; PUT new chunks → land per EffectivePolicy verdict (B → default, A → either, D → cluster weights)
- [ ] `go vet ./...` passes; typecheck passes; tests pass

### US-004: BucketDetail Placement tab gains "Strict placement" toggle
**Description:** As an operator, I want a per-bucket toggle on the Placement tab so I can opt this bucket into strict mode for compliance/data-sovereignty reasons.

**Acceptance Criteria:**
- [ ] New `<Toggle>` (or `<Switch>` per existing UI primitive) control in `web/src/components/bucket/PlacementTab.tsx` (or actual file path; verify) labeled "Strict placement"
- [ ] Default state: off (= mode "weighted")
- [ ] Tooltip on toggle: "When ON, this bucket will refuse PUTs and block drain if all clusters in its Placement policy are draining. When OFF (default), it falls back to cluster default routing weights. Turn ON for compliance-sensitive buckets (data-sovereignty, replication design)."
- [ ] Saving the Placement tab (existing flow) now sends `mode: "weighted"|"strict"` in the PUT body
- [ ] Visual indicator: strict-flagged bucket gets small "strict" `<Badge>` in BucketDetail page header (next to bucket name)
- [ ] When operator toggles ON, a confirmation prompt appears: "Strict placement may block drain workflows if this bucket's clusters become unavailable. Continue?"
- [ ] When operator toggles OFF, no confirmation needed (relaxing)
- [ ] Mode toggle does NOT require re-typing bucket name (less destructive than drain)
- [ ] Bundle size delta ≤ 2 KiB gzipped
- [ ] Typecheck passes
- [ ] Verify in browser using dev-browser skill

### US-005: BulkPlacementFixDialog filters to strict-flagged stuck buckets + per-bucket "flip to weighted" option
**Description:** As an operator, I want the BulkPlacementFixDialog to focus on strict-flagged buckets only AND offer a "flip to weighted" shortcut so I can resolve stuck buckets without manually editing policy clusters.

**Acceptance Criteria:**
- [ ] `<BulkPlacementFixDialog>` accepts `stuck` array; renders only entries where `bucket.PlacementMode == "strict"` (weighted stuck buckets auto-resolve via cluster weights — already non-stuck post US-003)
- [ ] "Bulk fix N stuck buckets" CTA label updated to: "Fix N compliance-locked buckets" (explicit about why fix is needed)
- [ ] Each row in dialog offers three actions:
  - "Flip to weighted" — sets `mode=weighted` for this bucket; auto-resolves via cluster weights
  - "Replace cluster" — operator picks a live cluster from the existing suggested-policies dropdown; keeps mode=strict
  - "Keep stuck" — leaves this bucket out of the bulk-fix; drain remains blocked for this bucket until operator acts later
- [ ] Top-of-dialog "Apply uniform to all selected" toggle still works but now allows "Flip all to weighted" as one of the uniform options
- [ ] On Apply: per-bucket PUT placement carrying the operator's choice (mode + policy if changed); drain-impact cache invalidated; modal closes; parent refetches /drain-impact
- [ ] `<ConfirmDrainModal>` "stuck" amber row label updated: "<N> compliance-locked buckets need fix" (instead of generic "<N> stuck buckets")
- [ ] When zero strict-stuck buckets remain → "stuck" row hides; Submit unlocked
- [ ] Bundle size delta ≤ 3 KiB gzipped
- [ ] Typecheck passes
- [ ] Verify in browser using dev-browser skill

### US-006: E2E smoke + Playwright + docs + ROADMAP close-flip + PRD removal
**Description:** As an operator and as a future-maintainer, I need every fix validated end-to-end against the multi-cluster lab + documented so I trust the cycle is actually done.

**Acceptance Criteria:**
- [ ] `scripts/smoke-effective-placement.sh` covers the 12-step walkthrough end-to-end against `docker compose --profile multi-cluster`
- [ ] Smoke scenarios (echo "==> Scenario X" per scenario):
  - **A:** Weighted bucket auto-fallback on drain. Seed bucket-B with `Placement={cephb:1}` mode=weighted. Drain cephb evacuate. /drain-impact reports migratable_chunks>0, stuck_single_policy=0. Worker migrates B's chunks to default. /drain-progress eventually reports deregister_ready=true (provided no real strict buckets).
  - **B:** Strict bucket blocks drain. Seed bucket-C with `Placement={cephb:1}` mode=strict. Drain cephb evacuate. /drain-impact reports stuck_single_policy=1. Drain Submit returns 409 "stuck buckets exist"; manual PUT placement on bucket-C → after invalidation, /drain-impact reports stuck=0; drain succeeds.
  - **C:** Flip strict→weighted resolves stuck. Seed bucket-C with mode=strict; drain cephb; observe stuck=1. PUT /buckets/bucket-C/placement {placement: {cephb:1}, mode: "weighted"}. /drain-impact reports stuck=0 immediately. Drain Submit succeeds.
  - **D:** All clusters drained → 503. Drain default first, then cephb. Both in evacuate mode. PUT to bucket-D (no Placement, mode=weighted) → cluster weights also empty → 503 DrainRefused.
- [ ] `make smoke-effective-placement` Makefile target wraps the script
- [ ] Playwright spec `web/e2e/effective-placement.spec.ts` covers UI half: BucketDetail toggle (off→on with confirm, on→off no confirm); BulkPlacementFixDialog filter (only strict buckets shown); ConfirmDrainModal stuck-row label updated to "compliance-locked"; drain workflow with mixed strict/weighted buckets
- [ ] `docs/site/content/best-practices/placement-rebalance.md` gains new "Strict vs Weighted placement" section:
  - When to use strict (data-sovereignty, replication design, hot/cold separation)
  - When to use weighted (typical operator deploys, auto-fallback friendly)
  - Migration note: existing buckets get mode="weighted" by default on first read; no operator action needed
- [ ] `docs/site/content/best-practices/web-ui.md` capability matrix gains rows for: Strict placement toggle, "Flip to weighted" bulk-fix action, Compliance-locked bucket badge
- [ ] Project root `CLAUDE.md` "Cluster state machine" section gets a note about the new mode field + EffectivePolicy resolution order
- [ ] `ROADMAP.md` close-flip the P2 entry → `~~**P2 — Effective-policy fallback to cluster weights — eliminate forced Placement edit on drain.**~~ — **Done.** <one-line summary>. (commit \`<pending>\`)`; closing SHA backfilled in follow-up commit on main
- [ ] `tasks/prd-effective-placement.md` REMOVED in the same commit (PRD lifecycle rule)
- [ ] Manual screenshots: BucketDetail toggle on/off states; BulkPlacementFixDialog filtered view; /drain-impact JSON before/after showing stuck_single_policy count drop
- [ ] `make docs-build` succeeds; `make vet` succeeds; `make test` succeeds; `pnpm run build` succeeds; `make smoke-effective-placement` succeeds; typecheck passes

## Functional Requirements

- **FR-1:** `bucket.PlacementMode` field stores per-bucket mode ("weighted" default | "strict"); persisted across memory + Cassandra + TiKV
- **FR-2:** PUT /admin/v1/buckets/{name}/placement body accepts optional `mode` field with enum validation
- **FR-3:** `placement.EffectivePolicy(bucket.Placement, mode, cluster.weights, clusterStates)` returns best policy
- **FR-4:** Logic: bucket policy excluding draining → if non-empty return; else if strict return empty; else fall back to cluster weights excluding non-live; else return empty
- **FR-5:** PutChunks routing (RADOS + S3) uses EffectivePolicy
- **FR-6:** Rebalance worker classifier uses EffectivePolicy
- **FR-7:** `stuck_single_policy` only fires when bucket is strict-flagged AND all its policy entries are draining
- **FR-8:** `stuck_no_policy` only fires when all live clusters are drained AND bucket.PlacementMode is weighted (genuine no-target)
- **FR-9:** BucketDetail Placement tab has "Strict placement" toggle with confirmation on enable
- **FR-10:** Strict-flagged buckets show small "strict" badge in BucketDetail header
- **FR-11:** BulkPlacementFixDialog filters to strict-flagged stuck buckets; offers "Flip to weighted" action per bucket
- **FR-12:** ConfirmDrainModal stuck-row labeled "compliance-locked buckets" when only strict-stuck remain
- **FR-13:** drainImpactCache invalidated on placement PUT (mode change is a placement mutation)
- **FR-14:** 12-step smoke + Playwright validation

## Non-Goals

- **No automatic re-routing of EXISTING chunks based on mode flip.** Flipping a bucket to strict on the fly doesn't restrict already-placed chunks; only future PUTs are affected by mode.
- **No backwards-compat alias for the old "no fallback" semantic.** Buckets created before this cycle default to mode="weighted" silently; operators must explicitly opt in to strict.
- **No per-class mode override.** Mode is per-bucket only; class env mappings stay orthogonal.
- **No bulk mode flip via admin API.** Each bucket's mode set individually (or via BulkPlacementFixDialog "Flip to weighted" action which iterates per-bucket).
- **No mode-based PUT routing differentiation aside from drain semantics.** During normal live cluster operation, mode has no effect — bucket Placement just routes normally.
- **No new admin endpoint.** PUT placement body field extension reuses the existing endpoint.

## Design Considerations

- **Reuse existing `<Toggle>` / `<Switch>` primitive** from web/src/components/ui/ — verify what's used in BucketDetail Quota/Lifecycle tabs.
- **Strict-mode confirmation prompt** mirrors the existing typed-confirm pattern but is lighter (single Yes/No) since flipping to strict is reversible.
- **EffectivePolicy is a pure function** — no side effects, no I/O. Easy to unit test exhaustively.
- **Mode field default**: empty string "" treated as "weighted" — backwards-compat for buckets created before this cycle.
- **Suggested policies in /drain-impact for stuck_single_policy buckets** now carry a `placement_mode_override` hint — "flip to weighted" is the operator's lightest fix.
- **BucketDetail Placement tab layout**: toggle at top (compact), slider matrix below (existing) — toggle is a global override for the per-cluster sliders.

## Technical Considerations

- **PUT placement is the cache-invalidation trigger** — already shipped via drain-followup US-002. Mode change goes through same handler, same invalidation. No new cache mechanism needed.
- **EffectivePolicy must be called consistently** in PutChunks AND rebalance worker — different call sites diverging would create incorrect categorization. Add helper to a shared package; both consume.
- **Backwards-compat for legacy buckets**: schema-additive ALTER on Cassandra; TiKV JSON blob extension; memory struct extension. Existing rows read placement_mode as empty string → coerced to "weighted" downstream.
- **Performance**: EffectivePolicy is O(clusters) — negligible cost on a few-cluster deploy. No new caches.
- **Drain-impact response shape**: existing fields unchanged; suggested_policies array gains optional `placement_mode_override: "weighted"|"strict"` field per suggestion entry. UI renders both the policy AND the mode hint.
- **Compliance trade-off documentation**: operator runbook explicitly covers the case "data-sovereignty bucket got accidentally flipped to weighted → chunks now live in wrong region after drain" — operators must audit strict-flagged buckets carefully.

## Success Metrics

- After cycle merge, drain workflow on typical multi-cluster lab requires 0 BulkPlacementFixDialog clicks (all non-strict buckets auto-resolve)
- Strict-flagged buckets correctly block drain until operator acts
- /drain-impact `stuck_single_policy_chunks` reported count drops to 0 for deploys with no strict buckets (verified by smoke scenario A)
- Operator can flip mode in 2 clicks (toggle + confirm)
- 12-step smoke + Playwright e2e green in ≤ 3 minutes against the lab

## Open Questions

- Should strict-flagged buckets show in a dedicated "Compliance" filter on the Buckets list page? Recommendation: defer — operator sees the strict badge per-bucket, and filtering by mode is a P3 nicety.
- Should mode flip notify the audit log with a specific action like `bucket.placement.mode.changed`? Recommendation: yes — `admin:UpdateBucketPlacementMode` action stamped in US-001 acceptance.
- Should there be a global default mode env (e.g. `STRATA_BUCKET_DEFAULT_PLACEMENT_MODE`)? Recommendation: no — per-bucket only, follows the existing no-env-knob discipline from drain-transparency.
