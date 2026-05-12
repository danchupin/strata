# PRD: Drain lifecycle — strict PUT refuse + progress + completion signal

## Introduction

The `ralph/placement-rebalance` cycle shipped drain semantics that flips a `cluster_state` row to `state="draining"`, makes `placement.PickClusterExcluding` skip draining clusters when walking the bucket's policy weight wheel, and lets the rebalance worker move chunks off the draining cluster onto target clusters named in each affected bucket's policy. But three operator-facing gaps remain:

1. **Fail-open PUT routing fallback** — when the placement picker returns "" (empty policy OR every policy entry is draining), `RADOS + S3 Backend.PutChunks` fall back to `spec.Cluster` (the class default from `STRATA_RADOS_CLASSES` / `STRATA_S3_CLASSES`) **without consulting the drain map**. Two real scenarios bypass drain: (a) buckets without a Placement policy whose storage class is mapped to a draining cluster via env; (b) buckets whose Placement policy names ONLY draining clusters. Operators expecting "drain = no new chunks land here" need an opt-in strict mode.

2. **No drain-complete signal** — drain is a soft flag. There is no admin endpoint that says "X chunks still on this cluster", no UI progress bar, no log/audit event when chunks-on-cluster reaches zero. Operators currently inspect `docker exec ceph rados -p ... ls | wc -l` or watch `strata_rebalance_chunks_moved_total{from=<cluster>}` rate flatline. Both are indirect.

3. **No safety on deregister** — to deregister a cluster, the operator edits `STRATA_RADOS_CLUSTERS` env and does a rolling restart. There is no warning if the cluster still holds chunks; pulling it from env mid-drain breaks reads on every object whose manifest still references the cluster.

This PRD adds all three: opt-in strict mode (`STRATA_DRAIN_STRICT=on`), a drain-progress endpoint + UI progress bar + completion signal, and a deregister-readiness indicator that gates the operator workflow.

Closes new ROADMAP P3 *Drain strict mode for PUT routing fallback* (added in cycle prep — promotes to lifecycle scope).

## Goals

- New env `STRATA_DRAIN_STRICT` (default `off`); when `on` RADOS + S3 `PutChunks` refuse to fall back to a draining cluster — 503 ServiceUnavailable with `code=DrainRefused` + `Retry-After: 300` header
- Strict mode refuses **only PUT chunks** — GET / HEAD / DELETE / multipart Complete / Abort / List on the drained cluster all keep working (drain semantic is "no new chunks land here", not "cluster invisible")
- New admin endpoint `GET /admin/v1/clusters/{id}/drain-progress` returns `{state, chunks_on_cluster, bytes_on_cluster, last_scan_at, eta_seconds, deregister_ready}` — backed by rebalance worker scan caching
- UI progress bar on each draining cluster card showing "12 GiB · 432 chunks · ~14m ETA"; bar hides when state=live
- Completion detection: rebalance worker logs INFO `"drain complete"` + emits one audit row when `chunks_on_cluster` transitions 0+→0 for a draining cluster; metric `strata_drain_complete_total{cluster}` counter increments
- Deregister-readiness flag: UI surfaces "Cannot remove from env — N chunks still attached" warning when operator hovers over draining cluster card (text guidance only — there is no admin "deregister" endpoint to gate; the env edit happens out-of-band)
- Operator runbook documents when drain is useful (multi-cluster placement deployments) vs not (single-cluster-per-class config) — adds explicit pre-drain checklist

## User Journey Walkthrough

Pre-cycle walkthrough exercise (per feedback memory `feedback_cycle_end_to_end.md`). Operator end-to-end scenario: drain cluster `cephb` from a 2-cluster (default + cephb) RADOS deployment with 3 demo buckets (mix of split / cephb-only / no-policy placement). Every step must have a concrete UI/API/log surface; gaps go in scope.

| # | Operator action | Surface | Backing story |
|---|-----------------|---------|---------------|
| 1 | Open `/console/storage`, see 2 cluster cards live | `<ClustersSubsection>` | (placement-ui, existing) |
| 2 | See bytes/chunks per cluster per pool in Pools table | Pools matrix (6 rows = 2 × 3) | **US-001** |
| 3 | Click "Show affected buckets" link on cephb card | `<BucketReferencesDrawer>` | **US-006** |
| 4 | Identify hot buckets that need policy update before drain | drawer lists buckets with `chunk_count` desc | **US-006** |
| 5 | Optionally flip `STRATA_DRAIN_STRICT=on` in env + restart | docs + cluster-card strict chip | **US-002 + US-004** |
| 6 | Click "Drain" on cephb card | `<ConfirmDrainModal>` | (placement-ui, existing) |
| 7 | Modal shows "<N> buckets reference this cluster — view list" row + typed-confirm input | enhanced modal | **US-006** |
| 8 | Type "cephb" → submit enabled → click Drain | typed confirm | (placement-ui, existing) |
| 9 | Banner appears in AppShell, card flips to draining state | `<PlacementDrainBanner>` | (placement-ui, existing) |
| 10 | Card now shows `<DrainProgressBar>` "12 chunks · 100 KiB · ~5m ETA" | progress bar | **US-003 + US-004** |
| 11 | Watch progress bar shrink as rebalance worker ticks every `STRATA_REBALANCE_INTERVAL` | TanStack poll 30s | **US-004** |
| 12 | Receive operator notification (notify worker) when drain hits 0 | `s3:Drain:Complete` event | **US-005** |
| 13 | Card shows green chip "✓ Ready to deregister" | `deregister_ready=true` | **US-004** |
| 14 | Edit `STRATA_RADOS_CLUSTERS` env, remove cephb entry, rolling restart | docs (out-of-band) | **US-007** docs |
| 15 | Verify cluster gone from console + every former cephb chunk readable on default cluster | Storage page rerender | **US-007** smoke |

Also covered by walkthrough (negative paths):
- Operator drains in strict mode, bucket has policy `{cephb:1}` only → PUT returns 503 DrainRefused with `Retry-After: 300` (**US-002**)
- Operator opens BucketDetail Placement tab for a bucket whose every policy entry is draining → warning chip explains (**US-006**)
- Operator clicks Undrain mid-drain → state flips back to `live`, rebalance worker stops moving, banner disappears, progress cache cleared (existing — verify in **US-007** smoke)
- Operator deploys with `STRATA_DRAIN_STRICT=off` (default) → strict chip absent, every fallback path preserves byte-for-byte fail-open behavior (regression-guarded in **US-002**)

## User Stories

### US-001: Pools matrix — DataHealth iterates `(cluster, pool)` pairs
**Description:** As an operator, I want the Pools table to reflect the actual per-cluster byte distribution (not just the class env routing config) so I can see the true footprint that drives drain decisions.

**Acceptance Criteria:**
- [ ] `internal/data/rados/health.go::DataHealth` rewritten to iterate `(cluster, distinct-pool)` matrix instead of `class → spec.Cluster`: for every registered cluster in `Backend.clusters` and every distinct pool in `Backend.classes`, call `ClusterStatsProbe.GetPoolStats(pool)` against THAT cluster and emit one `PoolStatus{Cluster, Name, Class, BytesUsed, ObjectCount}` row
- [ ] When multiple classes map to the same pool (rare, but allowed by `STRATA_RADOS_CLASSES`), `PoolStatus.Class` is the comma-joined sorted list (matches existing memory + S3 backend convention)
- [ ] S3 backend (`internal/data/s3/health.go`) same shape: for every cluster × distinct bucket, emit one `PoolStatus` row with actual `HeadBucket` / per-cluster size telemetry
- [ ] Memory backend keeps emitting one row with `Cluster=""` — single virtual cluster, no change
- [ ] Total row count = `#clusters × #distinct-pools` (e.g. 2 clusters × 3 pools = 6 rows in the two-cluster lab); was previously `#classes` rows all under default
- [ ] Sort order: ascending by `(Cluster, Name)` — matches existing UI expectation, empty cluster sorts last
- [ ] Unit test in `internal/data/rados/health_test.go` exercises a 2-cluster × 3-class fixture and asserts 6 rows with distinct `Cluster` values + per-cluster `BytesUsed` populated independently
- [ ] Unit test for the comma-joined class label when two classes share one pool
- [ ] OpenAPI spec at `internal/adminapi/openapi.yaml` already documents the schema — no path change, the new matrix shape is just more rows
- [ ] UI Storage page Pools table renders the matrix; the existing Cluster column from `placement-ui` cycle already supports the layout
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill — Pools table shows N×M rows after restart

### US-002: Strict mode env + PUT-only refuse + DrainRefused wire shape
**Description:** As an operator, I want PUT chunks routing fallback to refuse draining clusters when `STRATA_DRAIN_STRICT=on` so I can guarantee no new chunks land on a cluster I'm decommissioning.

**Acceptance Criteria:**
- [ ] New env `STRATA_DRAIN_STRICT` parsed in `internal/data/rados/Config` + `internal/data/s3/Config` (or via shared helper); default `off`; values `on` / `off` / boolean strings accepted; unknown → fail-fast at boot with a clear error
- [ ] When `STRATA_DRAIN_STRICT=on`, `RADOS Backend.PutChunks` (and `S3 Backend.PutChunks` symmetric path) consults the drain map after `placement.PickClusterExcluding` returns ""; if `spec.Cluster` ∈ `draining` → return new sentinel error `data.ErrDrainRefused` with the resolved cluster ID attached
- [ ] Strict refusal triggers ONLY on PUT chunks paths — `GetChunks`, `Delete`, `Head`, multipart `Complete` / `Abort`, `List` ALL continue to work against draining clusters (drain semantic is stop-write, not stop-read)
- [ ] s3api handler maps `data.ErrDrainRefused` to a custom AWS-style error: HTTP `503 ServiceUnavailable`, body `<Error><Code>DrainRefused</Code><Message>cluster <id> is draining and STRATA_DRAIN_STRICT=on refuses fallback</Message><Resource>/bucket/key</Resource></Error>`, response header `Retry-After: 300` (5 minutes, fixed for now — env-tunable is a P3 follow-up)
- [ ] Metric `strata_putchunks_refused_total{reason="drain_strict",cluster}` counter increments per refusal
- [ ] When `STRATA_DRAIN_STRICT=off` (default), existing fail-open behavior is preserved byte-for-byte — no test regressions on single-cluster smoke
- [ ] Unit test: simulated `STRATA_DRAIN_STRICT=on` + drained cluster + empty policy → `Backend.PutChunks` returns `ErrDrainRefused`; same scenario with `=off` → falls back to draining cluster as today
- [ ] Integration test (two-cluster lab): drain `default`, set `STRATA_DRAIN_STRICT=on`, PUT to bucket without Placement → expect 503 DrainRefused with `Retry-After: 300`
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: Drain progress admin endpoint + rebalance-worker manifest scan caching
**Description:** As an operator, I want a `/drain-progress` endpoint per cluster so I can see how many chunks remain and an ETA without ssh-ing into ceph nodes.

**Acceptance Criteria:**
- [ ] New admin endpoint `GET /admin/v1/clusters/{id}/drain-progress` returns JSON `{state: "live"|"draining"|"removed", chunks_on_cluster: int, bytes_on_cluster: int64, last_scan_at: RFC3339, eta_seconds: int|null, deregister_ready: bool}`
- [ ] When state=live → `chunks_on_cluster=null`, `bytes_on_cluster=null`, `eta_seconds=null`, `deregister_ready=null` (only meaningful while draining)
- [ ] Rebalance worker (`internal/rebalance`) extends its per-tick scan: for every draining cluster, walk all manifests via `meta.Store.ListBuckets` + `ListObjects` and count chunks where `BackendRef.Cluster == draining-id`; cache in-process under a new `progressCache map[clusterID]ProgressSnapshot` with `last_scan_at` stamp
- [ ] ETA computed from the last two scans: `eta_seconds = chunks_remaining / move_rate_chunks_per_sec`, where `move_rate` is derived from `strata_rebalance_chunks_moved_total{from=<id>}` rate over last 5 minutes; if move_rate is 0 → ETA = null
- [ ] `deregister_ready = chunks_on_cluster == 0` (and state==draining); flag is informational, no admin endpoint enforces it
- [ ] Endpoint reads from the progress cache — does NOT scan synchronously per request (would be O(buckets × objects))
- [ ] Stale-cache behavior: if `last_scan_at` older than `2 × STRATA_REBALANCE_INTERVAL`, response includes `warnings: ["progress data stale"]` + raw values still returned (operator can interpret)
- [ ] OpenAPI spec `internal/adminapi/openapi.yaml` documents the new endpoint + JSON schema
- [ ] Audit row stamped via `s3api.SetAuditOverride(ctx, "admin:GetClusterDrainProgress", ...)` (GET is usually skipped, but this endpoint exposes operational info that's worth auditing)
- [ ] Unit test: rebalance worker scan populates cache, endpoint returns expected counts + ETA
- [ ] Integration test (two-cluster lab): drain cluster, PUT 5 objects via a bucket whose policy excludes it, run one rebalance tick, call `/drain-progress` → assert chunks decrease over subsequent ticks, ETA shrinks, completion sets `deregister_ready=true`
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: UI progress bar on draining cluster card + completion checkmark
**Description:** As an operator, I want a visual progress bar on the cluster card while it's draining so I can see at a glance how close it is to deregister-ready without curl.

**Acceptance Criteria:**
- [ ] `<ClustersSubsection>` (`web/src/components/storage/ClustersSubsection.tsx`) renders a new `<DrainProgressBar>` block on cards where state=draining
- [ ] TanStack Query fetches `/admin/v1/clusters/{id}/drain-progress` every 30s with query key `clusterDrainProgress(<id>)`
- [ ] Bar content while draining: progress bar (filled = `1 - chunks_on_cluster / chunks_at_drain_start` — needs server to remember start count; fallback: shows "N chunks · M GiB remaining" with no fill if start unknown), text "<X> chunks · <Y> remaining · ~<ETA>"
- [ ] When `deregister_ready=true` (state=draining + chunks_on_cluster=0) → bar replaced with green chip "✓ Ready to deregister (env edit + restart)" + hover tooltip "Remove from STRATA_RADOS_CLUSTERS, then rolling restart"
- [ ] When state=live → bar not rendered
- [ ] Stale-cache warning: when response includes `warnings: ["progress data stale"]` → text rendered in muted color with "(stale)" suffix
- [ ] When STRATA_DRAIN_STRICT=on (need a new field on `GET /admin/v1/clusters` response, default `false`) → small "strict" chip appended next to the state badge; tooltip "PUTs to fallback path refused with 503 DrainRefused"
- [ ] Strict chip stays even when cluster is live (showing global mode); but operator UX hint is most relevant on draining cards
- [ ] Bundle size delta ≤ 3 KiB gzipped
- [ ] Typecheck passes
- [ ] Verify in browser using dev-browser skill

### US-005: Completion detection + log + audit + metric
**Description:** As an operator, I want a clear signal when a cluster's drain reaches zero chunks so I know it's safe to remove the cluster from env without checking ceph cli.

**Acceptance Criteria:**
- [ ] Rebalance worker compares scan-to-scan: when `chunks_on_cluster` transitions `>0 → 0` for a draining cluster, emit one INFO log `"drain complete"` with structured fields `cluster=<id>`, `scan_seconds=<duration>`, `final_bytes_moved=<n>`
- [ ] Emit one audit row via `meta.Audit` with action=`drain.complete`, resource=`cluster:<id>`, no principal (worker-emitted, mark `actor="system:rebalance-worker"`)
- [ ] Counter `strata_drain_complete_total{cluster}` increments by 1 on the transition
- [ ] Idempotent: if the cluster stays at 0 across N scans, the completion event fires ONCE (state held in the progressCache); only re-fires if a new chunk lands on the cluster (e.g. via strict-mode-off PUT) then drains again
- [ ] Optional notification: when `STRATA_NOTIFY_TARGETS` is configured, emit a `s3:Drain:Complete` event payload `{cluster, bytes_moved, completed_at}` through the existing notify worker pipeline (best-effort — notify failure does not block the worker)
- [ ] Unit test: simulate scan transitions `5 → 3 → 0 → 0`; assert log + audit + metric fire exactly once on the `3 → 0` transition
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-006: Pre-drain bucket impact preview + policy-all-drained warning
**Description:** As an operator, I want to see which buckets reference a cluster in their Placement policy BEFORE I drain it so I'm not surprised when PUTs start landing on the wrong cluster or get refused.

**Acceptance Criteria:**
- [ ] New admin endpoint `GET /admin/v1/clusters/{id}/bucket-references` returns `{buckets: [{name, weight, chunk_count, bytes_used}], total_buckets: int}` — backed by `meta.Store.ListBuckets` filtering on `Placement[<cluster>] > 0`, chunk_count + bytes_used pulled from the existing `bucket_stats` row
- [ ] Pagination via `?limit=N&offset=M` (default limit=100); response includes `next_offset` when truncated
- [ ] Sort: descending by `chunk_count`, then alphabetic on `name`
- [ ] Endpoint reads from meta + bucket_stats — no manifest scan (the rebalance worker progress scan covers the actual-distribution side)
- [ ] OpenAPI spec updated
- [ ] Audit row via `s3api.SetAuditOverride(ctx, "admin:GetClusterBucketReferences", "cluster:<id>", ...)`
- [ ] UI: new `<BucketReferencesDrawer>` on each cluster card with a "Show affected buckets" link (visible always, not only when draining); drawer lists the buckets with chunk_count column + "View Placement" link to BucketDetail Placement tab
- [ ] UI: Bucket Placement tab gains a `<PolicyDrainWarning>` chip rendered when EVERY cluster with non-zero weight in the current policy is `state=draining` — text "All clusters in this policy are draining. New PUTs will be refused (strict mode) or fall back to the class default (default mode). Update the policy before traffic resumes."
- [ ] Drain confirmation modal (`<ConfirmDrainModal>` from placement-ui US-002) gets a new info row: "<N> buckets reference this cluster — <link>view list</link>" linking to the new drawer; clicking the link opens the drawer without closing the modal
- [ ] Bundle size delta ≤ 4 KiB gzipped
- [ ] Unit test for the meta filter (bucket with `Placement={c1:1,c2:1}` → matches when querying both c1 and c2; bucket without policy → matches neither)
- [ ] Integration test: 3 buckets with mixed policies, query `/bucket-references` for each cluster, assert per-cluster lists match
- [ ] Typecheck passes, tests pass, `go vet ./...` passes
- [ ] Verify in browser using dev-browser skill

### US-007: End-to-end smoke validation + docs + ROADMAP close-flip + PRD removal + operator runbook update
**Description:** As an operator, I need every step of the drain workflow validated end-to-end against the multi-cluster lab AND documented — so I never hit a "wire field exists but nothing renders it" gap after a cycle merges. Per `feedback_cycle_end_to_end.md`, the final story bundles smoke + docs + close-flip; failed smoke = cycle NOT done.

**Acceptance Criteria:**
- [ ] **Smoke walkthrough script** at `scripts/smoke-drain-lifecycle.sh` (or `scripts/multi-replica-smoke.sh`-style) drives every step from the User Journey Walkthrough table against the running `multi-cluster` compose profile (`docker compose --profile multi-cluster up -d`); script EXITS NON-ZERO if any step fails the assertion
- [ ] Smoke covers all 15 happy-path steps from the Walkthrough section + the 4 negative paths (strict-mode 503; all-drained policy warning; undrain rollback; default fail-open regression)
- [ ] Smoke asserts via curl + `docker exec rados ls | wc -l` + `jq`: Pools table returns 6 rows (2 clusters × 3 pools), bucket-references endpoint returns expected buckets per cluster, /drain-progress endpoint flips `deregister_ready=true`, strict-mode PUT returns `503 DrainRefused` with `Retry-After: 300`
- [ ] `make smoke-drain-lifecycle` Makefile target runs the script
- [ ] Smoke runs ONCE manually before commit + once in CI (new GH workflow job `e2e-drain-lifecycle` gated by the `multi-cluster` profile, conditional on the multi-replica-style `[multi-cluster]` PR-label or scheduled nightly)
- [ ] Playwright spec `web/e2e/drain-lifecycle.spec.ts` covers UI half of the same 15 steps: every UI surface (drawer, progress bar, deregister chip, strict chip, policy-drain-warning) gets clicked + asserted; runs against the existing in-memory test rig (no Docker required for CI fast path)
- [ ] `docs/site/content/best-practices/placement-rebalance.md` gains new "Drain lifecycle" section covering: (a) when drain is useful (multi-cluster placement deployments with bucket Placement policies naming ≥2 clusters), (b) when drain is NOT useful (single-cluster-per-class config — chunks have nowhere to migrate; drain becomes pure stop-write), (c) pre-drain operator checklist (open bucket-references drawer, verify hot buckets have alternate-cluster policy, verify pool names exist on target clusters, optionally flip `STRATA_DRAIN_STRICT=on`), (d) drain procedure (Show affected buckets → Drain → typed confirm → wait for progress bar → green chip → env edit + rolling restart), (e) abort/recovery (Undrain at any point), (f) walkthrough table copied verbatim from this PRD into the operator runbook
- [ ] `docs/site/content/best-practices/web-ui.md` capability matrix gains rows for: Pools matrix (per-cluster × per-pool), Bucket references drawer, Drain progress bar, Deregister-ready chip, Strict-mode chip, Policy-all-drained warning chip
- [ ] Project root `CLAUDE.md` "Background workers" section gets an updated rebalance bullet describing the new progress scan + completion semantics + `STRATA_DRAIN_STRICT` env
- [ ] `ROADMAP.md` TWO entries close-flipped in the same commit: (a) `**P3 — Pools table shows class routing config, not actual cluster distribution.**`, (b) `**P3 — Drain strict mode for PUT routing fallback.**` — both flip to `~~**P3 — ...**~~ — **Done.** <one-line summary>. (commit \`<pending>\`)`; closing SHA backfilled in follow-up commit on main per CLAUDE.md convention
- [ ] `tasks/prd-drain-lifecycle.md` REMOVED in the same commit (PRD lifecycle rule)
- [ ] Manual screenshot of Storage page after smoke run captured and embedded in the operator runbook (visual proof the matrix renders)
- [ ] `make docs-build` succeeds
- [ ] `make vet` succeeds
- [ ] `make test` succeeds
- [ ] `pnpm run build` succeeds
- [ ] `make smoke-drain-lifecycle` succeeds against the running `multi-cluster` lab
- [ ] Typecheck passes

## Functional Requirements

- **FR-1:** `STRATA_DRAIN_STRICT` env (default `off`); `on` makes `PutChunks` refuse fallback to a draining cluster with `data.ErrDrainRefused`
- **FR-2:** Strict mode refuses **only PUT chunks** — every read / delete / multipart-finalize path keeps working against draining clusters
- **FR-3:** `data.ErrDrainRefused` → HTTP 503 `<Code>DrainRefused</Code>` + `Retry-After: 300` header
- **FR-4:** Metric `strata_putchunks_refused_total{reason="drain_strict",cluster}` counter increments per refusal
- **FR-5:** `GET /admin/v1/clusters/{id}/drain-progress` returns `{state, chunks_on_cluster, bytes_on_cluster, last_scan_at, eta_seconds, deregister_ready}` from rebalance worker scan cache
- **FR-6:** Rebalance worker scans manifests per draining cluster on each tick; results cached in `progressCache`
- **FR-7:** ETA derived from `chunks_remaining / move_rate (last 5min)`; null if rate is zero
- **FR-8:** Stale-cache warning when `last_scan_at` older than `2 × STRATA_REBALANCE_INTERVAL`
- **FR-9:** Completion detection: log INFO + audit row + counter `strata_drain_complete_total{cluster}` on `>0 → 0` transition; idempotent (fires once per transition)
- **FR-10:** Optional notify event `s3:Drain:Complete` when `STRATA_NOTIFY_TARGETS` is set
- **FR-11:** UI `<DrainProgressBar>` on draining cards: bar + text + green chip on completion + strict chip when `STRATA_DRAIN_STRICT=on`
- **FR-12:** Operator runbook documents when drain is useful + pre-drain checklist + procedure + recovery

## Non-Goals

- **No automatic deregister.** Removing a cluster from env stays manual (env edit + rolling restart). Indicator is text-only.
- **No env-tunable Retry-After.** Fixed at 300s for the first cut; tunable env is a P3 follow-up.
- **No per-cluster strict flag.** `STRATA_DRAIN_STRICT` is global; per-cluster override is a P3 follow-up.
- **No multi-pool-per-class.** `STRATA_RADOS_CLASSES` format stays one pool@cluster per class; class env extension for "alternate pool on drain" is out of scope.
- **No auto-policy-set on drain.** Operator must set Placement policies on buckets BEFORE drain to give chunks a target; the cycle does not auto-inject policies.
- **No drain scheduling.** Drain happens immediately; "drain at 02:00 UTC" is out of scope.
- **No bulk-drain.** One cluster at a time via admin API; CLI-side bulk operator script is a future P3.

## Design Considerations

- **Reuse the rebalance worker scan loop.** Adding manifest count + bytes count alongside the existing move-planning scan is cheap — same Walk pass, two extra counters.
- **Progress cache shape:** `map[clusterID]struct{ Chunks, Bytes, LastScanAt, BaseChunksAtStart }`. `BaseChunksAtStart` captured on `state=live → draining` transition (or first scan after the transition) so the UI bar can render percentage filled.
- **Strict chip placement on cluster card:** small "strict" badge next to the state badge (when `STRATA_DRAIN_STRICT=on` — global field on `GET /admin/v1/clusters` response).
- **DrainProgressBar reuses existing `<Progress>` primitive from `web/src/components/ui/`** if available; otherwise a simple flex div with width percentage.
- **Audit row "drain.complete":** action namespace consistent with existing audit (`drain.start` on POST /drain, `drain.complete` on transition). Mirror the shape of the existing `bucket.delete` audit row.
- **Notify event shape:** mirror the existing `s3:Replication:*` / `s3:LifecycleExpiration:*` patterns; payload is minimal — clients can poll the admin endpoint for full state.

## Technical Considerations

- **Manifest scan cost** on each rebalance tick is O(buckets × avg-objects-per-bucket). At default `STRATA_REBALANCE_INTERVAL=1h` it's acceptable; if operators run interval=1m for fast drain monitoring it could be expensive. Mitigation: scan only when ≥1 cluster is draining; skip the loop entirely when zero clusters need progress. Acceptable as v1.
- **Notify event idempotency** — completion fires once per transition. If operator drains → undrain → drain again → eventually 0 again → fires twice (one per cycle). That's the intended idempotency boundary.
- **Strict chip visibility lag:** `STRATA_DRAIN_STRICT` is read at boot; flipping in env requires restart. UI doesn't poll env; admin endpoint should expose the boot-time value. New `GET /admin/v1/diagnostics/drain-config` (or fold into `GET /admin/v1/clusters` response as a top-level `drain_strict: bool` field) — pick the latter, simpler.
- **No per-cluster Retry-After tuning** — fixed 300s. Operator deploys decide if 5min is too long; can patch the constant later. Adding an env knob is bikeshedding.
- **Backwards compat:** default `off` preserves byte-for-byte fail-open behavior. Every existing test passes without env change.

## Success Metrics

- Operator can read drain progress (count, ETA, completion) entirely from the console without `curl` or `ceph cli`
- `STRATA_DRAIN_STRICT=on` + drained cluster + class-bound PUT yields a clean 503 DrainRefused with `Retry-After: 300`
- AWS-SDK clients receive `Retry-After`, backoff, and retry against a non-draining cluster (verified by integration test)
- Completion event fires within `STRATA_REBALANCE_INTERVAL` of the actual zero-chunk transition (worst-case latency)
- Zero behavior change when `STRATA_DRAIN_STRICT=off` (default) — regression suite green

## Open Questions

- Should `s3:Drain:Complete` notify event get its own event type or piggyback an existing one (e.g. `s3:Cluster:Event`)? Recommend its own type — operator alerting rules will key on it.
- Should `chunks_on_cluster` count include chunks pinned to the cluster by class env mapping (i.e. chunks that will never migrate without operator intervention)? Recommend yes — the count is "what the operator must address before deregister", regardless of whether rebalance can autonomously fix it.
- Should the UI show a separate "stuck chunks" subcount (chunks attributable to single-cluster class env mapping) to nudge operator toward changing class env? Defer — first cut shows total + ETA.
