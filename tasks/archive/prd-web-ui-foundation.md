# PRD: Web UI — Foundation (Phase 1 of 3)

> **Status: SHIPPED.** All 12 stories merged into `main` via commit
> `e27cf21` on 2026-05-03. The embedded React+TS console is live at
> `/console/` on the gateway port; `/admin/v1/*` JSON API serves the
> read-only dashboards. `internal/heartbeat` (memory + cassandra) and
> `internal/adminapi` packages exist on `main`. `reservedBucketNames`
> is closed in `internal/s3api/validate.go` (rejects `console`, `admin`,
> `metrics`, `healthz`, `readyz`, `.well-known`). The branch's executed
> snapshot is preserved at
> `scripts/ralph/archive/2026-05-03-web-ui-foundation/`. Phase 2
> (`prd-web-ui-admin.md`) and Phase 3 (`prd-web-ui-debug.md`) build on
> this foundation.

## Introduction

Strata today ships two binaries (`strata`, `strata-admin`) and exposes
operations via S3 API + a standalone Prometheus + Grafana stack. There is
no first-party operator console — Grafana shows metrics but cannot do
bucket-browser, IAM admin, or operator-grade actions; aws-cli works but
the learning curve is high.

This PRD adds **Phase 1 of a three-phase web UI** modelled on the MinIO
Console (embedded in the binary, React+TS, single auth path) and the
CockroachDB DB Console (cluster overview as the home page, node listing,
status snapshot). Phase 1 ships the build system, auth, app layout, and
the read-only operator dashboards: cluster overview, top buckets, top
consumers, metrics charts, buckets list, bucket detail. **No write
operations** (admin actions like CreateBucket / IAM PutUser) — those
land in Phase 2 (`prd-web-ui-admin.md`). Performance debug tooling
(hotspot heatmaps, slow-query tracer, OTel trace browser) lands in Phase
3 (`prd-web-ui-debug.md`).

After Phase 1, an operator opens `https://<gateway>/console` (no port
change, embedded), logs in with their existing IAM access key + secret,
and sees:

1. **Cluster overview** (home page) — cluster status, node count + per-
   node health, build version, uptime. CockroachDB-shape header
2. **Top buckets** widget — sorted by stored bytes and request count,
   click-through to bucket detail
3. **Top consumers** widget — top access keys / IAM users by 24h
   request count and bytes
4. **Cluster metrics** charts — request rate, p50/p95/p99 latency,
   error rate over selectable time window
5. **Buckets list** page — search, sort, basic info per bucket
6. **Bucket detail** page — read-only object browser, stats

Tech: React 18 + Vite + TypeScript + Tailwind + shadcn/ui (radix-ui
under the hood) + TanStack Query (5 s polling) + Playwright e2e. Bundle
embedded into `cmd/strata` via `go:embed` and served from
`/console/*` on the same port as the S3 API. Auth is session-cookie
based (login form against existing IAM credentials store; JWT under
the hood, refresh every 24 h).

This is a P2 entry under `## Operations & observability` in `ROADMAP.md`
(this PRD also creates that section if missing).

## Goals

- New `web/` source tree (React+TS+Vite); `make web-build` produces
  an esbuild-style bundle that `go:embed` pulls into `cmd/strata`
- Single binary deploy — no separate node process; UI served from
  `/console/*` on the gateway's port
- Auth is session cookies derived from existing IAM credentials
  (Cassandra / TiKV `access_keys` table); login form + 24 h JWT
  refresh; same SigV4 path under the hood for `/admin/*` API calls
- New `/admin/v1/*` HTTP endpoints (JSON) that the UI consumes; reuse
  `meta.Store` + Prometheus client in-process; no new storage layer
- CockroachDB-shape **home page**: cluster status + node count +
  per-node health + version + uptime — the first thing an operator
  sees
- Top buckets widget (by stored bytes + 24h request count)
- Top consumers widget (top access keys / IAM users by 24h request
  count + bytes)
- Cluster metrics charts (request rate, p50/p95/p99, error rate)
  over selectable time window (15 min / 1 h / 6 h / 24 h / 7 d)
- Buckets list + bucket detail (read-only object browser)
- shadcn/ui + Tailwind + neutral grayscale + blue primary + red
  destructive design tokens; dark mode toggle (default = follow OS)
- Playwright e2e covers the critical paths (login → cluster overview
  → bucket detail) on every PR
- docs/ui.md operator guide
- ROADMAP P2 entry flips to Done close-flip per CLAUDE.md "Roadmap
  maintenance"

## User Stories

### US-001: Vite + React + TypeScript skeleton + `go:embed`
**Description:** As a developer, I want a minimal React+TS app under
`web/` that compiles to a static bundle and is embedded into `cmd/strata`
via `go:embed`, so subsequent stories can fill in real pages without
re-litigating the build pipeline.

**Acceptance Criteria:**
- [ ] New `web/` source tree (sibling of `cmd/`, `internal/`, `docs/`)
      with: `web/package.json`, `web/vite.config.ts`, `web/tsconfig.json`,
      `web/index.html`, `web/src/main.tsx`, `web/src/App.tsx` (renders
      "Strata Console — coming soon")
- [ ] Pinned versions: react@18.x, react-dom@18.x, vite@5.x,
      typescript@5.x. Lockfile checked in (`web/pnpm-lock.yaml` —
      pnpm preferred over npm/yarn for size + reproducibility)
- [ ] `make web-build` runs `cd web && pnpm install && pnpm run build`
      and produces `web/dist/` (index.html + assets/*)
- [ ] `cmd/strata/console.go` (new file) declares
      `//go:embed all:web/dist`
      `var consoleFS embed.FS` and exposes a handler `consoleHandler()`
      that serves the embedded files at `/console/*` (with SPA fallback
      to `index.html` for client-side routes)
- [ ] `internal/serverapp/serverapp.go` registers the handler under
      `/console/` ahead of the catch-all `/` S3 router so the UI does
      not collide with bucket names
- [ ] **Reserved bucket names — hard requirement** (auditing main on
      2026-05-02 confirmed `validBucketName` accepts `console`, `admin`,
      `health`, `metrics`, etc. as valid bucket names — collision risk):
      add a `reservedBucketNames` set in
      `internal/s3api/validate.go::validBucketName` that rejects
      `{console, admin, health, metrics, readyz, healthz, .well-known}`
      with `400 InvalidBucketName`. Test cases for each reserved name +
      a positive control (`bkt`) preserved
- [ ] `make build` runs `make web-build` first; missing `web/dist`
      fails the Go build cleanly with a helpful error
- [ ] CI workflow `.github/workflows/ci.yml` gains a `web-build` step
      using node@20.x + pnpm@9.x; cached `~/.local/share/pnpm/store`
- [ ] Verify in browser: `make run-memory` boots; `curl
      http://localhost:9000/console/` returns `index.html`; the page
      renders "Strata Console — coming soon" without console errors
- [ ] Typecheck passes (Go + `pnpm run typecheck`)
- [ ] Tests pass

### US-002: Design system — Tailwind + shadcn/ui + dark mode
**Description:** As a developer, I want Tailwind CSS + shadcn/ui (radix
primitives + design tokens) wired up so subsequent stories use a
consistent component vocabulary without per-page styling.

**Acceptance Criteria:**
- [ ] Add Tailwind 3.x + PostCSS + autoprefixer; `web/tailwind.config.ts`
      configures content scan, font stack (system-ui), and CSS variables
      for theme tokens
- [ ] Install shadcn/ui CLI; copy in baseline components: Button, Card,
      Input, Label, Tabs, Dialog, Sheet, DropdownMenu, Table, Select,
      Toast, Skeleton, Badge. Each lives under
      `web/src/components/ui/<name>.tsx`
- [ ] Design tokens in `web/src/styles/globals.css`: neutral grayscale
      foundation, primary = blue (`hsl(220 90% 56%)`), destructive =
      red (`hsl(0 72% 50%)`), success = green, warning = amber.
      Light + dark variants
- [ ] Dark mode toggle in the top bar (dropdown: Light / Dark / System
      default = System); persisted to `localStorage["strata.theme"]`;
      `class="dark"` on `<html>` driven by the toggle
- [ ] Typography scale: `text-xs` 12 / `text-sm` 14 / `text-base` 16 /
      `text-lg` 18 / `text-xl` 20 / `text-2xl` 24 (Tailwind defaults)
- [ ] No custom logo or brand mark in this story (out of scope; keep
      placeholder "Strata Console" wordmark)
- [ ] Verify in browser using Playwright: theme toggle flips
      `<html class="dark">` on/off; tokens render correctly in both
      themes (snapshot screenshot pair)
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: `/admin/v1/*` HTTP API scaffolding (Go side)
**Description:** As a developer, I want a versioned `/admin/v1/*` JSON
HTTP namespace on the gateway, with a stub handler tree that subsequent
stories fill in, so the UI has a stable contract to call.

**Acceptance Criteria:**
- [ ] New package `internal/adminapi/` with `Server` struct holding the
      `meta.Store` + Prometheus client + Logger + auth verifier
- [ ] Routes registered under `/admin/v1/` ahead of the S3 router (same
      prefix-routing pattern as `/console/`):
      - `GET /admin/v1/cluster/status` — stub returns `{"status":
        "ok", "version": "<git sha>", "started_at": <epoch>}`
      - `GET /admin/v1/cluster/nodes` — stub returns `{"nodes": []}`
      - `GET /admin/v1/buckets` — stub returns `{"buckets": []}`
      - `GET /admin/v1/buckets/{bucket}` — stub returns `404`
      - `GET /admin/v1/buckets/{bucket}/objects` — stub returns
        `{"objects": []}`
      - `GET /admin/v1/consumers/top` — stub returns `{"consumers": []}`
      - `GET /admin/v1/metrics/timeseries` — stub returns
        `{"series": []}`
- [ ] All endpoints return JSON; `Content-Type: application/json`
- [ ] All endpoints require authentication via the same SigV4 path the
      S3 API uses OR a session cookie (US-004 wires the cookie path);
      anonymous requests return `401 Unauthorized`
- [ ] OpenAPI 3.1 spec checked in at `internal/adminapi/openapi.yaml`
      describing all endpoints with request/response schemas — used by
      Phase 2/3 for typegen + as a contract document
- [ ] Per-endpoint unit tests under `internal/adminapi/*_test.go`
      asserting status codes + JSON shapes (stub data is fine; real
      data lands in later stories)
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: Auth — login form, session cookies, JWT
**Description:** As an operator, I want to log in to the console with my
existing IAM access key + secret, get a session cookie, and have all
subsequent `/admin/v1/*` calls authenticated without re-prompting until
24 h pass.

**Acceptance Criteria:**
- [ ] New endpoint `POST /admin/v1/auth/login` accepts JSON
      `{"access_key": "...", "secret_key": "..."}`; verifies against
      existing IAM credentials store (`internal/auth.Store`); on
      success returns a `Set-Cookie: strata_session=<jwt>; HttpOnly;
      Secure; SameSite=Strict; Path=/admin; Max-Age=86400`
- [ ] JWT payload: `{sub: <access-key>, iat, exp}`; signed with HS256
      using a per-deployment secret read from `STRATA_CONSOLE_JWT_SECRET`
      (random 32-byte hex; if unset, generated at startup with a WARN
      log line — production deployments MUST set it explicitly to
      survive restarts without invalidating sessions)
- [ ] Auth middleware on `/admin/v1/*` (except `/admin/v1/auth/login`):
      reads `strata_session` cookie, validates JWT, sets
      `auth.AuthInfo{AccessKey: <sub>}` on the request context.
      Falls back to SigV4 path for clients that don't carry a cookie
      (curl scripts, future programmatic admin clients)
- [ ] `POST /admin/v1/auth/logout` clears the cookie
      (`Max-Age=0`); always returns 200
- [ ] `GET /admin/v1/auth/whoami` returns
      `{"access_key": "...", "expires_at": <epoch>}` for the active
      session; unauthenticated returns 401
- [ ] React login page at `/console/login`: form with Access Key + Secret
      Key inputs (Secret Key is `type=password`), submits to
      `/admin/v1/auth/login`. On success: redirect to `/console/`
      (cluster overview). On failure: inline error message
- [ ] React `<RequireAuth>` wrapper redirects unauthenticated routes to
      `/console/login`; uses `whoami` probe on mount
- [ ] React `<UserMenu>` in the top bar shows the access key + a "Sign
      out" item; click triggers `POST /admin/v1/auth/logout` + redirect
      to `/console/login`
- [ ] Verify in browser using Playwright: login with valid creds → 200 +
      cookie set + redirect to `/console/`; invalid creds → inline
      error + still on `/console/login`; logout → cookie cleared,
      redirect to `/console/login`
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: App layout — sidebar + top bar + content area
**Description:** As an operator, I want the standard CockroachDB / MinIO
console layout — left sidebar with primary nav, top bar with cluster
name + user menu + theme toggle + global search, content area for the
active route — so navigation feels native.

**Acceptance Criteria:**
- [ ] Layout component at `web/src/components/layout/AppShell.tsx`:
  - Left sidebar (collapsible to icon-only on viewport <1024 px)
    with primary nav items: **Overview** (home), **Buckets**,
    **Consumers**, **Metrics**, **Settings** (placeholder for Phase 2)
  - Top bar: cluster name (from
    `/admin/v1/cluster/status::cluster_name`), global search input
    (placeholder for Phase 2), theme toggle, user menu (US-004)
  - Content area: routed via `react-router-dom` v6.x `<Outlet>`
- [ ] Active nav item is highlighted; clicking navigates without page
      reload (SPA)
- [ ] Sidebar collapse state persisted to `localStorage["strata.sidebar.collapsed"]`
- [ ] Mobile (viewport <640 px): sidebar becomes a Sheet (slide-out
      drawer) triggered by a hamburger button in the top bar
- [ ] Verify in browser using Playwright: nav between Overview and
      Buckets pages keeps the layout stable; sidebar collapse persists
      across reloads
- [ ] Typecheck passes
- [ ] Tests pass

### US-006: Cluster Overview home page (CockroachDB-shape)
**Description:** As an operator opening the console, I want the home
page to show cluster health at a glance — overall status, node count,
per-node health, build version, uptime — modelled on the CockroachDB
DB Console's overview page.

**Acceptance Criteria:**
- [ ] New endpoint `GET /admin/v1/cluster/status` returns:
      ```json
      {
        "cluster_name": "<from STRATA_CLUSTER_NAME or 'strata'>",
        "version": "<git sha + tag>",
        "started_at": <epoch-ms>,
        "uptime_sec": <int>,
        "status": "healthy" | "degraded" | "unhealthy",
        "node_count": <int>,
        "node_count_healthy": <int>,
        "meta_backend": "<cfg.MetaBackend value>",
        "data_backend": "<cfg.DataBackend value>"
      }
      ```
      `status` derived: healthy if all nodes report `/readyz` 200 in
      last 30 s; degraded if 1 or more failing but quorum holds;
      unhealthy if quorum lost. `meta_backend` / `data_backend` are
      pass-through strings from `internal/config.Config` —
      currently-supported values on main are `meta_backend ∈ {cassandra,
      tikv, memory}` and `data_backend ∈ {rados, memory}`. The
      `prd-s3-over-s3-backend.md` cycle will add `data_backend = "s3"`
      without UI changes (handler does no enum validation, just
      forwards the config value)
- [ ] New endpoint `GET /admin/v1/cluster/nodes` returns:
      ```json
      {
        "nodes": [
          {
            "id": "<hostname or pod name>",
            "address": "<host:port>",
            "version": "<git sha>",
            "started_at": <epoch-ms>,
            "uptime_sec": <int>,
            "status": "healthy" | "unhealthy",
            "workers": ["gc", "lifecycle", ...],
            "leader_for": ["gc-leader", ...]
          }
        ]
      }
      ```
      Source: every Strata replica writes a heartbeat row to
      `cluster_nodes` table every 10 s with TTL 30 s. **Lockstep
      across all three meta backends on main**: memory (in-process map
      with timestamp expiry), cassandra (`CREATE TABLE IF NOT EXISTS`
      via `alterStatements` with `default_time_to_live=30`), tikv
      (encoded under `s/cluster_nodes/<node_id>` with TTL applied via
      a sweeper goroutine — same shape as the existing audit-sweeper
      in `internal/meta/tikv/sweeper.go`). Replica reads the table to
      assemble the response. Leadership info comes from the existing
      `internal/leader` lease tables (one row per worker; both
      cassandra + tikv backends already implement the leader Locker)
- [ ] Cluster Overview page at `/console/`:
  - **Hero card**: cluster status badge (green/amber/red), cluster
    name, version, uptime ("3 days, 12 hours"), node-count summary
    ("4 of 5 nodes healthy")
  - **Nodes table**: id, address, version, uptime, status (badge),
    workers (chips), leader-for (chips). Sortable by status (unhealthy
    first), then id. Click-through to a node-detail panel (Phase 2);
    for now click is a no-op
  - **Backend chips**: meta backend + data backend pills under the
    hero ("Cassandra" + "RADOS")
- [ ] Polling: 5 s via TanStack Query (US-008 sets up the global
      QueryClient if not yet)
- [ ] Empty state: when `nodes` is empty (single-replica dev stack
      without heartbeats yet) the page still renders with the local
      node from `cluster/status` and an info banner "Heartbeat table
      empty — running single-replica or just started"
- [ ] Verify in browser using Playwright: page renders, hero card shows
      status, nodes table populated; manually kill a worker leader and
      verify the leader-for chip migrates within 30 s
- [ ] Typecheck passes
- [ ] Tests pass

### US-007: Top Buckets + Top Consumers widgets on home
**Description:** As an operator, I want the home page to also show the
top 10 buckets (by stored bytes and 24h request count) and top 10
consumers (by 24h request count and bytes) so I see who is active
without leaving the overview.

**Acceptance Criteria:**
- [ ] New endpoint `GET /admin/v1/buckets/top?by=size|requests&limit=10`
      returns `{"buckets": [{"name": "...", "size_bytes": 0,
      "object_count": 0, "request_count_24h": 0}]}`. Source:
      `bucketstats` package (already exists for Prometheus exposition)
      + Prometheus query for 24h request count
- [ ] New endpoint `GET /admin/v1/consumers/top?by=requests|bytes&limit=10`
      returns `{"consumers": [{"access_key": "...", "user": "...",
      "request_count_24h": 0, "bytes_24h": 0}]}`. Source: Prometheus
      `strata_http_request_total{access_key=...}` aggregation over
      24h via PromQL
- [ ] Two widgets on the Cluster Overview page below the nodes table:
  - **Top Buckets** card with a tabbed view (By Size / By Requests),
    each tab a small table (rank, name, value, sparkline of 24h
    activity). Click bucket name → `/console/buckets/<name>`
  - **Top Consumers** card with the same tabbed shape (By Requests /
    By Bytes), each tab a table (rank, access key (truncated 8 chars),
    user, value)
- [ ] Empty state when Prometheus is unreachable: render "—" in value
      cells + a small "Metrics unavailable" warning under the card
      title (do NOT crash the page)
- [ ] Verify in browser using Playwright: widgets render; tab-switch
      works; Prometheus-down case shows the warning
- [ ] Typecheck passes
- [ ] Tests pass

### US-008: TanStack Query setup + 5-second polling
**Description:** As a developer, I want a single TanStack Query
`QueryClient` configured for the whole app with sensible defaults
(5 s stale time, 5 s refetch interval, retry 1) so individual
components don't re-implement polling.

**Acceptance Criteria:**
- [ ] Install `@tanstack/react-query@5.x` +
      `@tanstack/react-query-devtools@5.x` (devtools only in dev mode)
- [ ] `web/src/lib/query.ts` exports a configured `queryClient` with:
      `staleTime: 5_000`, `refetchInterval: 5_000`,
      `refetchOnWindowFocus: true`, `retry: 1`
- [ ] `<QueryClientProvider>` wraps `<App>` in `web/src/main.tsx`
- [ ] `web/src/api/client.ts` provides typed wrappers
      (`fetchClusterStatus()`, `fetchClusterNodes()`,
      `fetchTopBuckets(by)`, `fetchTopConsumers(by)`,
      `fetchBucketsList(query)`, `fetchBucket(name)`,
      `fetchObjects(bucket, prefix, marker)`,
      `fetchMetricsTimeseries(metric, range)`) — each calls
      `/admin/v1/*` and parses the JSON
- [ ] Pages use `useQuery({ queryKey: [...], queryFn: ... })` —
      polling is handled by the global `refetchInterval`
- [ ] Network errors render an inline `<Toast>` "Failed to load <X>"
      with a Retry button; the page does NOT blank-out on intermittent
      network errors
- [ ] Verify in browser using Playwright: Cluster Overview page
      auto-refreshes every 5 s (mock the API to return changing
      `uptime_sec`; assert it ticks)
- [ ] Typecheck passes
- [ ] Tests pass

### US-009: Cluster metrics dashboard
**Description:** As an operator, I want a Metrics page showing request
rate, p50/p95/p99 latency, and error rate over a selectable time
window (15 min / 1 h / 6 h / 24 h / 7 d) so I can answer "is the
cluster healthy right now?" without leaving the console for Grafana.

**Acceptance Criteria:**
- [ ] New endpoint
      `GET /admin/v1/metrics/timeseries?metric=<name>&range=<duration>&step=<duration>`
      returns
      `{"series": [{"name": "...", "points": [[<epoch-ms>, <float>]]}]}`.
      Source: in-process Prometheus client (`promhttp` is already linked
      for the `/metrics` endpoint; for queries we go via PromQL against
      a configured Prometheus URL). Endpoint gracefully degrades to
      "Metrics unavailable" payload when `STRATA_PROMETHEUS_URL` is
      unset
- [ ] Supported metrics in `metric` param:
      `request_rate`, `latency_p50`, `latency_p95`, `latency_p99`,
      `error_rate`, `bytes_in`, `bytes_out`
- [ ] React `/console/metrics` page:
  - Time range selector (segmented control: 15m / 1h / 6h / 24h / 7d)
  - Four charts in a 2×2 grid: Request rate (req/s), Latency
    (p50/p95/p99 layered), Error rate (% of 5xx), Bytes
    (in/out layered). Use `recharts` 2.x — small bundle, declarative
  - Hover tooltips show timestamp + value; legend below each chart
  - Auto-refresh every 5 s for ranges ≤6 h; every 30 s for 24h+;
    every 5 min for 7d (controlled per-query `refetchInterval`)
- [ ] Empty state when Prometheus is unreachable: render skeleton +
      "Metrics unavailable — set STRATA_PROMETHEUS_URL" inline
      message
- [ ] Verify in browser using Playwright: navigate to /console/metrics,
      assert all 4 charts render with axis labels + at least one data
      point; switch range to 24h, assert request fires with
      `range=24h`
- [ ] Typecheck passes
- [ ] Tests pass

### US-010: Buckets list page
**Description:** As an operator, I want a Buckets page listing every
bucket with search, sort, and basic info (name, owner, region, created,
size, object count) so I can quickly find a bucket without aws-cli.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/buckets?query=<substring>&sort=<col>&order=<asc|desc>&page=<n>&page_size=<n>`
      returns
      `{"buckets": [{"name": "...", "owner": "...", "region": "...", "created_at": <epoch>, "size_bytes": 0, "object_count": 0}], "total": <int>}`.
      Source: `meta.Store::ListBuckets` + bucketstats
- [ ] React `/console/buckets` page:
  - Top toolbar: search input (debounce 300 ms), refresh button
  - Table columns: Name (link), Owner, Region, Created, Size, Object
    Count. Click column header to sort. Default sort: created DESC
  - Pagination: 50 rows per page, page numbers + prev/next
  - Empty state: "No buckets" + (Phase 2) "Create your first bucket"
    button (placeholder, not wired)
  - Click bucket name → `/console/buckets/<name>` (US-011)
- [ ] Search filters by case-insensitive substring match on the
      `name` column server-side
- [ ] Verify in browser using Playwright: list paginates, sort flips
      order, search filters
- [ ] Typecheck passes
- [ ] Tests pass

### US-011: Bucket detail page (read-only object browser)
**Description:** As an operator, I want a bucket detail page showing
the bucket's metadata + a read-only object browser (folder navigation,
sort, basic info per object) so I can inspect contents without aws-cli.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/buckets/{bucket}` returns
      `{"name": "...", "owner": "...", "region": "...", "created_at": <epoch>, "versioning": "Enabled" | "Suspended" | "Off", "object_lock": bool, "size_bytes": 0, "object_count": 0}`.
      `404` when bucket missing
- [ ] `GET /admin/v1/buckets/{bucket}/objects?prefix=<>&marker=<>&delimiter=/&page_size=<>`
      returns
      `{"objects": [{"key": "...", "size": 0, "last_modified": <epoch>, "etag": "...", "storage_class": "..."}], "common_prefixes": ["..."], "next_marker": "...", "is_truncated": bool}`.
      Source: gateway's existing `ListObjects` handler reused via
      `meta.Store::ScanObjects` (or `ListObjects` fan-out) — same
      shape as the S3 wire response, JSON-encoded
- [ ] React `/console/buckets/<name>` page:
  - Header: bucket name + status badges (Versioning / Object-Lock)
  - Stats bar: size, object count, region, created date
  - Object browser:
    - Breadcrumb ("/" → "logs/" → "logs/2026/")
    - Filter input (prefix prefix; debounce 300 ms)
    - Table columns: Name (link/folder icon), Size, Last Modified,
      Storage Class, ETag (truncated 8 chars). Click folder ↓
      drills in (updates URL `?prefix=...`); click file → object
      detail panel slide-in (Sheet) showing full metadata. Phase 2
      adds Download / Delete buttons; Phase 1 leaves them disabled
      (greyed out with "Coming soon" tooltip)
  - Pagination: continuation tokens (next page button); 100 rows
    per page
- [ ] Empty bucket: "This bucket is empty" empty state
- [ ] Verify in browser using Playwright: bucket detail loads,
      object list paginates, prefix navigation works, object detail
      panel opens
- [ ] Typecheck passes
- [ ] Tests pass

### US-012: Playwright e2e + critical-path tests + docs/ui.md + ROADMAP
**Description:** As a maintainer, I want Playwright e2e covering the
critical paths (login → cluster overview → buckets list → bucket detail
→ logout) running on every PR, an operator guide in `docs/ui.md`, and
the ROADMAP entry flipped to Done.

**Acceptance Criteria:**
- [ ] Install `@playwright/test@1.x` in `web/`; new
      `web/playwright.config.ts` with: chromium-only, baseURL
      `http://localhost:9000`, `webServer` running `make run-memory`
      (memory backend; no Cassandra/RADOS dependency for e2e)
- [ ] `web/e2e/critical-path.spec.ts` covers:
  1. Anonymous load → redirect to `/console/login`
  2. Login with seed creds (set via
     `STRATA_STATIC_CREDENTIALS=test:test:owner` in webServer env)
     → land on `/console/`
  3. Cluster Overview shows "1 of 1 nodes healthy" hero
  4. Click Buckets nav → list page (empty state initially)
  5. Use the harness API or aws-cli to create a bucket; refresh; bucket
     appears in list
  6. Click bucket → bucket detail loads
  7. Logout → cookie cleared, back on login page
- [ ] CI workflow `.github/workflows/ci.yml` gains an `e2e-ui` job
      after the existing `e2e` job; uploads the Playwright report
      `web/playwright-report/` as `e2e-ui-report` artefact on failure
- [ ] New `docs/ui.md` operator guide:
  - When to use the console vs aws-cli (decision matrix)
  - Required env vars: `STRATA_CONSOLE_JWT_SECRET`,
    `STRATA_CLUSTER_NAME`, `STRATA_PROMETHEUS_URL`
  - First-login flow: seed an IAM access key via aws-cli, then log in
  - Architecture diagram (browser → /console/ static → /admin/v1/ API
    → meta.Store + Prometheus)
  - Phase 1 vs 2 vs 3 capability table
- [ ] CLAUDE.md "Big-picture architecture" diagram updated: gateway
      box now reads `S3 API + /console/ + /admin/v1/`
- [ ] README.md "How to run" section gains a 6th option: open
      `http://localhost:9000/console/` after `make run-memory`
- [ ] Flip the ROADMAP P2 entry to Done close-flip format per
      CLAUDE.md "Roadmap maintenance" rule:
      `~~**P2 — Web UI — Foundation (Phase 1).**~~ — **Done.**
      Embedded React+TS console served at /console/, login + cluster
      overview + nodes + top buckets + top consumers + metrics +
      buckets list + bucket detail. (commit `<sha>` or `(commit pending)`)`
- [ ] Add P3 entries pointing at Phase 2 (`prd-web-ui-admin.md`) and
      Phase 3 (`prd-web-ui-debug.md`) so the follow-ups are tracked
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- FR-1: `web/` is the canonical source tree for the React+TS console;
  `make web-build` produces `web/dist/` which `cmd/strata` embeds via
  `go:embed`. Single-binary deploy preserved
- FR-2: UI served from `/console/*` on the gateway's port; the path
  prefix is reserved (cannot be used as a bucket name)
- FR-3: `/admin/v1/*` JSON HTTP API is the contract between UI and
  gateway; OpenAPI 3.1 spec checked in
- FR-4: Auth is session cookies derived from existing IAM credentials
  + 24 h JWT refresh; SigV4 fallback for programmatic clients. No
  separate user database for the console
- FR-5: Cluster Overview is the home page (`/console/`); shows status,
  node count, per-node health, version, uptime, top buckets, top
  consumers — CockroachDB-shape
- FR-6: Per-node heartbeat written to `cluster_nodes` table every 10 s
  with TTL 30 s; `cluster/nodes` endpoint reads this. Lockstep across
  memory + cassandra + tikv backends
- FR-7: Metrics page reads from Prometheus via PromQL when
  `STRATA_PROMETHEUS_URL` is set; degrades gracefully when unset
- FR-8: Buckets list + bucket detail are read-only in Phase 1; write
  actions ship in Phase 2
- FR-9: Theme is light / dark / system-default; persisted to localStorage
- FR-10: TanStack Query polls every 5 s; manual refresh always available
- FR-11: Playwright e2e covers login → overview → buckets list → bucket
  detail → logout on every PR
- FR-12: ROADMAP "Operations & observability" section seeded by US-012;
  Phase 1 close-flips to Done in the same commit per CLAUDE.md "Roadmap
  maintenance" rule

## Non-Goals

- **No write operations.** Phase 1 is read-only. CreateBucket /
  PutBucketLifecycle / IAM PutUser / etc. land in Phase 2
  (`prd-web-ui-admin.md`)
- **No performance debug tooling.** Hotspot heatmaps, slow-query
  tracer, OTel trace browser are Phase 3 (`prd-web-ui-debug.md`)
- **No multi-cluster awareness.** A single console serves a single
  Strata deployment. Multi-cluster picker / federated views are out
  of scope; deferred to a future P3 if asked
- **No SSO / OIDC integration.** Login is access-key + secret-key
  against the existing IAM store. Keycloak / Okta / Azure AD
  integration is a Phase 2+ item
- **No mobile-native app.** Responsive layout works on tablet (≥768
  px); phone (<640 px) gets a degraded but functional experience.
  No iOS / Android packaging
- **No custom branding / logo.** Wordmark "Strata Console" only;
  no SVG logo, no favicon work in Phase 1
- **No file upload from UI.** Object browser is read-only; uploads
  via aws-cli or future Phase 2 multipart wizard
- **No real-time tail (SSE / WebSocket).** Polling 5 s is sufficient
  for Phase 1; SSE for live audit log lands in Phase 3 with the
  debug tooling
- **No dashboard customisation.** Charts and widgets are fixed in
  Phase 1; user-defined dashboards are out of scope
- **No custom design system.** Tailwind + shadcn/ui defaults only;
  custom tokens live in `globals.css` but no fork of shadcn

## Technical Considerations

### Tech stack pin
- React 18.x (NOT 19.x — ecosystem still catching up at PRD authoring time)
- Vite 5.x
- TypeScript 5.x
- Tailwind 3.x (NOT 4.x alpha)
- shadcn/ui (current as of authoring)
- TanStack Query 5.x
- recharts 2.x (smaller than chart.js + chart.js)
- react-router-dom 6.x
- Playwright 1.x (chromium only on CI)
- pnpm 9.x as the package manager (smaller node_modules vs npm/yarn,
  reproducible install)

### Backend dependencies
- New `internal/adminapi/` package — owned by this PRD's stories
- `internal/auth.Store` — existing, reused for login
- `internal/meta.Store` — existing (memory + cassandra + tikv all
  shipped on main as of 2026-05-02), reused for cluster nodes table +
  bucket list + objects. Lockstep additions to all three backends for
  the `cluster_nodes` heartbeat
- `internal/bucketstats` — existing, reused for size + object-count
- `internal/leader` — existing, reused for leader-for chip on the
  cluster overview nodes table. Both cassandra + tikv backends ship a
  `Locker` impl already
- Prometheus client — existing (`promhttp` already linked); add a
  thin PromQL HTTP client for time-series queries

### Codebase audit (2026-05-02 — main = modern-complete + binary-consolidation + tikv-meta-backend merged)
The PRD was authored against a snapshot that assumed several other
cycles had also landed in main. Re-audit confirms:
- `cmd/strata` + `cmd/strata-admin` (binary-consolidation merged) ✓
- `internal/meta/{memory,cassandra,tikv}` all present (tikv merged) ✓
- `internal/leader` present ✓
- `internal/auth.Store` present ✓
- `internal/bucketstats` present ✓
- `cfg.DataBackend ∈ {memory, rados}` — **does NOT yet include `s3`**
  (`prd-s3-over-s3-backend.md` not merged; UI handler stays
  enum-agnostic so it will accept future values without code changes —
  see US-006 AC clarification)
- `cfg.MetaBackend ∈ {memory, cassandra, tikv}` ✓
- `internal/data/manifest.go::Manifest.BackendRef` field — **does NOT
  yet exist** on main (`prd-s3-over-s3-backend.md` not merged). Phase 1
  bucket-detail object browser does NOT depend on `BackendRef`; it
  reads the same fields the existing S3 ListObjects handler exposes
- `internal/auth/streaming.go::chunkSigner` chain validation — **does
  NOT yet exist** on main (`prd-auth-per-chunk-signature.md` not
  merged). Irrelevant to this PRD; mentioned only for the merge audit
  trail
- `validBucketName` does NOT reserve console / admin / health / etc. —
  **collision risk explicitly fixed in US-001** (see Reserved bucket
  names AC)

### Heartbeat table shape
```sql
-- Cassandra (additive; alterStatements):
CREATE TABLE IF NOT EXISTS cluster_nodes (
    node_id         text PRIMARY KEY,
    address         text,
    version         text,
    started_at      timestamp,
    workers         set<text>,
    leader_for      set<text>,
    last_heartbeat  timestamp
) WITH default_time_to_live = 30;
```
TiKV: same shape encoded under `s/cluster_nodes/<node_id>` with TTL
applied via the existing audit-sweeper-style background goroutine.
Memory: in-process map.

### Embedding policy
- `go:embed all:web/dist` — the `all:` prefix includes dotfiles
  (Vite emits `.vite/`-prefixed assets in some configs). Without
  `all:` the embed silently drops them
- SPA fallback: the embedded handler serves `index.html` for any
  `/console/<path>` not matching a static file (so client-side routes
  like `/console/buckets/foo` work on direct navigation / refresh)

### Performance envelope
- Bundle target: ≤500 KiB gzipped for the initial bundle (route-
  level code-splitting via React.lazy + Suspense for non-critical
  routes); measured via `vite build --report`
- API call budget: cluster overview loads ≤4 endpoints in parallel;
  total time-to-interactive ≤500 ms on a local stack
- Heartbeat write rate: 1 row / 10 s / replica = 360 writes/h/replica;
  Cassandra TTL handles cleanup

### Backwards compatibility
- All new endpoints are versioned `/admin/v1/*`; future-breaking
  changes ship under `/admin/v2/*`
- No changes to existing S3 API surface
- New env vars (`STRATA_CONSOLE_JWT_SECRET`, `STRATA_CLUSTER_NAME`,
  `STRATA_PROMETHEUS_URL`) are optional with sensible defaults
- Heartbeat table is additive (`CREATE TABLE IF NOT EXISTS`); old
  Strata replicas without heartbeat code keep working — the
  `cluster/nodes` endpoint just returns the local node only

### Concurrent reads
- All `/admin/v1/*` endpoints are read-mostly. Polling 5 s × N
  consoles = bounded load; the gateway already handles thousands of
  S3 req/s, +1 console request every 5 s is negligible
- Heartbeat writes use Cassandra `default_time_to_live=30` (no LWT,
  fire-and-forget). Acceptable: a stale heartbeat just shows up in
  the next 5 s poll

## Design Considerations

### Visual references (do not copy verbatim — extract patterns)
- **CockroachDB DB Console** Overview page — cluster status hero,
  nodes table with role chips, version + uptime in the header. Adopt
  the structure
- **MinIO Console** Buckets page — table with search + sort + drilldown,
  read-only object browser with breadcrumb navigation. Adopt the layout
- **Linear / Vercel** — sidebar density, typography scale, dark mode
  defaults. Adopt the polish

### Layout grid
- Sidebar 240 px wide (collapsed: 56 px)
- Top bar 56 px tall
- Content area max-width 1440 px, centered with `mx-auto`
- Card spacing: gap-4 (16 px) between cards, p-6 (24 px) inside

### Color tokens (CSS variables; light mode shown, dark mode mirrors)
- `--background: 0 0% 100%`
- `--foreground: 0 0% 9%`
- `--primary: 220 90% 56%` (blue)
- `--primary-foreground: 0 0% 100%`
- `--destructive: 0 72% 50%` (red)
- `--success: 142 71% 45%` (green)
- `--warning: 38 92% 50%` (amber)
- `--muted: 0 0% 96%`
- `--border: 0 0% 90%`

### Iconography
- `lucide-react` — included with shadcn/ui setup
- Status icons: CheckCircle2 (healthy), AlertTriangle (degraded),
  XCircle (unhealthy)

## Success Metrics

- All 12 stories shipped within one Ralph cycle on
  `ralph/web-ui-foundation`
- Bundle size ≤500 KiB gzipped for initial load
- Time-to-interactive on local stack ≤500 ms
- Playwright critical-path e2e green on every PR
- Operator can complete: log in → see cluster status → drill into a
  bucket → see object list → log out, in ≤30 s without docs
- ROADMAP P2 "Web UI — Foundation" flipped to Done

## Open Questions

(none — all decisions captured in Goals + Non-Goals + answers to
clarifying questions on 2026-05-01)
