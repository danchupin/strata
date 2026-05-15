---
title: 'Web UI (Strata Console)'
weight: 10
description: 'The embedded operator console — pages, env vars, end-to-end tests.'
---

# Strata Console (Web UI) — Operator Guide

The Strata Console is an embedded read-only web UI for cluster operators.
It ships in the same binary as the gateway (`go:embed`) and is served at
`/console/` on the gateway HTTP port. No separate process, no separate
deploy.

This document is the Phase 1 (foundation) operator guide. Phase 2 (admin
write actions) and Phase 3 (debug tooling — heatmaps, slow queries, OTel
trace browser) ship in their own cycles.

## Console vs aws-cli — when to use which

| Task | Tool | Why |
|---|---|---|
| Browse cluster status / health at a glance | Console | Single-page summary, auto-refreshes |
| List buckets across the whole cluster | Console | Sortable, paginated, search |
| Inspect a bucket's object tree | Console | Folder navigation + object details panel |
| Eyeball request rate / latency / error rate | Console | 4-up dashboard, 15m/1h/6h/24h/7d ranges |
| `mb` / `rb` / `cp` / `mv` / `presign` (write actions) | aws-cli | Phase 1 console is read-only — use S3 API |
| Anything scriptable / batch | aws-cli | Built for it; the console is a UI |
| IAM / access keys management | (Phase 2) | Static credentials only today |
| Debug a slow request / hot key | Phase 3 (debug) | Heatmaps + slow-query browser land later |

## First login

1. Seed an IAM access key via `STRATA_STATIC_CREDENTIALS` (Phase 1 only —
   Phase 2 introduces a Cassandra-backed credentials store and an admin
   endpoint to create/rotate keys):
   ```bash
   export STRATA_STATIC_CREDENTIALS=alice:s3cret:owner
   ```
2. Set a stable JWT secret so sessions survive restarts. If unset the
   gateway generates an ephemeral 32-byte hex secret on every boot and
   logs a `WARN` — fine for dev, never for prod.
   ```bash
   export STRATA_CONSOLE_JWT_SECRET=$(openssl rand -hex 32)
   ```
3. (Optional) Name the cluster — defaults to `strata`. The name appears
   in the top bar and on the overview hero card.
   ```bash
   export STRATA_CLUSTER_NAME=us-east-prod
   ```
4. (Optional) Wire Prometheus so the top widgets and metrics dashboard
   populate. Without this they degrade to "Metrics unavailable".
   ```bash
   export STRATA_PROMETHEUS_URL=http://prom.internal:9090
   ```
5. Start the gateway and open the console:
   ```bash
   make run-memory
   open http://localhost:9999/console/
   ```
6. Log in with the seeded access key and secret. The session cookie is
   `HttpOnly` + `SameSite=Strict` + `Path=/admin`, valid 24 h. Logging
   out clears the cookie.

## Required env vars

| Variable | Default | Purpose |
|---|---|---|
| `STRATA_CONSOLE_JWT_SECRET` | random hex (warns on stdout) | HS256 secret for session JWTs. Set explicitly so restarts don't invalidate sessions. |
| `STRATA_STATIC_CREDENTIALS` | empty | Comma-separated `accesskey:secret[:owner]` entries. Required to log in. |
| `STRATA_CLUSTER_NAME` | `strata` | Cluster label shown in the top bar + overview hero. |
| `STRATA_PROMETHEUS_URL` | empty | Base URL of the Prometheus that scrapes the gateway. Required for the metrics dashboard + top widgets. |
| `STRATA_NODE_ID` | `os.Hostname()` else `strata` | Stable per-replica id written to the heartbeat row. |
| `STRATA_REGION` | `strata-local` | Single-region label echoed onto every bucket row. |

## Architecture

```
                     ┌─────────────────────────────────────┐
                     │            Browser                  │
                     │  /console/ static SPA (React+Vite)  │
                     └──────────────┬──────────────────────┘
                                    │ XHR  /admin/v1/*
                                    │ Cookie: strata_session=<JWT>
                                    ▼
   ┌──────────────────────────────────────────────────────────────┐
   │                Strata gateway (single binary)                │
   │                                                              │
   │  ┌─────────────┐   ┌────────────────────┐   ┌─────────────┐  │
   │  │ S3 API      │   │ /console/  go:embed │   │ /admin/v1/* │  │
   │  │ (catch-all) │   │ static SPA bundle   │   │ JSON API    │  │
   │  └──────┬──────┘   └─────────────────────┘   └──────┬──────┘  │
   │         │                                           │         │
   │         └───────────────┬───────────────────────────┘         │
   │                         ▼                                     │
   │                ┌────────────────┐  ┌────────────────────┐     │
   │                │  meta.Store    │  │ Prometheus client  │     │
   │                │ (memory /      │  │ (PromQL → metrics  │     │
   │                │  cassandra)    │  │  dashboard + top   │     │
   │                └────────────────┘  │  widgets)          │     │
   │                                    └────────────────────┘     │
   └──────────────────────────────────────────────────────────────┘
```

`/console/*` serves the embedded React+Vite bundle (with SPA fallback to
`index.html` so deep links survive refresh). `/admin/v1/*` is the JSON
API that powers the SPA — full schema lives in
`internal/adminapi/openapi.yaml`. Both prefixes register ahead of the
catch-all S3 router so they never collide with bucket names; `console`,
`admin`, and `metrics` are reserved bucket names in
`internal/s3api/validate.go`.

## Pages (Phase 1)

| Route | Page | What it shows |
|---|---|---|
| `/console/login` | Login | Access key + secret → session cookie |
| `/console/` | Cluster Overview | Status hero, nodes table, top buckets + top consumers widgets |
| `/console/buckets` | Buckets | Sortable, searchable, paginated list (50/page) |
| `/console/buckets/<name>` | Bucket Detail | Read-only object browser with folder navigation |
| `/console/metrics` | Metrics | 2×2 dashboard: request rate, latency p50/p95/p99, error rate, bytes |
| `/console/consumers` | Consumers | (Phase 2 placeholder) |
| `/console/settings` | Settings | (Phase 2 placeholder) |

## Phase capability matrix

| Capability | Phase 1 | Phase 2 | Phase 3 |
|---|---|---|---|
| Read-only browse (cluster, buckets, objects, metrics) | ✓ | ✓ | ✓ |
| Login / logout / session cookie | ✓ | ✓ | ✓ |
| Bucket admin (create / delete / versioning toggle) | — | ✓ | ✓ |
| Object actions (download / delete / presign) | — | ✓ | ✓ |
| IAM (create / rotate access keys) | — | ✓ | ✓ |
| Multipart watchdog | — | ✓ | ✓ |
| Audit viewer | — | ✓ | ✓ |
| Hot-key heatmaps | — | — | ✓ |
| Slow-query browser | — | — | ✓ |
| OpenTelemetry trace UI | — | — | ✓ |
| SSE / WebSocket live tail | — | — | ✓ |

## Capability Matrix

Per-surface admin coverage. Phase 2 ships every Phase 2 column tick below;
Phase 3 layers debug tooling on top without removing Phase 2 surface.

| Surface | Phase 1 | Phase 2 | Phase 3 |
|---|---|---|---|
| CreateBucket | — | ✓ | ✓ |
| DeleteBucket | — | ✓ | ✓ |
| Lifecycle | — | ✓ | ✓ |
| CORS | — | ✓ | ✓ |
| Policy | — | ✓ | ✓ |
| ACL | — | ✓ | ✓ |
| Inventory | — | ✓ | ✓ |
| Logging | — | ✓ | ✓ |
| IAM Users | — | ✓ | ✓ |
| AccessKeys | — | ✓ | ✓ |
| ManagedPolicies | — | ✓ | ✓ |
| UploadObject | — | ✓ | ✓ |
| DeleteObject | — | ✓ | ✓ |
| ObjectTags | — | ✓ | ✓ |
| ObjectRetention | — | ✓ | ✓ |
| LegalHold | — | ✓ | ✓ |
| MultipartWatchdog | — | ✓ | ✓ |
| AuditLog | — | ✓ | ✓ |
| Settings | — | ✓ | ✓ |
| BackendPresign | — | ✓ | ✓ |
| AuditTail | — | — | ✓ |
| SlowQueries | — | — | ✓ |
| OTelTraceBrowser | — | — | ✓ |
| HotBuckets | — | — | ✓ |
| HotShards | — | — | ✓ |
| NodeDrilldown | — | — | ✓ |
| ShardDistribution | — | — | ✓ |
| ReplicationLag | — | — | ✓ |
| StorageStatus | — | — | ✓ |
| MultiReplicaCluster | — | — | ✓ |
| ClustersSubsection (Storage page) | — | — | ✓ |
| PlacementTab (BucketDetail) | — | — | ✓ |
| DrainBanner (AppShell) | — | — | ✓ |
| RebalanceProgressChip (cluster card) | — | — | ✓ |
| Pools matrix (per-cluster × per-pool) | — | — | ✓ |
| BucketReferencesDrawer (cluster card) | — | — | ✓ |
| DrainProgressBar (cluster card) | — | — | ✓ |
| DeregisterReadyChip (cluster card) | — | — | ✓ |
| StuckBucketsDrawer (cluster card, drain transparency) | — | — | ✓ |
| BulkPlacementFixDialog (drain transparency) | — | — | ✓ |
| PolicyDrainWarningChip (BucketDetail Placement tab) | — | — | ✓ |
| PendingClusterCard variant (cluster-weights US-003) | — | — | ✓ |
| ActivateClusterModal — typed-confirm + initial-weight slider | — | — | ✓ |
| LiveClusterWeightSlider — inline debounced PUT | — | — | ✓ |
| weight=0 chip on live cluster card | — | — | ✓ |

## Operational notes

- Bundle size budget is ≤500 KiB gzipped initial. Heavy routes (Metrics
  dashboard with recharts) are lazy-loaded via `React.lazy` so they only
  pay their cost when the operator visits them.
- Polling is 5 s by default; the metrics dashboard scales to 30 s for
  24 h windows and 5 min for 7 d.
- Heartbeats: every gateway replica writes a row to `cluster_nodes`
  every 10 s; rows expire at TTL 30 s. The overview's "X of Y nodes
  healthy" line reads this table. Workers (`strata-gc`,
  `strata-lifecycle`) do not yet write heartbeats — Phase 2 wires them
  up so the workers + leader chips populate.
- The metrics counter today is labelled `{method, code}` only. Top
  buckets / top consumers query labels `{bucket}` / `{access_key}`
  which are not yet emitted; the widgets render
  `metrics_available=true` with empty rows ("no traffic in the last
  24 h"). Phase 2 adds the labels.
- Object-lock state on the bucket detail page is hard-coded `false`
  because `meta.Bucket` has no ObjectLock column today. Phase 2 lifts
  it.

## End-to-end tests

Three Playwright specs run in CI under the `e2e-ui` job:

- `web/e2e/critical-path.spec.ts` — Phase 1 read-only flows
  (login → overview → buckets list → bucket detail → logout).
- `web/e2e/admin.spec.ts` — Phase 2 admin flows (US-022): bucket-lifecycle
  (create → upload 5 MB → delete object → delete bucket), iam-keys
  (create user → mint key → disable → delete key → delete user),
  lifecycle-rule (add 30-day expiration → save → reload → assert),
  policy-editor (PublicRead template → validate → save → reload → assert),
  multipart-watchdog (initiate via fetch → list → bulk-abort → assert empty).
- `web/e2e/debug.spec.ts` — Phase 3 debug flows (US-015): audit-tail
  (PUT object → row appears in live tail), slow-queries (min_ms=0 → recent
  rows render), trace-browser (paste request-id → spans render),
  hot-buckets-empty (Prom unset → MetricsUnavailable card renders),
  hot-shards-s3 (`empty:true` mocked → s3-explainer card renders).
- `web/e2e/storage.spec.ts` — Storage status cycle (US-006):
  storage-page-renders (login → /storage → Meta + Data tabs visible),
  cluster-hero-shows-storage-card (login → home → "Storage" hero card
  visible with at least one class chip), degraded-banner-on-warn
  (`/admin/v1/storage/health` spoofed via `page.route` → banner appears
  above shell, dismiss button hides it for the rest of the context).
  Operator guide for the underlying endpoints + warning meanings is at
  [Storage status]({{< ref "/architecture/storage" >}}).
- `web/e2e/drain-lifecycle.spec.ts` — Drain lifecycle cycle (US-007
  drain-lifecycle): login → /storage → click "Show affected buckets"
  on a cluster card → assert drawer enumerates the bucket-references
  list → close drawer → Drain via the typed-confirmation modal's
  readonly mode picker (US-004 rewrote the modal in
  drain-transparency) → assert card flips to draining +
  `<DrainProgressBar>` renders "chunks remaining" → spoof
  `chunks_on_cluster=0` on the next poll → assert green "Ready to
  deregister" chip → create a bucket, set policy to `{cephb: 100}`,
  save → assert `policy-drain-warning` testid renders on the Placement
  tab. Operator runbook for the underlying endpoints is at
  [Placement + rebalance — Drain lifecycle]({{< ref "/best-practices/placement-rebalance#drain-lifecycle" >}}).
- `web/e2e/drain-transparency.spec.ts` — Drain transparency cycle
  (US-008): three Playwright scenarios spoofing the new 4-state
  machine + impact analysis + bulk-fix flow.
  **Scenario A** drives the readonly mode picker — operator picks
  `Stop new writes (maintenance)`, types the cluster id, submits, and
  asserts the cluster card flips to `draining_readonly` with the
  `<DrainProgressBar>` orange stop-writes chip.
  **Scenario B** drives the evacuate path — flip the radio to
  `Full evacuate (decommission)`, assert categorized counters render
  via `cd-impact`, assert the amber `cd-stuck-warning` blocks submit
  even with typed-confirm matching, click `cd-bulk-fix` → assert the
  `<BulkPlacementFixDialog>` mounts, click `bpf-apply` → assert the
  modal refetches /drain-impact (stuck=0, submit enables) → submit →
  state flips to `evacuating` with the green deregister-ready chip on
  the next poll once spoofed chunks reach zero.
  **Scenario C** drives the upgrade path — start from
  `draining_readonly`, click the `dp-upgrade` button on the progress
  bar → modal opens with title `Upgrade to evacuate` and the readonly
  radio hidden → submit → state flips to evacuating.
  All admin endpoints (`/admin/v1/clusters`,
  `.../drain|undrain|drain-progress|drain-impact|bucket-references|
  rebalance-progress`, `/admin/v1/storage/data`,
  `/admin/v1/buckets/{name}/placement`) are spoofed via
  `page.route()` so the spec runs against the same memory-mode gateway
  as the other e2e jobs. Operator runbook is at [Placement + rebalance
  — Drain lifecycle]({{< ref "/best-practices/placement-rebalance#drain-lifecycle" >}}).
- `web/e2e/cluster-weights.spec.ts` — Cluster-weights cycle (US-005):
  three Playwright scenarios spoofing the 5-state machine + weight
  field. **Scenario A** drives the pending → live flow: pending badge
  visible, Activate CTA opens `<ActivateClusterModal>`, slider/numeric
  pair set to 25, typed-confirm arms Submit, card flips to live with
  `<LiveClusterWeightSlider>` mounted at value=25 + Drain CTA back.
  **Scenario B** drives the live-card inline slider: rapid drags
  coalesce to a single 500 ms debounced PUT, weight=0 path renders the
  muted "no default-routed writes" chip. **Scenario C** drives the 4xx
  revert path: armed-409 PUT reverts the slider to the last
  server-accepted value. All admin endpoints (`/admin/v1/clusters`,
  `.../activate`, `.../weight`, `.../drain-progress`,
  `.../rebalance-progress`, `.../bucket-references`,
  `/admin/v1/storage/data`) are spoofed via `page.route()` so the spec
  runs against the same memory-mode gateway as the other e2e jobs.
  Operator runbook is at [Placement + rebalance — Cluster
  lifecycle]({{< ref "/best-practices/placement-rebalance#cluster-lifecycle-register--activate--ramp" >}}).
- `web/e2e/placement.spec.ts` — Placement + cluster surfacing cycle
  (US-006 placement-ui): login → /storage → cluster cards rendered →
  create bucket → Placement tab → drag slider for `cephb` to 100 →
  Save → assert toast → re-navigate to /storage → Drain primary
  cluster via typed-confirmation modal (mistyped id keeps the Drain
  button disabled) → assert state=draining + AppShell banner →
  Undrain → banner gone after refetch → Reset to default on the
  Placement tab → confirmation dialog → DELETE → sliders zeroed.
  `/admin/v1/clusters`, `.../drain|undrain`, `.../rebalance-progress`,
  `/admin/v1/buckets/{name}/placement`, and `/admin/v1/storage/data`
  are spoofed via `page.route()` so the spec runs against the same
  memory-mode gateway as the other e2e jobs. Operator runbook for
  the underlying endpoints is at
  [Placement + rebalance]({{< ref "/best-practices/placement-rebalance" >}}).

Run locally with:

```bash
cd web
pnpm install
pnpm run e2e:install   # one-time chromium download
pnpm run e2e
```

Both specs boot `cmd/strata server` against memory-mode meta + data
(`STRATA_AUTH_MODE=off`, seeded `test:test:owner` credentials) so no
Cassandra / RADOS dependency is required.
