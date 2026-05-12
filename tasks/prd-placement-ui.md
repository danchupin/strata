# PRD: Web UI surfacing for placement policy + cluster topology

## Introduction

The `ralph/placement-rebalance` cycle shipped the server-side primitives for multi-cluster operation: per-bucket `Placement` policy (`map[cluster]int` weights), `cluster_state` drain sentinel, `--workers=rebalance` mover, `GET /admin/v1/clusters`, `POST /admin/v1/clusters/{id}/drain|undrain`, and `PUT/GET/DELETE /admin/v1/buckets/{name}/placement`. But the Web UI does NOT surface any of it — the Storage page renders a flat Pools list with no cluster column, BucketDetail has no Placement dialog, there is no drain banner. Operators must use `curl` to inspect split / set policy / drain.

This PRD adds the UI half: cluster cards on the Storage page (with Drain controls), a "Cluster" column on the Pools table, a Placement tab on BucketDetail (with a slider + numeric editor), a cluster-wide drain banner in the AppShell, and a Rebalance progress chip on each cluster card driven by Prometheus.

Closes ROADMAP P3 *Web UI — Placement + cluster surfacing* (Web UI section).

## Goals

- Operator can see every registered data cluster (id / state / backend / used / total) in the `/storage` page
- Operator can drain a cluster via a typed-confirmation modal (operator types the cluster id to enable the Drain button), undrain via simple click
- Operator can view per-pool cluster routing via a new `Cluster` column on the Pools table
- Operator can set / view / reset per-bucket `Placement` policy via a Placement tab on BucketDetail with a 0–100 slider + numeric input per cluster row, "Reset to default" button
- A cluster-wide draining banner appears in the AppShell when any cluster is `state=draining`; dismissible per-session via `localStorage`
- Each cluster card carries a Rebalance progress chip showing chunks-moved + refused counters per target cluster, pulled from Prometheus
- A Playwright e2e spec exercises the full flow (set policy → upload → drain → banner → undrain → banner gone) against the in-memory test rig

## User Stories

### US-001: Add `Cluster` field to `PoolStatus` + wire DataHealth
**Description:** As a developer, I need each pool row in `/admin/v1/storage/data` to carry its cluster id so the UI can split pools by cluster.

**Acceptance Criteria:**
- [ ] Add `Cluster string` field to `PoolStatus` (`internal/data/backend.go`) with `json:"cluster"` tag
- [ ] `internal/data/rados/health.go::DataHealth` populates `PoolStatus.Cluster` from the resolved `spec.Cluster` (falling back to `DefaultCluster` constant when empty) for every pool row
- [ ] `internal/data/s3/health.go::DataHealth` populates `PoolStatus.Cluster` from the per-class `ClusterID` resolved by `resolveClass`
- [ ] In-memory backend (`internal/data/memory`) sets `PoolStatus.Cluster = ""` (single virtual cluster — UI renders empty as "—")
- [ ] OpenAPI spec `internal/adminapi/openapi.yaml` updated to include the new field in the `PoolStatus` schema
- [ ] Unit test: `DataHealth` on the two-cluster RADOS lab (or fixture) returns at least two distinct `Cluster` values
- [ ] Unit test: S3 backend `DataHealth` returns the per-class cluster id from the class map
- [ ] Backward-compat: clients that ignore the field still parse the response (no breaking schema rename)
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: Storage page — Clusters subsection + Drain modal + Cluster column on Pools table
**Description:** As an operator, I want to see every registered cluster on the Storage page with its state and a Drain button so I can manage multi-cluster topology without `curl`.

**Acceptance Criteria:**
- [ ] New section `<ClustersSubsection>` rendered on `web/src/pages/Storage.tsx` (under the existing Data tab, above the Pools table) — TanStack Query fetches `/admin/v1/clusters` every 10s
- [ ] One card per cluster: shows `id` (large header), `state` badge ("live" = green, "draining" = orange), `backend` chip ("rados" / "s3"), used / total bytes ("12.4 GiB / 50.0 GiB · 24.8%") when `ClusterStats` returns data; renders "n/a" + tooltip "S3 backend has no cluster fill telemetry" for S3 backend
- [ ] Each card has a `Drain` action button (or `Undrain` when state already draining)
- [ ] Drain button opens a `<ConfirmDrainModal>` requiring the operator to type the exact cluster id into a text field — Drain submit button stays disabled until the typed value matches exactly; the modal shows a warning explainer ("New PUTs will skip this cluster. Existing chunks remain readable until rebalance moves them. This action is reversible via Undrain.")
- [ ] Submit calls `POST /admin/v1/clusters/{id}/drain`; success toast → query refetch → card flips to "draining" + "Undrain" button
- [ ] Undrain button calls `POST /admin/v1/clusters/{id}/undrain` directly (no typed confirmation needed — un-drain is safe)
- [ ] Pools table (already existing) gains a new "Cluster" column rendered to the left of "Class"; sorts by cluster (asc, "" sorts last)
- [ ] Empty cluster id renders as "—" with `text-muted` styling
- [ ] All new components reuse existing `<Card>` / `<Badge>` / `<Modal>` / `<Button>` primitives in `web/src/components/ui/`
- [ ] `pnpm run build` succeeds; bundle size delta ≤ 5 KiB gzipped
- [ ] Typecheck (`pnpm run typecheck` or equivalent) passes
- [ ] Verify in browser using dev-browser skill

### US-003: BucketDetail — Placement tab + slider editor + Reset to default
**Description:** As an operator, I want a Placement tab on BucketDetail with a 0–100 slider per cluster so I can set the per-bucket placement policy without `curl`.

**Acceptance Criteria:**
- [ ] New tab "Placement" added to `web/src/components/BucketDetail*.tsx` (or wherever the tab list lives) — sits alongside Lifecycle / CORS / Quota tabs in the existing order
- [ ] Tab body lists every live cluster (fetched via `/admin/v1/clusters`) as a row: cluster id label, 0–100 horizontal slider, paired numeric input (two-way bound — moving the slider updates the input and vice versa)
- [ ] Initial state: GET `/admin/v1/buckets/{name}/placement`; on 404 (no policy) → all sliders at 0 + a notice "Default routing (no per-bucket policy)"; on 200 → sliders populated from response
- [ ] "Save" button enabled only when (a) at least one cluster has weight > 0 AND (b) all weights are in `[0, 100]`; disabled state shows a tooltip explaining the rule
- [ ] "Save" calls `PUT /admin/v1/buckets/{name}/placement` body `{"placement": {...}}`; on 200 → success toast + close edit mode; on 4xx → error toast with API message
- [ ] "Reset to default" button calls `DELETE /admin/v1/buckets/{name}/placement`; confirmation popover ("Reset to default routing? Existing chunks unaffected.") → on confirm → success toast + sliders zeroed + notice flips to "Default routing"
- [ ] Form validation: numeric input clamps at `[0, 100]`; non-numeric entry blocked; sum-zero submit disabled
- [ ] Reuse existing `<Slider>`, `<Input>`, `<Button>`, `<Tooltip>` primitives
- [ ] Drained clusters (`state=draining`) appear in the editor with a `(draining)` chip next to the id but are still editable (operator may want to drop weight to 0 before drain finishes)
- [ ] `pnpm run build` succeeds
- [ ] Typecheck passes
- [ ] Verify in browser using dev-browser skill

### US-004: AppShell drain banner + per-session dismissal
**Description:** As an operator, I want a cluster-wide banner at the top of every page when any cluster is draining so I'm always reminded a maintenance op is in progress.

**Acceptance Criteria:**
- [ ] New component `web/src/components/layout/PlacementDrainBanner.tsx` mounted at the top of `<AppShell>` (above the main content, below the top nav)
- [ ] Banner renders only when `GET /admin/v1/clusters` returns ≥1 entry with `state=draining`
- [ ] TanStack Query polls `/admin/v1/clusters` every 15s; banner state derived from the response
- [ ] Banner content: orange background, exclamation icon, text "Draining cluster(s): <id1>, <id2>. Rebalance worker is moving chunks off them.", clickable link "View details →" navigating to `/storage#clusters`
- [ ] Dismiss button on the right side; click writes `drain_banner_dismissed=<sessionStamp>` to `localStorage` (sessionStamp = current set of draining ids joined by comma)
- [ ] Banner stays dismissed only while the same set of draining ids persists — if a NEW cluster enters draining or the set otherwise changes, the banner returns (compare current vs stored stamp)
- [ ] Banner does NOT render on the `/login` page (would confuse pre-auth state)
- [ ] Unit test for the dismissal-stamp comparison logic
- [ ] `pnpm run build` succeeds; bundle size delta ≤ 2 KiB gzipped
- [ ] Typecheck passes
- [ ] Verify in browser using dev-browser skill

### US-005: Rebalance progress chip per cluster card + Prometheus query
**Description:** As an operator, I want each cluster card to show how many chunks the rebalance worker has moved into it and how many moves have been refused so I can monitor migration progress at a glance.

**Acceptance Criteria:**
- [ ] On each card in `<ClustersSubsection>` (from US-002), add a `<RebalanceProgressChip>` below the size/state row
- [ ] Chip pulls two metric values per cluster via the existing Prometheus client (`STRATA_PROMETHEUS_URL`): `sum(strata_rebalance_chunks_moved_total{to="<id>"})` and `sum(strata_rebalance_refused_total{target="<id>"})`
- [ ] Renders as `"<N> chunks moved · <M> refused"` with a small inline sparkline of `rate(strata_rebalance_chunks_moved_total{to="<id>"}[5m])` over the last hour (60 1-minute samples)
- [ ] Refresh cadence: 30s (matches other metric widgets)
- [ ] Graceful degradation: when Prometheus is unset (`metrics_available=false` flag from `/admin/v1/metrics/status` or equivalent), chip renders "(metrics unavailable)" instead of erroring
- [ ] No chip for clusters with `backend=memory` (rebalance does not run there)
- [ ] Reuse the existing sparkline component pattern used by Metrics dashboard / Cluster Overview
- [ ] Typecheck passes
- [ ] Verify in browser using dev-browser skill

### US-006: Playwright e2e + docs + ROADMAP close-flip + PRD removal
**Description:** As a maintainer, I want a Playwright spec that exercises the full operator flow + docs that explain the new UI surface, so future regressions get caught and operators know where to look.

**Acceptance Criteria:**
- [ ] New file `web/e2e/placement.spec.ts` covering: (1) login → navigate `/storage` → assert ≥1 cluster card rendered; (2) navigate to demo bucket detail → open Placement tab → drag slider for `cephb` to 100 → Save → assert toast; (3) re-navigate to Storage → Drain primary cluster via typed-confirmation modal (mistyped name keeps button disabled) → assert state=draining + banner appears; (4) Undrain → assert banner gone after refetch; (5) "Reset to default" on Placement tab → confirm popover → DELETE call succeeds → sliders zeroed
- [ ] Spec runs against the in-memory test rig used by existing Playwright specs (single binary on `:9000` with `STRATA_META_BACKEND=memory`, two fake registered clusters via env)
- [ ] CI job `e2e-ui` picks up the new spec automatically (no new GH workflow needed)
- [ ] `docs/site/content/best-practices/web-ui.md` capability matrix gains rows for: Clusters subsection (Storage page), Placement tab (BucketDetail), Drain banner (AppShell), Rebalance progress chip
- [ ] `docs/site/content/best-practices/placement-rebalance.md` (created by previous cycle) gets a new "Web UI" section pointing operators at the new surfaces
- [ ] `ROADMAP.md` Web UI section `**P3 — Web UI — Placement + cluster surfacing.**` close-flip: `~~**P3 — ...**~~ — **Done.** <one-line summary>. (commit \`<pending>\`)` — closing SHA backfilled in follow-up commit on main
- [ ] `tasks/prd-placement-ui.md` REMOVED in the same commit (PRD lifecycle rule — Ralph snapshot is canonical)
- [ ] `make docs-build` succeeds
- [ ] `pnpm run build` succeeds
- [ ] Typecheck passes

## Functional Requirements

- **FR-1:** `PoolStatus.Cluster` field populated by RADOS + S3 + memory data backends
- **FR-2:** Storage page Clusters subsection fetches `GET /admin/v1/clusters` every 10s and renders one card per cluster
- **FR-3:** Drain button opens a typed-confirmation modal; Drain submit disabled until typed value matches cluster id exactly
- **FR-4:** Undrain button calls `POST /admin/v1/clusters/{id}/undrain` directly (no confirmation)
- **FR-5:** Pools table gains a `Cluster` column sortable asc with empty sorting last
- **FR-6:** BucketDetail gets a Placement tab with a slider + numeric input per live cluster + Save + Reset to default
- **FR-7:** Placement Save validates `weight ∈ [0, 100]` per cluster and `sum > 0` before enabling Save
- **FR-8:** Placement Reset calls `DELETE` after a confirmation popover
- **FR-9:** AppShell renders a drain banner when ≥1 cluster reports `state=draining`; banner is per-session dismissible keyed on the set of draining ids
- **FR-10:** Each cluster card carries a Rebalance progress chip pulling `strata_rebalance_chunks_moved_total{to=<id>}` + `_refused_total{target=<id>}` from Prometheus, refreshed every 30s
- **FR-11:** Rebalance chip gracefully degrades when Prometheus is unavailable (renders "(metrics unavailable)")
- **FR-12:** Playwright spec at `web/e2e/placement.spec.ts` exercises the full flow against the in-memory rig and runs in CI

## Non-Goals

- **No new admin endpoints.** This cycle consumes the endpoints shipped by `ralph/placement-rebalance` — no `GET /admin/v1/clusters/{id}/progress` or similar.
- **No metrics infrastructure changes.** Prometheus query goes through the existing `STRATA_PROMETHEUS_URL` client.
- **No dashboard-level "rebalance overview" page.** Progress lives on cluster cards only; a dedicated page is a future P3.
- **No drain scheduling / planning.** Operator can drain immediately; scheduled drain (e.g. "drain at 02:00 UTC") is out of scope.
- **No bulk-edit Placement across N buckets.** One bucket at a time; bulk operator workflow is a future P3.
- **No cluster registration UI.** Cluster set stays config-driven via env (per the prior won't-do decision); UI only surfaces what env defines.
- **No mobile / narrow-viewport optimization.** Reuse the existing AppShell breakpoints; the new components inherit them.
- **No localization.** All copy in English; i18n is out of scope.

## Design Considerations

- **Reuse existing primitives:** `<Card>`, `<Badge>`, `<Modal>`, `<Button>`, `<Slider>`, `<Input>`, `<Tooltip>`, `<Toast>` already exist in `web/src/components/ui/` from prior Web UI cycles. Do NOT introduce new libraries.
- **Sparkline:** reuse the same Recharts wrapper used by Metrics page; no @nivo / Chart.js deps.
- **Spec for the typed-confirmation modal:** mirror the existing `<ConfirmDeleteBucket>` modal shape, which already uses the type-the-name pattern for irreversible operations.
- **Tab order on BucketDetail:** Overview → Objects → Versioning → Lifecycle → CORS → Policy → ACL → Inventory → Access Log → **Placement** (new) → Quota → Replication → Multipart. Placement sits between ACL/Access Log and Quota — operators usually configure placement after the bucket's access surface is settled.
- **Banner styling:** use the same orange/yellow palette as the existing `<StorageDegradedBanner>` (from US-002 of the storage-status cycle) for consistency.
- **Drain confirmation copy:** explicit about reversibility and what changes (new PUTs skip; existing chunks readable; rebalance runs).
- **Empty-cluster id handling:** `Cluster=""` renders as "—" in the Pools column and pools without a `cluster` value group at the bottom when sorted.

## Technical Considerations

- **Per-session dismissal stamp:** key on the SET of draining ids, not just count. Operator drains `c1`, dismisses → drains `c2` later → banner SHOULD reappear because the set changed. Implementation: `JSON.stringify(sortedDrainingIds)` is the stamp.
- **Polling cadence harmony:** `/admin/v1/clusters` polled at 15s (banner) and 10s (Storage page); TanStack Query dedup will collapse the second fetch within the cache window so this is fine. Use a shared `clustersQuery` key.
- **Prometheus query performance:** `sum(strata_rebalance_chunks_moved_total{to="<id>"})` is a cheap range query; do not run it more often than 30s. Use a shared query key for all cluster cards in a single page render so we issue one request per cluster, not one per chip mount cycle.
- **OpenAPI breaking concern:** adding `cluster` field to `PoolStatus` is schema-additive; existing UI builds that ignore the field still parse the response.
- **Playwright in-memory rig:** the existing specs spin up the gateway on `:9000` with `STRATA_META_BACKEND=memory` + a fake `STRATA_RADOS_CLUSTERS` set. For this spec we need two fake clusters; reuse the existing test-only env (e.g. `STRATA_TEST_CLUSTERS=alpha,beta` or whatever the convention is — check `web/e2e/storage.spec.ts` for precedent).
- **Banner mount point:** must sit INSIDE the `<AppShell>` (where auth context is available) so it doesn't poll before login. Skip-render on `/login` route.

## Success Metrics

- Operator can drain a cluster + verify the new PUT routing in < 30s without `curl` or shell access
- Placement policy round-trip (PUT → GET → DELETE) is fully clickable from BucketDetail
- Storage page shows both clusters in the `multi-cluster` compose lab with at least one chunk attributed per cluster after the e2e test runs
- Playwright spec runs green in CI within ≤ 30s
- Bundle size delta across the cycle ≤ 12 KiB gzipped total

## Open Questions

- Should the Pools "Cluster" column be filterable (dropdown above the table) in this cycle, or deferred? Recommend deferred — sorting is enough for v1.
- Should the drain confirmation typed-name be case-sensitive? Recommend yes (matches AWS console convention for cluster destroys).
- Should we surface `STRATA_REBALANCE_INTERVAL` / `_RATE_MB_S` on the cluster card as a "next rebalance at HH:MM" hint? Defer — operator can read it from logs or env.
