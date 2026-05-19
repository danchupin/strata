# PRD: Docs + ADR bundle

## Introduction

Pure docs/site cycle. No code, no admin endpoint, no UI. Goal: drain the
ROADMAP "Design notes captured during MVP" backlog into formal ADRs, fill
the placeholder Reference section, and embed the OpenAPI viewer so
operators self-serve admin-API exploration without leaving the docs site.

5 stories. All work lives under `docs/site/content/` + a small Makefile
target for OpenAPI copy. Hugo 0.161+ extended required (already pinned in
`make docs-build`).

Branch: `ralph/docs-adr`. Starts from `main` per the cycle-branch policy.

## Goals

- Migrate every bullet from ROADMAP `## Design notes captured during MVP`
  into the new `docs/site/content/adr/` Hugo section as a properly-
  formatted ADR. ROADMAP section collapses to a single paragraph linking
  out.
- Fill the placeholder `docs/site/content/reference/_index.md` with three
  authored pages: STRATA_* env vars table, Admin API surface, S3 API
  operations table.
- Embed Redoc against the canonical `internal/adminapi/openapi.yaml` so
  operators browse admin endpoints inline (no jq + curl needed for quick
  reads).
- Prove the bundle with `make docs-build` + a manual `make docs-serve`
  click-through; close 4 ROADMAP entries in the same cycle.

## User Journey

Three personas, all touched by the new docs:

- **Operator landing on the docs site for the first time.** Today the
  reference section is a placeholder `_index.md`; operators bounce out
  to the source tree to find an env var or admin endpoint. After the
  cycle: `/reference/env-vars/`, `/reference/admin-api/`,
  `/reference/s3-api/`, `/reference/admin-api-viewer/` are all there
  and cross-linked from the top-level docs nav.
- **Contributor asking "why do we skip RADOS omap?"** Today the answer
  is split across CLAUDE.md + commit messages + ROADMAP bullets. After
  the cycle: `/adr/0001-skip-rados-omap/` is the canonical link to
  send.
- **Operator triaging an admin endpoint at 02:00.** Today they grep
  `internal/adminapi/openapi.yaml` in their editor. After the cycle
  they open `/reference/admin-api-viewer/` and Redoc renders every
  endpoint with try-out.

## User Stories

### US-001: ADR seed — Hugo section + 4 ADRs + ROADMAP migration

**Description:** As a contributor, I want canonical ADR pages capturing
the four foundational design decisions so I can link to them instead of
re-deriving the rationale from commits.

**Acceptance Criteria:**
- [ ] Create Hugo section `docs/site/content/adr/_index.md` with frontmatter
      `title: "Architecture Decision Records"`, `weight: <next-after-best-practices>`,
      intro paragraph explaining what an ADR is (3-5 sentences), and an
      ADR template block (Status / Context / Decision / Consequences).
- [ ] **No nav change required** — the docs site uses the `hugo-book`
      theme with `BookSection = '*'` in `docs/site/hugo.toml`, so the
      new `adr/` content section auto-appears in the sidebar. Verify
      after `make docs-build` that `/adr/` shows up under the
      top-level menu and lists the 4 ADR pages.
- [ ] Author `docs/site/content/adr/0001-skip-rados-omap.md`:
      - **Status**: Accepted (2024-XX — verify earliest related commit via
        `git log --diff-filter=A -- internal/data/rados/`)
      - **Context**: RGW uses RADOS omap for the bucket index; omap
        degrades on buckets > 100M objects (single-PG bottleneck, no
        native sharding). Cite the known-RGW-pain framing.
      - **Decision**: Strata stores metadata in Cassandra/TiKV (sharded
        `objects` table); RADOS holds only chunk data. omap is unused.
      - **Consequences**: Listing scale > 1B objects via Cassandra fan-out
        (64-way default per `STRATA_BUCKET_SHARDS`); two-store
        consistency (manifest CAS + chunk PUT ordering matters); operator
        runs two storage tiers.
- [ ] Author `docs/site/content/adr/0002-islatest-read-time.md`:
      - **Context**: Standard S3 versioning marks one version per key as
        `IsLatest=true`. Naive impl flips a boolean column on every PUT —
        write amplification + Paxos round trip on Cassandra LWT.
      - **Decision**: Don't store `IsLatest` at all. Derive at read time
        from clustering order (`(bucket_id, shard) PARTITION BY ...
        CLUSTERING ORDER BY key ASC, version_id DESC`). First row per key
        in the scan IS the latest.
      - **Consequences**: Zero write amplification on PUT; `ListObjects`
        path eats one extra in-memory pass to dedupe by key; the
        version-id `MaxUint64-ts8-BE` encoding (`internal/meta/tikv/keys.md`)
        is load-bearing — changing it breaks the read-time invariant.
- [ ] Author `docs/site/content/adr/0003-manifest-blob-column.md`:
      - **Context**: `data.Manifest` carries per-object chunk references
        + per-part counts + future SSE / EC / replication metadata.
        Normalising into separate columns would force a Cassandra `ALTER
        TABLE` on every schema-additive change.
      - **Decision**: Single blob column `objects.manifest` carrying
        protobuf (default since US-049) or JSON (legacy reads, transparent
        sniff in `data.DecodeManifest`). Format chosen at write-time via
        `STRATA_MANIFEST_FORMAT` env.
      - **Consequences**: Schema-additive evolution without `ALTER`
        (new field = new proto tag); Cassandra can't filter on manifest
        innards (acceptable — that's the meta-store contract); JSON↔proto
        coexistence handled by `data.DecodeManifest` sniff (`{` → JSON,
        else proto3 wire). Includes pointer to `strata server
        --workers=manifest-rewriter` for one-shot JSON→proto bulk
        conversion.
- [ ] Author `docs/site/content/adr/0004-leader-per-worker.md`:
      - **Context**: Strata ships ~10 background workers (gc, lifecycle,
        replicator, notify, access-log, inventory, audit-export,
        manifest-rewriter, rebalance, usage-rollup). Each requires
        single-leader semantics to avoid double-acks / duplicate
        side-effects.
      - **Decision**: Each worker holds its own `<name>-leader` lease via
        `leader.Session`; no co-located supervisor that owns multiple
        leases. Sharded workers (gc fan-out, rebalance fan-out) hold
        per-shard sub-leases via `SkipLease=true` + internal fan-out.
      - **Consequences**: One worker's lease loss doesn't cascade
        (leader-election decoupling buys fault isolation); operator can
        scale workers per-replica independently via `STRATA_WORKERS=`;
        more Cassandra LWT churn (one lease per worker per replica vs
        one per supervisor).
      - **Reconsideration note**: Link to the `## Consolidation &
        validation` section of ROADMAP. Current shape under review — if
        we collapse to a supervisor model, this ADR transitions to
        `Superseded by ADR-XXXX`.
- [ ] **ROADMAP migration**: the `## Design notes captured during MVP
      and "modern complete"` section currently has **7 bullets** (omap,
      IsLatest, `go-ceph.NewConnWithUser`, runtime image base, JSON
      blob column, leader-per-worker, protobuf decoder-first migration).
      Replace the 4 bullets the new ADRs cover (omap, IsLatest,
      manifest-blob, leader-per-worker) with a single paragraph:
      `Design rationale for foundational decisions is captured in the
      [Architecture Decision Records](docs/adr/) section — see ADR-0001
      (skip RADOS omap), ADR-0002 (IsLatest read-time), ADR-0003
      (manifest blob column), ADR-0004 (leader-per-worker).` Keep the
      remaining **3 bullets** untouched (out of scope this cycle —
      `go-ceph.NewConnWithUser` short ID, runtime image
      `quay.io/ceph/ceph:v19.2.3` base, protobuf decoder-first
      migration).
- [ ] Run `make docs-build` — Hugo generates 4 new ADR pages +
      `/adr/index.html`; zero warnings; exit code 0.
- [ ] Spot-check generated HTML for at least one ADR (`docs/site/public/adr/0001-skip-rados-omap/index.html`)
      — verify the Status / Context / Decision / Consequences heading
      anchors render.
- [ ] No code changes outside `docs/site/content/adr/` + `ROADMAP.md` +
      (if applicable) the top-level nav config.

### US-002: Reference page — STRATA_* env vars table

**Description:** As an operator, I want a single page listing every
`STRATA_*` env var with default, range, and consuming layer so I don't
have to grep the source tree to tune a knob.

**Acceptance Criteria:**
- [ ] Grep the codebase: `grep -rhoE 'STRATA_[A-Z_][A-Z0-9_]+' cmd/strata/ internal/`
      → sorted unique list. Cross-reference with `koanf` / `os.Getenv` /
      `env.Get` call sites to capture default + clamp range per var.
- [ ] Author `docs/site/content/reference/env-vars.md` with frontmatter
      `title: "STRATA_* environment variables"`, `weight: 10` (under
      `/reference/`).
- [ ] Markdown table columns: `Variable | Default | Range | Consuming layer | Notes`.
      Each row links the variable name to its primary doc (e.g.
      `STRATA_GC_GRACE` → `/best-practices/placement-rebalance/#bandwidth-tuning`
      or wherever the runbook treats it). `Consuming layer` is one of
      `gateway | gc worker | lifecycle worker | rebalance worker | notify
      worker | meta backend | data backend | health probe | tracing`.
- [ ] Group rows by consuming layer (h2 per group) so the page scans top-
      to-bottom by operator concern, not alphabetically.
- [ ] Top-of-page note (HTML comment + visible italic line): `Source of
      truth: grep the codebase via "grep -rhoE 'STRATA_[A-Z_][A-Z0-9_]+'
      cmd/strata/ internal/". Update this page when adding a new env var.`
- [ ] **Rewrite `docs/site/content/reference/_index.md`** — the current
      file is NOT a pure placeholder; it carries a `## Worker env vars
      (selected)` section listing recently-shipped worker env vars + a
      `expansion deferred to a P3 follow-up` paragraph that becomes
      false once this cycle ships. Action:
      (a) **Migrate** the `## Worker env vars (selected)` section
          content into the appropriate row groups of the new
          `env-vars.md` table (rebalance / gc / worker-control env
          vars). Verify the grep-derived list in `env-vars.md` covers
          every var that section enumerates.
      (b) **Remove** the `expansion deferred to a P3 follow-up`
          paragraph + the trailing bullet list pointing at README /
          openapi.yaml / s3-compatibility — superseded by the 4 new
          reference pages.
      (c) **Replace** with a short index intro (1-2 sentences: "This
          section is the operator reference. Four pages:") + a
          bookmarked list of the 4 new reference pages with one-line
          descriptions each. Keep the existing frontmatter (`title`,
          `weight`, `bookFlatSection: true`, `description`).
- [ ] Cover every `STRATA_*` var the grep finds. If a var is intentionally
      undocumented (debug-only, e.g. `STRATA_RADOS_HEALTH_OID`), include
      it with `Notes: internal — debug only`.
- [ ] `make docs-build` green; rendered page resolves all internal links
      (grep generated HTML for `href="/best-practices` + `href="/reference`
      to verify no 404 anchors).

### US-003: Reference page — Admin API surface

**Description:** As an operator, I want a flat page listing every admin
endpoint with its method, path, audit verb, and a one-line summary so I
can find the right one without scrolling 1000-line OpenAPI YAML.

**Acceptance Criteria:**
- [ ] Author `docs/site/content/reference/admin-api.md` with frontmatter
      `title: "Admin API surface"`, `weight: 20`.
- [ ] Parse `internal/adminapi/openapi.yaml` by hand (one-time author —
      the YAML is the source of truth; this page is a derived index).
      For each `paths.*.<method>` entry, render a markdown row.
- [ ] Group sections by feature area (h2 each): Cluster lifecycle (drain
      / activate / weight / list / bucket-references) · Drain & rebalance
      (drain-progress / rebalance-config / rebalance-bandwidth /
      gc-config) · Placement (bucket placement / mode) · Buckets (CRUD)
      · IAM (users / keys / policies) · Diagnostics (trace / metrics).
      Cross-reference grouping with the existing `internal/adminapi/`
      file layout for naming consistency.
- [ ] Per row: `Method | Path | Audit verb | Summary | Full schema`. The
      `Full schema` column links to the OpenAPI viewer page (US-005)
      with the operation-id anchor (Redoc auto-generates
      `#operation/<operationId>` anchors — verify by running US-005
      first OR document the anchor pattern in this page even if US-005
      is built second).
- [ ] Top-of-page note: `This page is the operator index. Authoritative
      contract lives in internal/adminapi/openapi.yaml; rendered viewer
      at /reference/admin-api-viewer/.`
- [ ] All audit verbs match the `s3api.SetAuditOverride(ctx, action, ...)`
      stamps in the handler files — spot-check 5 random entries against
      grep `SetAuditOverride` in `internal/adminapi/`.
- [ ] `make docs-build` green; rendered page resolves the cross-link to
      US-005's viewer page (run US-005 before this story, or land both
      then re-verify).

### US-004: Reference page — S3 API operations table

**Description:** As a developer wiring a client or debugging compat
issues, I want a flat table of every supported S3 operation with the
handler location and any AWS-divergence notes so I can cross-reference
expected behaviour to source.

**Acceptance Criteria:**
- [ ] Author `docs/site/content/reference/s3-api.md` with frontmatter
      `title: "S3 API operations"`, `weight: 30`.
- [ ] Source the operation list by reading the `handleBucket` +
      `handleObject` switch arms in `internal/s3api/server.go` (also
      check `handleService` if present). Each `case "<query>":` /
      route arm becomes one row.
- [ ] Markdown table columns: `Operation | Handler file:line | Shipped in
      | AWS gotchas`. `Operation` is the AWS canonical name (PutObject,
      GetObject, ListObjectsV2, CreateBucket, …); `Handler file:line`
      points to `internal/s3api/<file>.go:LINE`; `Shipped in` references
      the ROADMAP / PRD that landed it (best-effort — leave blank for
      pre-roadmap operations); `AWS gotchas` is empty for full-compat,
      one-line note for divergences (e.g. `ListObjectsV2`:
      `start-after handling matches AWS`; `Range: bytes=-N where
      N > size`: `returns full body — matches AWS`).
- [ ] Group by service area (h2 per group): Bucket lifecycle · Object
      lifecycle · Object metadata · Multipart upload · Versioning ·
      ACL / Policy · Encryption · Replication · Tagging · Object Lock ·
      Inventory · Notification.
- [ ] Hand-curated this cycle (option 3A from scoping). Add a `<!--
      Maintainer note: this table is hand-maintained. When adding a new
      handler in internal/s3api/, append a row here in the same PR. A
      lint test (docs_lint_test.go) is parked as a P3 ROADMAP entry. -->`
      HTML comment at top of file.
- [ ] **NEW P3 entry** added to ROADMAP in US-005 (per CLAUDE.md
      "Discovering a new gap" rule): "Drift-proof S3 ops reference
      table via Go lint test parsing internal/s3api/server.go switch
      arms." Parked, NOT closed in this cycle (option 3A explicitly
      defers the lint test).
- [ ] Spot-check 10 random rows: handler file:line points to the actual
      switch arm; AWS gotcha (if any) matches a known
      `internal/s3api/conditional.go` or PRD comment.
- [ ] `make docs-build` green.

### US-005: OpenAPI viewer (Redoc) + smoke + ROADMAP close-flip + PRD removal

**Description:** As an operator, I want an inline OpenAPI viewer so I can
browse + interact with admin endpoints without leaving the docs site.
Plus the standard cycle-end verification: smoke pass, ROADMAP close-flip,
PRD removal.

**Acceptance Criteria:**
- [ ] **Makefile target** for OpenAPI copy: add `docs-openapi-copy`
      recipe that copies `internal/adminapi/openapi.yaml` to
      `docs/site/static/openapi.yaml`. Wire as a `docs-build`
      prerequisite so `make docs-build` always grabs the latest YAML
      before Hugo runs. Document in the new Makefile comment +
      `docs/site/CONTRIBUTING.md` if it exists (else top of `make
      docs-build` recipe).
- [ ] Author `docs/site/content/reference/admin-api-viewer.md` with
      frontmatter `title: "Admin API viewer"`, `weight: 40`, layout
      hint that disables theme chrome if needed (Redoc usually wants
      full-width).
- [ ] Body renders Redoc via raw HTML in a Hugo `{{< rawhtml >}}` block
      (or `unsafe: true` if not already set in `config.toml` / `hugo.toml`
      — verify and document). Loads `/openapi.yaml` (relative — the
      Makefile-copied file lives at `static/openapi.yaml` so Hugo
      serves it at root).
- [ ] Redoc bundle: pin the version (e.g. `redoc-cli@2.x` script tag from
      a CDN URL `https://cdn.jsdelivr.net/npm/redoc@2.5.0/bundles/redoc.standalone.js`)
      — never use `@latest` (drift + supply-chain).
- [ ] Add a noscript fallback paragraph: "JavaScript required for the
      interactive viewer. Static contract: [openapi.yaml](/openapi.yaml).
      Operator index: [Admin API surface](/reference/admin-api/)."
- [ ] **Smoke pass**:
      - `make docs-build` → exit 0; no Hugo warnings.
      - `grep -r 'href="/reference' docs/site/public/reference/` →
        every link resolves (corresponding `index.html` exists).
      - `grep -r 'href="/adr' docs/site/public/adr/` → same.
      - `make docs-serve` → manually open `http://127.0.0.1:1313/adr/`,
        `http://127.0.0.1:1313/reference/env-vars/`, `/reference/admin-api/`,
        `/reference/s3-api/`, `/reference/admin-api-viewer/`. Capture
        a one-line confirmation in `scripts/ralph/progress.txt` per page.
- [ ] **ROADMAP close-flip** in the same commit (4 entries):
      - `Architecture decision records` (P3 Developer experience) →
        flipped to Done. Summary: 4 ADRs authored (omap / IsLatest /
        manifest-blob / leader-per-worker) under `docs/site/content/adr/`;
        ROADMAP `## Design notes captured during MVP` section collapsed
        to a single paragraph linking out.
      - `OpenAPI viewer (Redoc / Swagger UI) embedded in the API
        reference section` (P3 Developer experience) → flipped to Done.
        Summary: Redoc 2.5.0 pinned, OpenAPI YAML auto-copied via
        Makefile prerequisite, viewer at `/reference/admin-api-viewer/`.
      - `Reference section expansion (env vars table, Admin API surface,
        S3 API operations table)` (P3 Developer experience) → flipped to
        Done. Summary: 3 pages authored (env-vars / admin-api / s3-api),
        hand-curated this cycle, S3 ops drift-proofing parked as new P3.
      - The 4 ROADMAP `Design notes` bullets removed/collapsed counts as
        part of the ADR close-flip (1 ROADMAP edit covers both).
- [ ] **NEW P3 entry** added (per CLAUDE.md "Discovering a new gap"
      rule, from US-004): "Drift-proof S3 ops reference table via Go
      lint test parsing internal/s3api/server.go switch arms." Parked
      open under `## Developer experience`.
- [ ] Each close-flip carries `(commit pending)` per the established
      convention; SHA backfill lands on `main` post-merge as fast-follow.
- [ ] `tasks/prd-docs-adr.md` REMOVED via `git rm` per CLAUDE.md PRD
      lifecycle rule.
- [ ] `scripts/ralph/progress.txt` carries one US-005 block summarising
      smoke results + the manual page click-through observations.
- [ ] `make docs-build` final pass: green.

## Functional Requirements

- FR-1: New Hugo section `docs/site/content/adr/` MUST exist with
  `_index.md` + 4 ADR pages numbered 0001-0004.
- FR-2: Each ADR MUST follow the Status / Context / Decision /
  Consequences structure.
- FR-3: ROADMAP `## Design notes captured during MVP` section MUST
  collapse from 4 bullets (the omap / IsLatest / manifest-blob /
  leader-per-worker ones) to a single paragraph linking to
  `/docs/adr/`. Other bullets in that section stay.
- FR-4: `docs/site/content/reference/` MUST contain 4 authored pages:
  `env-vars.md`, `admin-api.md`, `s3-api.md`, `admin-api-viewer.md`.
- FR-5: `_index.md` MUST list the 4 reference pages instead of being
  a placeholder.
- FR-6: Makefile MUST copy `internal/adminapi/openapi.yaml` →
  `docs/site/static/openapi.yaml` as a `docs-build` prerequisite.
- FR-7: Redoc bundle MUST be pinned to a specific version (no
  `@latest`).
- FR-8: `make docs-build` MUST succeed and emit zero warnings against
  the new pages.
- FR-9: All internal cross-links in the new pages MUST resolve in the
  Hugo-generated HTML (no 404).
- FR-10: STRATA_* env vars table MUST cover every var found via
  `grep -rhoE 'STRATA_[A-Z_][A-Z0-9_]+' cmd/strata/ internal/`.
- FR-11: ROADMAP MUST flip 3 P3 entries (ADR / OpenAPI viewer /
  Reference section) and add 1 NEW P3 entry (drift-proof S3 ops table)
  in the US-005 commit.

## Non-Goals

- No code changes outside `Makefile` (OpenAPI copy recipe) — this is a
  pure docs cycle.
- No new admin endpoint; no UI work in `web/`.
- No CI-side drift lint for the S3 ops table (parked as new P3 per
  US-004).
- No ADRs beyond the 4 sourced from existing ROADMAP `Design notes`
  bullets — TiKV-default-lab / protobuf-decoder-first-migration /
  go-ceph-NewConnWithUser / runtime-image-base ADRs are out of scope
  for this cycle (they remain in ROADMAP if not already removed).
- No Swagger UI alternative (Redoc chosen per scoping; revisit if
  operators request try-out execution beyond Redoc's interactive
  rendering).
- No CDN-vs-self-hosted Redoc bundle decision — use the pinned
  jsdelivr URL; self-host as a future P3 if CDN-block hardens.
- No ADR review process / governance doc (parked — see Open Questions).

## Design Considerations

- **ADR template**: lightweight 4-section format (Status / Context /
  Decision / Consequences). 1-2 KB per ADR is fine; longer if the
  decision genuinely needs the airtime. Resist "we considered X, Y, Z"
  multi-page sprawl — link to alternatives in the Consequences section
  if needed.
- **ADR numbering**: zero-padded 4-digit (`0001`, `0002`). Future
  ADRs continue the sequence. Filename pattern:
  `<NNNN>-<kebab-slug>.md` matches the title slug.
- **Reference page weights**: env-vars (10) → admin-api (20) →
  s3-api (30) → admin-api-viewer (40) so the page order matches the
  typical operator lookup flow (knob → admin entry → S3 surface →
  interactive).
- **Redoc page width**: Hugo theme may constrain `<main>` to a max
  width. The viewer page needs full-width; check if the theme exposes
  `full_width: true` frontmatter or a custom layout. Document the
  choice inline.

## Technical Considerations

- **Hugo extended required** (already pinned in `make docs-build`).
  Verify `which hugo` → version ≥ 0.161 with extended SCSS.
- **`unsafe: true` for raw HTML in markdown** — Hugo's markdown
  renderer (Goldmark) blocks `<script>` by default. The Redoc viewer
  needs raw HTML; set `markup.goldmark.renderer.unsafe = true` in
  `config.toml` / `hugo.toml`. If already set, skip; document the
  pre-existing setting in US-005.
- **Theme submodule**: `docs/site/themes/` is a git submodule per
  `CLAUDE.md`. Don't modify theme files; do all customisation in
  `docs/site/content/` + `config.toml`.
- **OpenAPI copy via Makefile** is rebuild-safe — `static/openapi.yaml`
  gets overwritten every `make docs-build`. Add to `.gitignore` so the
  copy isn't committed; the source of truth is
  `internal/adminapi/openapi.yaml`.

## Success Metrics

- 4 ADR pages live at `/docs/adr/0001..0004` post-merge.
- 4 reference pages live at `/docs/reference/{env-vars, admin-api,
  s3-api, admin-api-viewer}/` post-merge.
- ROADMAP `## Design notes captured during MVP` section shrinks by 4
  bullets (replaced with one link-out paragraph).
- 3 ROADMAP P3 entries close in one cycle (+ 1 new-and-parked from
  US-004).
- `make docs-build` continues to take ≤ 5 s on the existing 63-page
  site (Hugo is fast — 8 new pages should not push past 10 s).

## Open Questions

- ADR review process: should new ADRs require a docs-tagged PR with
  reviewer rotation, or is "anyone can land an ADR" enough? Default:
  do nothing this cycle — revisit if ADR sprawl becomes a problem.
- Self-host Redoc bundle: jsdelivr CDN is the easy default, but a
  restricted-network deploy can't reach it. Parked — if operators
  surface this, vendor the bundle into `docs/site/static/vendor/redoc/`.
- ADR template versioning: the template embedded in
  `/adr/_index.md` is the canonical reference. If we evolve it (add a
  "Related ADRs" section, etc.) old ADRs stay on the original — no
  retro-rewriting. Document this norm in `_index.md`.
