# PRD: Drain follow-up cleanup

## Introduction

After `ralph/drain-cleanup` merged, operator walkthrough on the multi-cluster lab surfaced FOUR additional issues — three UX gaps (trace filter, UI confusion chip+button, multipart probe no-op on Cassandra) and one performance antipattern (ALLOW FILTERING on non-PK cluster column). The trace-filter mini-cycle prep (commit `3b95209` on stale `ralph/trace-filter` branch) is folded into this larger cycle.

Closes 4 ROADMAP entries: P3 Trace browser filter / search, P2 UI confusion chip+button, P2 Cassandra multipart probe no-op, P3 ALLOW FILTERING denormalize.

## Goals

- Trace browser filter / search via server-side query params + UI filter row with URL persistence
- UI clarity: "Cancel deregister prep" button renamed to "Restore to live (cancel evacuation)"; chip gets tooltip explaining the actual env-edit primary action; visual hierarchy puts chip above button
- Cassandra `ListMultipartUploadsByCluster` returns correct count (currently no-op); deregister_ready safety AND fully enforced
- Denormalize GC + multipart probes into `_by_cluster` lookup tables — remove ALLOW FILTERING antipattern from the drain hot path
- 4 ROADMAP entries close-flipped in one commit
- 12-step walkthrough validated end-to-end via smoke + Playwright

## State Truth Tables

### ClustersSubsection card action button (chip+button visual hierarchy — US-003)

| state | mode | chunks | deregister_ready | gc_pending | multipart_pending | Chip rendered | Button rendered |
|-------|------|--------|------------------|------------|-------------------|---------------|-----------------|
| evacuating | evacuate | >0 | false | — | — | "Migrating N chunks · ETA Xm" amber | "Undrain (cancel evacuation)" |
| evacuating | evacuate | 0 | false | true | — | "Not ready — gc_queue_pending" amber + tooltip | "Undrain" DISABLED + tooltip "GC queue still processing" |
| evacuating | evacuate | 0 | false | false | true | "Not ready — open_multipart" amber + tooltip | "Undrain" DISABLED + tooltip "Multipart still in flight" |
| evacuating | evacuate | 0 | true | false | false | "✓ Ready to deregister" green + tooltip "Edit STRATA_RADOS_CLUSTERS env..." | "Restore to live (cancel evacuation)" outline secondary |

## Cache Invalidation Ledger

| Cache | TTL | Invalidation triggers |
|-------|-----|----------------------|
| `placement.DrainCache` (existing) | 30s | POST /clusters/{id}/drain, POST /clusters/{id}/undrain, POST /clusters/{id}/activate |
| `drainImpactCache` (existing) | 5min | PUT /buckets/{name}/placement, DELETE /buckets/{name}/placement, DELETE /buckets/{name} |

No new caches introduced this cycle.

## Safety Claims Preconditions

| UI claim | Preconditions (ALL must hold) | Verified by story |
|----------|-------------------------------|-------------------|
| "✓ Ready to deregister" chip | (1) `total_chunks == 0` AND (2) `gc_queue_pending_for_cluster == 0` AND (3) `no_open_multipart_on_cluster` | US-004 fixes Cassandra (3) probe; existing US-006 from drain-cleanup enforces ANDing |
| "Restore to live (cancel evacuation)" button does what it says | typed-confirm input matches cluster id case-sensitively before submit; modal body declares no-rollback | US-003 |

## User Journey Walkthrough

| # | Operator action | Surface | Backing story |
|---|-----------------|---------|---------------|
| 1 | Open `/console/diagnostics/traces` | TraceBrowser page | (existing) |
| 2 | Filter row visible above 200 traces | `<RecentTracesPanel>` | **US-002** |
| 3 | Pick Method=PUT, Status=Error → URL updates | UI + URL persistence | **US-002** |
| 4 | Type "demo-cephb" in path search → debounced 250ms | server filter via /diagnostics/traces?path= | **US-001 + US-002** |
| 5 | List narrows to <5 matching traces | server-side filtered response | **US-001** |
| 6 | Click matching trace → waterfall renders | (existing) | (existing) |
| 7 | Copy URL → paste in second tab → same filtered view loads | URL params parsed on mount | **US-002** |
| 8 | Click "Clear filters" → URL clean | reset | **US-002** |
| 9 | Switch to `/storage` Clusters subsection | (existing) | (existing) |
| 10 | Find cluster mid-evacuation → DrainProgressBar shows "Not ready — gc_queue_pending" amber chip + tooltip | denormalized GC probe + clear chip text | **US-005** |
| 11 | Wait for GC drain → chip flips to green "✓ Ready to deregister" + hover tooltip "Edit STRATA_RADOS_CLUSTERS env to remove this cluster, then rolling restart" | new tooltip on existing chip | **US-003** |
| 12 | Card button reads "Restore to live (cancel evacuation)" outline variant — clearly secondary | renamed button + reduced visual weight | **US-003** |
| 13 | Click button → typed-confirm modal "Moved chunks remain on target clusters; no rollback" → submit → state=live | (existing pattern) | **US-003** |
| 14 | Open multipart on draining cluster → ListMultipartUploadsByCluster now returns >0 on Cassandra | persisted BackendUploadID.Cluster | **US-004** |
| 15 | deregister_ready=false until Complete/Abort | safety AND fully enforced on Cassandra | **US-004 + US-005** |
| 16 | Multi-cluster drain at scale: probe queries use `_by_cluster` lookup tables — no ALLOW FILTERING in query log | denormalized maintenance | **US-005** |

Negative paths:
- Invalid trace filter values (e.g. `?method=PATCH-INVALID`) → 400 InvalidFilter
- No traces match → "No traces match the current filters. Widen filters or click Clear."
- Multipart still in flight on Cassandra → deregister_ready=false until upload finishes
- Operator triggers full evacuation on cluster with no alternative → already-shipped "Bulk fix N stuck buckets" path remains

## User Stories

### US-001: Server-side filter query params on `/admin/v1/diagnostics/traces`
**Description:** As a developer, I need the admin diagnostics endpoint to accept filter query params and apply them in-memory before pagination so the UI can narrow the list without fetching everything.

**Acceptance Criteria:**
- [ ] Query params on `GET /admin/v1/diagnostics/traces`: `?method=` (one of PUT, GET, DELETE, POST, HEAD, OPTIONS, PATCH; case-insensitive), `?status=` (Error|OK), `?path_substr=` (free-form substring, max 256 chars), `?path=` (alias for path_substr for URL brevity), `?min_duration_ms=int`, `?max_duration_ms=int`
- [ ] Empty/missing params = no filter on that axis (backwards compat)
- [ ] Invalid values return `400 BadRequest` with `code="InvalidFilter"` + descriptive message
- [ ] When both min + max duration provided, `min > max` → 400 InvalidFilter
- [ ] `internal/otel/ringbuf.RingBuffer.Filter(opts FilterOpts) []TraceSummary` applies filter in-memory before pagination
- [ ] Filter logic: method matches root_name HTTP-method prefix; status matches `summary.Status` exact; path_substr matches `strings.Contains(strings.ToLower(root_name), strings.ToLower(query))`; min/max duration apply to `summary.DurationMs`
- [ ] Total in response = filtered count (not full ringbuf size)
- [ ] OpenAPI spec updated with new query params (description + enum/range) + 400 response shape
- [ ] Audit row preserved: `admin:GetDiagnosticsTraces`
- [ ] Unit tests: each param independently + combinations + invalid values + min>max
- [ ] Unit test: filter applied BEFORE pagination
- [ ] Integration test: 20 mixed traces → various filter combos → correct subset
- [ ] `go vet ./...` passes; typecheck passes; tests pass

### US-002: UI filter row in `<RecentTracesPanel>` + URL persistence
**Description:** As an operator, I want a filter row above recent traces with method/status/path/duration controls and URL-persistent state so I can find a specific request fast and share the view.

**Acceptance Criteria:**
- [ ] New filter row above the trace table in `web/src/components/diagnostics/RecentTracesPanel.tsx`: Method `<Select>` (All/PUT/GET/DELETE/POST/HEAD/OPTIONS/PATCH; default All); Status `<Select>` (All/Error/OK; default All); Path substring `<Input type="search">` debounced 250ms; Min duration ms `<Input type="number">`; "Clear filters" button
- [ ] Filter state synced to URL search params via `useSearchParams`: `?method=PUT&status=Error&path=demo-cephb&min_duration_ms=100`
- [ ] Initial mount reads URL → applies filter state
- [ ] Each filter change updates URL + triggers refetch via TanStack Query key including filter params
- [ ] Empty-result state: "No traces match the current filters. Widen filters or click Clear."
- [ ] Existing Sort by Started/Duration dropdown preserved
- [ ] Existing sessionStorage history panel below live list unaffected
- [ ] Reuse `<Select>`, `<Input>`, `<Button>` primitives
- [ ] Bundle size delta ≤ 3 KiB gzipped
- [ ] Playwright spec covers: navigate `?method=PUT` → only PUT shown; type path filter → debounced; Clear → URL clean; back/forward navigation
- [ ] Unit test: empty filter → no URL params; setting filter → URL updated; reading URL on mount → state initialised
- [ ] Typecheck passes; verify in browser using dev-browser skill

### US-003: UI clarity — rename "Cancel deregister prep" + chip tooltip + visual hierarchy
**Description:** As an operator, I want the cluster card after full evacuation to make clear what the primary action is (env edit + restart) and what the escape hatch is (restore to live) — so the chip and button don't contradict each other.

**Acceptance Criteria:**
- [ ] `web/src/components/storage/ClustersSubsection.tsx` button when `state=evacuating + chunks=0 + deregister_ready=true`: label "Cancel deregister prep" → "Restore to live (cancel evacuation)"
- [ ] Button styling: `variant="outline" size="sm"` (reduces visual weight)
- [ ] `<DrainProgressBar>` green chip "✓ Ready to deregister" gains hover tooltip: "Edit STRATA_RADOS_CLUSTERS env to remove this cluster, then rolling restart. See operator runbook for deregister procedure."
- [ ] Card layout: chip rendered ABOVE action button (vertical stack), not adjacent
- [ ] `<ConfirmUndrainEvacuationModal>` (existing typed-confirm modal for the Restore action) body text clarified: "Moved chunks remain on target clusters; no rollback. Cluster will accept writes again."
- [ ] Playwright UI test asserts new label + tooltip text + chip-above-button layout
- [ ] Bundle size delta ≤ 1 KiB gzipped
- [ ] Typecheck passes; verify in browser using dev-browser skill

### US-004: Persist `BackendUploadID.Cluster` on Cassandra multipart_uploads + probe via WHERE cluster=?
**Description:** As a developer, I need Cassandra `ListMultipartUploadsByCluster` to return the real count of in-flight multipart uploads on a cluster so the deregister_ready safety AND (manifest=0 AND gc_queue=0 AND multipart=0) is fully enforced on Cassandra deploys.

**Acceptance Criteria:**
- [ ] `ALTER TABLE multipart_uploads ADD cluster text` (schema-additive — idempotent via `alterStatements` helper that swallows "column already exists")
- [ ] Multipart Init handler in `internal/meta/cassandra/store.go` captures resolved cluster id from placement picker context + persists via INSERT
- [ ] Existing rows without cluster column: handler tolerates NULL value on read; backfill on first scan via `WHERE cluster IS NULL` → default to "default" (legacy single-cluster). Documented as one-time migration note.
- [ ] `ListMultipartUploadsByCluster` Cassandra impl: `SELECT cluster FROM multipart_uploads WHERE cluster=? LIMIT ? ALLOW FILTERING` (will be replaced by denormalized lookup in US-005)
- [ ] TiKV impl: verify multipart handle includes cluster in key prefix (already shipped via placement-rebalance cycle); probe scans by prefix
- [ ] Memory impl: already tracks cluster in handle — verify probe returns correct count
- [ ] Contract test `caseListMultipartUploadsByCluster` (extension to existing `internal/meta/storetest/contract.go`) covers all three backends
- [ ] Integration test (multi-cluster lab): open multipart Init on cephb → drain cephb evacuate → `ListMultipartUploadsByCluster(cephb, 1)` returns 1 → /drain-progress reports `not_ready_reasons=[open_multipart]` → Complete multipart → probe returns 0 → deregister_ready becomes true (assuming other conditions met)
- [ ] `go vet ./...` passes; typecheck passes; tests pass

### US-005: ALLOW FILTERING denormalize for GC + multipart probes
**Description:** As a developer, I need the GC queue + multipart probes to use partitioned lookup tables instead of ALLOW FILTERING so the drain hot path doesn't trigger Cassandra antipattern.

**Acceptance Criteria:**
- [ ] New Cassandra tables (idempotent CREATE TABLE IF NOT EXISTS via `tableDDL`):
  - `gc_entries_by_cluster (cluster text, region text, enqueued_at timeuuid, oid text, PRIMARY KEY ((cluster), region, enqueued_at, oid))`
  - `multipart_uploads_by_cluster (cluster text, bucket_id uuid, key text, upload_id text, PRIMARY KEY ((cluster), bucket_id, key, upload_id))`
- [ ] Dual-write maintenance:
  - `EnqueueChunkDeletion` writes BOTH `gc_entries_v2` AND `gc_entries_by_cluster`
  - `AckGCEntry` deletes from BOTH tables (idempotent)
  - Multipart Init writes BOTH `multipart_uploads` AND `multipart_uploads_by_cluster`
  - Multipart Complete + Abort delete from BOTH tables
- [ ] `ListChunkDeletionsByCluster` Cassandra impl: `SELECT cluster FROM gc_entries_by_cluster WHERE cluster=? LIMIT ?` — no ALLOW FILTERING
- [ ] `ListMultipartUploadsByCluster` Cassandra impl: `SELECT cluster FROM multipart_uploads_by_cluster WHERE cluster=? LIMIT ?` — no ALLOW FILTERING (replaces the US-004 ALLOW FILTERING query)
- [ ] Lazy migration on read of legacy `gc_entries_v2` + `multipart_uploads`: dual-write to lookup table if missing (best-effort, log on failure)
- [ ] One-shot reconcile worker at boot: scans full legacy tables once, writes missing lookup rows; logs "reconcile complete" summary; idempotent (skips already-present lookup rows). Wired in `internal/serverapp` boot path; runs once per process.
- [ ] Unit + integration tests: probes hit `_by_cluster` table (asserted via query log or instrumented mock); dual-write keeps both tables consistent through random churn (insert + delete + re-insert sequences)
- [ ] Memory + TiKV backends unchanged (they don't suffer ALLOW FILTERING) — verify probes still pass after the change
- [ ] Docs note: operators upgrading from previous cycle need the one-shot reconcile to populate lookup tables; reconcile runs automatically at next gateway boot
- [ ] `go vet ./...` passes; typecheck passes; tests pass

### US-006: E2E smoke + Playwright + docs + ROADMAP close-flip (4 entries) + PRD removal
**Description:** As an operator and as a future-maintainer, I need every fix validated end-to-end against the multi-cluster lab + documented so I trust the cycle is actually done.

**Acceptance Criteria:**
- [ ] `scripts/smoke-drain-followup.sh` covers the 16-step walkthrough end-to-end against `docker compose --profile multi-cluster`
- [ ] Smoke assertions: trace filter URL persistence works (curl with query params returns subset); UI Pools chip+button layout via API status; multipart-blocks-deregister flow (open multipart → drain → /drain-progress shows open_multipart in not_ready_reasons → Abort multipart → deregister_ready flips true); denormalized probes verified via Cassandra `EXPLAIN` or query log absence of ALLOW FILTERING
- [ ] Script EXITS NON-ZERO on any assertion failure; per-step `echo "==> Step N: ..."` lines
- [ ] `make smoke-drain-followup` Makefile target wraps script
- [ ] Playwright spec `web/e2e/drain-followup.spec.ts` covers UI half: trace browser filter + URL persistence; cluster card after evacuation with new button label + chip tooltip + chip-above-button layout; multipart-blocks-deregister UI flow
- [ ] `docs/site/content/best-practices/placement-rebalance.md` "Drain lifecycle" section updated: chip tooltip semantics + "Restore to live" rename rationale; multipart-blocks-deregister documented; one-shot reconcile note for upgrades from previous cycle
- [ ] `docs/site/content/best-practices/web-ui.md` capability matrix gains rows for: Trace browser filter row, Chip-above-button layout, Multipart-blocks-deregister surfacing
- [ ] `ROADMAP.md` FOUR entries close-flipped in same commit:
  - P3 — Trace browser recent-list has no filter / search → Done
  - P2 — UI confusion: "Ready to deregister" chip + "Cancel deregister prep" button shown together → Done
  - P2 — Multipart-on-cluster probe is a no-op on Cassandra backend → Done
  - P3 — ALLOW FILTERING on `cluster` column for GC + multipart probes (denormalize into lookup tables) → Done
  - all four flipped to `~~**Px — ...**~~ — **Done.** <summary>. (commit \`<pending>\`)`; closing SHA backfilled in follow-up commit on main
- [ ] `tasks/prd-drain-followup.md` REMOVED in the same commit (PRD lifecycle rule)
- [ ] Manual screenshots: filter row narrowing 200 traces; cluster card with new button label + chip tooltip; drain-progress JSON showing multipart-blocked deregister_ready=false
- [ ] `make docs-build` succeeds
- [ ] `make vet` succeeds
- [ ] `make test` succeeds
- [ ] `pnpm run build` succeeds
- [ ] `make smoke-drain-followup` succeeds against the running multi-cluster lab
- [ ] Typecheck passes

## Functional Requirements

- **FR-1:** `/admin/v1/diagnostics/traces` accepts `?method=`, `?status=`, `?path_substr=` / `?path=`, `?min_duration_ms=`, `?max_duration_ms=`
- **FR-2:** Invalid filter values → 400 `InvalidFilter`; total in response = filtered count
- **FR-3:** UI filter row in `<RecentTracesPanel>` with URL persistence via react-router useSearchParams
- **FR-4:** Empty-result state when no traces match
- **FR-5:** Button "Cancel deregister prep" renamed → "Restore to live (cancel evacuation)"; outline secondary styling
- **FR-6:** Chip "✓ Ready to deregister" gains hover tooltip with env-edit instruction
- **FR-7:** Card vertical stack — chip above button
- **FR-8:** Cassandra `multipart_uploads` row carries `cluster` column; `ListMultipartUploadsByCluster` queries it via `WHERE cluster=?`
- **FR-9:** Denormalized lookup tables `gc_entries_by_cluster` + `multipart_uploads_by_cluster` partitioned on cluster
- **FR-10:** GC + multipart probes use lookup tables — no ALLOW FILTERING
- **FR-11:** Dual-write maintenance on enqueue/dequeue + multipart lifecycle
- **FR-12:** One-shot reconcile worker scans legacy tables once at boot to populate lookups
- **FR-13:** 16-step smoke + Playwright + 4 ROADMAP entries close-flip

## Non-Goals

- **No new trace browser features beyond filter.** Tag filtering, regex on path, multi-select method/status — all deferred to future P3.
- **No filter on tail-sampled OTLP exported traces.** Filter applies only to in-process ringbuf.
- **No saved filter presets in UI.** URL is enough — operator copies / shares.
- **No drain-strict env reintroduction.** Drain remains unconditionally strict (per drain-transparency cycle).
- **No bulk drain.** One cluster at a time.
- **No automatic reconcile worker periodic schedule.** One-shot at boot only — lookup tables stay consistent via dual-write going forward.
- **No backwards-compat alias for `cancel-deregister-prep` button label.** Hard rename.

## Design Considerations

- **Chip-above-button layout** in ClustersSubsection card: vertical stack with chip occupying its own row prevents visual collision. Action buttons row below — chip is status, button is action.
- **Reuse `<ConfirmUndrainEvacuationModal>`** from drain-cleanup US-007 — just update its body text to match the rename; modal name stays for code stability.
- **Filter row layout in RecentTracesPanel**: horizontal flex row with method/status dropdowns left, path search center (flex-1), duration ms compact right, Clear button rightmost. Matches existing Sort dropdown row pattern.
- **URL params**: short form (`method`, `status`, `path`, `min_duration_ms`, `max_duration_ms`) for browser address bar readability; server accepts both `path` and `path_substr`.
- **Lookup table partition keys**: `(cluster)` only — operator drain queries scan per-cluster so partition fits the access pattern. Clustering columns provide uniqueness without scattering.
- **One-shot reconcile** runs as part of `internal/serverapp.Run` boot path, BEFORE the gateway accepts requests — operator sees "reconcile complete" log line in startup output. Cassandra paging used for memory safety on large legacy tables.

## Technical Considerations

- **Lazy migration**: in dual-write paths, the `_by_cluster` row insert should be idempotent (full PK), so the one-shot reconcile + dual-write don't race fatally — both writes converge to the same state. Test random-order insert + delete + re-insert sequences to confirm.
- **Reconcile cost**: O(total rows in gc_entries_v2 + multipart_uploads). For typical deploys (<100k rows) the reconcile completes in seconds. Larger tables → minutes. Document in operator runbook + boot log.
- **Probe performance**: `SELECT cluster FROM <_by_cluster> WHERE cluster=? LIMIT 1` is partition-key lookup — microseconds. Same as bucket_stats lookups in placement-rebalance cycle.
- **Cassandra ALTER TABLE multipart_uploads ADD cluster text**: idempotent via existing alterStatements + isColumnAlreadyExists helper. Pattern matches placement-ui PoolStatus.Cluster field addition.
- **Filter row debounce 250ms**: balance between server load and operator responsiveness. Same as drain-transparency BulkPlacementFixDialog input debounce.
- **Existing trace-filter cycle prep** (commit `3b95209` on stale branch): contents superseded by this PRD; can ignore that branch / never merge / delete safely.

## Success Metrics

- Operator narrows 200 traces to ≤5 matches in ≤2 seconds via filter row
- "✓ Ready to deregister" chip tooltip + "Restore to live" rename eliminate the dual-message confusion (operator survey or visual hierarchy review)
- Cassandra deploys correctly block deregister_ready when multipart in flight (smoke scenario verifies)
- Drain probes complete in microseconds (partition lookup) instead of seconds (ALLOW FILTERING scan) — measurable via Cassandra query log
- 4 ROADMAP entries closed in one commit; closing SHA backfilled
- 16-step smoke + Playwright e2e green in ≤ 3 minutes against the lab

## Open Questions

- Should one-shot reconcile log per-1k-rows progress for large tables? Recommendation: yes — operator on a 10M-row table needs the heartbeat. Add to US-005 acceptance.
- Should the `_by_cluster` lookup tables have a TTL? Recommendation: no — they mirror the source tables' lifecycle (delete on dequeue/Complete/Abort). Adding TTL risks divergence.
- Should the trace filter URL params support multiple values (e.g. `?method=PUT,POST`)? Recommendation: defer — single value covers 95% of cases; multi-value is future P3.
