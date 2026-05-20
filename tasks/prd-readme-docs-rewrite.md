# PRD: README + user-facing docs rewrite

## Introduction

Strata reaches alpha — but README and docs read as engineer-oriented
implementation memos, not as a user-facing landing page + manual. README
embeds Cassandra / gocql / LWT internals on the first scroll;
`docs/site/content/` is a mix of architecture deep-dives, migration
notes, and reference tables with no obvious "where do I start"
on-ramp.

This cycle rewrites the README + every user-facing docs section in the
style of cockroachdb (concise README, progressive-disclosure docs,
persona-driven navigation, diagrams for complex flows, impl details
behind an Architecture section). Existing `/architecture/`,
`/developers/`, `/adr/` sections stay as deep-dive buckets so an
operator who needs the inner machinery still finds it; their pages
get a sanitization pass (component names + interface signatures
allowed, arbitrary `file:line` banned).

**Pre-launch product** per [Pre-launch no deploys] memory — README
MUST carry an explicit alpha banner. No `LICENSE` file exists today —
US-001 adds Apache 2.0 alongside the README claim.

Branch: `ralph/readme-docs-rewrite`. Starts from `main`. **12 stories**
after PRD review found 11 fact issues including missing LICENSE,
existing Operate-overlapping pages already in `/best-practices/`, an
existing `architecture/workers.md`, and underestimated page count.

## Goals

- README ≤ 120 lines, cockroachdb-style: tagline + alpha banner +
  Apache 2.0 license alongside actual `LICENSE` file + key-features
  bullet (5-7) + features matrix vs RGW + status & maturity section
  (link to ROADMAP for shipped/open work) + two quickstarts (memory
  + TiKV stack) + link to docs.
- Add `LICENSE` file (Apache 2.0) at repo root — currently missing.
- Every `docs/site/content/` user-facing section (`get-started`,
  `concepts` NEW, `best-practices`, `deploy`, `operate` NEW,
  `reference`, `s3-compatibility`) rewritten user-first: no
  `file:line` references, no `gocql` / `LWT` / `goceph` jargon
  unexplained, no "see code at internal/...".
- `/operate/` section built by **physical move** of 4 existing
  `/best-practices/` pages (`backup-restore`, `capacity-planning`,
  `monitoring`, `sizing`) — NOT by creating duplicates. Two new
  pages added: `drain-cluster.md`, `scaling.md`. Best Practices
  stays as the tuning bucket (placement, GC, tracing, web-ui,
  s3-multi-cluster, quotas-billing, gc-lifecycle-tuning + new
  compliance/billing).
- `/architecture/` section retained as deep-dive bucket. Section
  landing rewritten with mermaid overview diagram. 4 NEW
  mermaid-bearing sub-pages added (`put-flow`, `multi-cluster`,
  `drain-pipeline`, `worker-leader-election` — last one renamed
  from `workers` to avoid collision with existing
  `architecture/workers.md`). 11 existing sub-pages audited:
  arbitrary `internal/<path>.go:LINE` references replaced with
  component names + interface signatures (`s3api.Server`,
  `meta.Store`, etc.); narrative mentions of arbitrary lines
  banned, but mentioning a package + exported type stays allowed.
- `/developers/` + `/adr/` left untouched (contributor-facing).
- Mermaid diagrams use fenced ` ```mermaid ` code blocks (hugo-book
  theme has `_shortcodes/mermaid.html` + `static/mermaid.min.js`
  built-in — no `hugo.toml` change needed; verify by writing one
  diagram first).
- Top-of-docs landing (`content/_index.md`) uses the theme's
  existing `{{< card href="..." >}}content{{< /card >}}` shortcode
  for a 6-8 card grid.
- Navigation: progressive-disclosure sidebar — Get Started 10 →
  Concepts 15 → Deploy 20 → Operate 25 → Best Practices 30 →
  Reference 40 → S3 Compatibility 50 → Architecture 60 →
  Developers 70 → ADR 80. Section weights TODAY are mis-ordered
  for the new flow (architecture is 30, best-practices 40,
  reference 60, s3-compatibility 50, ADR 45) — US-009 reshuffles.

## User Journey

Four personas covered by the rewrite:

- **Developer evaluating Strata for the first time.** Lands on
  README → sees alpha banner + Apache 2.0 license + clear value
  prop in 30 seconds → one-command quickstart → first `aws s3 cp`
  works.
- **Operator deploying to Kubernetes.** Goes to docs site →
  Deploy section → Helm install + values reference + monitoring
  setup. No detour into Cassandra LWT internals.
- **Operator running a drain on a degraded cluster.** Goes to
  docs site → Operate section → "Drain a cluster" page. Step-by-
  step instructions, diagrams of expected state transitions,
  "what to expect at each phase". Architecture link for readers
  who want to know why the GC grace exists.
- **Engineer curious about Strata internals.** Lands on docs →
  Architecture section → mermaid component overview → links to
  specific deep-dive pages (placement / drain / manifest blob /
  worker model / TiKV vs Cassandra).

## User Stories

### US-001: README rewrite + LICENSE add (Apache 2.0)

**Description:** As a developer evaluating Strata, I want a
concise README + an actual LICENSE file so I know what Strata is,
that it's alpha, what license it ships under, and how to spin it
up — without diving into Cassandra internals on the first scroll.

**Acceptance Criteria:**
- [ ] **NEW**: Add `LICENSE` file at repo root with Apache 2.0
      text (canonical text from
      https://www.apache.org/licenses/LICENSE-2.0.txt — copyright
      holder `Strata authors`, year `2024-`).
- [ ] `README.md` rewritten to ≤ 120 lines.
- [ ] **Section order** (top → bottom):
      (a) Title `# Strata` + CI badge + license badge
          (`![License](https://img.shields.io/badge/license-Apache--2.0-blue)`).
      (b) **Alpha banner** — prominent visual block
          (`> ⚠️ **Alpha software.** Pre-launch; no production
          deploys. APIs and schemas may change without notice.`)
          right under the title.
      (c) One-line tagline: "S3-compatible object gateway —
          drop-in replacement for Ceph RGW. Metadata in Cassandra
          or TiKV; data in RADOS, S3, or memory."
      (d) **What is Strata** — 3-5 sentences explaining the value
          prop without internals.
      (e) **Key features** — 5-7 bullet list, each one line in
          user-language: "Drop-in S3 compatibility", "Horizontally
          scalable metadata (Cassandra or TiKV)", "Multi-cluster
          RADOS / S3 / memory backends", "Per-bucket placement
          policies + safe drain + rebalance", "Object Lock +
          Lifecycle + SSE + Versioning", "Embedded operator
          console + admin API", "Open source (Apache 2.0)".
      (f) **Features vs RGW** — markdown table with 6-8 rows
          comparing on: bucket index scaling, online resharding,
          multi-cluster routing, drain ops, observability shape,
          admin API shape, deployment shape, license.
      (g) **Status & maturity** — short paragraph: "Strata is in
          alpha — pre-launch, no production deploys yet. Shipped
          features below; open work tracked in
          [ROADMAP.md](ROADMAP.md)." + 5-line "Shipped" /
          "In flight" / "Parked" summary (3 lines each). Phase
          status table from current README REMOVED entirely.
      (h) **Quickstart** — TWO compact blocks: "Memory backends
          (fastest)" `make run-memory` + "Full TiKV stack
          (Docker)" `make dev`.
      (i) **Documentation** — link to docs site.
      (j) **License** — one line: "Apache 2.0 — see
          [LICENSE](LICENSE)."
- [ ] **Removed**: phase status implementation table; gocql / LWT /
      goceph jargon; metadata-plane / data-plane internals
      paragraph; arbitrary `file:line` references; "Phase X"
      mentions.
- [ ] **Cockroachdb-style** prose: short paragraphs, scannable
      bullets, no embedded code reading.
- [ ] `make docs-build` green (README itself doesn't go through
      Hugo but verify no broken links to docs site URLs).
- [ ] License badge URL verified to render (img.shields.io is the
      canonical badge service).

### US-002: Concepts section NEW

**Description:** As a new user reading the docs, I want a
"Concepts" section that explains what Strata is, the S3 surface it
speaks, and how multi-cluster / placement / drain work in user-
language, with an architecture-overview mermaid diagram.

**Acceptance Criteria:**
- [ ] New section `docs/site/content/concepts/_index.md`
      (`weight: 15`, `bookFlatSection: true`,
      `description: "Strata concepts — S3 surface, multi-cluster
      routing, drain & rebalance, worker model."`).
- [ ] Pages:
      (a) `_index.md` — 3-5 sentences "What Strata is" + one
          architecture overview mermaid diagram (S3 client →
          Gateway → Metadata [Cassandra/TiKV] + Data
          [RADOS/S3/memory] + Workers).
      (b) `s3-surface.md` — what S3 surface Strata speaks (link to
          `/s3-compatibility/`). 1-2 paragraphs per major op
          family (bucket, object, multipart, versioning, ACL,
          lifecycle, encryption, replication).
      (c) `multi-cluster.md` — multi-cluster in Strata: per-bucket
          placement policies, weighted default routing, EC
          support. User-facing; impl details parked in
          Architecture.
      (d) `drain-rebalance.md` — drain lifecycle (live → draining
          → evacuating → removed) in plain English. What happens
          to chunks. ETA computation. Operator actions. Mermaid
          state-transition diagram.
      (e) `workers.md` — what workers Strata runs (GC, lifecycle,
          rebalance, replicator, notify, access-log, inventory,
          audit-export, manifest-rewriter, usage-rollup). One
          paragraph each. No leader-election internals — that's
          Architecture.
- [ ] **NO `file:line` references**, NO `Cassandra LWT` jargon,
      NO `goceph` mentions.
- [ ] Cross-link forward to `/architecture/` for readers who want
      impl details.
- [ ] Mermaid first-write sanity check: render via `make
      docs-serve` and confirm SVG appears (not raw fenced block).
- [ ] `make docs-build` green; all internal links resolve.

### US-003: Get Started section rewrite

**Description:** As a new user, I want a clear install +
first-bucket walkthrough so I can verify Strata works in
< 5 minutes.

**Acceptance Criteria:**
- [ ] Rewrite `docs/site/content/get-started/_index.md` to
      follow cockroachdb's "Start a local cluster" pattern.
- [ ] Sections:
      (a) **Prerequisites** — Docker (for stack) or Go 1.23+ (for
          source builds). Macos lima note.
      (b) **Install** — `git clone` + `make dev` (one command).
          Existing "One-command dev" section from `ralph/dx-lab`
          US-001 stays.
      (c) **Verify** — `curl http://localhost:9999/healthz` →
          200; open operator console at
          `http://localhost:9999/console/` (URL verified —
          `internal/serverapp/serverapp.go:254` mounts
          `strataconsole.ConsoleHandler()` at `/console/`).
      (d) **First bucket + first object** — `aws s3 mb` + `aws s3
          cp` against the lab. Include the `--endpoint-url` flag.
      (e) **What's next** — links to Concepts, Deploy, Operate.
- [ ] Pure user-language: NO `bucket_stats` / `manifest CAS` /
      `LWT` references.
- [ ] `make docs-build` green.

### US-004: Deploy section rewrite

**Description:** As an operator deploying Strata, I want a
straight-line guide for each deploy shape (Docker, Kubernetes,
single-node prod) without diving into compose YAML internals.

**Acceptance Criteria:**
- [ ] Rewrite `docs/site/content/deploy/_index.md` + all
      sub-pages (`docker-compose.md`, `kubernetes.md`,
      `multi-replica.md`, `single-node.md`) user-first.
- [ ] **Each sub-page MUST follow the same template**:
      Prerequisites → Install → Configure (top env vars with
      one-line explanation each) → Verify → Monitor → Troubleshoot.
- [ ] Helm install path documented (from `ralph/dx-lab` US-003).
- [ ] **Reference impl details by component name, not file:line**:
      "the gateway accepts S3 traffic on port 9999" (good); "see
      `internal/s3api/server.go:109`" (bad — refactor out).
- [ ] Cross-link to `/operate/` for ongoing ops + to
      `/reference/env-vars/` for full env knob table.
- [ ] `make docs-build` green.

### US-005: Operate section — PHYSICAL MOVE + new workflows

**Description:** As an operator running Strata in production-ish
mode, I want an Operate section covering drain, GC, monitoring,
scaling, backup. Built by physically MOVING the 4 day-2 ops pages
out of `/best-practices/` (not duplicating them) and adding 2 new
workflow pages.

**Acceptance Criteria:**
- [ ] New section `docs/site/content/operate/_index.md`
      (`weight: 25`, `bookFlatSection: true`,
      `description: "Day-2 ops workflows — drain a cluster,
      monitor, scale, back up."`).
- [ ] **Physical MOVE** (use `git mv` to preserve history) from
      `/best-practices/` → `/operate/`:
      - `backup-restore.md` → `/operate/backup-restore.md`
      - `monitoring.md` → `/operate/monitoring.md`
      - `sizing.md` → `/operate/scaling.md` (rename — "Scaling"
        reads better in Operate context).
      - `capacity-planning.md` → `/operate/capacity-planning.md`
- [ ] **Each moved page**: refresh frontmatter (`weight`,
      `description`); replace remaining arbitrary `file:line` /
      `internal/...` references with component names; verify
      cross-links inside the moved page still resolve (relative
      links may break after move).
- [ ] **NEW page** `/operate/drain-cluster.md`: step-by-step drain
      workflow. POST `/admin/v1/clusters/{id}/drain` → watch
      progress → wait for deregister-ready → remove from env.
      Reference the live ETA + bandwidth chips from
      `ralph/drain-rebalance-transparency`. Mermaid diagram of
      state transitions.
- [ ] **NEW page** `/operate/_index.md`: section landing — card
      grid linking to the 5 sub-pages.
- [ ] **Cross-link audit**: any page in the docs that linked to
      `/best-practices/backup-restore/` / `/best-practices/monitoring/`
      / `/best-practices/sizing/` / `/best-practices/capacity-planning/`
      MUST update to the new `/operate/...` path. Grep for old
      paths after move; zero hits expected.
- [ ] **NO impl details** (LWT, manifest CAS, leader election) —
      those stay in Architecture. Reference "the drain pipeline"
      + link to architecture deep dive.
- [ ] `make docs-build` green.

### US-006: Best Practices core refresh — placement / tracing / web-ui

**Description:** Refresh the 3 highest-traffic tuning pages in
Best Practices to user-language. (Split from original mega-story
to keep Ralph iteration within context window.)

**Acceptance Criteria:**
- [ ] Refresh under `docs/site/content/best-practices/`:
      (a) `placement-rebalance.md` — already strong from prior
          cycles; surgical pass to remove any
          `internal/rebalance/...` / `file:line` cells. Keep the
          operator runbook tone + the Bandwidth tuning section
          shipped by `ralph/drain-rebalance-transparency`.
      (b) `web-ui.md` — refresh.
      (c) `tracing.md` — refresh.
- [ ] **NO arbitrary `file:line` references**, NO
      `internal/...` path mentions. Component names like
      "gateway", "rebalance worker", "lifecycle worker" allowed.
- [ ] `make docs-build` green.

### US-007: Best Practices tail refresh — GC / quotas-billing / multi-cluster / compliance / billing

**Description:** Refresh the remaining 4 Best Practices pages +
add 2 new pages (compliance, billing) from `ralph/storage-
correctness` work that didn't get standalone docs yet.

**Acceptance Criteria:**
- [ ] Refresh:
      (a) `gc-lifecycle-tuning.md` — user-language pass.
      (b) `quotas-billing.md` — user-language pass; reference
          the new `STRATA_USAGE_ROLLUP_SAMPLES_PER_DAY` knob from
          `ralph/storage-correctness` US-002.
      (c) `s3-multi-cluster.md` — user-language pass.
- [ ] **NEW** `compliance.md` — Object Lock COMPLIANCE workflow
      + the new `objectlock:*` audit verbs from
      `ralph/storage-correctness` US-006.
- [ ] **NEW** `billing.md` — byte-seconds trapezoid math + the
      env knob; cross-link to `quotas-billing.md`.
- [ ] `_index.md` updated with a card grid for the remaining
      Best Practices pages (post-move: placement-rebalance,
      tracing, web-ui, gc-lifecycle-tuning, quotas-billing,
      s3-multi-cluster, compliance, billing — total 8).
- [ ] **NO arbitrary `file:line`**, component names only.
- [ ] `make docs-build` green.

### US-008: Architecture section overview + 4 new mermaid pages

**Description:** Rewrite the Architecture section landing with a
component-level overview + a mermaid diagram, and add 4 new
mermaid-bearing pages for the critical flows.

**Acceptance Criteria:**
- [ ] **Mermaid usage** confirmed: fenced ` ```mermaid ` code
      block renders to SVG via hugo-book's built-in
      `_shortcodes/mermaid.html` + `static/mermaid.min.js`. NO
      `hugo.toml` change needed. Verify with the first diagram
      via `make docs-serve`.
- [ ] Rewrite `docs/site/content/architecture/_index.md`:
      5-paragraph component-level overview + mermaid
      architecture diagram. List existing sub-pages
      (`auth`, `backends`, `benchmarks`, `data-backend`,
      `meta-store`, `migrations`, `observability`, `router`,
      `sharding`, `storage`, `workers`) + new 4 in a scannable
      card grid.
- [ ] **4 NEW mermaid-bearing sub-pages**:
      (a) `architecture/put-flow.md` — Object PUT flow mermaid
          sequenceDiagram: client → gateway → SigV4 verify →
          meta (CreateObject CAS) → data (chunk PUT) → meta
          (manifest update).
      (b) `architecture/multi-cluster-routing.md` — flowchart:
          placement policy → cluster weight wheel → data backend
          selector → RADOS/S3/memory.
      (c) `architecture/drain-pipeline.md` — stateDiagram:
          live → draining → evacuating → removed; rebalance
          worker + GC fan-out + deregister-ready gates.
      (d) `architecture/worker-leader-election.md` — flowchart:
          supervisor → per-worker leases + heartbeat chip +
          shard fan-out. **Renamed from "workers" to avoid
          collision with existing `architecture/workers.md`**.
- [ ] **Component names referenced in diagrams MUST match code
      identifiers** so engineers can grep: e.g. `s3api.Server`,
      `meta.Store`, `data.Backend`, `leader.Session`,
      `rebalance.ProgressTracker`.
- [ ] **No arbitrary `file:line`** in prose of these new pages —
      component names + interface signatures only.
- [ ] `make docs-build` green; verify each mermaid SVG renders
      via `make docs-serve`.

### US-009: Architecture existing pages — sanitization audit

**Description:** Audit the 11 existing Architecture sub-pages
(`auth`, `backends`, `benchmarks`, `data-backend`, `meta-store`,
`migrations`, `observability`, `router`, `sharding`, `storage`,
`workers`) — replace arbitrary `file:line` references with
component names + interface signatures. Architecture is the
deep-dive bucket, so component-level refs are allowed; arbitrary
line numbers are not.

**Acceptance Criteria:**
- [ ] For each of the 11 existing sub-pages, grep
      `internal/[^"]*\.go:[0-9]+` and `cmd/[^"]*\.go:[0-9]+`
      patterns. Replace each hit with the equivalent component
      reference:
      - `internal/s3api/server.go:223` → `s3api.Server.handleBucket`
      - `internal/meta/cassandra/store.go:4013` →
        `meta.cassandra.Store.UpdateObjectSSEWrap`
      - etc.
- [ ] Tables / code listings that genuinely benefit from a line
      number (e.g. benchmarks pointing at a specific
      benchmark-table row) MAY keep file:line — case-by-case
      judgment, log decisions in progress.txt.
- [ ] Migrations sub-section (`/architecture/migrations/`) is
      historical and intentionally line-anchored — leave
      untouched.
- [ ] `/architecture/benchmarks/` — only sanitize narrative
      paragraphs, keep raw bench output verbatim.
- [ ] `make docs-build` green.

### US-010: Reference section polish + S3 Compatibility refresh

**Description:** As an operator looking up an env var or admin
endpoint, I want the existing Reference section page titles +
descriptions tweaked to user-language and cross-links audited.

**Acceptance Criteria:**
- [ ] `docs/site/content/reference/`: existing pages
      (`env-vars.md`, `admin-api.md`, `s3-api.md`,
      `admin-api-viewer.md`) — pass through:
      (a) replace "see source at" / arbitrary `file:line` cells
          with component-name references (s3-api.md handler
          column is line-anchored by design — keep).
      (b) add cross-links from each row to the relevant Concept
          / Operate / Best Practices page where applicable.
      (c) refresh `_index.md` with a short intro + scannable
          card grid.
- [ ] `docs/site/content/s3-compatibility/`: refresh `_index.md`
      to a cockroachdb-style "what we support" page — top-level
      summary + link to detailed matrix. The detailed matrix
      stays.
- [ ] `make docs-build` green.

### US-011: Navigation + sidebar weight reshuffle + top-of-docs card grid

**Description:** As a docs reader, I want the sidebar to flow
logically from simple to complex so I can navigate the docs
progressively. Existing weights are mis-ordered for the new flow.

**Acceptance Criteria:**
- [ ] **Sidebar weight reshuffle** — update each section's
      `_index.md` frontmatter `weight:`:
      - get-started: 10 (unchanged)
      - concepts: 15 (NEW from US-002)
      - deploy: 20 (unchanged)
      - operate: 25 (NEW from US-005)
      - best-practices: 30 (was 40, **changed**)
      - reference: 40 (was 60, **changed**)
      - s3-compatibility: 50 (unchanged)
      - architecture: 60 (was 30, **changed**)
      - developers: 70 (unchanged)
      - adr: 80 (was 45, **changed**)
- [ ] Each section `_index.md` has a `description:` frontmatter
      (one sentence) — most already do; verify all 10.
- [ ] **Cross-link audit**: every section landing page links
      forward to the natural next section + backward to the
      parent. Reader can navigate the docs in order.
- [ ] **Top-of-docs landing** (`docs/site/content/_index.md`):
      cockroachdb-style 6-8 card grid using the theme's existing
      `{{< card href="..." >}}content{{< /card >}}` shortcode
      (verified at
      `docs/site/themes/hugo-book/layouts/_shortcodes/card.html`).
      One card per major section (Get Started, Concepts, Deploy,
      Operate, Best Practices, Reference, S3 Compatibility,
      Architecture) with a one-line description inside each.
- [ ] `make docs-build` green; all internal links resolve.

### US-012: Smoke validation + ROADMAP new-and-closed + PRD removal

**Description:** As a future-maintainer, I want the docs-rewrite
cycle verified with a docs-build + manual-browse + markdown-lint
grep + a ROADMAP entry recording the rewrite, plus the PRD markdown
removed.

**Acceptance Criteria:**
- [ ] Run `make docs-build` → 0 errors, 0 warnings, all pages
      render. Capture page count delta vs main in progress.txt
      (today ~75 pages; after cycle ~+12 from Concepts(5) +
      Operate(2 new — drain-cluster + scaling rename) +
      Best Practices(2 new — compliance + billing) +
      Architecture(4 new mermaid pages); some moves are
      zero-sum so net delta ~+13).
- [ ] Run `make docs-serve` and manually browse:
      `/` → `/get-started/` → `/concepts/` → `/deploy/` →
      `/operate/` → `/best-practices/` → `/reference/` →
      `/s3-compatibility/` → `/architecture/` → `/developers/`
      → `/adr/`. Each section's `_index.md` renders; sidebar
      ordering matches US-011 spec. Capture one-line
      confirmation per section in progress.txt.
- [ ] **Mermaid sanity check**: open each of the 5 mermaid
      diagrams (1 in concepts/_index, 4 in architecture sub-
      pages) in `make docs-serve` view — they must render to
      SVG inline (not the raw fenced code block).
- [ ] **Markdown-lint grep gate**: run
      ```
      grep -rE 'internal/[a-z]|file:[0-9]+|gocql|goceph|LWT|bucket_stats' \
        docs/site/content/{get-started,concepts,deploy,operate,best-practices,reference,s3-compatibility}/
      ```
      → zero matches in user-facing sections. The patterns
      (`LWT`, `gocql`, `goceph`, `bucket_stats`) MAY appear in
      `/architecture/`, `/developers/`, `/adr/` — those buckets
      are deep-dive / contributor-facing.
- [ ] **LICENSE file verified**: `head -3 LICENSE` shows
      `Apache License` text; `wc -l LICENSE` ≥ 200 lines (full
      Apache 2.0 text).
- [ ] **NEW ROADMAP entry** added under `## Developer experience`
      (per CLAUDE.md "Discovering a new gap" rule — docs rewrite
      was not pre-roadmapped). Title: `User-facing README + docs
      rewrite (cockroachdb-style).` Flipped to Done in same
      commit, references US-001..US-012 + the new Concepts +
      Operate sections + 5 mermaid diagrams + LICENSE add +
      sidebar reshuffle. Carries `(commit pending)` placeholder;
      SHA backfill lands on `main` post-merge as fast-follow.
- [ ] `tasks/prd-readme-docs-rewrite.md` REMOVED via `git rm`.
- [ ] `scripts/ralph/progress.txt` carries one US-012 block
      summarising docs-build + navigation + markdown-lint grep
      result + LICENSE verification.
- [ ] `make vet` + `go test -race ./...` green (no Go code
      touched — vacuous but listed for AC-format consistency).

## Functional Requirements

- FR-1: README ≤ 120 lines with alpha banner, license badge,
  key features bullet, RGW comparison table, status & maturity
  section, two quickstarts, docs link, license line.
- FR-2: `LICENSE` file MUST exist at repo root with canonical
  Apache 2.0 text.
- FR-3: `docs/site/content/concepts/` section MUST exist with
  5 pages (overview, s3-surface, multi-cluster, drain-rebalance,
  workers).
- FR-4: `docs/site/content/operate/` section MUST exist with
  6 pages (overview, backup-restore, capacity-planning,
  monitoring, scaling, drain-cluster). 4 pages moved from
  `/best-practices/` via `git mv`; 2 NEW (drain-cluster +
  rename sizing→scaling).
- FR-5: User-facing pages (Get Started, Concepts, Deploy,
  Operate, Best Practices, Reference, S3 Compatibility) MUST
  NOT contain arbitrary `file:line` references or
  `internal/...` path mentions; jargon like `LWT`, `gocql`,
  `goceph`, `bucket_stats` MUST live only in `/architecture/`
  deep-dive, `/developers/`, `/adr/`.
- FR-6: `/architecture/` section CAN reference component names
  + interface signatures (e.g. `s3api.Server`,
  `meta.Store.SetObjectTags`) — arbitrary `file:line` numbers
  still banned (use component refs); historical migration notes
  + raw bench output exempted.
- FR-7: 5 mermaid diagrams MUST render to SVG (1 in
  concepts/_index, 4 in architecture sub-pages).
- FR-8: Sidebar weight order MUST be Get Started 10 →
  Concepts 15 → Deploy 20 → Operate 25 → Best Practices 30 →
  Reference 40 → S3 Compatibility 50 → Architecture 60 →
  Developers 70 → ADR 80.
- FR-9: Top-of-docs landing (`content/_index.md`) MUST be a
  cockroachdb-style card grid using the existing
  `{{< card href="..." >}}content{{< /card >}}` shortcode.
- FR-10: `architecture/worker-leader-election.md` (US-008 new
  mermaid page) MUST be the rename of the proposed "workers"
  diagram to avoid collision with existing
  `architecture/workers.md`.
- FR-11: ROADMAP MUST gain one new-and-closed entry for the
  docs rewrite in US-012 commit.

## Non-Goals

- No `/architecture/` deep-dive page rewrites beyond the
  sanitization audit (US-009) + 4 new mermaid pages (US-008).
  Existing prose stays.
- No `/developers/` rewrite — contributor-facing.
- No `/adr/` changes — ADRs are append-only.
- No new admin endpoint, no UI work, no Go code changes.
- No translation (English only).
- No CI rewiring for markdown linting (parked — manual grep in
  US-012 sufficient).
- No "what's new" changelog page (parked — `git log` + ROADMAP
  serve that purpose).
- No screenshots of operator console (parked — UI may evolve).

## Design Considerations

- **Cockroachdb README** (style reference): tagline + badges +
  "What is CockroachDB?" paragraph + bulleted features + "Get
  started" with one-block quickstart + license. ~110 lines.
  Strata README mirrors this shape + status & maturity section
  for alpha framing.
- **Cockroachdb docs structure** (style reference): persona-
  driven sidebar — "Get Started", "Develop", "Deploy",
  "Operate", "Reference", "Tutorials". Each section's landing
  is a card grid linking to sub-topics with one-line
  descriptions.
- **Mermaid syntax** uses standard syntax (sequenceDiagram /
  flowchart / stateDiagram-v2 / classDiagram). Hugo-book theme
  ships mermaid.min.js — fenced ` ```mermaid ` code block
  triggers it automatically. No `hugo.toml` change needed.
- **Card shortcode** signature:
  `{{< card href="..." [image="..."] [class="..."] >}}content{{< /card >}}`.
  The shortcode renders `.Inner` as markdown — multi-line
  card descriptions work.
- **No screenshots this cycle** (parked).

## Technical Considerations

- **Hugo-book theme mermaid + card** both ship pre-wired in
  `themes/hugo-book/layouts/_shortcodes/` + `themes/hugo-book/
  static/mermaid.min.js`. No `hugo.toml` change needed; verify
  by writing one mermaid diagram + one card in US-002 / US-011
  respectively before scaling out.
- **`git mv` discipline for US-005**: physical move preserves
  page history. Hugo's relref `{{< ref "..." >}}` resolves by
  filename, so internal cross-references that used the old path
  break — grep for old paths after the move and update them.
- **Section weight reshuffle** is a 10-touch edit; bundle into
  US-011 as a single commit so the sidebar order changes
  atomically.
- **Page count today** (verified) ~75 pages. Net delta after
  cycle: +5 concepts + +2 operate NEW + +2 best-practices NEW +
  +4 architecture NEW = +13 pages. Hugo build time stays < 10s.
- **Existing `/best-practices/placement-rebalance.md`** has
  ~5-10 `internal/...` references from prior cycles — US-006
  must clean these.
- **Operator console** at `/console/` confirmed via
  `internal/serverapp/serverapp.go:254`. URL is `/console/`
  not `/console` (trailing slash matters in some clients).

## Success Metrics

- README ≤ 120 lines (today: 194).
- `LICENSE` file exists (today: missing).
- 0 `internal/[a-z]`, `file:[0-9]+`, `gocql`, `goceph`, `LWT`,
  `bucket_stats` substrings in
  `docs/site/content/{get-started,concepts,deploy,operate,best-practices,reference,s3-compatibility}/`
  (US-012 grep gate).
- 5 mermaid diagrams render to SVG.
- 2 new sections (`concepts/`, `operate/`) live with 5 + 6
  pages.
- `/best-practices/` shrinks to 8 pages (was 11; 4 moved out,
  2 added — net -1).
- `make docs-build` exit 0, 0 warnings.
- 1 ROADMAP entry added-and-closed in this cycle.
- Cycle ships in 12 stories.

## Open Questions

- Apache 2.0 vs another OSI license — confirm with the project
  owner before US-001 commits the LICENSE file. Default Apache
  2.0 if no other signal.
- Top-of-docs card grid layout — 6 cards (Get Started, Concepts,
  Deploy, Operate, Best Practices, Reference) or 8 (add S3
  Compatibility + Architecture)? Default 6 (entry-level
  personas); Architecture + S3 Compatibility surface via
  sidebar.
