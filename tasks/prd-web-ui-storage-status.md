# PRD: Web UI — Storage status (meta + data backend observability)

## Introduction

Today's Cluster Overview surfaces strata replica heartbeats, but the **storage layer** (meta backend = Cassandra / TiKV; data backend = RADOS / S3-over-S3) is opaque to the operator browsing the console. The only signal is `/readyz` returning 200 vs 503; if Cassandra has a degraded peer or RADOS has a `HEALTH_WARN` pool, the UI keeps reading "healthy" because the gateway's readiness check just probes liveness, not health.

This PRD adds a **dedicated `/storage` page** (under the existing console) that exposes:

- **Meta backend health** — Cassandra peer table (live/down peers, schema version drift) OR TiKV/PD store health (`/pd/api/v1/stores`, region count, leader balance).
- **Data backend health** — RADOS per-pool `librados.GetPoolStat` (bytes used / objects / replication state) OR S3-over-S3 backend reachability.
- **Per-storage-class breakdown** — bytes + object count per class (STANDARD / STANDARD_IA / GLACIER_IR), embedded under the Data backend section.
- **Cluster Overview hero summary** — short "X TiB across N pools" card so the operator gets storage-shape feedback at a glance.
- **Top-level degraded banner** — when meta backend reports peer-down or RADOS reports HEALTH_WARN, every page renders a banner above the layout (sticky, dismissible per session).

The new endpoints sit under `/admin/v1/storage/*` with the same JWT session-cookie auth as Phase 1/2/3.

## Goals

- **`/admin/v1/storage/meta`** — lockstep across Cassandra + TiKV: peers / stores + replication / region info.
- **`/admin/v1/storage/data`** — RADOS pool list + per-pool stats; per-class breakdown via meta join.
- **`/storage` page** — Meta + Data tabs, per-class card, refresh button, polling.
- **Cluster Overview hero** — total bytes + per-class breakdown chip strip.
- **Degraded warning banner** — top-level component reading `/admin/v1/storage/health` aggregate; visible until storage health flips to OK.
- **Playwright `storage.spec.ts`** — covers happy path + degraded simulation.
- **`docs/storage.md`** + ROADMAP close-flip.

## User Stories

### US-001: Meta backend health probe (Cassandra peers / TiKV PD stores)
**Description:** As a debug-tool author, I want a meta.Store-shaped probe that returns peer / store health so the storage page has data.

**Acceptance Criteria:**
- [ ] New `internal/meta.HealthProbe` interface: `MetaHealth(ctx) (*meta.MetaHealthReport, error)` with fields `{Backend string, Nodes []NodeStatus, ReplicationFactor int, Warnings []string}` where `NodeStatus = {Address string, State string, SchemaVersion string, DataCenter string, Rack string}`.
- [ ] Cassandra impl: `SELECT peer, data_center, rack, release_version, schema_version FROM system.peers` UNION ALL `SELECT broadcast_address … FROM system.local`. Aggregates schema-version drift into `Warnings`.
- [ ] TiKV impl: HTTP GET `http://{pdEndpoint}/pd/api/v1/stores` → parse `{stores: [{store: {id, address, state_name, ...}, status: {region_count, ...}}]}` into NodeStatus. Aggregates raft-leader imbalance into `Warnings` if any store has 0 leaders while peers have >0.
- [ ] Memory impl returns a single-node report for completeness.
- [ ] storetest contract `caseMetaHealth` exercises all three.
- [ ] Endpoint `GET /admin/v1/storage/meta` in adminapi returns the report as JSON.
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: Data backend health probe (RADOS pool stats / S3-over-S3 reachability)
**Description:** As a debug-tool author, I want a data.Backend-shaped probe that returns per-pool stats so the storage page has data.

**Acceptance Criteria:**
- [ ] New `internal/data.HealthProbe` interface: `DataHealth(ctx) (*data.DataHealthReport, error)` with fields `{Backend string, Pools []PoolStatus, Warnings []string}` where `PoolStatus = {Name string, Class string, BytesUsed uint64, ObjectCount uint64, NumReplicas int, State string}`.
- [ ] RADOS impl: iterate the configured `[rados] classes` map, call `librados.GetPoolStat` per pool, populate PoolStatus. Pulls cluster `HEALTH_OK/HEALTH_WARN/HEALTH_ERR` via `librados.MonCommand("status")` with summarised JSON parse — surface in `Warnings`.
- [ ] S3-over-S3 impl: HEAD on the configured backend bucket, returns one PoolStatus with `Name=<backend bucket>`, `Class=<all classes mapped to this single bucket>`, `BytesUsed=0`/`ObjectCount=0` (S3 backend has no native stats endpoint; document gap), `State=reachable|error`.
- [ ] Memory impl returns one PoolStatus with size=process-RSS proxy.
- [ ] Endpoint `GET /admin/v1/storage/data` returns the report.
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: Per-storage-class breakdown
**Description:** As an operator, I want bytes + object-count per storage class so I can see which class is consuming the most space.

**Acceptance Criteria:**
- [ ] Extend the existing `bucketstats.Sampler` (already per-shard from Phase 3 US-012) to ALSO emit per-(bucket, class) aggregates. New gauges `strata_storage_class_bytes` + `strata_storage_class_objects` keyed `{class, bucket}`.
- [ ] Lockstep meta backends: cassandra reads `objects` rows aggregated per `(bucket_id, storage_class)`. Memory computes from in-process map. TiKV scans existing range-scan and groups in-process by `o.StorageClass`.
- [ ] Cardinality: bounded by bucket-stats top-N envvar (existing `STRATA_BUCKETSTATS_TOPN`).
- [ ] Endpoint `GET /admin/v1/storage/classes` returns `{classes: [{class, bytes, objects}], pools_by_class: {class: pool_name}}` (the latter from the configured `[rados] classes` map so the UI can render "STANDARD → strata.rgw.buckets.data").
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: /storage page UI (Meta tab + Data tab + classes card)
**Description:** As an operator, I want a single page where I can see meta + data backend health and per-class breakdown.

**Acceptance Criteria:**
- [ ] New `web/src/pages/Storage.tsx` + route `/storage` + sidebar entry under a new top-level (NOT nested under `Diagnostics`) — storage is operational dashboard, not debug-only.
- [ ] Two shadcn Tabs: `Meta` (default) + `Data`.
- [ ] Meta tab: header card "Backend = cassandra / tikv / memory" + table of NodeStatus rows (Address / State / DC / Rack / SchemaVersion). Warning banner at top when `report.Warnings.length > 0`.
- [ ] Data tab: header card "Backend = rados / s3 / memory" + table of PoolStatus rows (Name / Class / Bytes used / Objects / Replicas / State). Below the table — "Storage classes" subsection with bytes-per-class horizontal stacked bar (recharts BarChart) + per-class chip list with byte counts.
- [ ] Polls every 30 s via TanStack Query.
- [ ] Refresh button mirrors Consumers / Buckets shape.
- [ ] Empty / loading / error states. When backend type=memory the tables render single rows with explainer.
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-005: Cluster Overview hero summary + degraded warning banner
**Description:** As an operator, I want a glance-level storage summary on the home page AND a top-level banner when storage is degraded so I notice problems before navigating.

**Acceptance Criteria:**
- [ ] Cluster Overview hero (`web/src/pages/ClusterOverview.tsx`) gains a "Storage" card — total bytes across all pools + per-class chip strip (e.g., `STANDARD: 800 GiB | STANDARD_IA: 400 GiB | GLACIER_IR: 50 GiB`). Reads from `/admin/v1/storage/classes` (cached 30 s).
- [ ] New `/admin/v1/storage/health` aggregate endpoint returns `{ok: bool, warnings: []string, source: meta|data}`. Combines the worst-state of Meta + Data probes — `ok=false` when either reports `Warnings.length > 0` or any node/pool is in a non-OK state.
- [ ] New `<StorageDegradedBanner>` component (`web/src/components/StorageDegradedBanner.tsx`) renders above the page layout (above sidebar + content). Polls `/admin/v1/storage/health` every 30 s; visible when `ok=false`. Lists the worst 3 warnings + "View Storage page" link. Dismissible per session via sessionStorage flag (re-shows on next refresh if still degraded).
- [ ] Banner position: above `<AppShell>` header — operator sees it on every page.
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill (simulate degraded by stopping a Ceph mon container)

### US-006: Playwright storage.spec.ts + docs + ROADMAP close + cycle merge
**Description:** As a maintainer, I want Playwright e2e + operator docs + ROADMAP closed in one commit at cycle end.

**Acceptance Criteria:**
- [ ] New `web/e2e/storage.spec.ts` covers:
      - `storage-page-renders` — login → navigate /storage → assert Meta tab + Data tab visible, NodeStatus row count >= 1, PoolStatus row count >= 1.
      - `cluster-hero-shows-storage-card` — login → home → assert "Storage" card visible with at least one class chip.
      - `degraded-banner-on-warn` — fixture writes a fake health response with `ok:false, warnings:[...]` via test-only `STRATA_STORAGE_HEALTH_OVERRIDE` env (set in CI for the e2e job) → assert banner visible above shell + dismiss button works.
- [ ] CI `e2e-ui` job adds `pnpm exec playwright test storage.spec.ts` after existing specs.
- [ ] New `docs/storage.md` covers: which env vars wire which backend; what each `Warnings` entry means; how to interpret RADOS HEALTH_WARN (link to ceph docs); per-class pool mapping.
- [ ] `docs/ui.md` Capability Matrix gains a row referencing the Storage page.
- [ ] ROADMAP gets `~~**P3 — Web UI — Storage status (meta + data backend observability).**~~ — **Done.** <one-line summary>. (commit \`<sha>\`)`.
- [ ] **Cycle-end merge**: fast-forward / squash-merge `ralph/web-ui-storage-status` into `main`, push, archive `scripts/ralph/prd.json` + `scripts/ralph/progress.txt` under `scripts/ralph/archive/2026-MM-DD-web-ui-storage-status/`.
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- **FR-1**: `internal/meta.HealthProbe` is a new optional interface; backends that support it implement (Cassandra, TiKV, memory) lockstep. Adminapi probes the meta store via type assertion — backends without the interface return a degenerate report.
- **FR-2**: `internal/data.HealthProbe` is the same shape on the data side; RADOS, s3, memory implement.
- **FR-3**: All `/admin/v1/storage/*` endpoints sit behind the existing JWT session-cookie + audit middleware; read-only (no write actions in this cycle).
- **FR-4**: Per-class aggregation extends the existing `bucketstats.Sampler` from Phase 3 US-012. Cardinality cap honours `STRATA_BUCKETSTATS_TOPN`.
- **FR-5**: `/storage` UI page is in the primary sidebar (not Diagnostics) — it's an operational dashboard, not a debug-only surface.
- **FR-6**: Storage hero card on Cluster Overview reads from the same `/admin/v1/storage/classes` endpoint — single source of truth.
- **FR-7**: Degraded banner is rendered ABOVE the AppShell layout, visible on every page, dismissible per session.
- **FR-8**: Polling cadence: 30 s for `/storage` page, 60 s for the hero card (less time-sensitive), 30 s for the health banner.

## Non-Goals

- **No write actions on storage page.** Pool creation / deletion / reweight is `ceph osd pool` / `kubectl edit` operator-side; UI is point-in-time read-only.
- **No alert / paging from UI.** Grafana + Alertmanager is the right tool — banner is informational only, not actionable.
- **No s3-over-s3 deep stats.** S3-over-S3 backend doesn't expose per-pool object counts (S3 LIST is too expensive); UI shows reachability only with explainer card.
- **No multi-cluster RADOS view.** Single-cluster only this cycle. (Multi-cluster shipped earlier as US-044 of modern-complete; observability for that is future P3.)
- **No historical trending.** Page shows current state, not time-series. For history, point operators at Grafana.
- **No object-level inspection.** Per-class breakdown is bucket-level aggregate, not "which objects are in GLACIER_IR". For object-level use the existing Bucket Detail page + filter.

## Design Considerations

- **`/storage` lives in primary sidebar**, between Buckets and Consumers. Icon: lucide `HardDrive` or `Database`.
- **Bundle budget**: stay within the existing ≤500 KiB gzipped initial. Storage page is lazy-loaded via React.lazy.
- **Recharts BarChart for class breakdown** — already in bundle (debug pages).
- **Cassandra peer table**: 4 cols (Address / DC / Rack / State + SchemaVersion as a chip). Schema-version drift is the operator's first signal of an in-progress / failed schema migration.
- **PD store table**: 4 cols (ID / Address / State / Region count). Region-count imbalance > 2× across stores is a warning.
- **RADOS pool table**: 5 cols (Name / Class / Bytes used / Objects / State). Replicas count is per-pool config (read once at startup).
- **Empty-state copy** mirrors Phase 3 conventions — explainer + link to docs/storage.md.

## Technical Considerations

- **Cassandra `system.peers` query**: simple read; avoid `LOCAL_QUORUM` because peer info is local. Use `LOCAL_ONE`. Cache 10 s in-process to avoid re-querying on each adminapi hit.
- **PD HTTP API**: `internal/promclient.Client` shape doesn't fit; create a dedicated thin client `internal/meta/tikv/pdclient.go` that takes `[]string` PD endpoints + tries them in order with a 2 s timeout each, returns first non-error.
- **`librados.MonCommand`**: returns JSON; parse `{health: {status: "HEALTH_WARN|HEALTH_OK", checks: {...}}}` into `Warnings`. Cap warning count at 5 to keep the wire payload small.
- **`librados.GetPoolStat`**: returns `(num_kb, num_objects, num_object_clones, ...)`. Translate kb → bytes for the wire shape.
- **bucketstats per-class extension**: similar shape to per-shard (US-012 of Phase 3). Cassandra reads `objects` rows aggregated per `(bucket_id, storage_class)`. Memory: in-process map by class. TiKV: scan with existing range-scan, group in-process by `o.StorageClass`.
- **Health banner above shell**: AppShell (`web/src/components/layout/AppShell.tsx`) is currently the outermost layout. Add `<StorageDegradedBanner>` as a sibling above the existing `<div className="flex">` so it occupies its own row at the top of the viewport.
- **STRATA_STORAGE_HEALTH_OVERRIDE env**: test-only knob; when set to a JSON string, the `/admin/v1/storage/health` handler returns it verbatim. Used by Playwright fixture to simulate degraded state without poking the real backend. Document in `internal/adminapi/storage_health_test.go`.

## Success Metrics

- Operator with no prior cluster knowledge can spot a Cassandra peer-down / RADOS HEALTH_WARN in under 30 s without leaving the console.
- Storage page bundle adds <30 KiB gzipped to the initial download.
- Playwright `storage.spec.ts` runs in <60 s on CI with no flaky retries.
- ROADMAP P3 entry `Web UI — Storage status` flips to Done at cycle close.

## Open Questions

- **Cassandra `system.peers` quorum semantics**: `LOCAL_ONE` is fine for read-but-might-be-stale. If operators want fresher peer info, fall back to `EACH_QUORUM`. Decision at story-start of US-001 — start with LOCAL_ONE; bump if real operator complaints.
- **PD endpoint discovery**: today the gateway is configured with one or more PD endpoints in `STRATA_TIKV_PD_ENDPOINTS`. The PD HTTP API has its own discovery path (`/pd/api/v1/members`); should `internal/meta/tikv/pdclient.go` use the configured list as bootstrap and then refresh from `/members`? Keeping bootstrap-only for simplicity unless we need cross-PD failover beyond client-go's built-in.
- **RADOS replication state granularity**: `librados.GetPoolStat` returns numeric replicas; degraded objects vs healthy objects requires `ceph osd df` + parsing. For this cycle we surface only the headline `HEALTH_OK/WARN/ERR` from `librados.MonCommand`. Per-pool detailed degradation is a future P3 if operators ask.
- **Hero card cardinality**: with many storage classes (>10), the chip strip overflows. Cap at top-5 by bytes; rest collapse into "+N more" link to `/storage`. Decision baked in US-005 visual review.
