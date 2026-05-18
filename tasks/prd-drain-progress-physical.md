# PRD: Drain progress UI surfaces physical chunks (not manifest count)

## Introduction

Closes ROADMAP P3 **"Drain progress UI shows manifest counts instead of physical chunks"**.

`/admin/v1/clusters/{id}/drain-progress` surfaces `chunks_on_cluster` from a manifest scan — it counts objects whose `BackendRef.Cluster == drained-id`. The semantic is safe (rebalance worker rewrites `BackendRef` via CAS before old chunks land in the GC queue), but it confuses operators in the middle of a drain:

- **Stage 1 — migration**: rebalance worker copies chunks `default→cephb`, runs `SetObjectStorage` CAS to rewrite `BackendRef`. Manifest count for `cephb` decreases as CAS completes.
- **Stage 2 — physical cleanup**: old chunks land in the GC queue and sit there until `STRATA_GC_GRACE` (5 min default) elapses + the gc worker tick fires.

Between stages 1 and 2, manifest scan returns 0 chunks on `cephb` but the RADOS pool still has them. The UI renders **"0 Migrating"** → operator thinks the drain failed to start; in reality physical cleanup is just pending.

The deregister-ready safety gate (US-006 drain-cleanup) already ANDs `total_chunks==0 && gc_queue_pending==0 && no_open_multipart==0`, so the green chip only flips when both stages complete. The operator-facing telemetry that drives the chip transition is fine. The bug is that the in-flight progress display shows manifest-count as if it were the physical-cleanup count.

Fix is additive: a new `ClusterObjectCount` probe method on RADOS backends (parity with the existing `ClusterStats` shape), an in-process 10 s TTL cache to keep `/drain-progress` polls cheap, three new additive fields on the JSON response, and a UI 3-state machine. ETA is a static tooltip — precise per-deploy ETA would require exposing GC tunables; out of scope here (a precise-ETA follow-up is parked on ROADMAP).

## Goals

- `/admin/v1/clusters/{id}/drain-progress` response gains `physical_chunks_on_cluster *int64`, `physical_bytes_on_cluster *int64`, and `gc_queue_pending int` fields. RADOS backend populates the physical fields via a new `ClusterObjectCount` probe (the existing `ClusterStats` covers bytes); S3 + memory backends return `null` for the physical fields (graceful degradation).
- `<DrainProgressBar>` primary metric becomes physical chunks (when the backend supports it). Manifest count surfaces as collapsible detail. Three explicit operator-facing states with copy + static tooltip describing the wait reason.
- New in-process 10 s TTL cache (`internal/data/placement/clusterstats_cache.go` or similar) absorbs the per-poll `ceph df` cost; the existing 5 s UI poll cadence therefore drives at most one MonCommand per cluster per 10 s window.
- Smoke harness exercises all three transition points (migration → awaiting-gc → ready-to-deregister). Throttled rebalance keeps the migration phase wide enough to sample.
- Existing operator wire shape (`chunks_on_cluster`, `not_ready_reasons`) preserved — change is additive, no breaking field rename.

## User Journey (pre-cycle walkthrough)

Happy path — operator drains `cephb` evacuate:

1. Operator clicks Drain → evacuate → Submit in `<ConfirmDrainModal>`. `POST /admin/v1/clusters/cephb/drain {mode:evacuate}` accepted; cluster state flips → `evacuating`; UI subscribes to `/drain-progress` poll (existing 5 s tick).
2. Rebalance worker picks up the drain in its next tick; starts copying chunks `default→cephb` + CAS-rewriting manifests + enqueuing old chunks for GC.
3. After ~10 s the first poll arrives: `physical_chunks_on_cluster: 100, chunks_on_cluster (manifest): 95, gc_queue_pending: 5`. UI renders "Migrating: 100 physical chunks remaining". Collapsible detail expands to show `Manifest chunks: 95, GC queue: 5, Bytes: 400 MB`.
4. Migration completes: rebalance worker finishes the last CAS. Next poll: `physical_chunks_on_cluster: 100, chunks_on_cluster: 0, gc_queue_pending: 100`. UI flips to "Awaiting GC cleanup: 100 chunks awaiting physical delete". Static tooltip below the count: "Physical delete completes after STRATA_GC_GRACE elapses (~5m default) plus the next gc worker tick."
5. GC grace elapses; gc worker drains queue tick-by-tick. Next polls: `physical_chunks_on_cluster: 60, gc_queue_pending: 60` → 30 → 0. UI counts down.
6. All zero: `physical=0, manifest=0, gc_queue=0, multipart=0` → green "✓ Ready to deregister" chip flips per existing deregister-ready safety gate.

Edge case — S3 / memory backend (no `ClusterObjectCount`):

1. `/drain-progress` returns `physical_chunks_on_cluster: null, physical_bytes_on_cluster: null`. `gc_queue_pending` and `chunks_on_cluster` still populated.
2. UI detects the null physical fields and renders the manifest count as the primary metric (the pre-cycle behavior) with a small "(physical count unavailable on this backend)" tooltip. Same 3-state machine logic uses `chunks_on_cluster` as the proxy for physical when physical is null.

Edge case — physical count diverges from manifest+GC:

1. Possible if some objects on `cephb` were placed there by direct PUT (not migration). Those land in `cephb` pool but no GC entry exists for them — they're alive objects, not migration leftovers. Counts diverge legitimately.
2. UI displays both metrics; operator sees the split via the collapsible detail. No anomaly chip — the divergence is correct behavior.

Negative path — `ceph df` MonCommand transient failure:

1. `ClusterObjectCount` returns error. The 10 s TTL cache serves the last-known value if available; otherwise the response sets `physical_chunks_on_cluster: null` for that poll. The error increments `strata_drain_progress_probe_errors_total{cluster}` once per failed MonCommand attempt (not once per poll).
2. UI renders the cached or null value; no special transient state — same fallback rendering as for unsupported backends.

## User Stories

### US-001: `ClusterObjectCount` probe + 10 s cache + `/drain-progress` response extension

**Description:** As a developer, I need a new probe method that returns per-cluster object count, cached briefly to absorb poll cost, plus three new additive fields on the drain-progress endpoint.

**Acceptance Criteria:**
- [ ] New interface in `internal/data/backend.go`:
  ```go
  type ClusterObjectCountProbe interface {
      ClusterObjectCount(ctx context.Context, clusterID string) (int64, error)
  }
  ```
  Defined next to the existing `ClusterStatsProbe`. Document that implementation is optional; admin handlers type-assert.
- [ ] `internal/data/rados/health.go::Backend.ClusterObjectCount` implementation iterates the existing `b.classes` map, filters by `clusterID`, opens an ioctx per (cluster, pool, ns) tuple, calls `ioctx.GetPoolStats()`, and sums `stat.Num_objects` across pools. Reuses the same path already used by `DataHealth`. Returns `(0, err)` on any per-pool error (does NOT silently zero-fill on failure — caller must distinguish missing data from zero).
- [ ] `ClusterStats` signature UNCHANGED — keeps `(usedBytes, totalBytes int64, err error)`. Existing rebalance worker target-full check call site untouched.
- [ ] New cache `internal/data/placement/clusterstats_cache.go` (or similar location next to `draincache.go`): `ClusterStatsCache` with `TTL: 10 * time.Second` (constant; not configurable in this cycle), `Get(clusterID) (bytes, objects int64, ok bool)`, `Set(clusterID, bytes, objects int64)`. Concurrency-safe (sync.RWMutex). Lazy refresh: `/drain-progress` builder calls `Get` first; on miss or expired entry, calls `ClusterStats` + `ClusterObjectCount` once, calls `Set`, returns fresh values.
- [ ] Cache invalidation: ONLY by TTL expiration. No admin endpoint or event invalidates it (no need — drain/undrain doesn't change the underlying `ceph df` payload).
- [ ] `internal/s3api/admin_clusters.go::buildDrainProgressResponse` (or wherever `/drain-progress` is assembled) gains three new fields:
  - `physical_chunks_on_cluster *int64` — pointer; null when backend doesn't implement `ClusterObjectCountProbe`
  - `physical_bytes_on_cluster *int64` — pointer; null when backend doesn't implement `ClusterStatsProbe`
  - `gc_queue_pending int` — explicit counter (length of `ListChunkDeletionsByCluster(cluster)`); already computed by existing US-006 drain-cleanup safety gate, just surfaced as a numeric field instead of swallowed into `not_ready_reasons`
- [ ] Existing fields (`chunks_on_cluster`, `bytes_on_cluster`, `not_ready_reasons`, `deregister_ready`) preserved verbatim — wire shape strictly additive.
- [ ] New counter `strata_drain_progress_probe_errors_total{cluster, probe}` (label `probe` ∈ `{"stats", "object_count"}`) increments on probe error. The response still succeeds with `null` physical fields for that poll (cache fallback if available, otherwise null).
- [ ] Unit test: cache TTL behavior — Set + Get within TTL returns ok=true; after TTL returns ok=false.
- [ ] Unit test: drain-progress builder with mock probe returning known objectCount → asserts physical fields populated.
- [ ] Unit test: drain-progress builder when type assertion fails (memory backend) → asserts physical fields are JSON null.
- [ ] Unit test: probe error → counter incremented + null returned (no panic).
- [ ] Integration test against memory backend: `/drain-progress` returns physical fields as null with valid JSON shape.
- [ ] `go vet ./...` passes
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: `<DrainProgressBar>` 3-state machine + collapsible detail + back-compat null fallback

**Description:** As an operator, I want the drain-progress UI to reflect physical pool state primary so I'm not confused by the manifest/physical split during the GC-grace window.

**Acceptance Criteria:**
- [ ] `<DrainProgressBar>` reads `physical_chunks_on_cluster` from the `/drain-progress` response. The "primary metric" displayed in the headline number is:
  - `physical_chunks_on_cluster` when non-null
  - `chunks_on_cluster` (existing manifest count) when null
- [ ] When physical is null, a small "(physical count unavailable on this backend)" tooltip renders next to the headline. No special transient state, no "probe-error retrying" — null is null regardless of cause (unsupported backend, MonCommand fail, cache cold).
- [ ] Three explicit primary states rendered:
  - **Migrating** (red/amber chip): `primary > 0 && chunks_on_cluster > 0` → "Migrating: X chunks remaining" where X = primary
  - **Awaiting GC cleanup** (amber chip): `primary > 0 && chunks_on_cluster == 0` → "Awaiting GC cleanup: X chunks awaiting physical delete"; static tooltip below the count: "Physical delete completes after STRATA_GC_GRACE elapses (~5m default) plus the next gc worker tick."
  - **Ready to deregister** (green chip): `primary == 0 && chunks_on_cluster == 0 && gc_queue_pending == 0 && no_open_multipart` → "✓ Ready to deregister" (existing `deregister_ready` flow)
- [ ] Collapsible detail below the primary metric (`<details>` element OR shadcn `<Accordion>` if already used in the codebase): shows `Manifest chunks: X`, `GC queue: Y`, `Physical bytes: Z B` (formatted humanely via existing byte-formatter helper). Hidden by default; operator clicks to expand. On backends with null physical, `Physical bytes` row reads "unavailable".
- [ ] No precise ETA computation in this cycle — tooltip text only describes the wait reason. Precise per-deploy ETA is parked on ROADMAP as a P3 follow-up if operator demand justifies the new admin endpoint (see Non-Goals).
- [ ] When `physical_chunks_on_cluster` is null AND backend was expected to support it: NO special UI handling — same fallback as for unsupported backends. The `strata_drain_progress_probe_errors_total` counter is the operator-side signal that the cache + probe path is degraded.
- [ ] Playwright spec extends `web/e2e/drain-transparency.spec.ts` (or new `web/e2e/drain-progress.spec.ts` if scope is cleaner separate): mocks three `/drain-progress` responses corresponding to the three states; asserts correct copy + collapsible detail content + chip color + tooltip text.
- [ ] Playwright spec ALSO covers the null-physical case: mock response with `physical_chunks_on_cluster: null` → asserts manifest count as primary + "(physical count unavailable on this backend)" tooltip.
- [ ] Verify in browser via the dev workflow: drain cephb in lab, observe transition through all three states.
- [ ] `pnpm run build` succeeds
- [ ] Typecheck passes

### US-003: Throttled smoke harness + docs + ROADMAP close-flip + PRD removal

**Description:** As an operator and as a future-maintainer, I need a smoke harness that reliably observes all three drain states, plus documented migration and the ROADMAP entry flipped.

**Acceptance Criteria:**
- [ ] New `scripts/smoke-drain-progress-ui.sh` script (rather than extending the existing `smoke-drain-transparency.sh` — cleaner separation for a new, more nuanced state machine). Steps:
  - Configure the strata container with `STRATA_REBALANCE_RATE_MB_S=1` (slow rebalance so migration phase is wide enough to sample) and `STRATA_GC_GRACE=60s` (shorten the grace so the test runs in under 5 min). Document why these env values are smoke-only.
  - Plant 300 objects on cephb via PUT with `Placement={default:1, cephb:1}` mode=weighted, ~1 MB each → 300 chunks
  - Drain cephb evacuate; poll `/drain-progress` every 3 s (faster than the UI's 5 s)
  - Assert at least one poll observes state Migrating: `physical_chunks_on_cluster > 0 && chunks_on_cluster > 0`
  - Assert at least one poll observes state Awaiting GC: `physical_chunks_on_cluster > 0 && chunks_on_cluster == 0 && gc_queue_pending > 0`
  - Assert eventual final poll observes Ready: `physical_chunks_on_cluster == 0 && chunks_on_cluster == 0 && gc_queue_pending == 0 && deregister_ready == true`
  - Total timeout: 5 min; script EXITS non-zero on timeout or if a state is never observed
- [ ] `make smoke-drain-progress-ui` Makefile target wraps the script; exits non-zero on any failure.
- [ ] `docs/site/content/best-practices/placement-rebalance.md` "Drain lifecycle" subsection updated: documents the three drain-progress states (Migrating / Awaiting GC / Ready), the static ETA tooltip, the physical-vs-manifest distinction, and the back-compat fallback for backends without `ClusterObjectCountProbe`.
- [ ] Migration note `docs/site/content/architecture/migrations/drain-progress-physical.md` (new) documents: the new `physical_*` fields on `/drain-progress`, the wire-shape additivity, the `chunks_on_cluster` semantic shift (still primary on backends without physical probe), and references to the new probe interface.
- [ ] Linked from `docs/site/content/architecture/migrations/_index.md`.
- [ ] `ROADMAP.md` close-flip the P3 entry → `~~**P3 — Drain progress UI shows manifest counts instead of physical chunks.**~~ — **Done.** <one-line summary referencing the new ClusterObjectCountProbe + cache + 3-state UI + smoke harness>. (commit \`<pending>\`)`; closing SHA backfilled on main.
- [ ] Add new P3 ROADMAP entry **"Precise drain-progress ETA from gateway GC tunables"** as a parked follow-up — operator-tunable ETA computation requires a new `/admin/v1/gc-config` endpoint + an ETA formula; deferred until operator demand justifies the work.
- [ ] `tasks/prd-drain-progress-physical.md` REMOVED in the same commit (CLAUDE.md PRD lifecycle rule).
- [ ] `make docs-build`, `make vet`, `make test`, `pnpm run build`, `make smoke-drain-progress-ui` all green.
- [ ] Typecheck passes

## Functional Requirements

- FR-1: New `data.ClusterObjectCountProbe` interface with method `ClusterObjectCount(ctx, clusterID) (int64, error)`. Defined next to existing `ClusterStatsProbe`. Optional capability; admin handlers type-assert.
- FR-2: `internal/data/rados/health.go::Backend.ClusterObjectCount` sums `GetPoolStats().Num_objects` across pools registered for the cluster.
- FR-3: `ClusterStatsProbe.ClusterStats` signature unchanged.
- FR-4: New in-process cache `ClusterStatsCache` with 10 s TTL absorbs per-poll probe cost. Lazy refresh; TTL-only invalidation.
- FR-5: `/admin/v1/clusters/{id}/drain-progress` response gains `physical_chunks_on_cluster *int64`, `physical_bytes_on_cluster *int64`, `gc_queue_pending int` fields (additive).
- FR-6: S3 + memory backends do not implement either probe → physical fields are JSON `null`; UI back-compat renders manifest count as primary.
- FR-7: `<DrainProgressBar>` 3-state machine: Migrating / Awaiting GC cleanup / Ready to deregister.
- FR-8: ETA in UI is a static tooltip; no precise computation in this cycle.
- FR-9: New counter `strata_drain_progress_probe_errors_total{cluster, probe}` tracks probe failures.
- FR-10: Smoke harness throttles rebalance + shortens GC grace to reliably observe all three states.

## State Truth Tables

### `<DrainProgressBar>` state derivation (primary = physical when non-null, else manifest)

| `physical_chunks` (null = unsupported / probe-err / cache cold) | `chunks_on_cluster` (manifest) | `gc_queue_pending` | `no_open_multipart` | Primary value | UI state |
|---|---|---|---|---|---|
| null | > 0 | n/a | n/a | manifest > 0 | Migrating (manifest fallback) |
| null | 0 | 0 | true | 0 | Ready (manifest fallback) |
| > 0 | > 0 | n/a | n/a | physical | Migrating |
| > 0 | 0 | > 0 | n/a | physical | Awaiting GC cleanup |
| 0 | 0 | 0 | true | 0 | Ready |
| 0 | 0 | > 0 | true | 0 | Awaiting GC cleanup (rare; gc queue not yet drained) |
| any | any | any | false | (any) | "Open multipart in progress — wait for completion" (existing `not_ready_reasons` token) |

### Probe capability matrix

| Backend | `ClusterStatsProbe` | `ClusterObjectCountProbe` | physical_bytes in JSON | physical_chunks in JSON |
|---|---|---|---|---|
| RADOS | yes (existing) | yes (new) | int64 | int64 |
| S3 (passthrough) | no | no | null | null |
| memory | no | no | null | null |

### `ClusterStatsCache` lifecycle

| Event | Action |
|---|---|
| `/drain-progress` poll | `Get(clusterID)`; on miss/expired, call both probes + `Set` |
| Probe error | Counter increments; if cached value exists, served; else null |
| TTL expiry | Next `Get` returns ok=false; refresh triggered lazily |
| Process restart | Cache empty; first poll repopulates |

## Cache Invalidation Ledger

- **New: `ClusterStatsCache` (in-process, per-replica)**
  - Key: `clusterID`
  - Value: `(usedBytes, totalBytes, objectCount int64, fetchedAt time.Time)`
  - TTL: 10 s (constant)
  - Invalidation: TTL expiry only. No event-driven invalidation (drain/undrain admin flips do NOT touch the cache — the underlying `ceph df` data doesn't change at those events).
  - Concurrency: sync.RWMutex
- Existing `placement.DrainCache` (30 s TTL, invalidated on drain/undrain): unaffected
- Existing TanStack `drain-progress` query (5 s poll): unaffected; new fields added to response shape
- No new TanStack caches

## Safety Claims Preconditions

- **Claim: physical fields are accurate enough for operator confidence.**
  - Precondition: per-pool `GetPoolStats().Num_objects` reflects pool state within Ceph's internal staleness bounds (typically <30 s lag). The 10 s cache TTL is shorter than typical Ceph staleness, so cache aging does not compound the lag. **Test**: smoke harness asserts the physical count decreases monotonically (within a small noise band) after `STRATA_GC_GRACE` elapses.
- **Claim: deregister-ready chip still only flips when truly safe.**
  - Precondition: existing US-006 drain-cleanup safety gate (`total_chunks==0 && gc_queue_pending==0 && no_open_multipart`) is preserved unchanged. New `physical_chunks_on_cluster==0` field is NOT added to the deregister-ready predicate (it's an observability signal, not a safety predicate — adding it would block deregister on backends without physical probe). **Test**: existing US-006 contract tests pass unchanged.
- **Claim: back-compat for backends without the new probe.**
  - Precondition: UI handles `null` physical fields by falling back to manifest count + a tooltip. **Test**: Playwright spec asserts S3-backend mocked response renders Migrating state via manifest count.
- **Claim: per-poll cost remains low.**
  - Precondition: 10 s TTL cache absorbs the burst of 5 s polls. **Test**: unit test asserts cache hits dominate within a 30 s simulated polling window.

## Downstream Consumer Grep

- `ClusterStatsProbe.ClusterStats` callers: `internal/rebalance/worker.go` target-full check. **Untouched** in this cycle (signature unchanged).
- `/admin/v1/clusters/{id}/drain-progress` response consumers: `<DrainProgressBar>.tsx` (or wherever it lives). New fields are JSON-additive; existing reads of `chunks_on_cluster` / `bytes_on_cluster` / `deregister_ready` / `not_ready_reasons` unchanged.
- New interface `ClusterObjectCountProbe`: no prior consumers (greenfield).
- New cache `ClusterStatsCache`: no prior consumers; used internally by `/drain-progress` builder only.
- New metric `strata_drain_progress_probe_errors_total`: no prior consumers; documented in `docs/site/content/architecture/observability.md` metrics catalog.

## Worst-Case Thought Exercise

What if `GetPoolStats()` reports a stale object count?

- The 10 s cache TTL plus Ceph's internal staleness window stack: worst case, operator sees a count up to ~40 s old. Smooth poll cadence and human perception absorb this. No safety impact.

What if the operator drains all clusters simultaneously?

- Each drain runs independently; physical counts surface per cluster via per-key cache. UI renders separate `<DrainProgressBar>` per drained cluster. No cross-talk.

What if gc worker is disabled (`STRATA_WORKERS` doesn't include `gc`)?

- `gc_queue_pending` grows; "Awaiting GC cleanup" state persists indefinitely. UI displays accurately. Operator notices and enables gc worker. Not a bug, just makes the missing worker visible.

What if rebalance worker is disabled?

- Manifest count never decreases; "Migrating: X chunks" persists indefinitely (no CAS rewrites happen). UI shows accurate but stuck count. Operator enables rebalance worker.

What if the gateway is restarted mid-drain?

- `cluster_state` row persists (`evacuating`); rebalance worker resumes on next tick; UI poll resumes. The `ClusterStatsCache` resets, but `/drain-progress` reads `ceph df` fresh on cache miss, so the UI display recovers within one tick.

What if multiple gateway replicas run (HA / lab-cassandra-3)?

- Each replica has its own 10 s cache. UI may hit different replicas; back-to-back polls observe cache states from up to two different replicas. Bounded staleness ≤10 s, same UX as single-replica.

What if a pool on the drained cluster has hundreds of millions of objects?

- `GetPoolStats()` returns a precomputed counter from Ceph (no scan). O(1) call. Cache TTL keeps the per-replica probe rate bounded. Scales fine.

What about cluster_state state=removed?

- `/drain-progress` returns 404 for non-existent or removed clusters (existing behavior). No probe attempted. Cache irrelevant.

## Non-Goals

- New backend type implementing `ClusterObjectCountProbe` (e.g. S3 implementing a sum-of-object-listings) — S3 + memory continue to return null; back-compat path covers it.
- Auto-tuning `STRATA_GC_GRACE` / `_CONCURRENCY` based on observed queue depth — out of scope.
- Persistent storage of drain-progress samples (historical drain timelines) — current real-time poll is sufficient.
- Anomaly detection on count divergence between physical / manifest / GC queue — out of scope; just display the numbers.
- Per-pool breakdown of physical chunks — `GetPoolStats` returns per-pool but UI aggregates. Per-pool detail belongs on the Storage Data tab, not the drain progress card.
- **Precise per-deploy ETA computation** — would require new `/admin/v1/gc-config` endpoint surfacing `STRATA_GC_GRACE` / `_INTERVAL` / `_BATCH_SIZE` / `_CONCURRENCY` / `_SHARDS` + an ETA formula `eta_min = grace_min + ceil(gc_queue / (batch_size × shards) × interval_min)`. Static tooltip in this cycle. Parked as P3 follow-up ("Precise drain-progress ETA from gateway GC tunables") added in US-003.
- Configurable cache TTL via env — 10 s constant suffices; revisit if production demands.

## Open Questions

- **Cache TTL value**: 10 s chosen as a compromise between freshness (operator sees recent counts) and probe cost (one MonCommand per 10 s per cluster per replica). Could lower to 5 s if probe is genuinely cheap; could raise to 30 s if operator UX tolerates staler counts. Decision: 10 s constant, documented; revisit on production feedback.
- **Cache scope**: per-replica vs shared via meta. Per-replica is simpler and good enough at the bounded staleness ≤10 s. Decision: per-replica.

## Technical Considerations

- The new `ClusterObjectCountProbe` interface is intentionally separate from `ClusterStatsProbe` rather than a signature extension. Avoids breaking the existing `ClusterStats` signature; the rebalance worker target-full check remains untouched. Both probes are implemented by the RADOS backend and can share the same per-cluster MonCommand under the hood if needed, but the public-facing interface stays clean.
- Per-pool `GetPoolStats()` is used instead of `ceph df` JSON parsing. The existing `DataHealth` code path already iterates classes + opens ioctxs + calls `GetPoolStats()`; the new method reuses that exact pattern. Avoids JSON-shape verification risk (whether `ceph df` JSON returns `objects` vs `num_objects` field).
- `pools[].stats.objects` from `ceph df` is NOT used — the per-pool ioctx path is more robust and already trusted in `DataHealth`.
- The 10 s cache lives in `internal/data/placement/` alongside `draincache.go` for locality (both are placement-related in-process caches with TTL invalidation).
- The new field shape uses `*int64` to support JSON `null` for unsupported backends. Go `encoding/json` marshals `nil *int64` as `null`.
- UI fallback path: when `physical_chunks_on_cluster` is null, the existing `<DrainProgressBar>` rendering path (pre-cycle) is preserved verbatim. The 3-state machine only activates when physical fields are present — but the state DERIVATION uses the same primary-value selection so the logic is uniform (just the SOURCE of the primary value differs).
- Tooltip text for S3/memory backends: "Physical chunk count is unavailable on this backend." Plain language; no marketing.
- Smoke harness uses `STRATA_REBALANCE_RATE_MB_S=1` + `STRATA_GC_GRACE=60s` to reliably observe state transitions; the prod default values produce too-fast a migration phase or too-long a grace window for a single-pass smoke run.

## Success Metrics

- `make smoke-drain-progress-ui` passes end-to-end against the lab; all three states observed during one drain cycle.
- Operator-reported confusion ("0 Migrating" misread) eliminated — verified via the smoke harness's transition-point assertions.
- Existing US-006 drain-cleanup safety gate unchanged; existing contract tests pass.
- `/drain-progress` per-poll cost bounded — cache hit ratio ≥ 80% in a typical 30 s observation window per cluster per replica.
- ROADMAP P3 entry closed.
