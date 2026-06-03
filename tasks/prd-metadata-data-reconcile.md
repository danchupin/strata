# PRD: Metadata ↔ data reconcile & index rebuild

## Introduction

Strata separates the S3-key→chunk map (the **metadata** tier: TiKV / Cassandra)
from the chunk **bytes** (the **data** tier: RADOS). That separation is the
source of Strata's scale advantage over Ceph RGW — but it also removes a safety
net RGW gets for free by colocating its bucket-index with the data. Today a
RADOS chunk is an opaque, random-OID blob: `data.ChunkRef = {Cluster, Pool,
Namespace, OID, Size}` carries **no back-reference** to the object it belongs to
(verified in `internal/data/manifest.go`; the `writeChunkBatched` `SetXattr`
path exists but is unwired — "until xattrs are added to the PUT/GET hot path").

The map is therefore **one-directional**: metadata → data, never data →
metadata. Two consequences, neither solved by any database backup:

1. **Restore skew is silent corruption.** Data (RADOS) and metadata (TiKV /
   Cassandra) are backed up by *different* systems on *different* schedules — they
   can never be a single consistent cut. After any restore the tiers disagree at
   the edges: chunks with no manifest (storage leak, GC can't see them — GC walks
   meta→data), and manifests pointing at chunks the data restore doesn't have
   (GET → 5xx / corrupt body). An ideal operator with perfect backups of *both*
   tiers still hits this, because it is structural, not a backup failure.
2. **No last-resort rebuild.** If a metadata backup is itself lost or corrupt,
   there is no way to reconstruct the manifest index from the surviving RADOS
   bytes. The objects are intact on disk and permanently unreadable.

This PRD adds the back-reference and the reconcile / rebuild tooling that makes
a restore *usable* and gives Strata the disaster escape hatch RGW already has.

### Scope boundary (decided with the user)

**The storage tier's own backup is NOT Strata's responsibility.** TiKV BR,
Cassandra `nodetool snapshot`, RPO/RTO, retention — the operator owns those for
the database they run; Strata does not reinvent `br` / `nodetool`. This PRD is
**only** the Strata-owned half: the back-reference (a product feature) and the
two-tier reconcile/rebuild (recovery tooling). A short ops-docs note states the
shared-responsibility line.

### Design prior art (this is an established pattern, not a novel risk)

Self-describing data + a reconcile pass is the standard answer for every system
that splits metadata from object storage:

- **Lustre LFSCK** — the closest analog. MDT (metadata) + OST (object storage);
  every OST object carries its parent **FID** in an xattr; `lfsck` scans OSTs,
  reads the FID, reconciles against the MDT, repairs orphans and dangling refs.
  This PRD is structurally the same: xattr back-reference + reconcile worker.
- **HDFS block reports** — NameNode (metadata) + DataNodes (blocks); blocks are
  self-describing; the block→location map is **rebuilt from DataNode reports**,
  not treated as the single source of truth. `hdfs fsck` is the reconcile.
- **GFS / Colossus** — the master does **not** persist chunk locations; it
  rebuilds them from chunkserver reports at startup. Locations are recoverable by
  design, not a single point of failure.
- **Ceph RGW itself** — ships `radosgw-admin bucket radoslist` /
  `rgw-orphan-list` / `rgw-gap-list` precisely because its index (omap) and its
  RADOS data objects can diverge. RGW objects carry a locator. Strata dropped the
  omap index for scale (ADR-0001) and must re-add the locator deliberately.

Counter-reference: **MinIO** avoids the problem by colocating `xl.meta` with the
data on the same disks — no split, no skew, but no independent metadata scaling
(the very thing Strata exists for). The architectural choice is binary:
colocate (problem never arises) or split + back-reference/reconcile (Strata,
Lustre, HDFS, GFS). Split *without* the back-reference — Strata today — is the
one unsafe point in that design space.

## Goals

- Make every RADOS chunk **self-describing**: stamp a durable back-reference to
  its owning object at write time, at near-zero hot-path cost.
- Ship a **reconcile** pass that detects and repairs RADOS↔meta divergence
  (orphan chunks, dangling manifests) after a restore or at scheduled cadence.
- Ship a **last-resort rebuild** that reconstructs the manifest index for a
  bucket (or all buckets) from a RADOS scan when the metadata backup is gone.
- Document the operator/Strata shared-responsibility boundary.
- TiKV + Cassandra parity throughout (the reconcile/rebuild targets both meta
  backends; the back-reference is a RADOS-data-tier feature, backend-agnostic).

## User Stories

### US-000: RADOS pool object-enumeration primitive (foundation)
**Description:** As the reconcile/rebuild tooling, I need to enumerate every
object in a RADOS pool, because orphan detection (a chunk the meta tier does not
know about) and last-resort rebuild are ONLY possible by walking the pool itself
— there is no such primitive today.

**Why this is a separate story (review finding A):** `cephimpl` exposes only
`GetPoolStats` (aggregate, `health.go`); there is NO object iteration. The
rebalance worker — the cited model for reconcile — walks the META tier
(`ListBucketsShard` → `ListObjects`), never the pool. US-002 (orphan detect) and
US-004 (rebuild) both rest on this primitive; it must land first.

**Acceptance Criteria:**
- [ ] `cephimpl` wraps go-ceph `ioctx.Iter()` (`rados_nobjects_list`) behind a
      small interface on the main-module `rados` shape: stream object IDs in a
      pool, resumable from a cursor/watermark, context-cancellable.
- [ ] Rate-limited (token bucket, reuse the rebalance knob) — a pool walk on a
      live cluster MUST NOT saturate OSDs.
- [ ] Filters to Strata chunk OIDs (skips foreign objects) and is namespace-aware
      (multi-cluster / per-class pools).
- [ ] Surfaced through the always-on `rados` package as a not-compiled sentinel
      off the `ceph` tag (mirrors `rados.New`), so the main module builds without
      librados.
- [ ] **Red/green proof:** seed N chunks into a test pool, enumerate → exactly N
      OIDs returned; resume from a mid-walk cursor → no dup, no drop.
- [ ] `make vet` passes; RADOS leg under `ceph`/integration (`make test-rados`).

### US-001: Chunk back-reference stamped at PUT (RADOS + S3 backend)
**Description:** As the system, I want every chunk written to RADOS to carry a
durable back-reference to its owning object so the data tier is self-describing.

**Acceptance Criteria:**
- [ ] `PutChunks` (RADOS path) stamps an xattr (e.g. `user.strata.backref`)
      carrying `{schema_byte, bucket_id, key, version_id, chunk_idx, mtime}` on
      each chunk, written in the **same** `WriteOp` as the chunk body via the
      existing `writeChunkBatched` SetXattr seam (one Operate, no extra
      round-trip). **`mtime` is required** (review finding C): `version_id` orders
      the chain but cannot derive IsLatest when a suspended-null version
      coexists — see Resolved Decision 3.
- [ ] The back-reference payload is a compact, versioned encoding (the leading
      `schema_byte` reserves room for a future refcount-aware / multi-owner shape
      should content-addressed dedup ship — see Known interactions); documented
      in code. Carries NO key material (safe under SSE — review finding B).
- [ ] **S3-passthrough backend leg:** the S3 backend stamps the same
      back-reference as object tags / user-metadata on the backing object (the S3
      backend reconciles via its native `ListObjects`, no US-000 dependency).
- [ ] Multipart and multi-cluster chunks are covered (the per-chunk ioctx
      resolution already exists; the back-reference rides the same path).
- [ ] **Hot-path cost measured** — a benchmark shows the SetXattr-in-WriteOp adds
      negligible p99 vs the bare WriteFull; numbers recorded in a bench doc.
- [ ] On by default with an env opt-out (`STRATA_CHUNK_BACKREF`, default on);
      off → legacy behaviour (no xattr), reconcile/rebuild degrade gracefully and
      log that back-references are absent.
- [ ] `make vet` + tests pass (memory backend no-ops; RADOS leg under
      `ceph`/integration; S3-backend leg under the s3 backend test seam).

### US-002: Reconcile worker — orphan chunk detection + policy
**Description:** As an operator who just restored from backup, I want a pass that
finds RADOS chunks with no matching manifest and resolves them by policy, so a
restore skew does not leak storage forever.

**Acceptance Criteria:**
- [ ] A reconcile pass scans RADOS chunks (per cluster/pool), reads each
      back-reference xattr, and looks up the manifest in the meta store.
- [ ] **Orphan chunk** (chunk present, no manifest references it): resolved by a
      configurable policy — `gc` (enqueue for deletion, default — the chunk's
      object was rolled back) or `restore` (rebuild the manifest row from the
      back-reference, for the "meta is older than data" case). Policy is per-run.
- [ ] The pass is **idempotent and resumable** (watermark per cluster/pool); a
      re-run after a partial pass converges.
- [ ] Rate-limited (token bucket, reuse the rebalance-style knob) so a reconcile
      on a live cluster does not saturate the OSDs.
- [ ] Metrics: `strata_reconcile_chunks_scanned_total`,
      `_orphans_found_total{resolution}`, `_errors_total`.
- [ ] **Red/green proof:** seed a bucket, simulate restore skew (delete the
      manifest rows but keep the chunks), run reconcile in `gc` mode → chunks
      enqueued for deletion; run in `restore` mode → manifest rebuilt and the
      object is GET-able again.
- [ ] `make vet` + tests pass (memory + TiKV contract; RADOS under integration).

### US-003: Reconcile worker — dangling manifest detection
**Description:** As an operator, I want the pass to find manifests that point at
chunks the data tier no longer has, so I learn which objects a restore left
broken instead of discovering it on a client 5xx.

**Acceptance Criteria:**
- [ ] The reconcile pass walks manifests (meta → data direction) and probes that
      each referenced chunk exists in RADOS.
- [ ] **Dangling manifest** (manifest present, chunk missing): reported and, by
      policy, either quarantined (marked unreadable with a clear error) or
      deleted; never silently left to surface as a corrupt GET.
- [ ] A summary report (per bucket: N orphans, N dangling, N healthy) is written
      to a report object / log, suitable for an operator go/no-go after restore.
- [ ] Metric `strata_reconcile_dangling_manifests_total{resolution}`.
- [ ] **Red/green proof:** seed objects, delete a chunk under one manifest, run
      reconcile → that object reported dangling and quarantined; a healthy object
      untouched.
- [ ] `make vet` + tests pass.

### US-004: `strata admin rebuild-index` — last-resort rebuild from RADOS
**Description:** As an operator who lost the metadata backup, I want to
reconstruct the manifest index for a bucket from the surviving RADOS chunks so
the intact bytes are not permanently lost.

**Acceptance Criteria:**
- [ ] New `strata admin rebuild-index` subcommand (single-binary invariant — NOT
      a new binary) that scans a cluster/pool, groups chunks by back-reference
      `{bucket_id, key, version_id}`, orders by `chunk_idx`, and writes the
      reconstructed manifest rows into the meta store.
- [ ] Handles versioned objects (every version reconstructed; latest-version
      ordering correct) and multipart-origin chunks.
- [ ] Idempotent + resumable; a re-run does not duplicate or corrupt rows already
      rebuilt.
- [ ] Refuses to overwrite a manifest that already exists in meta unless
      `--force` (the live meta wins by default; rebuild is for the empty/lost
      case).
- [ ] Gaps (missing `chunk_idx` in the sequence) are reported, not silently
      stitched into a short object — a partial object is flagged, not served as
      whole.
- [ ] **PLAINTEXT-ONLY scope (review finding B).** SSE-S3/KMS objects are
      reported `unrecoverable (wrapped DEK was in the lost meta)` and NEVER
      silently served — the wrapped DEK lives only in `meta.Object.SSEKey` and the
      back-reference carries no key material by design. Object metadata absent
      from the back-reference (Content-Type, user-metadata, tags, ACL,
      storage-class, multipart-composite ETag) is reported as lost, not
      fabricated; single-part ETag is recomputed from the rebuilt bytes.
- [ ] **Red/green proof:** seed a NON-SSE bucket, wipe its manifest rows
      entirely, run `rebuild-index` → every object GET-able again with correct
      bytes + size + version order (IsLatest correct across a suspended-null
      version, exercising the `mtime` field); a deliberately removed middle chunk
      → that object flagged gapped, not silently truncated; an SSE object →
      reported unrecoverable, not served.
- [ ] `make vet` + tests pass (TiKV + Cassandra meta targets; RADOS integration).

### US-005: Shared-responsibility ops note + reconcile runbook
**Description:** As an operator, I want a clear statement of who owns what and how
to run reconcile/rebuild after a restore, so I can make a safe go/no-go.

**Acceptance Criteria:**
- [ ] An operator doc states the boundary: **operator** owns TiKV/Cassandra and
      RADOS backup/restore; **Strata** owns the back-reference + reconcile/rebuild
      that realigns the two tiers after a restore.
- [ ] A runbook section: post-restore procedure (run reconcile in report mode →
      review summary → choose gc/restore policy → run rebuild-index only if the
      meta backup is unusable), with the exact commands and what each metric means.
- [ ] **States the rebuild-index recovery boundary loudly:** plaintext objects
      recover fully; SSE-S3/KMS objects are unrecoverable from RADOS alone (wrapped
      DEK was in the lost meta); object metadata not in the back-reference is lost.
      The operator MUST treat the meta backup as the primary recovery path and
      rebuild-index as the last resort for plaintext bytes only.
- [ ] Cross-links the design prior art (Lustre LFSCK / HDFS fsck) so operators
      recognise the pattern.
- [ ] Docs build clean (`make docs-build`).

### US-006: Reconcile / rebuild surfaced in the web console
**Description:** As an operator, I want to trigger and watch a reconcile from the
console (mirroring the drain-progress UX) rather than only via CLI.

**Acceptance Criteria:**
- [ ] An admin page / panel triggers a reconcile run (policy picker: report /
      gc / restore) and shows progress (scanned / orphans / dangling / % complete)
      reusing the `<DrainProgressBar>`-style component pattern.
- [ ] The post-run summary report is viewable in the console.
- [ ] `rebuild-index` is intentionally **CLI-only** (a destructive last-resort op
      gated behind shell access) — the console links to the runbook instead of
      exposing a one-click rebuild. Documented as a deliberate choice.
- [ ] Playwright spec covers trigger → progress → summary.
- [ ] Typecheck/lint passes; verify in browser using dev-browser skill.

## Functional Requirements

- FR-1: Every RADOS chunk MUST carry a versioned back-reference xattr
  (`{bucket_id, key, version_id, chunk_idx}`) written in the same WriteOp as the
  body, on by default with an opt-out env.
- FR-2: A reconcile pass MUST detect orphan chunks (chunk without manifest) and
  resolve them by an operator-chosen policy (gc | restore), idempotently and
  resumably, rate-limited.
- FR-3: The reconcile pass MUST detect dangling manifests (manifest without
  chunk) and quarantine/report them rather than leaving a silent corrupt GET.
- FR-4: `strata admin rebuild-index` MUST reconstruct manifests from a RADOS scan
  by back-reference, handling versions + multipart, flagging gaps, idempotent,
  not overwriting live meta without `--force`.
- FR-5: Reconcile/rebuild MUST target both TiKV and Cassandra meta backends at
  parity.
- FR-6: Behavioural fixes MUST ship with red/green proof of skew → repair.
- FR-7: The shared-responsibility boundary MUST be documented; Strata MUST NOT
  reimplement TiKV/Cassandra backup tooling.

## Non-Goals (Out of Scope)

- **Storage-tier backup/restore itself** (TiKV BR, Cassandra snapshots, RADOS
  backup, RPO/RTO, retention) — operator's responsibility.
- **Continuous online consistency checking** as a hot-path concern — reconcile is
  a recovery-time / scheduled-cadence pass, not a per-request gate (same posture
  as Lustre LFSCK / HDFS fsck).
- **Cross-region / multi-site DR orchestration** — separate concern.
- Changing the manifest format beyond the additive back-reference encoding.

## Technical Considerations

- The `writeChunkBatched` SetXattr seam already exists
  (`internal/data/rados/cephimpl/ops.go`); `STRATA_RADOS_BATCH_OPS` currently
  gates it. This PRD wires the back-reference through it — the long-noted "future
  xattr-writer work" this code was built for.
- RADOS xattr read for reconcile: `ListXattrs` is available; note the go-ceph
  v0.39 limitation that `rados_read_op_getxattrs` is not exposed (see the
  existing comment in `ops.go`) — reconcile reads xattrs via the available API.
- Reconcile scan is a recovery-time operation; cost (a full RADOS pool walk at
  ≳10⁹ objects) is acceptable off the hot path but MUST be rate-limited and
  resumable — model on the rebalance worker's token bucket + watermark.
- Back-reference for SSE objects: the xattr stores object identity, not
  plaintext, so it is safe under SSE-S3/KMS (no plaintext leak); confirm the
  encoding carries no secret material.
- Pairs with the architecture-hardening PRD's US-009 per-chunk CRC: the CRC
  proves a chunk's *bytes* are intact; the back-reference proves *whose* chunk it
  is. Together they make the data tier both verifiable and self-describing.

## Success Metrics

- A simulated restore skew (meta older than data, and data older than meta) is
  detected and repaired by reconcile with no silent leak and no silent corrupt
  GET — neither possible today.
- A bucket whose manifest rows are wiped is fully recovered by `rebuild-index`
  from RADOS alone, with correct bytes/size/version order, gaps flagged.
- Back-reference adds negligible measured p99 to the PUT hot path.
- Reconcile/rebuild work identically on TiKV and Cassandra.

## Resolved Decisions (post critical-review)

These resolve the original Open Questions plus the findings of the adversarial
review against the live code (2026-06-02).

1. **Orphan default policy → `report`.** Reconcile never auto-deletes; `report`
   is the default run mode, `gc`/`restore` are explicit per-run operator choices
   (least-surprise — mirrors Lustre LFSCK / HDFS fsck, which report by default).
2. **S3-passthrough back-reference → in scope this cycle (both tiers).** RADOS
   uses an xattr; the S3 backend uses object tags / user-metadata on the backing
   object. Note: the S3 backend has a NATIVE `ListObjects`, so the US-000 pool
   enumeration primitive is a RADOS-only concern — the S3 leg reconciles via the
   backend's own listing.
3. **Back-reference payload carries `mtime` → version chain + IsLatest are
   reconstructable.** `version_id` is already time-ordered (Cassandra
   `gocql.TimeUUID()` embeds the timestamp; TiKV uses the inverted-ts suffix), so
   `chunk_idx` + `version_id` ORDER the chain on their own. BUT the
   suspended-versioning null case (a null-sentinel version, ts=0, can be the
   latest by `mtime` while sorting LAST by `version_id` — the exact bug fixed in
   `ralph/architecture-hardening` US-012's `ListObjectVersions`) means IsLatest
   cannot be derived from `version_id` alone. The back-reference therefore
   includes `mtime` (8 bytes) so rebuild reconstructs IsLatest correctly. Final
   payload: `{schema_byte, bucket_id, key, version_id, chunk_idx, mtime}`.
4. **rebuild-index is PLAINTEXT-ONLY; SSE objects are flagged unrecoverable.**
   CRITICAL review finding: the per-object wrapped DEK (`meta.Object.SSEKey`)
   lives ONLY in the meta tier — if the meta backup is lost (the exact
   rebuild-index scenario), SSE-S3/KMS ciphertext chunks are permanently
   undecryptable; the back-reference deliberately carries NO key material (no
   plaintext, no wrapped DEK — keeps the meta/data security separation intact).
   rebuild-index reconstructs chunk lists + bytes + size + version order for
   NON-SSE objects only; SSE objects are reported as `unrecoverable (DEK in lost
   meta)`, never silently served. Rebuild also CANNOT recover object metadata not
   present in the back-reference (Content-Type, user-metadata, tags, ACL,
   storage-class, multipart-composite ETag) — these are reported as lost. This
   limitation is documented loudly in US-004 + US-005. (Stamping the wrapped DEK
   into a per-object xattr to recover SSE was considered and DEFERRED — it erodes
   the meta/data separation as a security boundary; revisit as a separate
   ROADMAP item if SSE disaster-recovery is later required.)

## Known interactions / non-goals carried from review

- **Content-addressed dedup (ROADMAP P2) is incompatible with a single-owner
  back-reference.** Dedup makes one chunk owned by N objects via a refcount; one
  `{bucket_id,key,version_id}` xattr cannot represent N owners, and orphan
  detection (chunk-without-manifest) would misfire on a shared chunk. Both
  features are unbuilt; CopyObject today RE-WRITES chunks to fresh OIDs
  (verified — `internal/s3api/copy_object.go` GetChunks→PutChunks), so there is
  no multi-owner chunk today. If dedup ships later, the back-reference encoding
  (schema_byte) must grow a refcount-aware / multi-owner shape; the schema byte
  reserves that room.
- **Execution model (pinned).** Reconcile (US-002/003) is a leader-elected
  worker that DRAINS admin-queued jobs — reuse the reshard worker + admin-queue
  pattern hardened in `ralph/architecture-hardening` US-005 (`POST` queues a job
  → 202; the worker runs it out-of-band; `GET` reads progress). `rebuild-index`
  (US-004) is a `strata admin` CLI one-shot (single-binary invariant), never a
  worker — it is a destructive last-resort op gated behind shell access.
- **US-009 per-chunk CRC32C already SHIPPED** (`ralph/architecture-hardening`,
  merged). `data.ChunkRef` now carries `Checksum uint32`; the CRC proves a
  chunk's BYTES are intact, this PRD's back-reference proves WHOSE chunk it is.
  The intro's `ChunkRef = {Cluster, Pool, Namespace, OID, Size}` is the
  pre-US-009 shape — the live struct also has `Checksum`.
