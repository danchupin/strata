# PRD: Drain UI + correctness cleanup

## Introduction

After `ralph/cluster-weights` merged the operator walkthrough on the multi-cluster lab surfaced four open ROADMAP entries — two regressions in drain UX, one cosmetic UI label gap, and one chunk-leak bug in admin force-empty. This cycle bundles all four into one cleanup so the next walkthrough completes end-to-end without operator confusion.

The four entries closed by this cycle:

1. **P2 — `<BucketReferencesDrawer>` regression.** drain-transparency cycle US-004 migrated `<ConfirmDrainModal>` to fetch `/drain-impact` (categorized chunks-on-cluster breakdown), but forgot `<BucketReferencesDrawer>` opened from "Show affected buckets" link on cluster cards. Drawer still uses old `/bucket-references` endpoint which filters only Placement-referencing buckets. Operator clicks drawer → sees misleading "No buckets reference this cluster" message even when cluster has chunks from nil-policy buckets.

2. **P2 — `/drain-impact` cache stale on bucket placement PUT.** Cache TTL = 5 minutes (`internal/adminapi/clusters_drain_impact.go:21`). Operator runs `<BulkPlacementFixDialog>` Apply → bucket policies updated → `<ConfirmDrainModal>` refetches `/drain-impact` → cache returns STALE categorization → stuck count unchanged → drain blocked for 5 minutes. Documented workflow "Fix N buckets → retry Drain" doesn't work within reasonable time.

3. **P3 — Storage Pools "Objects" column ambiguous.** `data.PoolStatus.ObjectCount` carries RADOS chunk count (one per 4 MiB chunk), not S3 object count. A 5 MiB S3 file → 2 chunks → Pools shows "Objects=2". Operator who uploaded 1 file is confused. BucketDetail correctly shows S3 `object_count=1`.

4. **P2 — Admin force-empty chunk leak.** `POST /admin/v1/buckets/{name}/force-empty` calls `Meta.DeleteObject` but does NOT enqueue chunks into GC queue. Meta rows + `bucket_stats` clean, but RADOS chunks remain in pool forever. Compare S3 DeleteObject path (`internal/s3api/server.go::deleteObject`) which calls `enqueueChunks(o.Manifest.Chunks)` after `Meta.DeleteObject`.

## Goals

- `<BucketReferencesDrawer>` shows ALL chunks-on-cluster categorized (migratable / stuck_single_policy / stuck_no_policy) via `/drain-impact` data — not just Placement-referencing buckets
- Inline "Bulk fix N stuck buckets" CTA opens existing `<BulkPlacementFixDialog>` with stuck buckets pre-selected
- Bucket placement PUT / DELETE invalidates `/drain-impact` cache synchronously — bulk-fix workflow completes within one HTTP round-trip
- Pools table column renamed "Objects" → "Chunks"; JSON field hard-renamed `object_count` → `chunk_count`; column header tooltip explains RADOS chunk semantic
- Admin force-empty drain-page enqueues chunks into GC queue → `rados ls` count drops to 0 after gc worker tick
- 8-step walkthrough validated end-to-end via smoke script + Playwright spec
- 4 ROADMAP entries close-flipped in one commit

## State Truth Tables (pre-cycle discipline per `feedback_cycle_end_to_end.md` item #8)

### ClustersSubsection card action buttons

| state | mode | chunks | deregister_ready | gc_pending | Buttons rendered |
|-------|------|--------|------------------|------------|------------------|
| pending | "" | — | — | — | "Activate" |
| live | "" | — | — | — | "Drain" |
| draining_readonly | readonly | — | — | — | "Upgrade to evacuate" + "Undrain" |
| evacuating | evacuate | >0 | false | — | "Undrain (cancel evacuation)" with confirm |
| evacuating | evacuate | 0 | false | true | "Undrain" DISABLED + tooltip "GC queue still processing" |
| evacuating | evacuate | 0 | true | false | "Cancel deregister prep" (typed-confirm) — NO Undrain |
| removed | "" | — | — | — | (card hidden — operator already deregistered) |

### Drain progress banner / chip

| state | chunks | deregister_ready | Banner / Chip rendered |
|-------|--------|------------------|------------------------|
| live | — | — | — (no banner — no drain in progress) |
| draining_readonly | — | — | "Stop-writes drain — reads/multipart continue" chip |
| evacuating | >0 | false | "Migrating N chunks · ETA Xm" amber banner |
| evacuating | 0 | false | "Waiting for GC queue (Y entries) / open multipart" amber banner with not_ready_reasons |
| evacuating | 0 | true | "✓ Ready to deregister (env edit + restart)" green chip — NO migration banner |

## Cache Invalidation Ledger (per `feedback_cycle_end_to_end.md` item #10)

| Cache | TTL | Invalidation triggers |
|-------|-----|----------------------|
| `placement.DrainCache` (existing — drain-transparency cycle) | 30s | POST /clusters/{id}/drain, POST /clusters/{id}/undrain |
| `drainImpactCache` (existing — drain-transparency cycle) | 5min | **NEW in this cycle (US-002):** PUT /buckets/{name}/placement, DELETE /buckets/{name}/placement, DELETE /buckets/{name} (entire bucket) |

No new caches introduced this cycle. The fix is to add invalidation triggers to the existing `drainImpactCache`.

## Safety Claims Preconditions (per `feedback_cycle_end_to_end.md` item #9)

| UI claim | Preconditions (ALL must hold) | Worst-case if wrong |
|----------|-------------------------------|---------------------|
| "✓ Ready to deregister (env edit + restart)" | (1) `manifest_count_referencing_cluster == 0` AND (2) `gc_queue_pending_for_cluster == 0` AND (3) `no_open_multipart_with_cluster_in_handle` | Operator removes cluster from env → GC worker can't dial → orphan chunks in pool forever; OR open multipart finalizes on phantom cluster after dereg → 5xx errors |
| "No chunks on this cluster — safe to drain" (BucketReferencesDrawer empty state) | `total_chunks_on_cluster == 0` (sum of all three categories via /drain-impact synchronous scan) | Operator drains cluster expecting clean state, then PUTs land on it via class-env fallback → strict-refuse 503 storm; OR rebalance worker plans no moves and operator never sees what's stuck |
| "Drain cluster X (5 chunks will migrate)" (ConfirmDrainModal Submit button) | `stuck_single_policy + stuck_no_policy == 0` AND impact analysis is fresh (cache invalidated since last bucket placement change) | Operator submits drain → some chunks get stuck forever because policies still reference only the draining cluster |

## User Journey Walkthrough

Pre-cycle walkthrough exercise per `feedback_cycle_end_to_end.md`. Operator end-to-end scenario: drain `cephb` cluster on the multi-cluster lab with mixed bucket policies. Every step has a concrete UI/API surface; gaps go in scope.

| # | Operator action | Surface | Backing story |
|---|-----------------|---------|---------------|
| 1 | Open `/console/storage` Clusters subsection | `<ClustersSubsection>` | (existing — placement-ui) |
| 2 | Click "Show affected buckets" on cephb card | `<BucketReferencesDrawer>` opens | **US-001** |
| 3 | Drawer shows 3 sections: "Migrating", "Stuck (single-policy)", "Stuck (no policy)" with chunk_count + bytes_used per bucket | categorized render | **US-001** |
| 4 | Click inline "Bulk fix 2 stuck buckets" CTA | `<BulkPlacementFixDialog>` opens with stuck buckets pre-selected | **US-001** |
| 5 | Pick suggested policy → Apply | PUT each `/buckets/X/placement` → cache invalidated synchronously | **US-002** |
| 6 | Modal closes; drawer counts refresh IMMEDIATELY (no 5min wait) | refetch reflects new state | **US-002** + **US-001** |
| 7 | Drawer now shows total_chunks=remaining (only migratable category) | empty stuck sections collapse | **US-001** |
| 8 | Close drawer; click Drain on cephb card → ConfirmDrainModal evacuate mode | impact shows stuck=0 | (existing) |
| 9 | Typed-confirm + Submit | drain proceeds | (existing) |
| 10 | Wait for migration completion → green "Ready to deregister" chip | (existing) | (existing) |
| 11 | Switch to a test bucket with chunks; click "Force delete" in UI | force-empty job runs | **US-004** |
| 12 | After job completes, `docker exec rados ls` reports 0 chunks (GC worker drained them) | chunks enqueued to GC | **US-004** |
| 13 | Storage page Pools table shows "Chunks" column header (not "Objects") with hover tooltip | UI label clarity | **US-003** |

Negative paths covered:
- Drawer empty (cluster has no chunks at all) → shows "No chunks on this cluster — safe to drain or remove" (only this message, not the legacy "Default routing buckets are not listed" one)
- Cache invalidation race (operator opens drawer while bulk-fix is mid-flight) → drawer refetches on next mount, sees clean state
- Force-empty on a bucket with zero chunks → no GC enqueue calls (skip enqueue when Manifest.Chunks is empty), no errors

## User Stories

### US-001: `<BucketReferencesDrawer>` migration to `/drain-impact` + categorized rendering + inline Bulk-fix
**Description:** As an operator, I want the "Show affected buckets" drawer to surface ALL chunks on a cluster (including nil-policy buckets via class-env or default routing) so I see the real drain impact before clicking Drain.

**Acceptance Criteria:**
- [ ] `web/src/components/storage/BucketReferencesDrawer.tsx` rewritten to fetch `/admin/v1/clusters/{id}/drain-impact` instead of `/bucket-references` (replace `fetchClusterBucketReferences` → `fetchClusterDrainImpact`)
- [ ] Drawer renders three sections in order: "Migrating" (green palette), "Stuck — single-policy" (amber palette), "Stuck — no policy" (amber palette); each section shows category count + collapsible per-bucket list
- [ ] Per-bucket row: name (link to BucketDetail Placement tab) + chunk_count + bytes_used + small badges showing top 2 `suggested_policies` labels
- [ ] Inline CTA button "Bulk fix N stuck buckets" rendered above stuck sections; enabled only when `stuck_single_policy_chunks + stuck_no_policy_chunks > 0`; N = total stuck buckets count
- [ ] Click CTA opens existing `<BulkPlacementFixDialog>` with the stuck buckets passed via prop (pre-selected); after Apply, dialog closes back to drawer which refetches /drain-impact (cache will be fresh thanks to US-002)
- [ ] Empty-state message rewritten: shown only when `total_chunks == 0`; text reads "No chunks on this cluster — safe to drain or remove."
- [ ] Loading state preserves existing `<Loader2>` spinner
- [ ] Error state preserves existing `<AlertCircle>` red banner
- [ ] Pagination preserved if `by_bucket` list exceeds page size; "Page N of M" controls
- [ ] Drop or alias the legacy `fetchClusterBucketReferences` admin call from `web/src/api/clusters.ts` — pick one: (a) drop entirely and remove the `/bucket-references` endpoint server-side, (b) keep as thin alias forwarding to /drain-impact. PRD prefers (a) for clarity unless removing the endpoint surfaces external consumer
- [ ] Bundle size delta ≤ 4 KiB gzipped
- [ ] Typecheck passes
- [ ] Verify in browser using dev-browser skill — drawer shows 3 categories on cluster with mixed-policy buckets; empty state when cluster is clean

### US-002: `/drain-impact` cache invalidation on bucket placement mutation
**Description:** As an operator, I want the BulkPlacementFixDialog workflow to complete end-to-end within one HTTP round-trip — without waiting 5 minutes for the drain-impact cache to expire.

**Acceptance Criteria:**
- [ ] `PUT /admin/v1/buckets/{name}/placement` handler in `internal/adminapi/buckets_placement.go` calls `s.drainImpact().InvalidateAll()` synchronously BEFORE returning 200 OK
- [ ] `DELETE /admin/v1/buckets/{name}/placement` handler same
- [ ] `drainImpactCache.InvalidateAll()` method added: clears all entries under cache lock; cheap (recomputed on next /drain-impact request, operator-driven)
- [ ] Unit test: stub cache; PUT placement → assert InvalidateAll called once before response written
- [ ] Integration test (multi-cluster lab via testcontainers OR compose): set bucket Placement → call /drain-impact → assert categorized counts reflect the new policy IMMEDIATELY (within one HTTP round-trip, no 5-minute wait); changes Placement again → call /drain-impact → assert reflects second change
- [ ] Optimization is intentionally simple — invalidate-all instead of per-cluster set; comment in code explains the choice (placement keys may add/remove clusters; tracking diff adds complexity for minor speedup)
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: Pools column "Objects" → "Chunks" rename + JSON field rename
**Description:** As an operator, I want the Pools table column header to say "Chunks" instead of "Objects" so I don't confuse RADOS chunk count with S3 object count.

**Acceptance Criteria:**
- [ ] `data.PoolStatus` struct field `ObjectCount` → `ChunkCount` rename (Go field name)
- [ ] JSON tag updated `json:"object_count"` → `json:"chunk_count"` (hard rename — no prod deploys per user explicit; no backwards-compat alias)
- [ ] `internal/data/rados/health.go` + `internal/data/s3/health.go` + `internal/data/memory/backend.go` DataHealth implementations populate the renamed field
- [ ] `internal/data/storage_test.go` (if any tests assert the field) + memory/RADOS/S3 health test fixtures updated
- [ ] OpenAPI spec at `internal/adminapi/openapi.yaml` updated: PoolStatus schema field renamed
- [ ] `web/src/api/storage.ts` (or equivalent TypeScript wire type) field renamed `object_count` → `chunk_count`
- [ ] `web/src/pages/Storage.tsx` Pools table column header renamed "Objects" → "Chunks"
- [ ] Add `<Tooltip>` on the column header: "RADOS chunk count — large S3 objects span multiple 4 MiB chunks. For S3 object count see BucketDetail."
- [ ] All callers of the renamed JSON field updated (smoke scripts, e2e tests, etc — grep `object_count` in repo and update mentions in PoolStatus context; leave bucket-stats `object_count` field alone since that's S3 object count)
- [ ] Bundle size delta ≤ 1 KiB gzipped
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill — column header reads "Chunks" with hover tooltip

### US-004: Admin force-empty enqueues chunks into GC queue
**Description:** As an operator, I want the "Force delete" button to actually delete chunks from RADOS / S3, not just delete meta rows — so I don't leak storage when removing a bucket.

**Acceptance Criteria:**
- [ ] `internal/adminapi/buckets_delete.go::forceEmptyDrainPage` modified to enqueue returned `*meta.Object.Manifest.Chunks` into the GC queue after each successful `Meta.DeleteObject` call
- [ ] Enqueue path: call `s.Meta.EnqueueChunkDeletion(ctx, region, chunks)` directly (adminapi.Server has access to s.Meta); region resolved from server config (same way s3api does it). Alternative: extract a shared helper `chunkqueue.Enqueue` callable from both s3api + adminapi packages — choose the simpler path that doesn't introduce a new package
- [ ] Skip enqueue when `Manifest.Chunks` is empty or nil (zero-chunk objects, e.g. multipart abort residue)
- [ ] Both the ListObjectVersions path AND the ListObjects fallback path in `forceEmptyDrainPage` enqueue chunks (both call `Meta.DeleteObject` — both need the enqueue)
- [ ] Unit test: stub Meta with a returned `*meta.Object.Manifest.Chunks = []ChunkRef{...}`; assert `EnqueueChunkDeletion` called with those chunks after each `DeleteObject`
- [ ] Unit test: empty-chunks case — `Manifest.Chunks = nil` → `EnqueueChunkDeletion` NOT called (no needless rows)
- [ ] Integration test (multi-cluster lab): create bucket → PUT 10 chunks → `POST /admin/v1/buckets/{name}/force-empty` → wait for job completion → trigger gc worker tick → assert `docker exec strata-ceph rados -p strata.rgw.buckets.data ls | wc -l` reports 0 chunks for those OIDs
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-006: `deregister_ready` true-safety — GC queue check + banner hide on completion
**Description:** As an operator, I want "✓ Ready to deregister" chip to be a HARD safety guarantee — not just "manifest scan reports 0" — so I don't deregister a cluster while GC queue still has pending chunk deletions for it, leaving orphaned RADOS data after env edit + restart.

**Acceptance Criteria:**
- [ ] Server-side: `buildDrainProgressResponse` in `internal/adminapi/clusters_drain_progress.go::197` updated. `deregReady` now requires THREE conditions ALL true:
  1. `total_chunks == 0` (manifest scan reports zero) — existing condition
  2. NEW: `gc_queue_pending_for_cluster == 0` (no pending chunk deletions in `EnqueueChunkDeletion` queue with `cluster == <id>`); query `meta.Store.ListChunkDeletionsByCluster(ctx, region, clusterID, limit=1) → returns first row or none`
  3. NEW: `no_open_multipart_on_cluster == true` (no `multipart_uploads` rows where Cluster handle component == drained cluster id); query `meta.Store.ListMultipartUploadsByCluster(ctx, clusterID, limit=1) → returns first row or none`
- [ ] If any condition false → `deregReady = false`; response includes new `not_ready_reasons: ["chunks_remaining"|"gc_queue_pending"|"open_multipart"]` array surfacing all unmet conditions (operator sees WHY it's not ready)
- [ ] If all three true → `deregReady = true`; UI shows green chip
- [ ] New `meta.Store` methods `ListChunkDeletionsByCluster` + `ListMultipartUploadsByCluster` implemented on memory + Cassandra + TiKV — both signature `(ctx, region OR clusterID only, limit) (count int, err error)` for fast existence-check (single row probe)
- [ ] OpenAPI spec updated: `ClusterDrainProgressResponse` carries optional `not_ready_reasons []string`
- [ ] UI `<DrainProgressBar>` reads `not_ready_reasons` — when present + state=evacuating, renders amber chip "Not ready — <reasons joined comma>" instead of either green deregister chip OR raw migrating banner
- [ ] UI `<ClustersSubsection>` "Rebalance worker is migrating chunks off this cluster" banner (line 286-291) — only rendered when `total_chunks > 0`. When zero (whether deregister-ready or stuck in GC/multipart) — banner hidden; `<DrainProgressBar>` is the single source of truth for state
- [ ] Unit test (server): each `not_ready_reasons` combination — chunks-only / GC-only / multipart-only / multiple — produces correct array
- [ ] Integration test (multi-cluster lab): scenario where worker just finished move → manifest scan shows 0 but GC queue still has entries → assert deregister_ready=false + reason="gc_queue_pending"; wait for gc worker tick + grace → assert deregister_ready=true (and chip flips green)
- [ ] Documentation: operator runbook explains the three conditions + how to interpret each `not_ready_reasons` value
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-007: State-aware action buttons in evacuate-state cluster card
**Description:** As an operator, I want action buttons on the cluster card to reflect the actual state — so I can't accidentally undrain a fully-evacuated cluster (reverting hours of migration) or click Drain on a cluster that's already mid-evacuation.

**Acceptance Criteria:**
- [ ] `web/src/components/storage/ClustersSubsection.tsx` cluster card renders action buttons conditional on (state, deregister_ready):
  - `state=live` → "Drain" button only
  - `state=draining_readonly` → "Upgrade to evacuate" + "Undrain"
  - `state=evacuating` AND `chunks > 0` → "Undrain (cancel evacuation)" with confirm modal explaining "Moved chunks remain on target clusters; no rollback"
  - `state=evacuating` AND `chunks == 0` AND `deregister_ready=true` → NO Undrain button; replaced with "Cancel deregister prep" button + dedicated confirm modal "This will re-enable the cluster for writes. All migrated chunks stay on their new clusters."
  - `state=evacuating` AND `chunks == 0` AND `deregister_ready=false` (e.g. GC queue pending) → "Undrain" disabled with tooltip "Cannot undrain while GC queue is processing"
  - `state=pending` → "Activate" button only (existing)
- [ ] Confirm modal for "Cancel deregister prep" mirrors `<ConfirmDrainModal>` typed-confirm pattern: case-sensitive cluster id input arms submit (prevents accidental clicks)
- [ ] Cancel deregister prep submit → POST `/admin/v1/clusters/{id}/undrain` → state=live; toast "Cluster restored to live. No chunks restored."
- [ ] Unit test (UI): render each (state, chunks, deregister_ready) combination → assert correct button labels + disabled states
- [ ] Bundle size delta ≤ 2 KiB gzipped
- [ ] Typecheck passes
- [ ] Verify in browser using dev-browser skill — walk through all 5 state combinations on the multi-cluster lab

### US-008: Trace browser list view — RingBuffer.List + recent-traces panel
**Description:** As an operator, I want to see a list of recently captured traces in the Trace Browser without having to know a request_id upfront — so I can discover and inspect traffic that just hit the gateway.

**Acceptance Criteria:**
- [ ] `internal/otel/ringbuf/RingBuffer` gains `List(limit, offset int) []TraceSummary` method returning the LRU front-N as `{RequestID, TraceID, RootName, StartedAt, DurationMs, Status}` summaries
- [ ] Method is thread-safe (existing ringbuf already has a mutex for reads/writes); avoids materialising more than `limit` entries per call
- [ ] New admin endpoint `GET /admin/v1/diagnostics/traces?limit=50&offset=0` returns `{traces: [...summaries], total: int}`; cap `limit` at 200; audit row stamped via `s3api.SetAuditOverride(ctx, "admin:GetDiagnosticsTraces", "diagnostics", "-", owner)`
- [ ] OpenAPI spec at `internal/adminapi/openapi.yaml` documents the new endpoint
- [ ] UI: `<TraceBrowser>` page (`web/src/pages/TraceBrowser.tsx`) renders a new `<RecentTracesPanel>` ABOVE the existing search box; lists recent traces sortable by `started_at` desc (default), `duration_ms` desc; click a row → loads the full trace via existing /trace/{request_id} fetch
- [ ] Polling: 10s refetch via TanStack Query (key `recentTraces`); pause when tab not focused (existing pattern)
- [ ] Empty state: "No traces captured yet. Send a request to populate the ringbuf."
- [ ] Status column: error traces (status=Error) highlighted with red dot; OK = green dot
- [ ] localStorage-backed "history" panel (existing from placement-ui Phase 3) renders BELOW the new live list — keeps the "I opened this trace before" affordance
- [ ] Bundle size delta ≤ 3 KiB gzipped
- [ ] Unit test (server): RingBuffer.List with various (limit, offset) combinations; verifies LRU front-N ordering + bounds
- [ ] Integration test: generate 10 requests against the gateway; GET /admin/v1/diagnostics/traces; assert 10 entries returned with correct fields
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill — navigate to /diagnostics/traces → see live list → click a trace → waterfall renders

### US-005: E2E smoke + walkthrough + docs + ROADMAP close-flip + PRD removal
**Description:** As an operator and as a future-maintainer, I need every fix validated end-to-end against the multi-cluster lab + documented so I trust the cycle is actually done.

**Acceptance Criteria:**
- [ ] `scripts/smoke-drain-cleanup.sh` covers the 13-step walkthrough end-to-end against `docker compose --profile multi-cluster`
- [ ] Smoke assertions: drawer renders 3 categories with expected counts; bulk-fix Apply → /drain-impact immediately reflects new categorization (no 5-min sleep in script); Pools API returns `chunk_count` field (not `object_count`); force-empty completes + rados ls count drops to 0 within gc worker grace
- [ ] Script EXITS NON-ZERO on any assertion failure; per-step `echo "==> Step N: ..."` lines so failing step is obvious
- [ ] `make smoke-drain-cleanup` Makefile target wraps the script
- [ ] Playwright spec `web/e2e/drain-cleanup.spec.ts` covers UI half: navigate /storage → click "Show affected buckets" → drawer with 3 categories → click "Bulk fix" → BulkPlacementFixDialog → pick policy → Apply → drawer counts update immediately → close drawer → Drain → ConfirmDrainModal shows stuck=0 → typed-confirm → Submit
- [ ] `docs/site/content/best-practices/placement-rebalance.md` "Drain lifecycle" section updated: drawer screenshot or text refresh reflecting 3-category breakdown; pre-drain checklist mentions inline Bulk fix flow
- [ ] `docs/site/content/best-practices/web-ui.md` capability matrix updated: BucketReferencesDrawer entry mentions categorized 3-section render; Pools "Chunks" column row added
- [ ] `ROADMAP.md` SEVEN entries close-flipped in the same commit:
  - `P2 — <BucketReferencesDrawer> shows only Placement-referencing buckets` (US-001) → Done
  - `P2 — /drain-impact cache not invalidated on bucket placement PUT` (US-002) → Done
  - `P3 — Storage page Pools column "Objects" ambiguous` (US-003) → Done
  - `P2 — POST /admin/v1/buckets/{name}/force-empty leaks chunks` (US-004) → Done
  - `P2 — deregister_ready ignores GC queue + open multipart` (US-006) → Done
  - `P2 — UI state-aware action buttons missing` (US-007) → Done
  - `P2 — Trace browser has no list view` (US-008) → Done
  - all seven flipped to `~~**Px — ...**~~ — **Done.** <one-line summary>. (commit \`<pending>\`)`; closing SHA backfilled in follow-up commit on main
- [ ] `tasks/prd-drain-cleanup.md` REMOVED in the same commit (PRD lifecycle rule)
- [ ] Manual screenshot: updated drawer with 3 categories + Pools "Chunks" column + force-empty zero-chunk verification — embedded or linked in operator runbook
- [ ] `make docs-build` succeeds
- [ ] `make vet` succeeds
- [ ] `make test` succeeds
- [ ] `pnpm run build` succeeds
- [ ] `make smoke-drain-cleanup` succeeds against the running multi-cluster lab
- [ ] Typecheck passes

## Functional Requirements

- **FR-1:** `<BucketReferencesDrawer>` fetches `/drain-impact` and renders 3-category breakdown
- **FR-2:** Drawer inline "Bulk fix N stuck buckets" CTA opens `<BulkPlacementFixDialog>` with stuck buckets pre-selected
- **FR-3:** Drawer empty-state message shown only when `total_chunks == 0`
- **FR-4:** `PUT /admin/v1/buckets/{name}/placement` invalidates `/drain-impact` cache synchronously
- **FR-5:** `DELETE /admin/v1/buckets/{name}/placement` invalidates `/drain-impact` cache synchronously
- **FR-6:** `drainImpactCache.InvalidateAll()` clears all entries under cache lock
- **FR-7:** `data.PoolStatus` field `ObjectCount` → `ChunkCount` (Go) + JSON tag `object_count` → `chunk_count`
- **FR-8:** All three data backends (RADOS, S3, memory) populate `ChunkCount`
- **FR-9:** OpenAPI spec + TypeScript wire type + Storage page UI all aligned on `chunk_count` / "Chunks"
- **FR-10:** Pools column header tooltip explains RADOS chunk semantic
- **FR-11:** `forceEmptyDrainPage` enqueues `Manifest.Chunks` into GC queue after each successful `Meta.DeleteObject`
- **FR-12:** Enqueue skipped when `Manifest.Chunks` empty / nil
- **FR-13:** 13-step smoke + Playwright e2e verify end-to-end
- **FR-14:** ROADMAP 4-entry close-flip in single commit

## Non-Goals

- **No new admin endpoints.** This cycle uses what exists (`/drain-impact`, bucket placement PUT/DELETE, force-empty).
- **No per-cluster cache invalidation.** Invalidate-all is simpler and the cost is operator-driven, not request-driven.
- **No legacy `/bucket-references` endpoint backwards-compat.** Drop the endpoint or alias it — caller's choice. PRD prefers dropping for clarity.
- **No JSON field deprecation period.** Hard rename `object_count` → `chunk_count`; no transition window per user "no prod deploys".
- **No flake fixes.** Pre-existing `TestRunCleanRunNoInconsistencies` + `TestThreeReplicaDistribution` flakes are tracked separately and out of scope.
- **No new GC worker logic.** Existing gc worker processes the enqueued entries; no change to its drain loop or interval.
- **No bulk-delete-buckets workflow.** "Force delete N buckets at once" is out of scope; per-bucket only.
- **No drain-impact endpoint refactor.** Cache invalidation is the only change to the endpoint side; categorization logic stays.

## Design Considerations

- **Drawer 3-category render**: reuse existing `<Card>` / `<Badge>` primitives; section headers with category-colored chip + count. Migratable section is green; stuck sections are amber. Mirror palette from `<DrainProgressBar>` (drain-transparency).
- **Inline Bulk-fix CTA**: rendered above stuck sections (visually distinct from per-bucket list). Reuse existing `<Button variant="default">` primitive. Disabled state when stuck=0.
- **Cache invalidation method name**: `InvalidateAll()` matches the simplest API; alternatives like `Reset()` or `Clear()` are equally valid — pick one and document.
- **Force-empty enqueue ordering**: per loop iteration — `Meta.DeleteObject` first, then `EnqueueChunkDeletion` with returned chunks. If enqueue fails, log warning but DON'T fail the loop — chunks would re-orphan but the bucket-empty job completes (operator can re-run; gc has eventual-consistency anyway).
- **Pools column tooltip placement**: top of column header on hover; reuse existing `<Tooltip>` primitive.
- **Smoke script numbering**: 13 steps from walkthrough table — keep `echo "==> Step N"` per step so failures are obvious without re-reading the script.

## Technical Considerations

- **Cache invalidation atomic with respect to placement PUT**: the handler MUST call `InvalidateAll()` before returning 200, NOT in a `defer`. A reader following up immediately after the response sees the cleared cache. Use synchronous call inside the handler body before the writeJSON. Verified by US-002 integration test.
- **`fetchClusterBucketReferences` removal**: if dropping the endpoint, also remove the OpenAPI path + audit registration + tests that reference it. If aliasing, the alias must forward request path + return /drain-impact response shape (which differs from /bucket-references shape — alias requires response transformation, more work than a clean removal).
- **Pools field rename — wire-shape break**: every JSON consumer of /admin/v1/storage/data response must update simultaneously. Grep for `object_count` in PoolStatus context across Go + TypeScript + smoke scripts + docs. Bucket-stats unrelated `object_count` (S3 object count) is in a different shape — leave that field alone.
- **`forceEmptyDrainPage` two paths**: the function has a ListObjects fallback when ListObjectVersions returns empty. Both paths call `Meta.DeleteObject` — both need the enqueue. Test both paths.
- **Memory backend force-empty integration test**: the multi-cluster lab uses Cassandra meta + RADOS data. Memory backend has no rados, so the chunk-leak assertion (`rados ls`) only applies to RADOS / S3 backends. Memory backend test asserts EnqueueChunkDeletion was CALLED with the expected chunks; gc-side cleanup is the gc worker's concern.

## Success Metrics

- BucketReferencesDrawer correctly shows nil-policy buckets in stuck_no_policy category — operator sees the same picture as ConfirmDrainModal
- BulkPlacementFixDialog Apply → /drain-impact reflects new state within < 1 second (no 5-min TTL wait) — verified by US-002 integration test
- Pools table label "Chunks" + tooltip — no more operator confusion conflating RADOS chunks with S3 objects
- Force-empty on 10-chunk bucket → 0 chunks remain after gc grace — no RADOS leak
- 13-step smoke + Playwright e2e green in < 2 minutes against the running multi-cluster lab
- All 4 ROADMAP entries closed in one commit; closing SHA backfilled per CLAUDE.md convention

## Open Questions

- Should "Show affected buckets" link label rename to "Show drain impact" to match the new categorized semantic? Recommendation: defer to a future UX polish pass — current label still makes sense (operator wants to see WHICH buckets are affected by draining this cluster).
- Should we add a "Recently fixed" callout in the drawer when bulk-fix completed in the same session? Defer — niche UX.
- Should the cache invalidation also fire on bucket DELETE (entire bucket)? Yes — bucket deletion changes the chunks-on-cluster picture. Add to acceptance criteria of US-002 as a small bonus. (Already in acceptance — DELETE handler invalidates.)
