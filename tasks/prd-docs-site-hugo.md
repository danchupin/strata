# PRD: Documentation site (Hugo + GitHub Pages)

## Introduction

`docs/` today is a flat collection of operator-facing notes (`ui.md`,
`multi-replica.md`, `storage.md`, `backends/*.md`, `benchmarks/*.md`,
`migrations/*.md`) — no navigation, no cross-linking, no rendered home page. New
contributors and operators can't get a guided tour of "what is Strata, how do I
deploy it, where do I look for X". This cycle ships a Hugo-based static site under
`docs/` with a sectioned tree, builds it via a GitHub Action on every merge to main,
and publishes to GitHub Pages at `https://danchupin.github.io/strata/`.

OpenAPI viewer (Redoc/Swagger UI) for `internal/adminapi/openapi.yaml` is **out of
scope** this cycle — queued as a separate P3 follow-up after the basic site is live.

## Goals

- Hugo + technical-docs theme (e.g. `hugo-book` or `Lotus Docs`) under `docs/site/`,
  with `docs/` markdown reorganised into a sectioned tree.
- Sections: Getting started → Architecture → Operators → Backends → Migrations →
  Benchmarks → Developers.
- Existing `.md` files become content pages with proper frontmatter (title, weight)
  for sidebar ordering.
- `make docs-serve` brings up local Hugo dev server on `:1313` for live preview.
- GitHub Action `.github/workflows/docs.yml` builds the site on every push to main
  and publishes to the `gh-pages` branch.
- README links to the published site.
- Auto-link cross-references between docs pages so the site is genuinely navigable.

## User Stories

### US-001: Hugo scaffolding + theme + first build
**Description:** As a maintainer, I need a Hugo site under `docs/site/` that builds
to static HTML so the publishing pipeline has something to publish.

**Acceptance Criteria:**
- [ ] `docs/site/` contains a Hugo project (`config.toml` or `hugo.toml`,
  `content/`, `themes/`, `static/`, `layouts/` as needed).
- [ ] Theme: `hugo-book` (or equivalent technical-docs theme) added as a Git
  submodule under `docs/site/themes/<theme>/`.
- [ ] Site title: "Strata Documentation". Base URL: `https://danchupin.github.io/strata/`.
- [ ] `cd docs/site && hugo` produces a `public/` directory with rendered HTML.
- [ ] `docs/site/public/` is gitignored.
- [ ] Stub `content/_index.md` with project tagline + link to "Getting started".
- [ ] No regressions: `go vet ./...`, `go test ./...` pass.

### US-002: Reorganise existing `docs/*.md` into Hugo content tree
**Description:** As a reader, I need the existing operator notes laid out in a
sectioned tree so I can navigate them via the sidebar.

**Acceptance Criteria:**
- [ ] `docs/site/content/` gains the section structure:
  - `getting-started/` (new content — index page with Quick start + Architecture
    overview synopsis)
  - `architecture/` (new — distilled from CLAUDE.md "Big-picture architecture")
  - `operators/` — destinations for `docs/storage.md`, `docs/multi-replica.md`,
    `docs/ui.md`
  - `backends/` — destinations for `docs/backends/{tikv,scylla,s3}.md`
  - `migrations/` — destinations for `docs/migrations/{binary-consolidation,
    gc-lifecycle-phase-2}.md`
  - `benchmarks/` — destinations for `docs/benchmarks/{gc-lifecycle,
    meta-backend-comparison}.md`
  - `developers/` (new — placeholder index "How to contribute" with a stub for
    later expansion)
- [ ] Existing files are MOVED (`git mv`) into the tree, not duplicated. The old
  flat layout under `docs/*.md` is gone (paths under `docs/site/content/` are the
  single source).
- [ ] Each moved page gets Hugo frontmatter: `title`, `weight` (controls sidebar
  ordering inside its section), and (optional) `description`.
- [ ] Section index pages (`_index.md`) per section with a one-paragraph summary +
  link list.
- [ ] `hugo` build is clean (no warnings about missing pages).
- [ ] All inter-doc cross-links updated to use Hugo's `{{< ref >}}` shortcode (or
  relative paths under the new layout) so they resolve in the rendered site.
- [ ] No regressions in any code paths that reference docs paths in tooling
  (e.g. CLAUDE.md, ROADMAP.md, code comments). Update those references in this
  story.

### US-003: Landing page + Getting started + Architecture overview
**Description:** As a first-time visitor, I want a project home page that explains
what Strata is and a Getting started flow that lets me bring up a local stack.

**Acceptance Criteria:**
- [ ] `content/_index.md` (the landing page) covers: one-paragraph elevator pitch
  ("S3-compatible object gateway, Cassandra/TiKV metadata, RADOS data, drop-in for
  Ceph RGW"), key features (sharded fan-out, TiKV ordered scan, multi-replica,
  online reshard, lifecycle, …), link to "Getting started", link to "Architecture",
  link to GitHub repo.
- [ ] `content/getting-started/_index.md` covers: prerequisites (Go, Docker,
  RADOS for prod), `make up-all && make wait-cassandra` walkthrough, first
  bucket via `aws s3 mb`, smoke pass via `make smoke`.
- [ ] `content/architecture/_index.md` distills the "Big-picture architecture"
  block from CLAUDE.md into a reader-friendly page (auth/router/meta/data layers,
  worker model, leader-election). Reuse the existing ASCII diagram from CLAUDE.md.
- [ ] All three pages render with no broken links.

### US-004: README → site link + cross-link audit
**Description:** As a GitHub visitor, I should be one click from the rendered
docs.

**Acceptance Criteria:**
- [ ] Top of `README.md` gains a prominent badge or link: "📖 Read the docs:
  https://danchupin.github.io/strata/" (or equivalent).
- [ ] Any existing `[link](docs/foo.md)` references in `README.md` rewritten to
  point at the published site URLs (`https://danchupin.github.io/strata/...`).
- [ ] Any references to `docs/foo.md` in code comments, CLAUDE.md, or ROADMAP.md
  updated to the new content paths under `docs/site/content/...` (canonical
  source) — published-URL form is fine for ROADMAP/README user-facing copy.
- [ ] No regressions: `go vet ./...`, `go test ./...` pass.

### US-005: GitHub Action — build + publish to gh-pages
**Description:** As a maintainer, I want every merge to main to automatically
publish the site so docs never go stale.

**Acceptance Criteria:**
- [ ] New workflow `.github/workflows/docs.yml`:
  - Trigger: push to `main` (paths filter `docs/**`, `.github/workflows/docs.yml`).
  - Job: checkout (with submodules), install Hugo extended (pinned version), run
    `hugo --minify` from `docs/site/`, deploy `docs/site/public/` to `gh-pages`
    branch via `peaceiris/actions-gh-pages` (or `actions/deploy-pages` if using
    Pages-native deploy).
- [ ] The workflow has a manual `workflow_dispatch` trigger so it can be re-run
  on demand.
- [ ] First successful run publishes the site at
  `https://danchupin.github.io/strata/`. Verified by visiting the URL after the
  cycle merges.
- [ ] The action does NOT run on PRs (only main) to avoid leaking pre-merge
  drafts.
- [ ] Repo `gh-pages` branch is configured as the Pages source. (If repo settings
  changes are not scriptable here, document the manual one-time step in
  `docs/site/content/developers/_index.md`.)

### US-006: `make docs-serve` + developer ergonomics
**Description:** As a contributor editing docs, I want a one-command local
preview.

**Acceptance Criteria:**
- [ ] `Makefile` gains `docs-serve` target: `cd docs/site && hugo server -D`.
  Listens on `:1313`. (`-D` shows draft pages so authors can iterate before
  publishing.)
- [ ] `Makefile` gains `docs-build` target: `cd docs/site && hugo --minify`.
  Useful for CI smoke-checking the build outside the GH Action.
- [ ] `make docs-serve` documented in the `## Common commands` table of
  CLAUDE.md.
- [ ] `docs/site/content/developers/_index.md` (or a "Editing docs" subpage)
  explains: where to add new pages, frontmatter conventions, how to preview
  locally, how the GH Action publishes.
- [ ] Hugo binary installation pointer (e.g. `brew install hugo` on macOS) in
  the developers page.

### US-007: ROADMAP close-flip + cycle archive
**Description:** As a maintainer, I need the ROADMAP P2 entry flipped Done with
a pointer to the published site.

**Acceptance Criteria:**
- [ ] `ROADMAP.md` line covering "P2 — Full project documentation site
  (GitHub Pages)" flipped to
  `~~**P2 — Full project documentation site (GitHub Pages).**~~ — **Done.**
  <one-line summary citing Hugo + theme choice, sectioned tree, GH Action
  publish, link to live URL>. (commit \`<pending>\`)`.
- [ ] New ROADMAP entry under `## Documentation`: P3 follow-up "OpenAPI viewer
  embedded in the API reference section" so the deferred bonus from the original
  P2 entry isn't lost.
- [ ] `tasks/prd-docs-site-hugo.md` REMOVED in this commit per CLAUDE.md PRD
  lifecycle rule.
- [ ] No regressions: `go vet ./...`, `go test ./...`, `make smoke` clean.
- [ ] Closing-SHA backfill follow-up commit on main per established convention.

## Functional Requirements

- FR-1: Hugo site lives under `docs/site/`. Content under `docs/site/content/<section>/`.
- FR-2: Theme installed as Git submodule under `docs/site/themes/<theme>/`. (Do not
  vendor theme content.)
- FR-3: Existing `docs/*.md` files are moved (`git mv`) into the new tree, not
  duplicated.
- FR-4: GH Action `docs.yml` builds + publishes on every merge to main, manual
  trigger supported, never runs on PRs.
- FR-5: Site is reachable at `https://danchupin.github.io/strata/` after first
  successful publish.
- FR-6: `make docs-serve` brings up local preview on `:1313`. `make docs-build`
  runs a static build.
- FR-7: README has a prominent link to the published site.
- FR-8: All cross-references between docs pages use Hugo's `{{< ref >}}` (or
  relative paths) and resolve in the rendered site.

## Non-Goals

- **No OpenAPI viewer** in this cycle — queued as a separate P3 follow-up.
- No versioned docs (single "main" version is enough for now).
- No blog / news section.
- No full-text search beyond what the chosen theme provides out of the box.
- No multi-language content.
- No automatic API-docs generation from Go source (godoc.org-style).
- No custom domain — `github.io` URL is fine for this cycle.

## Technical Considerations

- **Theme choice: `hugo-book` (recommended) or `Lotus Docs`.** Both are
  technical-docs first, support nested sections, sidebar nav, light/dark mode,
  Mermaid diagrams. Pick whichever is healthier as of cycle start (check last
  commit date + open issues). `hugo-book` is older and battle-tested.
- **Hugo extended** is required for SCSS-based themes. Pin the version in the
  GH Action so local + CI builds match. Latest stable as of 2026-05 is fine.
- **Submodule vs Hugo modules:** start with a Git submodule (simpler CI). Hugo
  modules add a Go toolchain dependency the docs build doesn't otherwise need.
- **`gh-pages` deployment:** use `peaceiris/actions-gh-pages@v3` (well-supported,
  pins releases). Alternative: GitHub's native `actions/deploy-pages` flow with
  `actions/upload-pages-artifact` — slightly more configuration but no
  third-party action. Pick the simpler `peaceiris` shape.
- **Site title + navigation order** lives in `config.toml` (or `hugo.toml` if
  using the newer format). Sidebar ordering uses `weight` in page frontmatter.
- **CLAUDE.md "Big-picture architecture" duplication:** the architecture page
  on the site distills the same content but is its own canonical source for
  *external* readers; CLAUDE.md remains the *internal* canonical. Cross-link
  both ways: site → "Internal contributors: see CLAUDE.md"; CLAUDE.md → "User
  docs at <site URL>".

## Success Metrics

- Site is reachable at `https://danchupin.github.io/strata/` after the cycle
  merges.
- `hugo` clean build (no warnings about missing pages or broken refs).
- Sidebar navigation surfaces every existing operator note within 2 clicks of
  the home page.
- README link lands on the home page.
- A new contributor can go from "what is Strata?" → "how do I run it locally?"
  in ≤ 3 clicks.

## Open Questions

- Repo Pages settings flip (gh-pages branch as source) requires a one-time
  manual step in the repo settings UI — document this in the migration
  story (US-005) and have the operator (user) flip it after the first action
  publishes the branch.
- Custom domain (`docs.strata.dev` or similar) — out of scope for this cycle,
  but if the user wants one, queue a P3 follow-up.
- Search — `hugo-book` ships a basic JS search. If the corpus grows past ~50
  pages, consider Algolia DocSearch in a follow-up cycle.
