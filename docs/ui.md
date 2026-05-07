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
  [`docs/storage.md`](storage.md).

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
