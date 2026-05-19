# PRD: Storage perf + correctness bundle

## Introduction

Storage perf + correctness bundle cycle. 10 stories: 5 perf items
(quota check fan-out, byte_seconds accuracy, RADOS op batching, RADOS
conn pool, OTel ringbuf eviction) + 1 correctness audit (Object Lock
COMPLIANCE) + 1 forward-compat schema (EC manifest) + 2 latent fixes
(Range zero-length, aws-chunked-trailer) + smoke story.

**Pre-launch product** — no existing deploys, no historical data, no
backwards-compat shims required. Hard cutovers fine on schema
changes. KMS + module-split parked to a separate Cycle 2
(`ralph/auth-dx`).

Branch: `ralph/storage-correctness`. Starts from `main`.

## Goals

- Lift `UserQuota.TotalMaxBytes` check from O(buckets-owned) to O(1)
  via `user_stats` denorm.
- Replace single-sample `byte_seconds` rollup math with N-sample
  trapezoid integration; hard cutover (no `method` column — pre-
  launch).
- Cut RADOS round-trips on PUT/GET hot path via WriteOp/ReadOp
  batching where bench shows ≥10% p99 improvement.
- Add `STRATA_RADOS_POOL_SIZE` env knob with round-robin pool;
  ship default > 1 only if bench shows ≥20% p99 PUT improvement.
- Bench OTel ring-buffer eviction at burst; bump default only if
  ≥30% retained-trace-age improvement at higher budget.
- Add dedicated `objectlock:*` audit row class for COMPLIANCE
  posture queries.
- Add EC-aware metadata to manifest + bucket model + admin
  endpoint with full validation against data backend capability
  probe (no pretend half-measure).
- Cover Range zero-length edge cases with tests.
- Implement aws-chunked-trailer parsing for sha256 checksum on
  the streaming decoder.

## User Journey

Five personas covered by the 9 functional stories:

- **Operator running a high-bucket-fan-out tenant.** Today every
  PUT into the tenant's bucket fans out to `ListBuckets(owner)` +
  N `bucket_stats` reads — pathological at N=1000 buckets-per-user.
  After cycle: O(1) `user_stats` row carries the aggregate.
- **Billing engineer running per-day byte-seconds invoicing.**
  Today a bucket that grows 0 → 1 TiB at noon bills as if 1 TiB
  all day (12 TiB·s over-state). After cycle: hourly trapezoid
  samples integrate to actual byte-seconds.
- **Operator running large-object PUT / GET workloads on RADOS.**
  Today every chunk PUT issues N round-trips (head xattr + chunk
  write separate). After cycle: WriteOp bundles them — measured
  ≥10% p99 gain or documented baseline showing batching didn't
  help.
- **Operator deploying with prom-operator + OTel exporter.** Today
  ring-buffer evicts traces under burst, p99 retention age
  uncertain. After cycle: bench-determined default + env knob
  surfaced for tuning.
- **Auditor querying COMPLIANCE posture per-bucket.** Today the
  `audit_log` has generic mutation rows. After cycle:
  `objectlock:CompliancePut` etc. as first-class audit verbs.
- **S3 client developer using aws-cli 2.22+.** Today PUT with
  chunked-trailer decodes to `ErrSignatureInvalid`. After cycle:
  trailer parsed + sha256 validated.

## User Stories

### US-001: user_stats denorm — O(1) UserQuota.TotalMaxBytes check

**Description:** As an operator, I want `UserQuota.TotalMaxBytes`
to be enforced via a single denormalised lookup so a tenant with
1000 buckets doesn't pay an O(N) fan-out tax on every PUT.

**Acceptance Criteria:**
- [ ] New `meta.UserStats {Owner string; UsedBytes int64;
      UsedObjects int64; BucketCount int}` struct in
      `internal/meta/store.go`.
- [ ] `meta.Store` gains `GetUserStats(ctx, owner) (*UserStats,
      error)` and the bump primitives `BumpUserStats(ctx, owner,
      deltaBytes int64, deltaObjects int64)` +
      `IncrUserBucketCount(ctx, owner, delta int)`.
- [ ] Memory impl in `internal/meta/memory/store.go` uses a
      `sync.RWMutex` and updates inside the same critical
      section that updates `bucket_stats` (single in-memory
      atomic).
- [ ] Cassandra impl in `internal/meta/cassandra/store.go`:
      new `user_stats` table (`owner text PRIMARY KEY,
      used_bytes counter, used_objects counter, bucket_count
      counter` OR LWT-shape if counters are not aligned with
      LWT discipline elsewhere — verify pattern in existing
      `bucket_stats` and match). Every existing
      `bucket_stats` bump site MUST batch a `user_stats`
      bump in the same `BEGIN BATCH ... APPLY BATCH` so
      consistency holds.
- [ ] TiKV impl in `internal/meta/tikv/store.go`: pessimistic
      txn that locks BOTH `bucket_stats:<bid>` and
      `user_stats:<owner>` keys, reads, computes deltas, writes,
      commits. Mirrors the existing `bucket_stats` txn shape.
- [ ] **TWO fan-out sites in `internal/s3api/quota.go` MUST
      be lifted** (verified via grep):
      (a) `userUsedBytes` (line 80-94) — replaces
          `Meta.ListBuckets(ctx, owner)` + per-bucket
          `Meta.GetBucketStats` loop with a single
          `Meta.GetUserStats(ctx, owner).UsedBytes`.
      (b) `checkUserBucketQuota` (line 101-120) —
          replaces `len(Meta.ListBuckets(ctx, owner))`
          with `Meta.GetUserStats(ctx, owner).BucketCount`.
- [ ] `CreateBucket` / `DeleteBucket` paths increment /
      decrement `user_stats.bucket_count` atomically with
      the bucket row creation / deletion (same LWT / txn).
- [ ] **`bucket_stats` is BIGINT-shaped** (not counter)
      per `internal/meta/cassandra/schema.go:226-231`. LWT
      batch shape works directly — no counter-to-bigint
      migration needed. `user_stats` follows the same
      bigint shape with LWT.
- [ ] Contract test in `internal/meta/storetest/contract.go`:
      seed 3 buckets across 2 owners, write 100 chunks each,
      assert `GetUserStats` reports correct totals; delete a
      bucket, assert totals drop accordingly; race-test the
      bump primitives via 100 concurrent goroutines.
- [ ] Cassandra integration test in
      `internal/meta/cassandra/store_integration_test.go`
      (build tag `integration`) exercises the LWT batch
      shape against testcontainers Cassandra.
- [ ] No backfill worker — pre-launch product, no existing
      rows to migrate (per [Pre-launch no deploys] memory
      rule).
- [ ] `go vet ./...` passes; `go test -race ./...` passes.

### US-002: byte_seconds trapezoid integration for usage rollup

**Description:** As a billing engineer, I want byte-seconds to
reflect actual sub-daily growth via trapezoid integration so a
mid-day bucket-fill doesn't over-state by 12 TiB·s.

**Acceptance Criteria:**
- [ ] New env knob `STRATA_USAGE_ROLLUP_SAMPLES_PER_DAY`
      (default 24 = hourly) in
      `cmd/strata/workers/usagerollup.go`. Range [1, 1440];
      out-of-range clamped + WARN-logged at Build time.
- [ ] Usage-rollup worker schedules N intermediate sampling
      ticks per day in addition to the daily roll-up tick.
      Each intermediate tick reads `bucket_stats.used_bytes`
      and stores the sample in an in-memory ring keyed by
      `(bucket_id, storage_class)`.
- [ ] On the daily roll-up tick, the worker emits one
      `usage_aggregates` row per `(bucket, storage_class,
      yesterday-UTC)` with `byte_seconds` computed as
      trapezoid integration over the N samples (`sum of
      (sample[i]+sample[i+1])/2 * delta_t`).
- [ ] **Hard cutover** — no `method` column on the
      `usage_aggregates` schema; old `byte_seconds = used ×
      86400` math removed. Pre-launch product, no historical
      rows.
- [ ] Memory + Cassandra + TiKV `usage_aggregates`
      schemas unchanged. Cassandra schema (verified):
      `(bucket_id, storage_class, day, byte_seconds,
      object_count_avg, object_count_max, computed_at)` PK
      `((bucket_id, storage_class), day)` per
      `internal/meta/cassandra/schema.go:232-241`. Plus
      sibling lookup `usage_aggregates_classes`.
- [ ] **`object_count_avg` + `object_count_max` columns**:
      verify worker.go current shape; if today's single-
      sample computes `avg = max = used_objects`, the
      trapezoid pass yields proper averages over the N
      samples (`avg = sum/N`, `max = max(samples)`).
      Preserve column semantics — billing already reads
      these.
- [ ] Unit test `internal/usagerollup/trapezoid_test.go`:
      table-driven sample sequences → expected byte_seconds.
      Covers (a) flat usage all day, (b) step-up at noon,
      (c) step-down at noon, (d) sawtooth, (e) single
      sample at start of day, (f) N=1 degrades to old
      single-sample math intentionally.
- [ ] Race-safe — sample ring uses sync.RWMutex; concurrent
      bumps from chunk PUT path don't corrupt.
- [ ] Worker test `internal/usagerollup/worker_test.go`
      exercises a full virtual day (`time.Now` via injectable
      `clock` interface) with 24 samples + daily flush.
- [ ] Documentation in
      `docs/site/content/best-practices/billing.md` (or
      wherever billing is documented; verify via `ls
      docs/site/content/best-practices/`): trapezoid math
      explained + the env knob.
- [ ] `go vet ./...` passes; `go test -race
      ./internal/usagerollup/...` passes.

### US-003: RADOS ReadOp / WriteOp batching

**Description:** As an operator running PUT-heavy workloads on
RADOS, I want chunk + head-xattr ops bundled into a single
librados WriteOp so the PUT path doesn't pay a round-trip per
xattr.

**Acceptance Criteria:**
- [ ] New file `internal/data/rados/ops.go` (build tag
      `ceph`) defining `(b *Backend) writeChunkBatched(ctx,
      pool, oid, data, xattrs map[string]string) error` that
      builds a `rados.WriteOp` via `goceph.CreateWriteOp()`,
      adds `Write` + per-xattr `SetXattr` ops, executes via
      `WriteOp.Operate(ioctx, oid, flags)`.
- [ ] Mirror stub file `internal/data/rados/ops_stub.go`
      (build tag `!ceph`) returning `ErrRadosNotCompiled`.
- [ ] Same pattern for `readChunkBatched(ctx, pool, oid)
      (data []byte, xattrs map[string]string, err error)`
      via `rados.ReadOp` with `Read` + `GetXattrs`.
- [ ] Retrofit existing `backend.go` `Put` / `Get` paths to
      call the batched variants. Behavior must be
      semantically identical from caller's POV (same return
      shape, same error classes including the
      `data.ErrChunkNotFound` lift from `ralph/polish-dx`).
- [ ] Bench harness `scripts/bench-rados-ops.sh`: drives
      ~10 k PUT + ~10 k GET against a single-cluster lab
      with `make up-all`, measures p50 / p95 / p99 latency
      via prom queries
      `histogram_quantile(0.99, sum by (le) (rate(
      strata_rados_op_duration_seconds_bucket[5m])))`.
      Runs baseline (no batching — temp env toggle
      `STRATA_RADOS_BATCH_OPS=false`) vs batched
      (`...=true`).
- [ ] **Ship-or-document gate**: if batched p99 PUT ≥ 10%
      better, ship batching as default (env knob removed,
      or default `true` with knob retained for opt-out).
      If gain < 10%, ship the code path BUT keep batching
      off-by-default + record the bench numbers in
      `docs/site/content/architecture/benchmarks/rados-ops.md`
      with the explanation "WriteOp batching shows X%
      improvement, below threshold; keeping op-per-call
      default for path simplicity".
- [ ] Build under both tags: `go build ./...` (stub) +
      `go build -tags ceph ./...` (real impl).
- [ ] Bench numbers captured in `progress.txt` for the
      story.
- [ ] `go vet -tags ceph ./...` passes; `go test -tags
      ceph ./internal/data/rados/...` passes (run inside
      ceph-base image if local helm doesn't carry
      librados).

### US-004: RADOS conn pool — bench-then-ship

**Description:** As an operator running write-heavy
workloads, I want multi-connection RADOS pooling so PUT
throughput doesn't bottleneck on a single `*rados.Conn`.

**Acceptance Criteria:**
- [ ] New env knob `STRATA_RADOS_POOL_SIZE` (default 1 =
      current single-conn). Range [1, 32]; out-of-range
      clamped + WARN-logged.
- [ ] New file `internal/data/rados/pool.go` (build tag
      `ceph`) defining `connPool struct { conns []*rados.Conn;
      counter atomic.Uint64 }` with `(p *connPool) Next()
      *rados.Conn { return p.conns[p.counter.Add(1) %
      uint64(len(p.conns))] }`. Round-robin counter (option
      5A from scoping).
- [ ] Stub mirror under build tag `!ceph` for compilation
      parity.
- [ ] `Backend` struct gains `pool *connPool` field
      replacing direct `conn` field. All `Backend` op sites
      consume `p.pool.Next()` instead of `b.conn`.
- [ ] Each conn in the pool is `Connect()`'d at backend
      construction; failures fail backend Build (no
      half-connected pool).
- [ ] Bench harness `scripts/bench-rados-pool.sh`: drives
      ~10 k PUT (large 4 MiB) + ~10 k GET workloads with
      `STRATA_RADOS_POOL_SIZE ∈ {1, 2, 4, 8}` against a
      single-cluster lab. Measures p99 + throughput. Output
      table to
      `docs/site/content/architecture/benchmarks/rados-pool.md`.
- [ ] **Ship-or-default gate**: if N=4 improves p99 PUT by
      ≥ 20%, set `STRATA_RADOS_POOL_SIZE` default to 4 in
      `cmd/strata/server.go` (or wherever defaults are
      seeded). Otherwise keep default 1 + document the bench
      numbers + the conclusion that multi-conn pooling
      wasn't sufficient win on the local lab.
- [ ] The env knob ships regardless of bench outcome —
      operators with different workloads can flip it.
- [ ] Bench numbers captured in `progress.txt`.
- [ ] `go vet -tags ceph ./...` passes; `go test -tags ceph
      ./internal/data/rados/...` passes (verify pool
      construction unit-test under mock conn factory).

### US-005: OTel ring-buffer eviction — bench-then-ship

**Description:** As an operator running burst traffic with
ringbuf=on, I want bench-validated defaults for the
`STRATA_OTEL_RINGBUF_BYTES` budget so I'm not eyeballing
4 MiB.

**Acceptance Criteria:**
- [ ] Bench harness `scripts/bench-otel-ringbuf.sh`: starts
      `make run-memory` with `STRATA_OTEL_RINGBUF=on` +
      varying `STRATA_OTEL_RINGBUF_BYTES ∈ {4, 8, 16, 32}
      MiB`. Drives 60 s of `hey -z 60s -c 100
      http://127.0.0.1:9000/test-bucket/$(rand-key)` via
      `hey` HTTP benchmarker.
- [ ] Per run, capture:
      (a) eviction rate via internal
      `strata_otel_ringbuf_evicted_total` counter (add if
      not present — count drops in
      `internal/otel/ringbuf.RingBuffer.Add`);
      (b) p99 trace retention age via
      `time_now - oldest_trace_ts` snapshot;
      (c) memory ceiling via `runtime.ReadMemStats`.
- [ ] Output table to
      `docs/site/content/architecture/benchmarks/otel-ringbuf.md`.
- [ ] **Ship-or-default gate**: if bumping default to 16 MiB
      gives ≥ 30% increase in p99 retained-trace-age vs
      4 MiB without ≥ 2× memory hit, bump the default to
      16 MiB in `internal/otel/Init`. Otherwise keep 4 MiB
      + surface the env knob more prominently in
      `docs/site/content/best-practices/web-ui.md` (or
      wherever ring-buffer is documented).
- [ ] Bench numbers captured in `progress.txt`.
- [ ] `go vet ./...` passes; `go test -race
      ./internal/otel/...` passes.

### US-006: Object Lock COMPLIANCE audit log

**Description:** As an auditor, I want a per-bucket query for
COMPLIANCE mode events so I can verify retention compliance
without scanning every `audit_log` row.

**Acceptance Criteria:**
- [ ] New audit verbs added to `s3api.SetAuditOverride`
      stamp logic in `internal/s3api/objectlock.go` (or
      wherever Object Lock handlers live; grep
      `ObjectLockRetention` to find):
      - `objectlock:CompliancePut` — stamped on
        `PutObjectRetention` with `Mode: COMPLIANCE`.
      - `objectlock:ComplianceRetentionAttemptedReduce` —
        stamped when a request would shorten COMPLIANCE
        retention; AWS S3 returns
        `AccessDenied`, but the audit row records the
        attempt regardless of outcome.
      - `objectlock:ComplianceRetentionExpired` — stamped
        when a worker passes the retention-elapsed boundary
        and the GC / lifecycle path runs (requires hook in
        lifecycle worker — verify path).
- [ ] Audit row carries `resource: object:<bucket>/<key>`
      + `principal: <Auth.Owner OR system:lifecycle-worker>`.
- [ ] Contract test in
      `internal/meta/storetest/contract.go`: assert
      COMPLIANCE PUT emits exactly one
      `objectlock:CompliancePut` row; that an attempted
      reduce emits exactly one
      `objectlock:ComplianceRetentionAttemptedReduce` row;
      that the retention-expired worker hook emits the third
      verb.
- [ ] Memory + Cassandra + TiKV all pass the new contract
      cases (no backend-specific shape).
- [ ] AWS S3 parity validated against
      `scripts/s3-tests/run.sh` Object Lock suite — no
      regression; the new audit rows are additive,
      doesn't affect S3 wire response.
- [ ] Documentation:
      `docs/site/content/best-practices/compliance.md` (or
      append to existing audit-log doc) explains the new
      audit verbs + the operator query shape
      (`audit_log WHERE action LIKE 'objectlock:%' AND
      resource LIKE 'object:<bucket>/%'`).
- [ ] `go vet ./...` passes; `go test -race
      ./internal/s3api/... ./internal/meta/...` passes.

### US-007: EC-aware manifest schema + admin endpoint with full validation

**Description:** As an operator managing RADOS EC pools, I
want a typed EC policy on the bucket + manifest schema
field so I can declare k+m parameters and have the gateway
validate the bucket placement actually routes to an
EC-capable pool.

**Acceptance Criteria:**
- [ ] `data.Manifest.ECParams *ECParams` proto field +
      JSON tag (`json:",omitempty"`). `ECParams struct { K
      int; M int }`. Tagged with a new proto field number
      in `internal/data/manifest.proto` — increment from
      last used.
- [ ] `meta.Bucket.ECPolicy *ECPolicy` field +
      Cassandra/TiKV/memory schema additive change.
      `ECPolicy struct { K int; M int }`. Memory: in-struct.
      Cassandra: `ALTER TABLE buckets ADD ec_policy text`
      (JSON-encoded struct); idempotent column-add per
      `alterStatements`. TiKV: persisted via `updateBucket`
      helper.
- [ ] New admin endpoint `PUT
      /admin/v1/buckets/{name}/ec-policy` body `{k: int,
      m: int}`. Audit verb `admin:UpdateBucketECPolicy`.
- [ ] **Full validation at PUT** (option 3B from scoping):
      - Read `bucket.Placement` (effective placement via
        `placement.EffectivePolicy`).
      - For each cluster the placement could route to, call
        new `data.Backend.ClusterECCapability(ctx,
        clusterID) (ec bool, k int, m int, err error)` —
        new optional interface on `data.Backend`.
      - RADOS impl in `internal/data/rados/ec.go` (build
        tag `ceph`) probes the target pool via `librados`
        `pool_get_metadata` (or equivalent — verify which
        API exposes `erasure_code_profile`).
        Returns `(true, k, m, nil)` for EC pools,
        `(false, 0, 0, nil)` for replicated.
      - S3 backend impl returns `(false, 0, 0, nil)` (S3
        backend doesn't expose EC at the gateway-S3 hop).
      - Memory backend impl returns `(false, 0, 0, nil)`
        (no underlying EC).
      - If ANY target cluster's `(ec, k, m)` doesn't match
        the requested `{k, m}` on the request body, return
        409 `InconsistentECPolicy` with body identifying
        which cluster mismatches.
- [ ] GET `/admin/v1/buckets/{name}/ec-policy` returns the
      stored policy or 404 if unset.
- [ ] DELETE `/admin/v1/buckets/{name}/ec-policy` clears
      the stored policy + audited as
      `admin:DeleteBucketECPolicy`.
- [ ] OpenAPI YAML updated with the new endpoint + its
      schema + the explicit doc note that this validates
      against data backend capability probe.
- [ ] Unit tests in `internal/adminapi/buckets_ec_test.go`:
      happy path (matching EC capability accepts), mismatch
      path (replicated pool + k+m request rejects with
      409), absent placement path (no policy assigned to
      bucket rejects with 409 `NoPlacement`).
- [ ] PUT path (`Put` in
      `internal/data/rados/backend.go`) consults
      `bucket.ECPolicy` if set: stamps `Manifest.ECParams`
      from the bucket policy. No EC encoding/decoding at
      the gateway — RADOS pool config still does the
      actual encoding.
- [ ] Decoder transparent for old rows (zero value).
- [ ] Contract test asserts: bucket without ECPolicy →
      manifest ECParams nil; bucket with ECPolicy →
      manifest ECParams populated; round-trip.
- [ ] No "metadata-only" disclaimer needed — full
      validation means operator can't set a bogus policy
      that doesn't match the underlying pool.
- [ ] `go vet -tags ceph ./...` + `go vet ./...` both pass.

### US-008: Latent fix — GET Range zero-length edge tests

**Description:** As an S3 client developer, I want
documented test coverage for `Range` headers against
zero-length objects so AWS parity is verified.

**Acceptance Criteria:**
- [ ] Add test cases to `internal/s3api/range_test.go` (or
      `get_test.go` — verify location of existing Range
      tests):
      - Zero-length object + `Range: bytes=0-` → 416
        `InvalidRange`.
      - Zero-length object + `Range: bytes=-10` → 200
        empty body (consistent with `Range: bytes=-N` on
        non-empty objects when N > size).
      - Zero-length object + `Range: bytes=0-9` → 416
        `InvalidRange`.
      - Empty object + no Range → 200 empty body
        `Content-Length: 0`.
- [ ] If existing handler returns wrong status, fix the
      handler in `internal/s3api/get.go` (or wherever GET
      lives — verify) to match AWS parity, NOT the test
      expectation.
- [ ] Cross-validate against the existing `s3-tests`
      runner output if it covers these cases — record
      pass-rate delta in `progress.txt`.
- [ ] `go vet ./...` passes; `go test -race
      ./internal/s3api/...` passes.

### US-009: Latent fix — aws-chunked-trailer streaming decoder

**Description:** As an aws-cli 2.22+ user, I want
chunked-trailer PUTs (with sha256 trailer checksum) to
authenticate correctly so my client doesn't hit
`ErrSignatureInvalid` on every PUT.

**Acceptance Criteria:**
- [ ] Parse `x-amz-trailer` request header in
      `internal/auth/streaming.go`. If header present and
      value is `x-amz-checksum-sha256`, switch the decoder
      to trailer-aware mode.
- [ ] Trailer-aware mode reads the chunked body as today
      (per `\r\n`-delimited chunks with
      `;chunk-signature=<hex>`), then on the final
      0-length chunk reads the trailing header block:
      `x-amz-checksum-sha256:<base64>\r\n` +
      `x-amz-trailer-signature:<hex>\r\n\r\n`.
- [ ] Validate `x-amz-trailer-signature` via the
      established chain-HMAC scheme — extending
      `prevSig` to cover the trailer chunk per AWS spec.
      Trailer signature mismatch → `ErrSignatureInvalid`.
- [ ] Validate the body sha256 matches the trailer
      checksum — accumulate while streaming, compare on
      EOF. Mismatch → `ErrSignatureInvalid` (NOT
      `ErrChecksumMismatch` — AWS surfaces this as
      sig invalid).
- [ ] Sha256-only scope this story per scoping decision
      (option 4A): `x-amz-trailer: x-amz-checksum-sha256`
      handled; other algos (`crc32`, `crc32c`, `sha1`)
      respond with `ErrUnsupportedChecksumAlgorithm` 400
      until a follow-up cycle. Document this in the
      `internal/auth/streaming.go` package comment +
      ROADMAP NEW P3 entry (per "Discovering a new gap"
      rule, captured in US-010).
- [ ] Test fixtures: capture a real aws-cli 2.22+
      `aws s3 cp file.bin s3://bucket/key` chunked-trailer
      PUT via `tcpdump` / `mitmproxy` (or hand-construct
      from the AWS spec doc), stash as
      `internal/auth/testdata/chunked-trailer-sha256.bin`.
- [ ] Unit test `internal/auth/streaming_trailer_test.go`:
      happy-path decode, trailer-signature mismatch,
      body-checksum mismatch, unsupported-algo header.
- [ ] Integration test (build tag `integration`) starts
      `make run-memory` + drives `aws-cli 2.22+` PUT
      against it via the existing `scripts/smoke.sh`
      shape; asserts 200 OK.
- [ ] `go vet ./...` passes; `go test -race
      ./internal/auth/...` passes.

### US-010: Smoke validation + ROADMAP close-flip + PRD removal

**Description:** As a future-maintainer, I want one
explicit verification pass that proves all 9 stories landed
correctly, plus the ROADMAP entries flipped + the PRD
markdown removed.

**Acceptance Criteria:**
- [ ] Run `make smoke` → green.
- [ ] Run `make smoke-signed` → green (covers SigV4 +
      streaming-trailer paths against US-009).
- [ ] Run full `go test -race ./...` (default tag) →
      green; capture duration.
- [ ] Run `go test -tags ceph ./...` (ceph tag) → green
      OR documented SKIP if local box has no librados;
      run inside `deploy/docker/Dockerfile` build for
      authoritative pass.
- [ ] Run `make test-integration` (Cassandra
      testcontainers) → green.
- [ ] Run `scripts/s3-tests/run.sh` → record pass-rate;
      should not regress vs main baseline. Update
      `scripts/s3-tests/README.md` baseline section per
      CLAUDE.md `## Commits and PRs` rule.
- [ ] Run `make vet` + `make docs-build` → green.
- [ ] All bench harnesses ran during the per-story work:
      `bench-rados-ops.sh`, `bench-rados-pool.sh`,
      `bench-otel-ringbuf.sh`. Numbers landed in
      `docs/site/content/architecture/benchmarks/*.md`.
- [ ] **ROADMAP close-flip** × 9 in the same commit
      (verify exact line numbers via grep before
      committing):
      - user_bucket_count denorm (P3 Scalability) → Done,
        references US-001 + the LWT batch shape.
      - byte_seconds trapezoid integration (P3
        Scalability) → Done, references US-002 + the new
        env knob + hard-cutover note (no method column).
      - RADOS ReadOp/WriteOp batching (P3 Scalability) →
        Done, references US-003 + the bench outcome.
      - RADOS connection pool tuning (P3 Scalability) →
        Done, references US-004 + the env knob + bench-
        determined default.
      - OTel ring-buffer eviction tuning (P3 Web UI) →
        Done, references US-005 + bench outcome + chosen
        default.
      - Object Lock COMPLIANCE audit log (P3
        Correctness) → Done, references US-006 + the
        three new audit verbs.
      - Erasure-code aware manifests (P3 Scalability) →
        Done, references US-007 + the full data-backend-
        probe validation (NO "metadata-only forward-compat
        only" disclaimer — full validation shipped).
      - Range zero-length edge (latent bug under Known
        latent bugs) → Done, references US-008.
      - Streaming chunked-trailer support (latent bug) →
        Done, references US-009 + sha256-only scope.
- [ ] **NEW P3 entry** added (per CLAUDE.md "Discovering
      a new gap" rule, from US-009): `Streaming chunked-
      trailer support for non-sha256 checksum algorithms
      (crc32, crc32c, sha1)` — parked open under
      `## Correctness & consistency`.
- [ ] Each close-flip carries `(commit pending)`
      placeholder; SHA backfill lands on `main` post-merge.
- [ ] `tasks/prd-storage-correctness.md` REMOVED via `git
      rm`.
- [ ] `scripts/ralph/progress.txt` carries one US-010
      block summarising smoke + bench results + s3-tests
      pass-rate.

## Functional Requirements

- FR-1: `meta.Store` MUST expose `GetUserStats`,
  `BumpUserStats`, `IncrUserBucketCount` and all three
  backends MUST keep `user_stats` consistent with
  `bucket_stats` via single LWT batch / single txn.
- FR-2: PUT-validate path MUST consult `Meta.GetUserStats`
  instead of `ListBuckets` fan-out for
  `UserQuota.TotalMaxBytes`.
- FR-3: `usage-rollup` worker MUST trapezoid-integrate N
  intermediate samples per day; N driven by
  `STRATA_USAGE_ROLLUP_SAMPLES_PER_DAY`.
- FR-4: RADOS `Put` / `Get` MUST use batched WriteOp /
  ReadOp when batching shows ≥ 10% p99 improvement;
  bench numbers MUST land in
  `docs/site/content/architecture/benchmarks/rados-ops.md`.
- FR-5: RADOS backend MUST support a connection pool sized
  by `STRATA_RADOS_POOL_SIZE` (round-robin selection);
  default chosen by bench outcome.
- FR-6: OTel ring-buffer default MUST be bench-validated;
  default bumped only if ≥ 30% retained-trace-age improvement
  vs current.
- FR-7: COMPLIANCE Object Lock events MUST emit
  `objectlock:CompliancePut`,
  `objectlock:ComplianceRetentionAttemptedReduce`,
  `objectlock:ComplianceRetentionExpired` audit rows.
- FR-8: `bucket.ECPolicy` MUST be validated at PUT against
  data backend EC capability probe; mismatched policies
  rejected with 409.
- FR-9: Manifest.ECParams field MUST be wire-compatible
  with old rows (zero-value decode).
- FR-10: `Range: bytes=...` semantics on zero-length
  objects MUST match AWS S3 (416 on start ≥ 0 with
  size=0; 200 empty on suffix forms).
- FR-11: Streaming decoder MUST parse `x-amz-trailer:
  x-amz-checksum-sha256` chunked-trailer PUTs from
  aws-cli 2.22+; unsupported algos respond with
  `ErrUnsupportedChecksumAlgorithm` 400.

## Non-Goals

- No KMS-backed per-bucket signing keys (Cycle 2
  `ralph/auth-dx`).
- No module split for go-ceph (Cycle 2).
- No EC encoding/decoding at the gateway — RADOS pool
  config still drives the actual EC math.
- No additional checksum algos beyond sha256 in
  US-009 (parked as new P3).
- No `method` column on `usage_aggregates` (pre-launch
  hard cutover per [Pre-launch no deploys] memory rule).
- No backfill worker for `user_stats` (pre-launch — no
  existing rows).
- No CI rewiring for the bench harnesses (operator-run,
  not CI-gated — would balloon CI minutes).

## Technical Considerations

- **LWT batch shape on Cassandra**: must verify whether
  `bucket_stats` today is a `counter` column (no LWT) or
  a `bigint` column (LWT-friendly). Counters can't go in
  LWT batches. If counter-shaped, switch both
  `bucket_stats` and `user_stats` to bigint via LWT — or
  use a separate non-LWT batch for counter-shape that
  accepts eventual consistency on the user-aggregate.
- **RADOS pool conn lifetime**: each `*rados.Conn` opens
  a separate librados handle. Test the bench harness on
  a Ceph monitor that doesn't enforce a per-client conn
  cap (default is high — verify).
- **EC capability probe latency**: `pool_get_metadata`
  hits the mon — not the OSD — so the latency is low
  but adds a round-trip per admin PUT. Cache the result
  with 60 s TTL keyed on `cluster_id + pool_name`.
- **`s3-tests` baseline** — record pre-cycle pass-rate;
  the only intentional change is from US-006 and US-009.
  Any other regression is a bug in the cycle.

## Success Metrics

- O(1) `UserQuota.TotalMaxBytes` check (US-001) —
  measured PUT latency on a 1000-bucket tenant drops to
  baseline-single-bucket latency.
- Byte-seconds error budget (US-002) — for a step-up
  bucket fill at hour-12, trapezoid integral vs single-
  sample is ≥ 8 TiB·s closer to ground truth.
- RADOS PUT p99 (US-003) — bench-measured improvement
  ≥ 10% to ship default; else env knob retained.
- RADOS pool throughput (US-004) — bench-measured PUT
  p99 improvement ≥ 20% at N=4 to ship default; else
  env knob default 1.
- OTel ringbuf trace retention (US-005) — bench-measured
  ≥ 30% improvement at higher default to ship.
- 3 new COMPLIANCE audit verbs queryable via standard
  audit_log scan.
- EC policy admin endpoint validates against data
  backend probe — bogus policies rejected.
- 9 ROADMAP entries close in one cycle + 1 new-and-
  parked.
- Cycle ships in 10 stories.

## Open Questions

- COMPLIANCE retention-expired hook in lifecycle worker
  — does the existing lifecycle worker have a hook for
  "retention boundary crossed" or does US-006 need to
  add one? Resolve in US-006 acceptance pass.
- RADOS `pool_get_metadata` API — exact go-ceph binding
  exposing erasure-code profile? May need to fall back to
  `mon_command` JSON parse if no direct API. Resolve in
  US-007 implementation.
