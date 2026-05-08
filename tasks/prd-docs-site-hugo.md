# PRD: Documentation site (Hugo + GitHub Pages, CockroachDB-shape)

## Introduction

`docs/` today is a flat collection of operator-facing notes — no navigation, no
landing page, no guided tour. New contributors and operators can't get from
"what is Strata?" to "how do I deploy it on K8s?" without spelunking.

This cycle ships a Hugo-based static site under `docs/site/` whose information
architecture mirrors CockroachDB's documentation
(<https://www.cockroachlabs.com/docs/stable/>): a marketing-quality landing page
that surfaces killer features, a Get Started flow, deployment guides for every
target environment (local, Docker, multi-replica, Kubernetes), Best Practices,
an explicit Supported / Unsupported S3 surface page, and a deep Architecture
section that captures every implementation detail. Built + published via GitHub
Action on every merge to main; live at `https://danchupin.github.io/strata/`.

OpenAPI viewer (Redoc/Swagger UI) for `internal/adminapi/openapi.yaml` is **out
of scope** this cycle — queued as a separate P3 follow-up after the basic site is
live.

## Goals

- **Landing page (home)** with elevator pitch, killer features grid, Quick start /
  Architecture / Reference CTAs — visually competitive with CockroachDB's home.
- **Get Started**: 5-minute quick start (memory backend), prerequisites,
  first-bucket walkthrough.
- **Deploy** section covers every target: single-node lab, Docker Compose,
  multi-replica with TiKV, Kubernetes (raw manifests + Helm-chart-future
  pointer), cloud notes.
- **Best Practices**: sizing, monitoring (Prometheus + Grafana + OTel), GC /
  lifecycle tuning, capacity planning, backup / disaster recovery.
- **Killer features** page surfacing the differentiators vs Ceph RGW: sharded
  fan-out, TiKV ordered scan, online reshard, multi-cluster RADOS, multi-replica
  scaling (Phase 2), Web UI admin/debug, OTel tracing.
- **S3 Compatibility — Supported / Unsupported** page: explicit table of S3
  operations Strata implements + an explicit "NOT SUPPORTED" section so
  operators know up-front what won't work (e.g. Object Lock COMPLIANCE audit
  log, region replication subsets, etc.). Reference s3-tests pass-rate.
- **Architecture deep dive** mirrors CLAUDE.md "Big-picture architecture" and
  goes deeper: layer-by-layer (auth → router → meta → data), worker model,
  leader-election, sharding (objects table, gc fan-out), manifest format
  (proto vs JSON), RADOS chunking, OTel ring buffer, audit log.
- Hugo + theme + GH Action publish pipeline. `make docs-serve` for local
  preview.
- README links prominently to the published site.

## User Stories

### US-001: Hugo scaffolding + theme + Makefile targets
**Description:** As a maintainer, I need a Hugo site under `docs/site/` plus
`make docs-serve` / `make docs-build` so the publishing pipeline has something to
publish and contributors have a one-command preview.

**Acceptance Criteria:**
- [ ] `docs/site/` contains a Hugo project (`hugo.toml`, `content/`, `themes/`,
  `static/`, `layouts/` as needed).
- [ ] Theme: `hugo-book` added as a Git submodule under
  `docs/site/themes/hugo-book/`. (Or another technical-docs theme if hugo-book
  is unhealthy at cycle start — pick by last-commit date + open-issue count.)
- [ ] Site title: "Strata Documentation". Base URL:
  `https://danchupin.github.io/strata/`.
- [ ] `cd docs/site && hugo --minify` produces a `public/` directory with
  rendered HTML.
- [ ] `docs/site/public/` is gitignored.
- [ ] `Makefile` gains `docs-serve` (→ `cd docs/site && hugo server -D` on
  `:1313`) and `docs-build` (→ `cd docs/site && hugo --minify`).
- [ ] `make docs-serve` documented in CLAUDE.md `## Common commands` table.
- [ ] Stub `content/_index.md` with project tagline + link to "Get Started"
  (full landing page content lands in US-003).
- [ ] No regressions: `go vet ./...`, `go test ./...` clean.
- [ ] Typecheck passes.

### US-002: Section skeleton + move existing `docs/*.md` into the tree
**Description:** As a reader, I need the existing operator notes laid out in a
sectioned tree so I can navigate them via the sidebar.

**Acceptance Criteria:**
- [ ] `docs/site/content/` gains the section structure mirroring CockroachDB:
  - `_index.md` (home — stub, populated in US-003)
  - `get-started/_index.md` (stub for US-004)
  - `deploy/_index.md` (stub — section index)
  - `architecture/_index.md` (stub for US-007)
  - `best-practices/_index.md` (stub for US-008)
  - `s3-compatibility/_index.md` (stub for US-009)
  - `reference/_index.md` (stub — references to env vars / API surface; full
    content deferred to a follow-up cycle, this story leaves the placeholder)
  - `developers/_index.md` (stub — editing docs, frontmatter, contributing)
- [ ] Existing files MOVED via `git mv` into the new tree (no duplicates):
  - `docs/storage.md` → `docs/site/content/architecture/storage.md`
  - `docs/multi-replica.md` → `docs/site/content/deploy/multi-replica.md`
  - `docs/ui.md` → `docs/site/content/best-practices/web-ui.md` (or
    `operating/web-ui.md` — pick whichever section reads cleaner)
  - `docs/backends/{tikv,scylla,s3}.md` → `docs/site/content/architecture/backends/`
  - `docs/benchmarks/{gc-lifecycle,meta-backend-comparison}.md` →
    `docs/site/content/architecture/benchmarks/`
  - `docs/migrations/{binary-consolidation,gc-lifecycle-phase-2}.md` →
    `docs/site/content/architecture/migrations/`
  - Per-section `data/` artifact dirs (e.g. `docs/benchmarks/data/`) move
    alongside their parent doc.
- [ ] Each moved page gets Hugo frontmatter (`title`, `weight`, `description`).
- [ ] All inter-doc cross-links updated to use Hugo's `{{< ref >}}` shortcode
  (or relative paths) so they resolve in the rendered site.
- [ ] References to `docs/foo.md` in tooling files (CLAUDE.md, ROADMAP.md, code
  comments) updated to the new content paths under
  `docs/site/content/...` (canonical source). User-facing copy in README/ROADMAP
  may use the published-URL form.
- [ ] `hugo --minify` build is clean (no warnings about missing pages or broken
  refs).
- [ ] Typecheck passes; tests pass.

### US-003: Landing page — pitch + killer features + CTAs
**Description:** As a first-time visitor I want a home page that sells me on
Strata and points me at where to start.

**Acceptance Criteria:**
- [ ] `content/_index.md` (home) covers:
  - **Hero**: one-paragraph elevator pitch ("S3-compatible object gateway,
    Cassandra/TiKV metadata, RADOS data, drop-in for Ceph RGW") + a primary CTA
    button (link to Get Started) + a secondary CTA (link to Architecture).
  - **Killer features grid**: 6–8 cards. Each card = title + 1-line value prop
    + link to the relevant doc page. Coverage: (1) Sharded objects table dodges
    RGW bucket-index ceiling, (2) TiKV ordered scan short-circuits 64-way
    fan-out, (3) Multi-replica scaling via STRATA_GC_SHARDS (Phase 2),
    (4) Online bucket reshard, (5) Multi-cluster RADOS routing, (6) Embedded
    operator console (Web UI), (7) OTel tracing with ring-buffer trace browser,
    (8) Drop-in Ceph RGW replacement.
  - **What Strata is** (and isn't): one short paragraph on positioning vs RGW /
    MinIO / SeaweedFS. Link to "S3 Compatibility — Supported / Unsupported".
  - Footer: link to GitHub repo, ROADMAP.md, CLAUDE.md (for internal devs).
- [ ] Landing page renders cleanly in both light and dark themes.
- [ ] All links resolve (`hugo --minify` clean build).
- [ ] Typecheck passes.

### US-004: Get Started — 5-minute quick start
**Description:** As a developer evaluating Strata I want to bring up a local
gateway and put my first object in under 5 minutes.

**Acceptance Criteria:**
- [ ] `content/get-started/_index.md` covers:
  - **Prerequisites**: Go 1.22+, Docker, `aws` CLI (or `mc`).
  - **Option A — pure-memory (fastest)**: `make run-memory`, then `aws s3 mb
    s3://test --endpoint http://localhost:8080`, `aws s3 cp foo.txt s3://test/`.
    No Docker required.
  - **Option B — Cassandra-backed**: `make up && make wait-cassandra && make
    run-cassandra`. Same client commands.
  - **Option C — full stack (RADOS data)**: `make up-all && make wait-cassandra
    && make wait-ceph`. Ports & service discovery noted.
  - **Smoke pass**: `make smoke` (signed: `make smoke-signed`).
  - **Verify**: `aws s3 ls`, expected output.
  - Pointer to next sections: Deploy → Multi-replica, Architecture.
- [ ] Code blocks copy-paste cleanly (no smart-quote pitfalls).
- [ ] All commands tested against the actual repo (run them end-to-end).
- [ ] Typecheck passes.

### US-005: Deploy — single-node + Docker Compose + multi-replica
**Description:** As an operator I want step-by-step guides for the three
non-Kubernetes deployment shapes Strata officially supports.

**Acceptance Criteria:**
- [ ] `content/deploy/single-node.md`: standalone gateway against memory or
  Cassandra metadata + memory or RADOS data. Sizing rule of thumb. When to
  pick this shape (lab, single-tenant, dev).
- [ ] `content/deploy/docker-compose.md`: distill `deploy/docker/docker-compose.yml`
  for human readers — service map, ports, env vars, volume mounts, the `tracing`
  / `tikv` / `lab-tikv-3` profiles. How to bring up each profile.
- [ ] `content/deploy/multi-replica.md` (already exists from US-002 move; expand
  here): how to run 3 strata replicas behind a load balancer with TiKV metadata,
  STRATA_GC_SHARDS sizing guidance (Phase 2), shared S3-backend or RADOS data,
  leader-election behaviour.
- [ ] Each guide has a "Production checklist" callout: env vars to set,
  metrics to wire, log shipping.
- [ ] All three pages cross-link to the relevant Architecture pages
  (sharding, multi-replica scaling, RADOS chunking).
- [ ] `hugo --minify` clean build.
- [ ] Typecheck passes.

### US-006: Deploy — Kubernetes
**Description:** As an operator running Kubernetes I want a deployment guide and
ready-to-apply manifest examples.

**Acceptance Criteria:**
- [ ] `content/deploy/kubernetes.md` covers:
  - Architecture: 3 strata replicas as a `Deployment`, `Service` (ClusterIP +
    optional LoadBalancer), `ConfigMap` for `app.toml`-equivalent env, `Secret`
    for credentials (S3 root key + Cassandra/TiKV creds + RADOS keyring).
  - **Worked example**: full YAML for a 3-replica strata Deployment + Service +
    ConfigMap + Secret (committed under `deploy/k8s/` so the doc references
    real, applied-tested manifests, not pseudocode). At minimum: namespace,
    Deployment with affinity rules + readiness probe `/readyz` + liveness probe
    `/healthz`, Service, ConfigMap, Secret. PD+TiKV stateful sets are out of
    scope (operators run TiKV-Operator); document the dependency.
  - **Helm chart**: out of scope this cycle. Document the future story —
    "Helm chart packaging is queued as a P3 ROADMAP follow-up; today, apply
    the example manifests under `deploy/k8s/` directly".
  - **Storage**: stateless gateway pods → no PVCs needed for strata; RADOS /
    Cassandra / TiKV are external dependencies. Pointer to Cassandra-Operator,
    TiKV-Operator, and Rook-Ceph for the storage tier.
  - **Scaling**: `kubectl scale deployment strata --replicas=N` with
    STRATA_GC_SHARDS=N env knob.
  - **Ingress**: example `Ingress` manifest with TLS termination + the SigV4
    `Host` header gotcha (vhost routing depends on the original Host header).
- [ ] `deploy/k8s/` committed with apply-tested example manifests referenced from
  the doc.
- [ ] `hugo --minify` clean build.
- [ ] Typecheck passes.

### US-007: Architecture deep dive
**Description:** As a developer / advanced operator I want every implementation
detail in one navigable section so I can reason about behaviour under failure
modes.

**Acceptance Criteria:**
- [ ] `content/architecture/_index.md`: high-level diagram (reuse the ASCII
  block from CLAUDE.md "Big-picture architecture") + one-paragraph summary of
  every layer with links to the per-layer pages below.
- [ ] `content/architecture/auth.md`: SigV4 flow, presigned URLs, streaming
  chunk decoder + chain-HMAC validation, virtual-hosted-style routing, identity
  attribution via context.
- [ ] `content/architecture/router.md`: `internal/s3api/server.go` — bucket-vs-
  object scoped query-string router pattern, vhost rewriting AFTER auth.
- [ ] `content/architecture/meta-store.md`: `meta.Store` interface contract
  (LWT semantics, clustering order, range scans, sharded objects table), backend
  parity (memory / Cassandra / TiKV), `RangeScanStore` short-circuit on TiKV.
  Reuse content from existing backends pages where possible.
- [ ] `content/architecture/data-backend.md`: RADOS 4 MiB chunking, manifest
  format (proto vs JSON sniff, schema-additive evolution rule), multi-cluster
  routing, S3-over-S3 backend.
- [ ] `content/architecture/workers.md`: supervisor model, registration via
  `init()`, leader-election shape, panic restart with backoff, per-worker pages
  for gc / lifecycle / notify / replicator / access-log / inventory / audit-export
  / manifest-rewriter.
- [ ] `content/architecture/sharding.md`: objects-table partitioning
  (`bucket_id, shard`), `cursorHeap` / `versionHeap` fan-out merge, gc fan-out
  (1024 logical shards × runtime shardCount, US-001..US-007 of phase-2 cycle),
  online reshard worker.
- [ ] `content/architecture/observability.md`: structured logs (slog), audit
  log, request_id propagation, OTel tracing (tail-sampler + ring buffer),
  per-storage observers (Cassandra QueryObserver, RADOS ObserveOp).
- [ ] Every page is at least 2 paragraphs of substance + links to source files
  (`internal/...` paths).
- [ ] `hugo --minify` clean build.
- [ ] Typecheck passes.

### US-008: Best Practices — sizing, monitoring, tuning
**Description:** As an operator running Strata in production I want a one-stop
reference for the operational knobs that matter.

**Acceptance Criteria:**
- [ ] `content/best-practices/sizing.md`: CPU / RAM / disk recommendations per
  replica based on object PUTs/s. Anchor to bench numbers from
  `docs/benchmarks/gc-lifecycle.md`. Cassandra / TiKV cluster sizing pointers.
- [ ] `content/best-practices/monitoring.md`: Prometheus scrape config, key
  metrics (`strata_http_requests_total`, `strata_worker_panic_total`,
  `strata_replication_queue_age_seconds`, `strata_cassandra_lwt_conflicts_total`),
  example Grafana dashboards (link to `deploy/grafana/` if it ships any), OTel
  collector wire-up (`OTEL_EXPORTER_OTLP_ENDPOINT`, sample ratio),
  STRATA_OTEL_RINGBUF for in-process trace browser.
- [ ] `content/best-practices/gc-lifecycle-tuning.md`: STRATA_GC_CONCURRENCY /
  STRATA_LIFECYCLE_CONCURRENCY (Phase 1) + STRATA_GC_SHARDS (Phase 2) tuning
  guide with the bench curve. STRATA_GC_DUAL_WRITE cutover playbook (point at
  `docs/migrations/gc-lifecycle-phase-2.md` page in the new tree).
- [ ] `content/best-practices/backup-restore.md`: bucket inventory worker as a
  manifest source, RADOS pool snapshots, Cassandra/TiKV snapshot strategy,
  cross-region replication (replicator worker).
- [ ] `content/best-practices/capacity-planning.md`: chunk fan-out math, lifecycle
  cadence vs storage growth, when to scale shards / replicas, dedup roadmap (P2).
- [ ] `hugo --minify` clean build.
- [ ] Typecheck passes.

### US-009: S3 Compatibility — Supported / Unsupported
**Description:** As an operator considering Strata as an RGW replacement, I need
to know up-front what works and what doesn't.

**Acceptance Criteria:**
- [ ] `content/s3-compatibility/_index.md` covers:
  - **At a glance**: s3-tests pass-rate headline (read from
    `scripts/s3-tests/README.md` baseline).
  - **Supported operations table**: bucket-level (Create / Delete / List /
    Versioning / Lifecycle / CORS / Policy / Tagging / Logging / Inventory /
    Notification / Replication / Object Lock), object-level (PUT / GET / HEAD
    / DELETE / Copy / Multipart / Tagging / Retention / Legal Hold / ACL /
    Storage Class), IAM (basic users + access keys + policy attach/detach).
    Each row: status (✅ supported, ⚠️ partial, ❌ not supported) + link to
    relevant ROADMAP entry or PRD.
  - **Explicitly NOT supported**: distinct subsection so operators can fail
    fast. Source rows from ROADMAP "Known latent bugs" + the open ROADMAP
    items that aren't shipped. Examples: Object Lock COMPLIANCE audit log
    (P3 open), region-replication subsets (depending on roadmap), OAuth /
    SAML SSO for IAM, lifecycle expiration on versioned buckets in some
    edge cases — pull the actual list from ROADMAP at cycle time.
  - **AWS feature parity caveats**: identity provider (Strata IAM is
    self-contained, no IdP federation), cross-account ACLs, Requester Pays,
    Transfer Acceleration, intelligent-tiering — all not supported.
- [ ] Page is operator-actionable: every "not supported" row links to the
  ROADMAP entry tracking it (so future support is discoverable).
- [ ] `hugo --minify` clean build.
- [ ] Typecheck passes.

### US-010: README site link + cross-link audit
**Description:** As a GitHub visitor I should be one click from the rendered
docs.

**Acceptance Criteria:**
- [ ] Top of `README.md` gains a prominent badge or link: "📖 Read the docs:
  https://danchupin.github.io/strata/" (or equivalent).
- [ ] Any existing `[link](docs/foo.md)` references in `README.md` rewritten to
  point at the published site URLs.
- [ ] References to `docs/foo.md` in code comments, CLAUDE.md, ROADMAP.md
  updated to the new content paths under `docs/site/content/...` (canonical
  source). Published-URL form is fine for ROADMAP/README user-facing copy.
- [ ] No regressions: `go vet ./...`, `go test ./...` clean.
- [ ] Typecheck passes.

### US-011: GitHub Action — build + publish to gh-pages
**Description:** As a maintainer I want every merge to main to automatically
publish so docs never go stale.

**Acceptance Criteria:**
- [ ] New workflow `.github/workflows/docs.yml`:
  - Trigger: push to `main` (paths filter `docs/**`,
    `.github/workflows/docs.yml`) + `workflow_dispatch` (manual).
  - Job: `actions/checkout@v4` (with `submodules: recursive`), install Hugo
    extended (pinned version), run `hugo --minify` from `docs/site/`, deploy
    `docs/site/public/` to `gh-pages` branch via
    `peaceiris/actions-gh-pages@v3`.
- [ ] Workflow does NOT run on PRs (only main + manual dispatch) to avoid
  leaking pre-merge drafts.
- [ ] First successful run publishes the site at
  `https://danchupin.github.io/strata/`. Operator confirms the URL responds 200
  after the cycle merges.
- [ ] Repo Pages settings flip (selecting `gh-pages` branch as source) is
  documented as a one-time manual step in
  `docs/site/content/developers/_index.md`.
- [ ] Typecheck passes.

### US-012: ROADMAP close-flip + cycle archive
**Description:** As a maintainer I need the ROADMAP P2 entry flipped Done and
the deferred OpenAPI viewer queued as a fresh P3 entry.

**Acceptance Criteria:**
- [ ] `ROADMAP.md` line covering "P2 — Full project documentation site
  (GitHub Pages)" flipped to
  `~~**P2 — Full project documentation site (GitHub Pages).**~~ — **Done.**
  <one-line summary citing CockroachDB-shape sectioned tree, GH Action publish,
  link to live URL>. (commit \`<pending>\`)`. Closing-SHA backfill follow-up
  commit on main per established convention.
- [ ] New ROADMAP entry under the same Documentation section: P3 follow-up
  "OpenAPI viewer (Redoc/Swagger UI) embedded in the API reference section" so
  the deferred bonus from the original P2 entry isn't lost.
- [ ] New ROADMAP entry under `## Documentation`: P3 "Helm chart packaging for
  Kubernetes deployment" — referenced from `content/deploy/kubernetes.md` as a
  follow-up.
- [ ] New ROADMAP entry under `## Documentation`: P3 "Reference section
  expansion (env vars table, Admin API surface, S3 API operations table)" so
  the placeholder section under `content/reference/_index.md` has a closing
  story queued.
- [ ] `tasks/prd-docs-site-hugo.md` REMOVED in this commit per CLAUDE.md PRD
  lifecycle rule.
- [ ] No regressions: `go vet ./...`, `go test ./...`, `make smoke` all clean.
- [ ] Typecheck passes.

## Functional Requirements

- FR-1: Hugo site lives under `docs/site/`. Content under
  `docs/site/content/<section>/`.
- FR-2: Theme installed as Git submodule under `docs/site/themes/<theme>/`.
- FR-3: Existing `docs/*.md` files MOVED (`git mv`) into the new tree, not
  duplicated.
- FR-4: GH Action `docs.yml` builds + publishes on every merge to main, manual
  trigger supported, never runs on PRs.
- FR-5: Site reachable at `https://danchupin.github.io/strata/` after first
  successful publish.
- FR-6: `make docs-serve` brings up local preview on `:1313`. `make docs-build`
  runs a static build.
- FR-7: README has a prominent link to the published site.
- FR-8: Every cross-reference between docs pages uses Hugo's `{{< ref >}}` or
  relative paths and resolves in the rendered site.
- FR-9: Information architecture mirrors CockroachDB docs: Home → Get Started
  → Deploy → Architecture → Best Practices → S3 Compatibility → Reference →
  Developers.
- FR-10: Landing page surfaces 6–8 killer features in a card grid with one-line
  value props each.
- FR-11: Deploy section includes a Kubernetes guide with apply-tested example
  manifests under `deploy/k8s/`.
- FR-12: S3 Compatibility page includes both a "Supported" matrix and an
  explicit "NOT SUPPORTED" subsection sourced from ROADMAP.
- FR-13: Architecture section has at least one page per layer (auth, router,
  meta-store, data-backend, workers, sharding, observability) plus the existing
  backends/benchmarks/migrations subtrees.

## Non-Goals

- **No OpenAPI viewer** — queued as P3 follow-up.
- **No Helm chart** — manifest examples only; Helm queued as P3 follow-up.
- **No Reference section content** in this cycle — placeholder `_index.md`
  only; full env-vars / Admin API / S3 API tables queued as P3 follow-up.
- **No Migrate-from-RGW playbook** — useful but defer to a follow-up cycle.
- **No Troubleshoot section** — defer to follow-up cycle.
- **No versioned docs** (single "main" version is enough for now).
- **No blog / news section.**
- **No multi-language content.**
- **No automatic API-docs generation from Go source.**
- **No custom domain** — `github.io` URL is fine for this cycle.

## Design Considerations

- **Visual quality bar**: home page should not look like a default Hugo theme
  index. Hero CTA + killer-features card grid require either custom layout
  partials or a theme that ships card components (hugo-book has limited
  hero/landing support; consider using a custom `_index.md` layout under
  `docs/site/layouts/index.html` if the theme home is too plain).
- **Ordering** in the sidebar follows CockroachDB shape: Get Started first,
  then Deploy, then Architecture (advanced), Best Practices, S3 Compatibility,
  Developers last.
- **Killer features cards** colour-code by category: scalability (blue),
  observability (green), drop-in compatibility (orange).

## Technical Considerations

- **Theme**: `hugo-book` is the default recommendation. If the chosen theme
  doesn't render the landing-page hero well, override
  `docs/site/layouts/index.html` with a custom partial — keep the theme
  submodule for the rest of the sidebar / page chrome.
- **Hugo extended** is required for SCSS-based themes. Pin the version in the
  GH Action so local + CI builds match.
- **`gh-pages` deploy** via `peaceiris/actions-gh-pages@v3`.
- **Submodules**: GH Action checkout step needs `submodules: recursive` so the
  theme is present at build time.
- **Repo Pages settings flip** (gh-pages branch as source) is a one-time
  manual UI step. Document it.

## Success Metrics

- Site reachable at `https://danchupin.github.io/strata/` after the cycle merges.
- `hugo --minify` clean build (no warnings about missing pages or broken refs).
- A new contributor can go from "what is Strata?" → "how do I deploy on K8s?"
  in ≤ 4 clicks.
- An operator considering Strata vs RGW can answer "is feature X supported?" by
  opening one page (S3 Compatibility — Supported / Unsupported).
- README link lands on the home page.
- Every layer of the system has its own architecture page that someone debugging
  a production issue can read without reading source.

## Open Questions

- Repo Pages settings flip is a one-time manual UI step — operator needs to flip
  it after the first action publishes the gh-pages branch.
- Custom domain (`docs.strata.dev`) — out of scope for this cycle.
- Search — `hugo-book` ships a basic JS search. If the corpus grows past ~50
  pages, consider Algolia DocSearch in a follow-up cycle.
- The "Killer features" card-grid layout may require a theme override partial.
  If the chosen theme's landing capabilities are too limited, a custom layout
  under `docs/site/layouts/index.html` is the escape hatch.
