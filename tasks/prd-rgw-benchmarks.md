# PRD: Benchmarks vs RGW — validation cycle

## Introduction

Strata's README claims "drop-in replacement for Ceph RGW". Today the
claim is unbacked — no published benchmark numbers compare Strata to
stock RGW on the same hardware + workload + lab shape. ROADMAP P2
entry on line 54 has tracked this gap since the consolidation phase.

This cycle ships a reproducible bench harness against an RGW-bearing
lab profile and publishes the numbers at
`/docs/architecture/benchmarks/rgw-comparison/`. After this cycle the
"drop-in RGW replacement" framing has data behind it — including a
mandatory **Limitations** section disclosing the 4 known biases
(dual-cluster Strata vs single-cluster RGW, TiKV-default backend
only, loopback-only network, lima docker I/O penalty on macOS) so
operators can interpret the ratios honestly.

**Pre-launch product** per [Pre-launch no deploys] memory — bench
runs against the local lab + an operator-spun RGW container.

Branch: `ralph/rgw-benchmarks`. Starts from `main`. **12 stories**
after PRD review found 12 production-grade issues: s3-bench tool
unverified, dual-cluster Strata bias, missing `-tags ceph` check,
underestimated bench duration (~30min claim → ~120min reality),
no cleanup discipline between workloads, missing Limitations
section, RGW realm/zone setup complexity underestimated, no
pre-flight health check, no concurrency sweep, no stddev capture,
README backfill format fragility, Cassandra-backend column absent.

## Goals

- Reproducible bench harness `scripts/bench-rgw-comparison.sh`
  using a tool verified to exist + support the required flags
  BEFORE writing workloads (s3-bench-first strategy with fallback
  chain).
- New compose profile `bench-rgw` spinning a separate RGW
  container (`ceph/ceph:v19.2.3`) configured against the existing
  `ceph` cluster — pool naming distinct from Strata's
  `strata.rgw.buckets.data`.
- RGW configured with **stock defaults** + minimal realm/zonegroup
  setup required for v19+ RGW to start (research in US-001).
- Strata side: TiKV-default lab forced to single-cluster mode for
  the bench (override `STRATA_RADOS_CLUSTERS=default:...` only) so
  hardware count matches RGW (1 RADOS cluster each).
- Strata build MUST be `-tags ceph` lab image; pre-flight check
  asserts `data_backend=rados` before bench starts.
- Optional Cassandra-default sweep as a 3rd target (US-011) — only
  triggers when operator passes `--include-cassandra` flag, since
  it doubles bench duration.
- Workload coverage: PUT/GET (1KB / 1MB) with concurrency sweep
  1 / 8 / 32 / 128 + PUT/GET 100MB + multipart 5GB + ListObjects
  100k-key + Range GET + delete + IAM-authenticated. 8 workload
  classes (PUT-1KB sweep counts as 1 class).
- Numbers published in
  `docs/site/content/architecture/benchmarks/rgw-comparison.md`
  with a mandatory **Methodology + Limitations** section.
  Limitations explicitly discloses the 4 biases.
- Per-workload stats: p50 / p95 / p99 + throughput + stddev across
  the 3 runs (not just median).
- README features-vs-RGW table backfilled — but only after a
  grep-verify pass that the table row labels still match what
  `ralph/readme-docs-rewrite` US-001 shipped.
- Operator-run-only — NOT wired into CI matrix. Realistic bench
  duration is **~120 min for full sweep** (not 30 — original
  estimate was wrong).
- Explicit cleanup discipline between workloads: `cleanup_workload()`
  function drops bucket + verifies pool free-space recovery before
  next workload starts.

## User Journey

Three personas covered:

- **Operator evaluating Strata vs RGW for migration.** Today no
  numbers to cite. After cycle:
  `/docs/architecture/benchmarks/rgw-comparison/` carries
  side-by-side latency + throughput tables across 8 workloads
  with stddev + concurrency sweep + Limitations section.
- **Maintainer defending the "drop-in" framing in README.** After
  cycle: README features-vs-RGW table cells backed by
  `make bench-rgw-comparison` output + Limitations link.
- **Contributor running a perf change.** Pre-cycle: no baseline.
  After cycle: `make bench-rgw-comparison` is the canonical
  regression check + cleanup discipline lets it run repeatedly
  without disk-fill.

## User Stories

### US-001: RGW container + `bench-rgw` compose profile (with realm/zone research)

**Description:** As a maintainer, I want a `bench-rgw` compose
profile that spins a separate RGW container against the existing
lab ceph cluster. **Research first** the minimal RGW v19+
realm/zonegroup setup (RGW since Jewel requires explicit
realm/zonegroup/zone — `ceph orch apply rgw` shortcuts this;
manual setup needs `radosgw-admin realm create` +
`radosgw-admin zonegroup create` + `radosgw-admin zone create` +
`radosgw-admin period update --commit`).

**Acceptance Criteria:**
- [ ] **Research first** the minimal config — document the
      exact commands in `deploy/docker/rgw-entrypoint.sh`
      header comment. RGW boots with `ceph orch apply rgw
      <name>` from the ceph-orchestrator side OR manual
      `radosgw-admin realm/zonegroup/zone create` chain.
      Document the chosen path inline.
- [ ] New compose service `rgw` in
      `deploy/docker/docker-compose.yml` under
      `profiles: ["bench-rgw"]`: image `ceph/ceph:v19.2.3`
      (matches Strata runtime image base — same librados
      version); host port `9991:8080`.
- [ ] **RGW config**: stock defaults except (a) bench user via
      `radosgw-admin user create --uid=bench
      --display-name=Bench`; (b) realm/zonegroup/zone setup as
      researched above.
- [ ] Entrypoint script `deploy/docker/rgw-entrypoint.sh`:
      (a) wait for ceph-a mon reachable;
      (b) bootstrap realm/zonegroup/zone if absent (idempotent);
      (c) create bench user (idempotent — `|| true` on
          duplicate);
      (d) parse access-key + secret-key from `radosgw-admin
          user info --uid=bench` JSON (use `jq` if available
          in `ceph/ceph` image; if not, fall back to
          `python3 -c "import json,sys; ..."`);
      (e) write creds to `/etc/strata-bench/rgw-creds.env`
          mounted as a docker volume;
      (f) `exec radosgw -d -n client.rgw.bench`.
- [ ] **Pool naming check**: RGW auto-creates
      `default.rgw.buckets.data` + index pools. Strata uses
      `strata.rgw.buckets.data`. No name collision; both
      coexist on the same OSDs. Document this in the
      methodology page (US-008) since the OSD competition
      affects fairness.
- [ ] `make up-bench-rgw` Makefile target +
      `make wait-rgw` (polls `curl -fs http://localhost:9991/`
      → 200, 60s timeout).
- [ ] `make down` (existing) cleans up `--profile bench-rgw`
      — verify recipe has the profile cleanup.
- [ ] **Locally verified**: `make up-all && make up-bench-rgw
      && make wait-rgw` → RGW listens; `aws --endpoint-url
      http://localhost:9991 s3 ls` returns empty list.
- [ ] `make smoke-tikv-default-lab` still passes — `bench-rgw`
      profile must NOT affect bare default.
- [ ] **`jq` fallback path documented**: if `jq` absent in
      `ceph/ceph:v19.2.3` image, entrypoint falls back to
      `python3` (verified present in `ceph/ceph` image since
      it's needed for ceph orchestrator). Log the chosen
      parser at startup.
- [ ] Typecheck passes; tests pass.

### US-002: s3-bench verify-first + base script scaffold

**Description:** As a benchmark author, I want to verify the
chosen s3-bench tool exists, is buildable, and supports the
required flags BEFORE scaling out to all workloads — production-
grade benchmarks can't have an unverified dependency.

**Acceptance Criteria:**
- [ ] **Tool verification step (DO FIRST before any workload
      work)**: try the install line + flag check chain:
      (a) `go install github.com/wasabi-tech/s3-bench@latest`
          → verify binary runs + supports `--operation put`,
          `--operation get`, `--operation multipart-put`,
          `--operation list`, `--operation delete`,
          `--object-size`, `--threads`, `--duration`, JSON
          output.
      (b) If (a) fails OR the project doesn't exist, fall
          back to `minio/warp` (`go install
          github.com/minio/warp@latest`) — `warp` supports
          most workloads via `warp mixed`, `warp put`,
          `warp get`, `warp multipart`, `warp list`, `warp
          delete`. JSON output via `--analyze.out=json`.
      (c) If (b) also fails, fall back to
          `dvassallo/s3-benchmark` (simpler — single-object
          PUT/GET only; multipart + list workloads need
          custom Go shim).
      (d) Document the chosen tool + version + reasoning
          in `progress.txt`. The chosen tool's flag mapping
          → bench script's workload functions becomes the
          contract for US-003..US-007.
- [ ] **`scripts/bench-rgw-comparison.sh`** scaffold:
      (a) Bench-tool path discovery + cache to
          `$HOME/go/bin/`.
      (b) Reads `STRATA_ENDPOINT_URL` (default
          `http://localhost:9999`),
          `STRATA_BENCH_CREDS_FILE`, `RGW_ENDPOINT_URL`
          (default `http://localhost:9991`),
          `RGW_BENCH_CREDS_FILE` (written by US-001
          entrypoint).
      (c) Positional args:
          `bench-rgw-comparison.sh <workload> <target>`
          where target ∈ {strata, rgw, both} and an optional
          `--concurrency=N` flag.
      (d) Per-workload run output: structured JSON one row
          per run (`{target, workload, object_size,
          concurrency, run_id, p50_ms, p95_ms, p99_ms,
          throughput_mbps, throughput_ops_per_sec,
          errors, duration_sec}`) appended to
          `scripts/bench-results/rgw-comparison-<date>.jsonl`.
      (e) `--report` flag aggregates jsonl into a markdown
          table with **mean ± stddev** across the 3 runs per
          workload (not just median).
      (f) Reference workload `put-small` (1KB PUT × 60s × 8
          concurrent × 3 runs) lands in this story as a
          working example.
- [ ] **Smoke run**: `bash scripts/bench-rgw-comparison.sh
      put-small both` against `make up-all` + `make
      up-bench-rgw` → produces 6 jsonl rows (3 runs × 2
      targets). Capture sample output in progress.txt.
- [ ] `make bench-rgw-comparison` Makefile target — wrapper
      for full sweep (estimated ~120 min, not 30).
- [ ] Typecheck passes; tests pass.

### US-003: Pre-flight health checks + cleanup discipline + single-cluster Strata override

**Description:** As an operator running the bench, I want
pre-flight checks that catch the lab in a bad state BEFORE the
bench wastes 2 hours producing garbage numbers. Also: explicit
cleanup discipline between workloads + a documented
single-cluster Strata bench mode so the dual-cluster bias is
controlled.

**Acceptance Criteria:**
- [ ] **Pre-flight checks** added to `bench-rgw-comparison.sh`
      base script — runs before any workload:
      (a) `curl -fs http://localhost:9999/readyz` →
          200, response asserts `data_backend=rados`
          (parse JSON; abort with explanatory error if
          `data_backend=memory` — bench is meaningless on
          memory backend).
      (b) `curl -fs http://localhost:9991/` → 200 (RGW
          responds to bucket-list root).
      (c) `df -h /var/lib/docker` (or
          `$HOME/Library/Containers/com.docker.docker` on
          macOS) ≥ **300GB** free (not 200 — multipart
          workload × 2 targets × 3 runs needs ~300GB
          transient).
      (d) `docker stats --no-stream` — total memory usage
          ≤ 6GB (room for bench tool overhead on an 8GB
          lima default).
      (e) `STRATA_RADOS_CLUSTERS` reflected on strata-a
          — log the value at bench start so the doc
          methodology can reference the actual config.
- [ ] **Single-cluster Strata bench mode**: new env override
      `STRATA_BENCH_SINGLE_CLUSTER=1` (consumed by the bench
      script). When set, the script restarts strata-a with
      `STRATA_RADOS_CLUSTERS=default:/etc/ceph-a/ceph.conf:/etc/ceph-a/ceph.client.admin.keyring`
      ONLY (drops `cephb`). Documented as the **fair-
      comparison** mode in US-008. Bench default IS
      single-cluster; multi-cluster benchmarking parked.
- [ ] **`cleanup_workload()` function** runs after each
      workload — drops the workload bucket, optionally polls
      `ceph df` to verify free-space recovery before next
      workload starts. Recovery wait capped at 30s (some
      cleanups are async — Strata's GC worker on a default
      5m interval won't finish in 30s; document this limitation
      in progress.txt).
- [ ] **Bucket isolation per workload**: each workload uses
      a uniquely-named bucket `bench-<workload>-<target>` so
      concurrent runs don't collide (even though the script
      is sequential — defensive shape).
- [ ] **Sanity assertion** between targets: after each
      workload run on target A, before running on target B,
      assert the workload bucket on A is gone (no leftover
      drift).
- [ ] Locally verified: pre-flight catches a fake
      `data_backend=memory` lab + a `<10GB` disk lab; both
      paths abort with the expected error.
- [ ] Typecheck passes; tests pass.

### US-004: PUT/GET 1KB + 1MB workloads with concurrency sweep

**Description:** As an operator running both low-latency
single-object workloads + bursty concurrent workloads, I want
p50/p99 + throughput across concurrency 1 / 8 / 32 / 128 so I
can see scaling characteristics, not just one operating point.

**Acceptance Criteria:**
- [ ] 4 workload functions:
      `put-small` (1KB PUT), `put-medium` (1MB PUT),
      `get-small` (1KB GET — seed bucket first),
      `get-medium` (1MB GET — same).
- [ ] **Concurrency sweep** per workload: run at concurrency
      ∈ {1, 8, 32, 128}, 60s wall-clock per concurrency point.
      That's 4 concurrency points × 4 workloads × 2 targets ×
      3 runs = **96 bench runs**. At ~75s avg per run
      (60s + 15s setup/teardown) ≈ 2h alone — but cleanup
      between concurrency points is light (same bucket,
      just clear keys).
- [ ] **Output**: jsonl row per `(target, workload,
      concurrency, run_id)` with stddev computable at report
      time.
- [ ] **Markdown table per workload** in
      `rgw-comparison.md`: 4-column-by-4-row grid (rows =
      concurrency points, cols = Strata p99 / RGW p99 / ratio
      / stddev) for each of the 4 workloads.
- [ ] Run against both targets. Capture jsonl.
- [ ] Conclusion line per workload — e.g. "Strata 1KB PUT
      scales sub-linearly at c=128 vs RGW; both saturate
      around 4000 ops/sec on lima lab."
- [ ] Typecheck passes; tests pass.

### US-005: PUT/GET 100MB workload (large objects)

**Description:** Large objects exercise different code paths
than small (chunking, manifest size, network buffer). Split
from US-004 so the concurrency sweep + large-object bench
each fit one Ralph iteration.

**Acceptance Criteria:**
- [ ] 2 workload functions: `put-large` (100MB PUT × 50 ops
      × 4 concurrent), `get-large` (same shape).
- [ ] 100MB × 50 ops × 4 concurrent × 2 targets × 3 runs =
      ~120GB transient; cleanup_workload() between runs
      mandatory.
- [ ] Capture: aggregate throughput MB/s, per-op p99,
      completion time.
- [ ] Run against both. Capture jsonl + doc update.
- [ ] Conclusion: Strata 4MiB chunking vs RGW striping (RGW
      uses 4MB striping by default → similar shape; expect
      close numbers).
- [ ] Typecheck passes; tests pass.

### US-006: Multipart 5GB workload

**Description:** Multipart 5GB upload throughput numbers.

**Acceptance Criteria:**
- [ ] `multipart-5g` workload: 5 parallel multipart uploads
      of 5GB each, part size 64MB. Total 25GB per run × 2
      targets × 3 runs = 150GB transient. Pre-flight check
      in US-003 already gates this.
- [ ] Bench tool invocation per the tool chosen in US-002
      (s3-bench `--operation multipart-put` or warp
      `multipart`).
- [ ] Capture: aggregate throughput MB/s, per-part p99
      latency, completion time per upload + stddev across
      runs.
- [ ] Run against both. Capture jsonl + doc update.
- [ ] Conclusion: multipart Complete CAS overhead on Strata
      (manifest LWT) vs RGW omap-index bookkeeping.
- [ ] Typecheck passes; tests pass.

### US-007: ListObjects 100k-key workload (bucket-index ceiling claim)

**Description:** ListObjects p99 for 100k-key bucket — THE
bucket-index claim Strata makes.

**Acceptance Criteria:**
- [ ] `list` workload function. Setup phase (idempotent —
      skip if bucket already has ≥ 100k keys): seed 100k
      keys via parallel PUT (1KB each, 32 concurrent). At
      ~4000 ops/s on lab, ~25s per target × 2 targets ≈ 1
      min seed.
- [ ] Bench phase: 100 ListObjectsV2 calls (`max-keys=1000`,
      paginate through all 100 pages), measure full-list
      completion time per call.
- [ ] Capture: p50 + p99 first-page latency, p50 + p99
      full-list completion time + stddev.
- [ ] Expected: Strata 64-way Cassandra/TiKV fan-out faster
      than RGW omap seq scan. **Confirm OR refute with
      numbers** — if Strata is slower, this is a CRITICAL
      finding (the bucket-index claim doesn't hold) and US-012
      MUST add new P1 ROADMAP entry (not P3).
- [ ] Run against both. Capture jsonl + doc update.
- [ ] Typecheck passes; tests pass.

### US-008: Range GET + Delete + IAM-authenticated workloads

**Description:** Range GET (streaming / analytics), Delete
(TTL / lifecycle workloads), IAM-authenticated request flow
(SigV4 + policy verify) numbers. Bundled because each is
relatively small.

**Acceptance Criteria:**
- [ ] 3 workload functions:
      `range-get` — 1000 range requests of random 1MB
      ranges against a single 100MB object, concurrency 8.
      `delete` — 1000 small objects deleted in concurrency 8
      after seed phase (mirror seed shape from US-004).
      `iam-auth` — pre-create IAM user via target's admin API
      (Strata: `POST /admin/v1/iam/users` confirmed in
      openapi.yaml; RGW: `radosgw-admin user create
      --uid=bench-iam --caps='buckets=read'` or equivalent —
      verify). Then 1000 GET ops with the limited
      credentials, concurrency 8.
- [ ] Capture: p50 + p99 + throughput + stddev per workload.
- [ ] Run against both. Capture jsonl + doc update.
- [ ] Typecheck passes; tests pass.

### US-009: Optional Cassandra-default sweep (3rd target)

**Description:** As an operator considering Strata's Cassandra
backend, I want optional bench numbers vs the TiKV-default
baseline. Triggered only by `--include-cassandra` flag — adds
~50% to bench duration.

**Acceptance Criteria:**
- [ ] New flag `--include-cassandra` to
      `bench-rgw-comparison.sh`. When set:
      (a) Brings up `make up-cassandra` (Cassandra-backed
          strata-cassandra on port 9998) alongside the
          existing TiKV stack.
      (b) Adds a 3rd target value `cassandra` to the
          target ∈ {strata, rgw, cassandra, all} arg.
      (c) Runs the same 8 workloads against `cassandra`
          target (Strata + Cassandra meta + RADOS data).
- [ ] All 3 targets share the same `default` RADOS cluster
      (single-cluster bench mode from US-003).
- [ ] **Result framing** in `rgw-comparison.md`:
      3-column comparison (Strata-TiKV / Strata-Cassandra /
      RGW). Headline narrative — "TiKV vs Cassandra on the
      same RADOS cluster shows X% delta; the choice is workload-
      dependent."
- [ ] Default bench run (without `--include-cassandra`)
      remains the 2-target comparison from US-004..US-008.
- [ ] Typecheck passes; tests pass.

### US-010: Report doc page + Methodology + Limitations + mermaid chart

**Description:** Comprehensive `/docs/architecture/benchmarks/
rgw-comparison/` doc page. Production-grade benchmark report
needs explicit methodology + limitations + reproducibility.

**Acceptance Criteria:**
- [ ] Author
      `docs/site/content/architecture/benchmarks/rgw-comparison.md`
      with these sections in order:
      (a) **Headline conclusion** — 2-3 sentences summarizing
          the bench (cite specific workloads where Strata
          wins/loses).
      (b) **Methodology** — lab CPU/RAM/disk, bench tool +
          version, Strata commit SHA, RGW image tag, stock-
          defaults caveat, single-cluster bench mode, the
          tool fallback chain documented in US-002.
      (c) **Limitations** (MANDATORY — production-grade
          disclosure): explicit 4-bullet list of biases:
          (1) Strata bench mode uses single RADOS cluster
              (multi-cluster default exists; benched mode
              matches RGW count).
          (2) Strata + RGW share OSD on same `ceph-a`
              cluster — no isolated-cluster bench.
          (3) localhost loopback (no real network latency
              vs production-grade network).
          (4) Docker Desktop / lima on macOS adds I/O
              penalty vs Linux native (caveat: relative
              comparison Strata vs RGW still valid since
              both run in same docker context).
      (d) **8 workload sections** — one per US-004..US-008
          + summary table with mean ± stddev p99 +
          throughput.
      (e) **Overall conclusion** — short paragraph: where
          Strata wins, where Strata is on par, where Strata
          loses + cause analysis.
      (f) **Reproducibility** — `make up-all && make
          up-bench-rgw && make bench-rgw-comparison` is the
          one-command reproduction path. Plus the optional
          `--include-cassandra` for the 3-way comparison.
- [ ] **Mermaid chart** (mermaid 11.9 supports
      `xychart-beta` — verified): one bar chart with 8
      workloads on x-axis + p99 ratio (Strata/RGW) on y-axis.
      Bars above 1.0 = Strata slower; below = Strata faster.
- [ ] Cross-link from `/operate/scaling.md` →
      `rgw-comparison.md`.
- [ ] `make docs-build` green; mermaid renders as SVG.
- [ ] Typecheck passes; tests pass.

### US-011: README features-vs-RGW backfill (grep-verified)

**Description:** Backfill README table cells with real
performance ratios. **Grep-verify** the table structure first
so the backfill doesn't break the `ralph/readme-docs-rewrite`
US-001 shape.

**Acceptance Criteria:**
- [ ] **Verify-first**: `grep -A 20 'Features vs RGW' README.md`
      → capture the existing table structure (column count,
      row labels). Document captured shape in progress.txt
      BEFORE editing.
- [ ] Backfill ONLY rows where bench numbers map cleanly:
      bucket-index scaling (US-007 conclusion), single-object
      PUT/GET (US-004), multipart (US-006). Other rows
      (online resharding, deployment shape, license)
      untouched.
- [ ] Each backfilled cell carries a footnote link to
      `/docs/architecture/benchmarks/rgw-comparison/#<workload>`
      so the reader can verify.
- [ ] Add a footer line under the table: "Bench numbers
      from `make bench-rgw-comparison` on lima/macOS
      M3 Pro; see [Limitations](/docs/architecture/benchmarks/
      rgw-comparison/#limitations)."
- [ ] **Fail-loud regression check**: `wc -l README.md`
      MUST stay ≤ 120 lines per the
      `ralph/readme-docs-rewrite` US-001 constraint.
- [ ] `make docs-build` green; backfilled README still
      renders correctly on GitHub.
- [ ] Typecheck passes; tests pass.

### US-012: Smoke + ROADMAP close-flip + new P3 entries + PRD removal

**Description:** As a future-maintainer, I want the bench
cycle verified end-to-end + ROADMAP entry flipped + any
slow-workload regressions captured as new P3 entries + PRD
removed.

**Acceptance Criteria:**
- [ ] Run `make up-all && make up-bench-rgw && make
      bench-rgw-comparison` end-to-end → all 8 workloads
      complete without errors; jsonl + markdown report
      generated. **Expected duration: ~120 min**
      (significantly more than the original ~30min PRD
      claim; document actual measured duration in
      progress.txt).
- [ ] `make smoke-tikv-default-lab` (existing) still passes.
- [ ] Run `make docs-build` → doc renders; mermaid chart +
      ratio table + Limitations section visible.
- [ ] Run `make vet` → green.
- [ ] Run `go test -race ./...` → green.
- [ ] **ROADMAP close-flip** × 1 on line 54 — `P2 —
      Benchmarks vs RGW` flipped to Done. Summary references
      US-001..US-011 + bench page URL + headline result +
      single-cluster bench mode disclosure + Limitations
      pointer.
- [ ] **NEW ROADMAP entries** (per CLAUDE.md "Discovering a
      new gap" rule) for any workload where:
      (a) Strata > 1.5× SLOWER than RGW → new P3 perf entry
          under `## Scalability & performance`.
      (b) Strata SLOWER on `list` workload (US-007) than RGW
          → NEW P1 entry (bucket-index claim regression —
          critical).
      (c) Strata Cassandra > 2× slower than Strata TiKV on
          same workload → NEW P3 entry for Cassandra perf
          investigation.
      Added open in same commit.
- [ ] `tasks/prd-rgw-benchmarks.md` REMOVED via `git rm`.
- [ ] `scripts/ralph/progress.txt` carries one US-012 block
      summarising 8-workload result + new P3 entries
      surfaced + lab box specs + actual bench duration vs
      estimate.
- [ ] Typecheck passes; tests pass.

## Functional Requirements

- FR-1: `bench-rgw` compose profile MUST spin a separate RGW
  container against the existing `ceph` cluster.
- FR-2: RGW MUST use stock defaults + minimal realm/zonegroup
  setup researched in US-001.
- FR-3: Bench tool MUST be verified to exist + support all
  required flags BEFORE workloads land (US-002 verify-first).
- FR-4: Pre-flight checks MUST gate the bench start —
  `data_backend=rados` assertion, disk ≥ 300GB free,
  Strata + RGW both healthy.
- FR-5: Strata bench mode MUST be single-cluster (default
  RADOS cluster only) for fair hardware-count comparison.
- FR-6: `cleanup_workload()` MUST run between workloads
  + verify free-space recovery (or document the
  GC-async limitation explicitly in progress.txt).
- FR-7: Per-workload report MUST capture mean ± stddev
  across 3 runs (not just median).
- FR-8: Concurrency sweep 1/8/32/128 MUST run for PUT-1KB,
  PUT-1MB, GET-1KB, GET-1MB workloads.
- FR-9: Doc page MUST carry a **Methodology + Limitations**
  section disclosing the 4 known biases (single-cluster, shared
  OSDs, loopback, lima penalty).
- FR-10: Optional Cassandra-default 3rd target via
  `--include-cassandra` flag.
- FR-11: README backfill MUST grep-verify the existing table
  structure + MUST keep README ≤ 120 lines + MUST add a
  footnote pointer to Limitations.
- FR-12: New ROADMAP entries gated by severity:
  Strata > 1.5× slower on any workload → P3;
  Strata slower on `list` → **P1** (bucket-index claim
  regression);
  Cassandra > 2× slower than TiKV → P3.
- FR-13: Bench MUST NOT be wired into CI matrix.
- FR-14: `make smoke-tikv-default-lab` MUST still pass.

## Non-Goals

- No comparison vs MinIO / SeaweedFS — RGW only target.
- No 3-replica horizontal-scale comparison (parked).
- No tuned RGW configuration column (parked — stock is honest).
- No CI integration (~120 min duration).
- No bench against external prod RGW.
- No multi-cluster Strata bench (parked — single-cluster
  fairness-mode is the explicit choice).
- No Go code changes in the binary — bench is shell + bench
  tool. Pre-flight `data_backend=rados` check uses the
  existing `/readyz` endpoint.

## Design Considerations

- **Bench tool fallback chain** (US-002 verify-first):
  wasabi-tech/s3-bench → minio/warp → dvassallo/s3-benchmark.
  Each subsequent fallback covers fewer workload classes;
  document which classes need custom shim if the chosen tool
  doesn't cover them.
- **RGW v19+ realm/zonegroup setup** — RGW since Jewel can't
  start without these. `ceph orch apply rgw` shortcuts via
  ceph-orchestrator; manual chain via `radosgw-admin realm
  create + zonegroup create + zone create + period update
  --commit`. Pick whichever is shorter for the entrypoint
  script.
- **Mermaid xychart-beta** — verified supported in mermaid
  11.9.0 (hugo-book ships `static/mermaid.min.js` with that
  version). Syntax:
  `xychart-beta` + `title` + `x-axis` + `y-axis` + `bar`.
- **Stddev computation** — `awk` one-liner over the 3 runs;
  no python dependency.
- **Concurrency sweep cost budget**: 4 workloads × 4
  concurrency × 2 targets × 3 runs × 60s = 4h IF run
  serially. Bench script bundles 3 runs per
  `(target, workload, concurrency)` into a single
  s3-bench/warp invocation (`--samples=3` or equivalent) to
  cut 30% overhead.

## Technical Considerations

- **`/readyz` data_backend field** — verify it exists today
  in the Strata response shape; if not, US-003 must FIRST add
  the field to `/readyz` response (small backend change) OR
  fall back to grep'ing `docker compose logs strata-a` for
  the data backend init line.
- **`docker stats --no-stream`** memory check on macOS lima
  reports the VM memory, not host memory. Document the
  reading caveat.
- **`ceph df` polling for cleanup recovery** is async — GC
  is on a 5m default tick; the 30s cap is an honest
  documented limitation, not a bug.
- **RGW user create idempotency** — `radosgw-admin user
  create` errors on duplicate; entrypoint uses `|| true`
  + follow-up `radosgw-admin user info` to fetch creds.
- **`s3-bench` flag verification** must happen in US-002
  BEFORE writing US-004..US-008 workload functions — they
  depend on the tool's flag shape.

## Success Metrics

- 8 workloads × 2 targets × 3 runs benched + concurrency
  sweep on 4 PUT/GET workloads.
- `rgw-comparison.md` doc page with Methodology +
  Limitations + mermaid + reproducibility + 8 workload
  sections.
- README features-vs-RGW backfilled with bench numbers,
  ≤ 120 lines preserved.
- 1 ROADMAP P2 entry closes; 0-3 new P3 entries (+ possible
  P1 if listing regresses).
- Cycle ships in 12 stories.
- **Actual bench duration ≤ 150 min** (15% buffer over
  120min estimate).

## Open Questions

- `/readyz` response shape carries `data_backend` field
  today? If not, US-003 either adds it (small backend
  change) or falls back to docker-logs grep. Verify before
  US-003 starts.
- RGW realm/zonegroup setup minimal command sequence —
  resolved in US-001 research phase.
- `jq` vs `python3` in `ceph/ceph:v19.2.3` image —
  resolved in US-001 entrypoint script.
