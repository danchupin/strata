# Strata roadmap

MVP (phases 0–8) is complete. The "modern complete" Ralph cycle (US-001..US-049 on
`ralph/modern-complete`) closed the bulk of the original P1/P2/P3 backlog: SSE-S3 + SSE-KMS,
notifications, replicator, access-log delivery, structured logs, full Prometheus + Grafana,
health probes, virtual-hosted URLs, audit log + export, per-part GET, versioning null literal,
website redirects, OTel tracing through Cassandra and RADOS, admin CLI, race harness, Inventory,
Access Points, ScyllaDB CI, multi-cluster RADOS, online shard resize, examples, protobuf manifest.

Next phase is **consolidation and validation**, not feature breadth. Items below are labeled
as before:
- **P1** — correctness or production-blockers
- **P2** — meaningful gaps; expected for serious deployments
- **P3** — nice-to-have, visibility, DX

S3-compatibility headline: **92.7% (165/178)** on the executable subset of `ceph/s3-tests`. See
`scripts/s3-tests/README.md` for the gap breakdown.

---

## Consolidation & validation (the new top of the stack)

These are not feature work. The codebase shipped a lot of surface in a short window; before
adding more, prove what is there.

- ~~**P1 — Single-binary `strata` (CockroachDB-shape).**~~ — **Done.** Two binaries
  (`strata`, `strata-admin`); `strata server` runs the gateway plus an opt-in subset of
  workers (gc, lifecycle, notify, replicator, access-log, inventory, audit-export,
  manifest-rewriter) selected via `STRATA_WORKERS=`. Each worker keeps its own
  `internal/leader` lease keyed on `<name>-leader`. SSE master-key rotation moved to
  `strata-admin rewrap`. One Docker image, one compose service. (commit `ae4e338`)
- ~~**P1 — Race harness as a real test, not a gate.**~~ — **Done.** Carved
  out `internal/racetest`, shipped `cmd/strata-racecheck` standalone binary,
  extended the workload to multipart + versioning + conditional + DeleteObjects
  with read-after-write + listing-convergence + version-monotonicity oracles,
  added the memory-tuned `ci`-profile compose stack (`make up-all-ci`),
  wired `make race-soak` + `scripts/racecheck/{run,summarize}.sh`, and
  scheduled `.github/workflows/race-nightly.yml` (03:00 UTC, ubuntu-latest,
  90 min budget). Zero-inconsistency baseline recorded at
  `docs/racecheck/baseline-2026-05.md`. (commit `3c04a05`)
- ~~**P1 — s3-tests 80% → 90%+.**~~ — **Done.** Lifted to **91.5% (162/177)** by the
  `ralph/s3-compat-95` cycle (US-001..US-006 — multipart copy range-parser + special-char
  URL handling, ?partNumber=N GET wire shape flipped to whole-object multipart ETag,
  CRC32 / CRC32C / CRC64NVME `FULL_OBJECT` composite combine math, multipart Complete
  `If-Match`-on-missing-object → 404 NoSuchKey AWS-parity, suspended-bucket GET
  stale-row dual-probe, missing-bucket DELETE → 404 ahead of auth, ListObjectVersions
  Owner DisplayName, validate-then-flip on Complete in cassandra+memory). The 4
  follow-up real failures filed as separate P2 entries below were closed by
  `ralph/s3-compat-finish` (US-001..US-003), lifting the headline to 92.7%
  (165/178). See `scripts/s3-tests/README.md` for the per-test gap breakdown.
  (commit `494b62b`)
- **P2 — Benchmarks vs RGW.** "Drop-in RGW replacement" is unproven without numbers. Run
  `warp` and `cosbench` against both gateways on the same RADOS cluster. Publish absolute
  latency / throughput per workload class (small-object PUT, large-object GET, listing,
  multipart) in a dedicated `docs/site/content/architecture/benchmarks/` directory. Update on each release.
- **P2 — ScyllaDB benchmarks.** `docs/site/content/architecture/backends/scylla.md` (US-042) documents the path; the
  expected 3–5× LWT speedup on Paxos hot paths (bucket-create, versioning-flip,
  multipart-complete) needs measurement. Same harness as the RGW benches, swap the
  metadata backend.
- ~~**P3 — Drop unused background daemons.**~~ — **Done.** Default compose stack
  is `cassandra + ceph + strata` (gateway + gc + lifecycle); the feature workers
  (notify / replicator / access-log / inventory / audit-export) live behind a
  single `--profile features` `strata-features` replica. (commit `5841043`)
- ~~**P1 — TiKV first-class metadata backend.**~~ — **Done.** `internal/meta/tikv`
  implements the full `meta.Store` surface; native ordered range scans via the
  optional `meta.RangeScanStore` interface short-circuit Cassandra's 64-way
  fan-out on `ListObjects` (~5× faster on a 100k-object bucket per
  `docs/site/content/architecture/benchmarks/meta-backend-comparison.md`). Wired through
  `STRATA_META_BACKEND=tikv` + `STRATA_TIKV_PD_ENDPOINTS`; ships with
  `--profile tikv` in compose, `make up-tikv` / `make smoke-tikv`,
  `.github/workflows/ci-tikv.yml`, race-soak coverage, contract suite parity,
  and `docs/site/content/architecture/backends/tikv.md`. Memory is now tests-only; the previously listed
  community backends (FoundationDB, PostgreSQL+Citus / Yugabyte) are dropped
  from the roadmap. (commit `40b45de`)

## Correctness & consistency

- **P2 — Default-routing weights + `pending` cluster state (safe gradual cluster activation).** Today a bucket without `meta.Bucket.Placement` policy falls back to `spec.Cluster` (class env default) — a single cluster regardless of how many are registered. Two consequences in multi-cluster deploys: (1) symmetric pool layout stays imbalanced unless operator sets per-bucket Placement everywhere; (2) adding a new cluster to env triggers no UX safety AND no gradual rollout — operator can't ramp traffic from 10% → 100% to observe under load. Fix scope: (a) extend state machine with new `pending` state — newly registered clusters auto-init as `pending`; operator validates + explicit `POST /admin/v1/clusters/{id}/activate {weight: N}` flips to `live` with initial weight N (default suggestion: 10/100); (b) new `cluster_weight int (0..100)` field per cluster (meaningful only when `state="live"`) — `placement.PickCluster` for nil-policy buckets synthesises policy `{<each-live-cluster-id>: <its-weight>}` so chunks distribute proportional to per-cluster weights; (c) `PUT /admin/v1/clusters/{id}/weight {weight: N}` adjusts weight while live — operator can step up 10→25→50→100 manually after observing each tier; weight=0 + state=live is legal (cluster keeps reads + serves explicit policies but receives no new default-routed writes); (d) class env `@cluster` suffix remains explicit pin (back-compat for operators who want single-cluster routing per class); (e) `pending` / `draining_readonly` / `evacuating` excluded from default-routing weight wheel regardless of weight field; (f) UI: `<Pending>` cluster card variant with badge + "Activate" button + confirm modal containing initial-weight slider (0..100, default 10); live cluster cards expose inline weight slider + numeric input (debounced PUT); tooltip explains "Share in default uniform routing for buckets without explicit policy"; (g) admin endpoints `POST /clusters/{id}/activate {weight}` (audit + 409 if already live) + `PUT /clusters/{id}/weight {weight}` (audit + 409 if not live); (h) migration on upgrade: existing clusters with chunks on them get state="live" weight=100 via one-shot admin endpoint OR auto-detect at boot via `bucket_stats` sum > 0 referencing the cluster (preserves existing routing — all weight=100 = uniform among already-live clusters). ~5 stories. NOTE: do NOT bundle into the running `ralph/drain-transparency` cycle — that cycle's state machine refactor is mid-flight (US-001 already passed); fold `pending` + weights as a 5th state + new field in a follow-up cycle after drain-transparency merges.
- **P2 — `POST /admin/v1/buckets/{name}/force-empty` leaks chunks in storage backend.** `internal/adminapi/buckets_delete.go::forceEmptyDrainPage` calls `s.Meta.DeleteObject(...)` per row but does NOT enqueue the returned `*meta.Object.Manifest.Chunks` into the GC queue. Meta rows + bucket_stats counters get cleaned, but the underlying RADOS / S3 chunks remain in the pool / bucket forever — no GC worker picks them up. Compare to the S3 DeleteObject path (`internal/s3api/server.go::deleteObject`) which explicitly calls `s.enqueueChunks(ctx, o.Manifest.Chunks)` after a successful `Meta.DeleteObject`. The admin path is missing that call. Severity: operator running "Force delete" on a 10 TiB bucket leaks 10 TiB of orphan RADOS objects. Fix scope: extract the enqueue helper into a place reachable from both admin + s3api packages (or inject the s3api enqueue function into `adminapi.Config`); add unit test that asserts chunks get enqueued after force-empty; integration test on the multi-cluster lab verifies `rados ls` count drops to 0 after force-empty completes.

- ~~**P2 — Multipart copy edges (UploadPartCopy).**~~ — **Done.** US-001 closed the
  copy-source-range parser (`internal/s3api/multipart.go::parseCopySourceRangeStrict`
  splits 400 InvalidArgument syntax errors from 416 InvalidRange out-of-bounds) and
  the special-char URL handling (`copy_object.go::parseCopySource` splits on `?`
  before `url.PathUnescape` so literal `?` in keys round-trips). `_improper_range`
  passes. The `_small` / `_special_names` / `_multiple_sizes` tests still fail on a
  separate axis (GET-side checksum echo on the destination — see new entry below).
  (commit `968a32a`)

- ~~**P2 — Multipart copy GET-side checksum echo divergence.**~~ — **Done.**
  Root-caused via `ralph/s3-compat-finish` baseline rerun: the failure was NOT
  destination-side checksum recompute drift but a Range-GET echo bug on the
  source object. boto3 1.36+ default-on FlexibleChecksum auto-sets
  `x-amz-checksum-mode: ENABLED` on every GET, the test's `_check_key_content`
  issues a `bytes=0-N` Range GET on the source, server emitted the
  whole-object `x-amz-checksum-*` (a stored digest covering every byte) and
  boto3 validated it against the partial response body — guaranteed mismatch.
  AWS suppresses checksum echo on Range responses; Strata now does the same
  in `internal/s3api/server.go::getObject`. US-001 (CRC64NVME / CRC32 / CRC32C
  empty-type → FULL_OBJECT default at multipart Initiate) and US-003 (Range-GET
  suppression) together flip all three tests
  (`test_multipart_copy_small`, `_special_names`, `_multiple_sizes`) green.
  (commit `d8aa9fa`)

- ~~**P2 — `?partNumber=N` GET quoted-ETag wire shape.**~~ — **Done.** US-002
  flipped the wire shape: `?partNumber=N` GET / HEAD now echoes the WHOLE-OBJECT
  multipart ETag (`"<32hex>-<count>"`), matching `complete_multipart_upload`'s
  response — not the per-part ETag. Out-of-range `partNumber` returns
  `400 InvalidPart` (was 416 InvalidRange). Non-multipart `?partNumber=1` returns
  the whole object 200 OK; `?partNumber=N` for N≥2 returns 400 InvalidPart. Three
  s3-tests pass: `test_multipart_get_part`, `_sse_c_get_part`, `_single_get_part`.
  (commit `6b4e304`)

- ~~**P2 — Multipart composite checksum CRC32 / CRC32C / CRC64NVME.**~~ — **Done.**
  US-003 added the AWS `FULL_OBJECT` composite shape for the CRC family in
  `internal/s3api/checksum.go::combineCRCParts` — standard zlib `crc32_combine` /
  `crc64_combine` math (gf2 matrix square+times) over per-part (CRC, size) yields
  the whole-stream CRC. SHA1 / SHA256 stay COMPOSITE (`BASE64(HASH(concat(raw_i)))-N`).
  Three s3-tests pass: `test_multipart_use_cksum_helper_crc32`, `_crc32c`,
  `_crc64nvme`. (commit `798ab58`)

- ~~**P2 — Multipart `If-Match`-on-missing-object error code.**~~ — **Done.** US-004
  reverted CompleteMultipartUpload's `If-Match`-on-missing-object branch from
  RFC 7232 §3.1's `412 PreconditionFailed` to AWS S3's `404 NoSuchKey`. The
  ETag-mismatch-on-existing-object path stays `412`. `putObject`'s single-PUT path
  is unchanged (per RFC). Two s3-tests pass: `test_multipart_put_object_if_match`,
  `_put_current_object_if_match`. (commit `8b5ca84`)

- ~~**P1 — Cassandra null-version timeuuid sentinel.**~~ — **Done.** Fixed in the
  prior `ralph/s3-compat-90` cycle (cassandra-internal v1 sentinel
  `00000000-0000-1000-8000-000000000000` translated at every gocql.ParseUUID /
  .String() boundary; `internal/meta/cassandra/store.go::versionToCQL`). The
  `ralph/s3-compat-95` cycle's US-006 added the missing CI coverage:
  `internal/meta/cassandra/store_integration_test.go::TestCassandraNullSentinelOnDisk`
  asserts the on-disk `objects.version_id` is the v1 sentinel while
  `meta.NullVersionID` surfaces the canonical zero UUID to clients, plus a
  Suspended-mode INSERT step in the storetest contract `caseVersioningNullSentinel`.
  CI integration job runs `go test -v` so the test name prints, catching skip
  regressions. (commit `494b62b`)

- ~~**P1 — `koanf` env provider does not skip empty env values.**~~ — **Done.**
  Fixed in the prior `ralph/s3-compat-90` cycle (`env.Provider` →
  `env.ProviderWithValue` with an empty-skip callback;
  `internal/config/config.go::Load`). The `ralph/s3-compat-95` cycle's US-006 added
  `internal/config/config_test.go::TestLoadEmptyEnvDoesNotOverrideTOMLValue` that
  routes through `STRATA_CONFIG_FILE` (TOML default `gc.interval=30s`) +
  `STRATA_GC_INTERVAL=""` and asserts `cfg.GC.Interval == 30*time.Second`. Locks
  the empty-skip callback against future regression. (commit `494b62b`)

- ~~**P2 — Multipart Complete leaks `completing` state on per-part ETag mismatch.**~~
  — **Done.** US-005 flipped the cassandra + memory backends' `CompleteMultipartUpload`
  to validate-then-flip: ListParts + per-part ETag walk runs first, only then does
  the LWT (cassandra) / in-place mutation (memory) flip status to `completing`. A
  stale-ETag retry now leaves the upload in `uploading` and a corrected retry
  succeeds. TiKV was already correct (deferred `rollbackOnError(txn, &err)` rolls
  the flip back on validation failure). Note: `test_multipart_resend_first_finishes_last`
  still fails on a separate axis (duplicate PartNumber in Complete XML — see new
  entry below). (commit `ba9368a`)

- ~~**P2 — Multipart Complete rejects duplicate PartNumber in Parts list.**~~ — **Done.**
  `internal/s3api/multipart.go::completeMultipart` strict-ascending check
  relaxed from `<= prev` to `< prev`; on equality the previously appended
  `meta.CompletePart` is overwritten with the LAST entry's ETag (AWS take-latest
  semantics) before the per-part walk. Storage-side `meta.Store.SavePart`
  last-write-wins on all three backends, so the LAST submitted ETag resolves
  against the LATEST stored part. `[1, 3, 2]` still rejects with
  `InvalidPartOrder`. s3-test `test_multipart_resend_first_finishes_last` flips
  to PASS. (commit `d8aa9fa`)

- **P3 — Object Lock `COMPLIANCE` audit log.** `audit_log` (US-022) records all
  state-changing requests, but a denied DELETE under `COMPLIANCE` is not flagged
  distinctly. Regulated customers want a queryable "blocked retention violation" feed —
  add a typed `audit.Event.Reason` field that `audit_log` reads to filter.

## Auth

- ~~**P3 — STS / assume-role.**~~ — **Done.** Minimal AssumeRole endpoint at `?Action=AssumeRole` (`internal/s3api/sts.go` + `internal/auth/sts.go`). Issues a temporary credential triple (`STSSession{AccessKey, Secret, SessionToken}`) with `DefaultSTSDuration` validity; verifier honours `SessionToken` on subsequent SigV4 requests. (commit `cec9c06`)
- **P3 — Per-bucket request signing keys (KMS-backed).** Rotate the signing material on
  a schedule, reject keys older than `STRATA_KEY_MAX_AGE`. Hooks onto the existing
  Vault provider.

## Scalability & performance

- ~~**P1 — gc / lifecycle workers serialise inside a single goroutine; throughput cap ~50–500 ops/s.**~~ — **Done.** Phase 1 shipped via the `ralph/gc-lifecycle-scale` cycle (US-001..US-005). Bounded `errgroup` inside the elected leader (`STRATA_GC_CONCURRENCY` / `STRATA_LIFECYCLE_CONCURRENCY`, default 1, max 256) lifts per-worker throughput ~9× (gc) / ~19× (lifecycle) on the canonical lab-tikv stack. Measured on N=10000 with `STRATA_DATA_BACKEND=memory` + TiKV: gc 11108 → 100275 chunks/s (c=1 → c=256, knee at c=64 with +11 % beyond), lifecycle 485 → 9150 objects/s (c=1 → c=256, no knee inside the swept range). Recommended production default: `STRATA_GC_CONCURRENCY=64`, `STRATA_LIFECYCLE_CONCURRENCY=64` (push to 128–256 if the meta backend has headroom). Bench harness lands as `strata-admin bench-gc` + `strata-admin bench-lifecycle`; results captured in `docs/site/content/architecture/benchmarks/gc-lifecycle.md`. (commit `cc5c7fb`)
- ~~**P1 — gc / lifecycle Phase 2 — sharded leader-election.**~~ — **Done.** Shipped via the `ralph/gc-lifecycle-scale-phase-2` cycle (US-001..US-007). gc gets `gc-leader-0..N-1` per-shard leases driven by `STRATA_GC_SHARDS` (default 1, range `[1, 1024]`); each replica races for one or more leases and drains via the new `Meta.ListGCEntriesShard` API on memory + Cassandra (`gc_entries_v2` partitioned on `(region, shard_id)`) + TiKV (new `s/qG/<region>/<shardID2BE>/<oid>` prefix). Lifecycle gets per-bucket leases (`lifecycle-leader-<bucketID>`) plus a `fnv32a(bucketID) % STRATA_GC_SHARDS == myReplicaID` distribution gate so each replica owns a strict subset of buckets. Multi-leader bench (US-006) on the 3-replica `lab-tikv-3` lab confirms the expected ~3× aggregate ceiling at `STRATA_GC_SHARDS=3` / `STRATA_GC_CONCURRENCY=64` (gc ≈ 250–270k chunks/s, lifecycle ≈ 18–19.5k objects/s) — full curve in `docs/site/content/architecture/benchmarks/gc-lifecycle.md` (Phase 2 — multi-leader). Cassandra + TiKV cutover runs through `STRATA_GC_DUAL_WRITE` (default `on`); operator runbook in `docs/site/content/architecture/migrations/gc-lifecycle-phase-2.md`. Single-replica deploy at `STRATA_GC_SHARDS=1` reproduces Phase 1 behaviour byte-for-byte. (commit `3743931`)
- ~~**P2 — Dynamic RADOS / S3 cluster registry + zero-downtime add.**~~ — **Won't-do.** Design decision (2026-05-11): cluster set stays config-driven via env (`STRATA_RADOS_CLUSTERS` for RADOS; `STRATA_S3_CLUSTERS` to be added for S3). Adding a new cluster requires a gateway restart, but multi-instance deployments hide per-instance downtime — rolling restart of N replicas keeps the service available throughout. A prior attempt at the dynamic registry (`ralph/dynamic-clusters`, merged `cba53b5`, reverted `055ef26`) shipped `meta.ClusterRegistry` + admin API + RADOS watcher; rolled back because single-source-of-truth via env is simpler operationally + tests are easier when config is static. The remaining gap is the S3-backend half: today `internal/data/s3/backend.go` is single-bucket-per-instance; covered by a separate P2 entry below for env-driven multi-cluster S3.
- ~~**P2 — Env-driven multi-cluster S3 data backend.**~~ — **Done.** Shipped via the `ralph/s3-multi-cluster` cycle (US-001..US-006). `internal/data/s3/Backend` lifted to `clusters map[string]*s3Cluster` + `classes map[string]ClassSpec` with per-cluster `*awss3.Client` + `*manager.Uploader` lazy-built via `connFor`. Two new envs replace the 13 retired `STRATA_S3_BACKEND_*` envs: `STRATA_S3_CLUSTERS` (JSON array of bucket-less `S3ClusterSpec`s — endpoint / region / force_path_style / part_size / upload_concurrency / max_retries / op_timeout_secs / sse_mode / sse_kms_key_id / credentials) + `STRATA_S3_CLASSES` (JSON object mapping storage class → `{cluster, bucket}` — both REQUIRED per class). Credentials never plaintext — `CredentialsRef` discriminator (`{type:"chain"}` / `{type:"env", ref:"VAR1:VAR2"}` / `{type:"file", ref:"/path:profile"}`); `env:` + `file:` probed at boot, `chain` defers to SDK first-connect. Bucket arg routing through `resolveClass` on every data-plane method (Put / Get / Delete / Head / Copy / List / PutChunks / GetChunks / Presign / Multipart) — the multipart handle was widened to `cluster\x00bucket\x00key\x00uploadID` so Complete / Abort recover routing without re-walking the class table. Test fixture `s3test.NewFixture(t)` collapses inline per-file setup (-216 LOC across six test files). Operator guide: [S3 multi-cluster routing](docs/site/content/best-practices/s3-multi-cluster.md). Adding / removing a cluster requires gateway restart — multi-replica rolling restart hides per-instance downtime. NOT in scope: chunk-side data migration (covered by P2 'Per-bucket placement policy + cross-cluster rebalance worker' below); KMS-fetched cluster credentials (P3 follow-up). (commit `ccae6ab`)
- ~~**P2 — Per-bucket placement policy + cross-cluster rebalance worker.**~~ — **Done.** Shipped via the `ralph/placement-rebalance` cycle (US-001..US-007). Per-bucket policy lives on `meta.Bucket.Placement` (`{cluster: weight}` map, validated weight ∈ [0,100] + sum>0, JSON blob persisted via `setBucketBlob` on memory / Cassandra / TiKV); `internal/data/placement.PickCluster` routes via stable `fnv32a("<bucketID>/<key>/<chunkIdx>") % sum(weights)` walk over sorted cluster ids so the same chunk always lands on the same cluster across retries. RADOS + S3 `Backend.PutChunks` consult the policy via ctx-threaded helpers (`data.WithPlacement` / `WithObjectKey` / `WithBucketID`); legacy buckets with `Placement == nil` short-circuit to the class default — zero schema or behavior change. New `strata server --workers=rebalance` (leader-elected on `rebalance-leader`, env knobs `STRATA_REBALANCE_INTERVAL` / `_RATE_MB_S` / `_INFLIGHT` clamped + WARN-logged) walks every bucket with a policy, plans per-chunk moves whose current cluster ≠ verdict, and dispatches through a `MoverChain`: RADOS mover (`Read(srcIoctx) → Write(tgtIoctx, fresh OID)`) and S3 mover (`awss3.CopyObject` server-side when endpoint+region match, else streaming `GetObject → manager.Uploader.PutObject`). Per-object manifest CAS via `meta.Store.SetObjectStorage` with pre-CAS sanity check on the live chunk locator; CAS losers (old chunks on success, new on reject) routed to GC via `meta.EnqueueChunkDeletion`. Safety rails refuse moves into `draining` clusters (new `cluster_state` row backed by meta + admin API `POST /admin/v1/clusters/{id}/drain|undrain` + 30s-TTL in-process `placement.DrainCache` invalidated on flip) and (RADOS only) into clusters above 90% fill via optional `data.ClusterStatsProbe` capability that runs `MonCommand({"prefix":"df"})`; refusals bump `strata_rebalance_refused_total{reason,target}`. Metric family `strata_rebalance_planned_moves_total` / `_bytes_moved_total{from,to}` / `_chunks_moved_total{from,to,bucket}` / `_cas_conflicts_total{bucket}` / `_refused_total{reason,target}`; trace shapes `worker.rebalance.tick` / `rebalance.scan_bucket` / `rebalance.move_chunk`. `data.BackendRef.Cluster` populated at PutChunks time so the rebalance scan can emit a single virtual move per S3 object. Operator runbook: [Placement + rebalance](docs/site/content/best-practices/placement-rebalance.md). Operator workflow `register → set Placement → drain old → rebalance → deregister` is now zero-downtime. (commit `97d4984`)
- ~~**P3 — Pools table shows class routing config, not actual cluster distribution.**~~ — **Done.** Shipped via the `ralph/drain-lifecycle` cycle (US-001). `internal/data/rados/health.go::DataHealth` + `internal/data/s3/health.go::DataHealth` rewritten to iterate `(cluster, distinct-pool)` matrix instead of `class → spec.Cluster`. The Pools table now returns `#clusters × #distinct-pools` rows (e.g. 2 clusters × 3 pools = 6 rows in the multi-cluster lab) with per-cluster `BytesUsed` populated independently per cluster via `ClusterStatsProbe.GetPoolStats(pool)`. Memory backend keeps the single `Cluster=""` synthetic row. Multi-class-per-pool aggregates to a comma-joined sorted class label, matching the memory + S3 convention. Operators can now see the true per-cluster footprint that drives drain decisions. (commit `9bb1b36`)
- ~~**P3 — Drain strict mode for PUT routing fallback.**~~ — **Done.** Shipped via the `ralph/drain-lifecycle` cycle (US-002 + US-004). New env `STRATA_DRAIN_STRICT` (default `off`, accepts `on`/`off`/boolean strings; unknown → fail-fast at boot) plumbed into `internal/data/rados/Config` + `internal/data/s3/Config`. When `on`, RADOS + S3 `Backend.PutChunks` refuse to fall back to a draining cluster (`data.ErrDrainRefused` carrying the resolved cluster id) — gateway maps the sentinel to HTTP `503 ServiceUnavailable` with `<Code>DrainRefused</Code>` body + `Retry-After: 300` header. **PUT only** — reads, deletes, HEAD, multipart Complete/Abort, List against draining clusters all keep working (drain semantic is stop-write, not stop-read). Counter `strata_putchunks_refused_total{reason="drain_strict",cluster}` increments per refusal. `GET /admin/v1/clusters` surfaces the boot-time value as a top-level `drain_strict: bool` field; the operator console renders a "strict" chip per cluster card so the global flag is visible without a separate fetch. Companion drain-lifecycle UX (US-003..US-006): `GET /admin/v1/clusters/{id}/drain-progress` reads the rebalance worker's in-process `ProgressTracker` (chunks_on_cluster + base + ETA + deregister_ready); `<DrainProgressBar>` + green "Ready to deregister" chip; per-tick completion detection logs INFO `drain complete`, writes a `drain.complete` audit row, bumps `strata_drain_complete_total{cluster}`, and best-effort fans `s3:Drain:Complete` through `STRATA_NOTIFY_TARGETS`. Pre-drain bucket-impact preview via `GET /admin/v1/clusters/{id}/bucket-references` + `<BucketReferencesDrawer>` + amber "All clusters in this policy are draining" chip on Bucket Placement tab. Operator runbook: [Placement + rebalance — Drain lifecycle](docs/site/content/best-practices/placement-rebalance.md#drain-lifecycle). Smoke walkthrough: `scripts/smoke-drain-lifecycle.sh` (driven by `make smoke-drain-lifecycle` against the `multi-cluster` compose profile). (commit `9bb1b36`) *Follow-up:* `STRATA_DRAIN_STRICT` env removed in `ralph/drain-transparency` (commit `<pending>`) — drain is now unconditionally strict; the `drain_strict` admin field, the "strict" UI chip, and the `reason="drain_strict"` counter label were retired alongside the env. See the new P3 *Drain transparency + drain/evacuate split* entry above.
- **P2 — Content-addressed object deduplication.** Today every chunk gets a fresh random OID even when two objects share identical bytes — duplicate uploads waste full-copy storage. Fix scope: chunk OID = `dedup/<sha256(content)>`; new `chunk_refcount` table in `meta.Store` keyed on OID; PUT path hashes the chunk, checks refcount, increments + skips RADOS write if the blob exists, else writes + sets refcount=1; DELETE / lifecycle-expire decrements; GC only deletes the underlying RADOS blob when refcount hits 0. Edge cases: (a) SSE-S3 / KMS — same plaintext encrypts differently per-object DEK, so dedup is incompatible with default SSE unless the operator opts into `dedup-friendly` mode where the DEK is derived from `hash(plaintext)` (weakens crypto independence; flag explicitly in `docs/sse.md` so operators understand the tradeoff); (b) hash hot-path cost — ~500 MB/s per core sha256 is acceptable; (c) cross-class dedup is opt-in (separate pools per class still mean separate storage even for same content); (d) manifest schema unchanged — chunk references stay `{Pool, OID, Length}` whether OID is random or content-addressed.
- ~~**P2 — Bucket / user quotas + usage-based billing.**~~ — **Done.** Shipped via the `ralph/bucket-quotas-billing` cycle (US-001..US-011). Per-bucket `BucketQuota{MaxBytes, MaxObjects, MaxBytesPerObject}` + per-user `UserQuota{MaxBuckets, TotalMaxBytes}` persisted across all three meta backends (memory + Cassandra + TiKV) via shared `internal/meta/quota.go` JSON codec. Live `bucket_stats{bucket_id, used_bytes, used_objects}` updated atomically on every PUT / DELETE / multipart-Complete (memory mutex / Cassandra LWT-CAS loop / TiKV pessimistic txn) and read at PUT-validate time by `internal/s3api/quota.go::checkQuota` — overage rejects with `403 QuotaExceeded` (RGW shape). Drift correction via leader-elected `--workers=quota-reconcile` (env `STRATA_QUOTA_RECONCILE_INTERVAL` default 6h, gauge `strata_quota_reconcile_drift_bytes{bucket}`); nightly aggregation via leader-elected `--workers=usage-rollup` writes one `usage_aggregates` row per `(bucket_id, storage_class, day)` (envs `STRATA_USAGE_ROLLUP_AT` default `00:00`, `STRATA_USAGE_ROLLUP_INTERVAL` default 24h) — single-sample `byte_seconds × 24h` v1 approximation, continuous-integration is a P3 follow-up. Admin API surface (`GET/PUT/DELETE /admin/v1/buckets/{name}/quota` + `/iam/users/{user}/quota` + per-bucket / per-user usage history) wired into `internal/adminapi/openapi.yaml`. Web UI: per-bucket Usage tab on BucketDetail + new `/iam/users/:userName/billing` page with cross-bucket breakdown + Edit Quota dialogs. Operator guide: [Quotas + billing](docs/site/content/best-practices/quotas-billing.md). Out of scope this cycle (kept on roadmap as P3 follow-ups below): invoice ledger / payment integration; continuous-integration `byte_seconds`; denormalised `user_bucket_count`. (commit `f2973db`)
- **P3 — Continuous-integration `byte_seconds` for usage rollup.** v1 single-sample approximation: rollup samples `bucket_stats` once per day and writes `byte_seconds = used_bytes × 86400`. A bucket that grows from 0 → 1 TiB at 12:00 UTC bills as if it had 1 TiB all day (over-states by 12 TiB·s). Fix scope: per-bump emit a `(bucket_id, storage_class, ts, used_bytes)` event the rollup integrates over the day; OR sample at higher cadence and trapezoid-integrate. Acceptable bounded error → low priority unless billing accuracy becomes a tenant ask.
- **P3 — Denormalised `user_bucket_count` for `UserQuota.TotalMaxBytes` checks.** PUT-validate fans out via `ListBuckets(owner)` to sum `bucket_stats` across the user's buckets — O(buckets-owned) on every write. Cheap for typical workloads; pathological at high bucket-fan-out per user. Fix shape: maintain `user_stats{user, used_bytes, used_objects, bucket_count}` updated atomically alongside `bucket_stats` and `CreateBucket` / `DeleteBucket`. Mirrors the bucket-stats pattern and lifts the user-scope check to O(1).
- ~~**P2 — Parallel chunk upload in `PutChunks`.**~~ — **Done.** Shipped via the `ralph/parallel-chunks` cycle (US-001). Bounded errgroup worker pool dispatches RADOS chunk writes concurrently while a single dispatcher goroutine owns the byte-stream MD5 hasher; manifest order + ETag (MD5 of source bytes) preserved regardless of completion order. Knob `STRATA_RADOS_PUT_CONCURRENCY` (default 32, range `[1, 256]`) read at `rados.Backend` constructor. Scheduler lives in tag-free `internal/data/rados/parallel.go` (librados-free unit tests). Multi-cluster manifests (US-044) handled automatically — worker resolves per-chunk ioctx via existing `b.ioctx(...)`. (commit `7d341f9`)
- ~~**P2 — Parallel chunk read / prefetch in `GetChunks`.**~~ — **Done.** Shipped via the `ralph/parallel-chunks` cycle (US-002). Bounded-depth prefetch reader fetches up to `STRATA_RADOS_GET_PREFETCH` chunks in flight while the caller drains the current chunk; default depth 4 (16 MiB inflight memory budget per request), range `[1, 64]`. Memory-bounded via semaphore + per-chunk size-1 future channel — peak buffered-but-unconsumed bytes ≤ depth × chunk_size. Range-GET (`offset`, `length`) still short-reads first/last chunks. Close cancels in-flight fetches and waits for goroutines within 500 ms (no leak — verified by `runtime.NumGoroutine()` baseline test). Scheduler lives in tag-free `internal/data/rados/prefetch.go` (librados-free unit tests). Bench harness `BenchmarkGetChunks_*_Prefetch` shows 3.7×–4.5× wall-clock speedup at 5 ms per-OSD latency; numbers in [Parallel chunk PUT + GET]({{< ref "/architecture/benchmarks/parallel-chunks" >}}). (commit `7d341f9`)
- **P3 — Erasure-code aware manifests.** For EC pools, track k+m parameters in the
  manifest for restore-path optimizations and accurate space accounting.
- **P3 — `ReadOp` / `WriteOp` batching in RADOS.** Bundle the head xattr read with the
  first chunk read in one OSD op (single round-trip for small objects).
- **P3 — Connection pool tuning.** Benchmark one `*rados.Conn` vs several for write-heavy
  workloads; measure CGO contention inside librados.

## Web UI

- ~~**P2 — Web UI — Foundation (Phase 1).**~~ **Done.** Embedded React+TS console served at `/console/` on the gateway port (`go:embed` + SPA fallback). Versioned `/admin/v1/*` JSON API + OpenAPI 3.1 spec at `internal/adminapi/openapi.yaml`. Session-cookie auth (HS256 JWT, 24 h, `HttpOnly`+`SameSite=Strict`+`Path=/admin`) backed by the existing static-credentials store, with SigV4 fallback for programmatic clients. Pages: login, cluster overview (CockroachDB-shape hero + nodes table + top-buckets + top-consumers widgets), buckets list (search/sort/paginate), bucket detail (read-only object browser with folder navigation + object detail panel), metrics dashboard (request rate / latency p50/p95/p99 / error rate / bytes — 15m/1h/6h/24h/7d ranges). Heartbeat infra in `internal/heartbeat` (memory + Cassandra; 10 s write, 30 s TTL). TanStack Query 5 polling (5 s default, per-range overrides on Metrics). Recharts 2 lazy-loaded. Bundle ≤500 KiB gzipped initial. Critical-path Playwright e2e (`web/e2e/critical-path.spec.ts`) running in CI under the `e2e-ui` job. Operator guide at `docs/site/content/best-practices/web-ui.md`. (commit `e27cf21`)
- ~~**P3 — Web UI — Phase 2 (admin).**~~ — **Done.** 22 stories: bucket admin (create / delete with force-empty job / versioning + object-lock toggle / lifecycle / CORS / policy / ACL / inventory / access-log), IAM users + access keys + managed policies (attach/detach), object upload (per-part presigned + Web Worker progress) / delete / tags / retention / legal-hold, multipart watchdog (cluster-wide list + bulk abort), audit-log viewer, settings (JWT secret rotation + S3-backend config + BackendPresign toggle). Playwright e2e: `web/e2e/admin.spec.ts` covers the five Phase 2 critical paths. `docs/site/content/best-practices/web-ui.md` capability matrix lists all 20 admin surfaces. (commit `5a6058b`)
- ~~**P3 — Web UI — Phase 3 (debug).**~~ — **Done.** 15 stories: SSE audit-tail (broadcaster + live-tail page with virtualised list, pause/resume, reconnect backoff), slow-queries (`total_time_ms` audit column + `ListSlowQueries` across memory/cassandra/tikv + filter/histogram UI), OTel trace ring buffer (in-process bytes-budgeted LRU with per-trace span cap, ringbuf-served via `/admin/v1/diagnostics/trace/{requestID}`) + waterfall renderer (depth-first bar layout, span detail sheet, recent-trace history, optional Jaeger deep link), hot-buckets heatmap (PromQL `sum by (bucket) (rate(...))` + custom canvas heatmap component, no @nivo dep), hot-shards heatmap (`strata_cassandra_lwt_conflicts_total{bucket,shard}` instrumentation + per-bucket tab with s3-backend explainer + drill panel), per-node drilldown drawer (5 PromQL sparklines via `instance="<addr>"` filter), bucket-shard distribution (per-shard sampler in `bucketstats` + Distribution tab with skew detection), replication-lag chart (`strata_replication_queue_age_seconds{bucket}` gauge + Replication tab gated on `replication_configured`). Playwright e2e `web/e2e/debug.spec.ts` covers five Phase 3 critical paths. `docs/site/content/best-practices/web-ui.md` capability matrix lists the eight new debug surfaces. (commit `7677cdd`)
- **P2 — Trace browser has no list view.** `internal/otel/ringbuf.RingBuffer` exposes `GetByRequestID` / `GetByTraceID` only — no `List(limit) []TraceSummary`. The UI's "Recent traces" panel reads from `localStorage`, populated only when the operator successfully opens a trace by id. Without an id, the page is search-only — operators can't discover what's in the ringbuf. Fix scope: add `RingBuffer.List(limit, offset)` returning the LRU front-N as `{request_id, trace_id, root_name, started_at, duration_ms, status}` summaries; expose via `GET /admin/v1/diagnostics/traces?limit=50`; render in the existing TraceBrowser page above the search box (sortable by start time, click → load full trace by request_id).
- ~~**P2 — TiKV meta backend emits no trace spans.**~~ — **Done.** `internal/meta/tikv/observer.go` wraps every public `Store` method with `Observer.Start(ctx, op, table)` returning a `finish(err)` closure that emits `meta.tikv.<table>.<op>` client-kind child spans (attrs `db.system=tikv`, `db.tikv.table`, `db.operation`, `strata.component=gateway`). `tikv.Config.Tracer` is wired by `internal/serverapp.buildMetaStore` via `tp.Tracer("strata.meta.tikv")`. Failing ops flip span status to Error so the tail-sampler exports regardless of `STRATA_OTEL_SAMPLE_RATIO`. Operator filter recipes live at `docs/site/content/best-practices/tracing.md`. (commit `2ccd209`)
- ~~**P3 — S3-over-S3 data backend emits no trace spans.**~~ — **Done.** `internal/data/s3/observer.go::installOTelMiddleware` registers the upstream `go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-sdk-go-v2/otelaws` middleware (v0.68, semconv v1.40) at `connFor` BEFORE the metrics instrumentation, so otelaws Initialize-after brackets the full retry loop and emits one client-kind `S3.<Operation>` span per SDK call with `rpc.system.name=aws-api` / `rpc.method=S3/<op>` / `aws.region` / `http.response.status_code` attrs; a custom `AttributeBuilder` stamps `strata.component=gateway` + `strata.s3_cluster=<id>`. `cfg.Tracer = tp.Tracer("strata.data.s3")` wired in `internal/serverapp.buildDataBackend::case "s3"`. (commit `2ccd209`)
- **P3 — OTel ring-buffer eviction tuning under burst load.** The 4 MiB default + per-trace 256-span cap was sized by hand. Run a burst-load harness (`hey -z 60s -c 100 …` against `make run-memory` with ringbuf=on) and measure (a) eviction rate, (b) p99 trace retention age, (c) memory ceiling vs configured budget. Document the observed cap and either bump the default or expose `STRATA_OTEL_RINGBUF_BYTES` more prominently in `docs/site/content/best-practices/web-ui.md`.
- ~~**P3 — Web UI — TiKV heartbeat backend.**~~ — **Done.** `internal/meta/tikv/heartbeat.go` implements `heartbeat.Store` against the TiKV transactional client. Rows live under `s/hb/<nodeID>` with a JSON payload carrying `ExpiresAt = LastHeartbeat + DefaultTTL`; readers lazy-skip expired rows and writers eager-delete up to 16 expired rows per write so the prefix does not leak disk. Wired in `internal/serverapp.buildHeartbeatStore`. (commit `c37487b`)
- ~~**P3 — Heartbeat `leader_for` chip wired to actual lease state.**~~ — **Done.** `cmd/strata/workers.Supervisor` now exposes a buffered (cap 8) `LeaderEvents()` channel emitting `(workerName, acquired)` on every per-worker lease acquire/release; `internal/heartbeat.Heartbeater.SetLeaderFor(worker, owned)` mutates `Node.LeaderFor` under a mutex and the next write tick (~10 s) propagates to the cluster_nodes row consumed by Cluster Overview. `internal/serverapp.Run` wires a goroutine from `Supervisor.LeaderEvents()` into `hb.SetLeaderFor`. (commit `6f81734`)
- ~~**P3 — Multi-replica lab (TiKV).**~~ — **Done.** New `lab-tikv` compose profile spins up two TiKV-backed Strata replicas (`strata-tikv-a`, `strata-tikv-b`) behind an `nginx` LB at host port 9999, sharing a JWT secret via the `strata-jwt-shared` named volume (file-based atomic bootstrap via POSIX `O_EXCL`). `Supervisor.LeaderEvents()` → `Heartbeater.SetLeaderFor` propagates lease rotation into the Cluster Overview `leader_for` chip within ~35 s of a holder kill. `scripts/multi-replica-smoke.sh` (target `make smoke-lab-tikv`) drives 5 host-side scenarios; `web/e2e/multi-replica.spec.ts` mirrors the same in a `[multi-replica]`-gated CI job (`e2e-ui-multi-replica`). Operator guide at `docs/site/content/deploy/multi-replica.md`. (commit `9e36975`)
- ~~**P3 — Web UI — Storage status (meta + data backend observability).**~~ — **Done.** New `/storage` page (Meta + Data tabs + per-class card), Cluster Overview "Storage" hero card, and top-level `<StorageDegradedBanner>` above the AppShell. Backed by `meta.HealthProbe` (Cassandra `system.peers`+`system.local` merge with 10 s in-process cache; TiKV bootstrap-only `pdclient` against `/pd/api/v1/stores`; memory single-row), `data.HealthProbe` (RADOS `(*IOContext).GetPoolStats()` + `(*Conn).MonCommand({"prefix":"status"})`; S3-over-S3 `HeadBucket`; memory RSS proxy), and the `bucketstats` sampler extended with optional `ClassSink`+`Snapshot` for cluster-wide per-(class) totals (cardinality bound by `STRATA_BUCKETSTATS_TOPN`; cadence via new `STRATA_BUCKETSTATS_INTERVAL`). New endpoints `/admin/v1/storage/{meta,data,classes,health}`; aggregate `/health` honors `STRATA_STORAGE_HEALTH_OVERRIDE` for e2e. Playwright spec `web/e2e/storage.spec.ts` exercises page render, hero chip, and degraded-banner dismissal. Operator guide at `docs/site/content/architecture/storage.md`. (commit `cde5581`)
- ~~**P3 — Drain transparency + drain/evacuate split (always-strict).**~~ — **Done.** Shipped via the `ralph/drain-transparency` cycle (US-001..US-008). 4-state machine `{live, draining_readonly, evacuating, removed}` with mode picker on `POST /drain {mode:"readonly"|"evacuate"}` (no default — 400 if missing); 4-state transitions enforced server-side with 409 `InvalidTransition` payload. Worker scan categorizes chunks (`MigratableChunks` / `StuckSinglePolicyChunks` / `StuckNoPolicyChunks` + per-(cluster, bucket) `ByBucket` breakdown) and runs ONLY when `state=evacuating`; `draining_readonly` keeps stop-write semantics with no migration scan. New `GET /admin/v1/clusters/{id}/drain-impact` pre-evacuate analysis returns categorized counters + per-bucket `suggested_policies[]` (uniform-live + per-live-single-target); paginated, 5-min in-process cache. `<ConfirmDrainModal>` redesigned with radio mode picker + impact analysis on evacuate flip + amber stuck-warning panel that blocks submit while `stuck>0`; new `<BulkPlacementFixDialog>` lets the operator fix N affected buckets in one workflow (per-bucket suggestion picker + "Apply uniform to all" toggle, intersection-of-labels). `<DrainProgressBar>` rewritten — readonly renders a single orange stop-writes chip with Upgrade/Undrain buttons; evacuate renders a red label + three categorized counters + clickable stuck cells opening `<StuckBucketsDrawer>` + ETA (migratable-only) + deregister-ready emerald chip on `total_chunks=0`. Always-strict refactor: `STRATA_DRAIN_STRICT` env retired (legacy values log one WARN at boot and are ignored); `drain_strict` field on `/clusters` + "strict" UI chip removed; counter relabeled to `strata_putchunks_refused_total{reason="drain_refused",cluster}` (breaking dashboard change). In-flight multipart graceful contract preserved by the existing handle shape (`cluster\x00bucket\x00key\x00uploadID`) — UploadPart / Complete / Abort decode the cluster directly and never re-consult the picker. Smoke + Playwright: `scripts/smoke-drain-transparency.sh` (`make smoke-drain-transparency`) drives three scenarios end-to-end against the `multi-cluster` lab (A: stop-writes drain + in-flight multipart graceful, B: full evacuate with impact analysis + bulk-fix, C: upgrade readonly→evacuate); `web/e2e/drain-transparency.spec.ts` mirrors the UI half. Operator runbook rewritten at [Placement + rebalance — Drain lifecycle]({{< ref "/best-practices/placement-rebalance#drain-lifecycle" >}}). (commit `<pending>`)
- ~~**P3 — Web UI — Placement + cluster surfacing.**~~ — **Done.** Shipped via the `ralph/placement-ui` cycle (US-001..US-006). `data.PoolStatus` carries `Cluster string` end-to-end (RADOS + S3 + memory); Storage page Data tab renders a `<ClustersSubsection>` (one `<Card>` per registered cluster with id + state badge + backend chip + aggregated `bytes_used`) above the Pools table, which gained a Cluster column with empty-last sort. Each cluster card carries a `<ConfirmDrainModal>` typed-confirmation Drain button (mirrors `DeleteBucketDialog` — exact case-sensitive match arms submit) and a one-click Undrain. New BucketDetail Placement tab between Access Log and Hot Shards renders one row per registered cluster (slider + paired numeric `<Input>` two-way bound, draining clusters carry a `(draining)` chip but stay editable) wired to `PUT/DELETE /admin/v1/buckets/{name}/placement` with a confirmation Dialog on Reset. AppShell-level `<PlacementDrainBanner>` polls the shared `clusters` TanStack key (dedup with `<ClustersSubsection>` and `<BucketPlacementTab>`), renders only when ≥1 cluster is `state=draining`, and supports per-session dismissal stamped on `localStorage` keyed to the sorted draining-id set (new cluster entering draining → stamp differs → banner returns). New `GET /admin/v1/clusters/{id}/rebalance-progress` admin handler `fmt.Sprintf`s per-cluster PromQL (`sum(strata_rebalance_chunks_moved_total{to="<id>"})` + `sum(strata_rebalance_refused_total{target="<id>"})` + a 1h/1m rate range) and graceful-degrades to `metrics_available=false` on Prom-unset or upstream error; the per-card `<RebalanceProgressChip>` renders `"N chunks moved · M refused"` + an inline Recharts sparkline (h-6, no new chart libs) and silently degrades to `"(metrics unavailable)"` instead of toasting on transient Prom blips. Playwright spec `web/e2e/placement.spec.ts` exercises the full operator flow (login → /storage cluster cards → create bucket → slider save → typed-confirmation Drain → banner → Undrain → Reset to default) by spoofing the spoof-friendly admin endpoints via `page.route()` so the in-memory gateway is sufficient. Operator runbook gained a [Web UI section](docs/site/content/best-practices/placement-rebalance.md#web-ui); the console capability matrix at `docs/site/content/best-practices/web-ui.md` lists the four new surfaces. (commit `32804ba`)

## S3 API surface

- **P3 — Intelligent-Tiering.** Access-time tracking + auto-transition. Needs hot/cold
  access counters per object.
- **P3 — Select / Select Object Content.** SQL over CSV/JSON/Parquet in place. Large
  effort for narrow win.
- **(out of scope) — Object Lambda.** Storage layer should not host user code.

## Developer experience

- **P3 — Module tags cleanup.** `github.com/ceph/go-ceph` is in `go.mod` regardless of
  `-tags ceph`. A `go mod tidy` without the tag removes it, breaking reproducibility. Fix
  by wrapping the import in a default-on tag file, or pinning it as an explicit `require`.
- **P3 — `make dev` for one-command developer cluster.** Single command that bootstraps
  Cassandra + Ceph + the consolidated `strata` binary and streams logs.
- **P3 — Architecture decision records.** Move the design notes captured below into
  `docs/adr/` once external contributions start.
- ~~**P2 — Full project documentation site (GitHub Pages).**~~ — **Done.** Hugo site under `docs/site/` (CockroachDB-shape sectioned tree: landing, Get Started, Deploy, Architecture, Best Practices, S3 Compatibility, Reference, Developers), `make docs-serve` / `make docs-build`, GitHub Action `.github/workflows/docs.yml` publishes to `gh-pages` on every merge to main, README banner links to `https://danchupin.github.io/strata/`. (commit `c497a66`)
- **P3 — OpenAPI viewer (Redoc / Swagger UI) embedded in the API reference section.** `internal/adminapi/openapi.yaml` is the canonical Admin-API contract; today the docs site has no rendered viewer. Embed Redoc (or Swagger UI) under `docs/site/content/reference/admin-api.md` so operators can browse + try-out admin endpoints inline. Pulls from the YAML at build time so drift is impossible.
- **P3 — Helm chart packaging for Kubernetes deployment.** `deploy/k8s/` ships apply-tested example manifests (Deployment + Service + ConfigMap + Secret + Ingress); operators wanting templated values + chart releases must wrap them by hand. Author a `deploy/helm/strata/` chart with `values.yaml` covering replicas / image / env / resources / ingress / TLS, wire `helm lint` into CI, document `helm install strata deploy/helm/strata/ -n strata` as the alternate flow alongside the raw manifests in `docs/site/content/deploy/kubernetes.md`.
- **P3 — Reference section expansion (env vars table, Admin API surface, S3 API operations table).** `docs/site/content/reference/_index.md` is a placeholder. Author three reference pages: every `STRATA_*` env var with default + range + cross-link to the consuming layer; full Admin API surface (mirrors / cross-links the OpenAPI viewer once that lands); S3 API operations table mapping every supported S3 operation to its handler + ROADMAP entry. Source-of-truth grep against `internal/...` so the table stays drift-proof.

---

## Alternative metadata backends

Strata supports two production metadata backends: **Cassandra** (with **ScyllaDB** as a
CQL-compatible drop-in — zero code changes, gocql works unchanged, CI matrix landed in
US-042) and **TiKV** (raw KV via `tikv/client-go`, native ordered range scans short-circuit
Cassandra's 64-way fan-out via the optional `meta.RangeScanStore` interface; ships with
`docs/site/content/architecture/backends/tikv.md`, `docs/site/content/architecture/benchmarks/meta-backend-comparison.md`, and
`.github/workflows/ci-tikv.yml`). Both are first-class — the core team benchmarks,
documents, and maintains both paths. Memory is for tests only.

Headline gap from `docs/site/content/architecture/benchmarks/meta-backend-comparison.md`: TiKV's native ordered
range scan finishes a 100k-object `ListObjects` in **30–50 ms** vs Cassandra's
64-way fan-out + heap-merge at **150–300 ms** — **~5× faster** on the listing hot path.
LWT-equivalent operations (`CreateBucket`, `CompleteMultipartUpload`) are ~1.5–2× faster
on TiKV pessimistic-txn vs Cassandra Paxos; small-object Get hot paths
(`GetObject`, `GetIAMAccessKey`) are dominated by network RTT and look comparable.
Cassandra wins on audit retention (`USING TTL` is free; TiKV runs an explicit
sweeper).

The `meta.Store` interface stays intentionally minimal (LWT semantics, clustering-order
reads, range scans). Capability-specific features (e.g. native range scans across
partitions) land behind **optional interfaces** that a backend opts into:

```go
// In internal/meta. Optional, not required by Store.
type RangeScanStore interface {
    Store
    ScanRange(ctx, bucketID, start, end string, limit int) (*ListResult, error)
}
```

Gateway code uses type-assertion (`if rs, ok := store.(RangeScanStore); ok {...}`) to
pick the better code path when available, falling back to the fan-out/heap-merge default
otherwise. TiKV implements `RangeScanStore`; Cassandra explicitly does not (the fan-out
path is the only shape Cassandra serves efficiently).

Non-goals:
- A backend that cannot honor at least `LOCAL_QUORUM`-equivalent semantics. Single-node-
  consistent-only stores (Redis standalone, SQLite) will never be a supported production
  path.
- Backends that cannot represent the `(bucket_id, shard, key, version_id DESC)` clustering
  natively. Anything slower than O(page_size) per page on `ListObjects` is not acceptable.

## Known latent bugs

- GET with `Range: bytes=start-` where `start >= size` returns `416` — same as AWS.
  `Range: bytes=-N` with `N > size` returns full body — matches AWS. Edge cases around
  zero-length objects: not tested.
- Streaming chunked decoder assumes `\r\n` strictly and reads via `bufio`. Does not handle
  `aws-chunked-trailer` (newer aws-cli variants). aws-cli 2.22 observed to use plain
  `x-amz-content-sha256: <hex>` for `s3api put-object` and STREAMING for `s3 cp`, both
  tested working.
- Lifecycle worker has no retry on transient failures — next tick re-tries.
- **`TestThreeReplicaDistribution` is flaky.** `internal/lifecycle/distribute_test.go:111`
  seeds 9 buckets with random UUIDs and asserts each of 3 replicas processes 1..5 buckets
  via `bucketReplicaIndex(bucket.ID, 3)`. With 9 random UUIDs the FNV-32a distribution can
  legitimately produce 6/3/0 or 6/2/1 splits — observed flake rate ~30 % under `-count=20`.
  The flake reproduces on `main` independent of US-003 changes. Fix: either seed UUIDs from
  a fixed PRNG so the split is deterministic, or relax the guard to `0..6` (the test still
  pins the `totalDeletes == buckets` invariant a few lines above, which is the real
  correctness signal).
- **`gc.Worker.drainCount` infinite-loops when `Data.Delete` fails persistently.**
  `internal/gc/worker.go:123-126` logs the warn + returns `nil` from the goroutine
  *without* ack'ing the entry; the outer `for {}` loop re-issues `ListGCEntries`
  and gets the same batch back. Any non-retryable error (RADOS ENOENT for an OID
  already swept by a sibling leader, pool not found, mis-routed cluster id) wedges
  the worker on a single batch forever. Surfaced by the Phase 1 bench harness
  (`strata-admin bench-gc` against the real `strata.rgw.buckets.data` pool with
  synthetic OIDs). Fix: ack on ENOENT (the chunk is already gone — that's the
  terminal state) and on any non-retryable RADOS error class. Out of scope for
  the gc-lifecycle-scale Phase 1 cycle; bench numbers in `docs/site/content/architecture/benchmarks/gc-lifecycle.md`
  were captured against `STRATA_DATA_BACKEND=memory` to bypass the spin.

---

## Design notes captured during MVP and "modern complete"

Documented in `memory/project_strata.md` (internal) and in commit messages. A few that
deserve a dedicated `docs/adr/` entry:

- Why we skip RADOS omap entirely (the thing RGW uses and we are replacing).
- Why `IsLatest` is derived at read-time from clustering order, not flipped on every PUT.
- Why `go-ceph.NewConnWithUser("admin")` takes the short ID, not `client.admin`.
- Why the runtime image is based on `quay.io/ceph/ceph:v19.2.3` (matching librados version,
  multi-arch) instead of stock debian librados (stale at v16).
- Why `data.Manifest` lives in a JSON-encoded blob column instead of normalised columns —
  schema-additive evolution without `ALTER`.
- Why each background worker is leader-elected separately rather than co-locating them in
  a single supervisor (and why that choice is being reconsidered — see Consolidation
  section above).
- Why the protobuf manifest (US-048/049) ships behind a decoder-first migration: every
  reader handles both shapes for one full release before the writer flips.
