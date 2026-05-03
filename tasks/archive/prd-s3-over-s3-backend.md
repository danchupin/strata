# PRD: First-class S3-compatible Data Backend (S3-over-S3)

## Introduction

Today Strata's `data.Backend` interface has two implementations: an in-memory
backend for tests/dev and a librados-backed backend that stores 4-MiB chunks
in a Ceph RADOS pool. The gateway terminates S3 traffic, manages metadata in
Cassandra, and writes object bytes into RADOS.

Many operators do not want to operate Ceph. They already run an S3-compatible
service — AWS S3, MinIO cluster, an on-prem Ceph RGW deployment owned by a
separate team, Garage, Wasabi, Backblaze B2's S3 endpoint — and want Strata's
value-add layer on top: SigV4 IAM, audit log, per-bucket quotas, replication,
lifecycle policies, Cassandra-coherent metadata. They do not need (or want)
Strata to also own the underlying object storage.

This PRD adds a **first-class S3-compatible data backend** as an equal-tier
production option alongside RADOS. The shape is **native**: one Strata object
maps to one backend S3 object via the backend's own multipart upload during
streaming PUTs. This is in contrast to a "naive" port (each 4-MiB Strata
chunk as its own backend S3 object) which would generate ~250× more requests
than necessary and would not be viable for production.

After this PRD, an operator picks `STRATA_DATA_BACKEND=rados` (existing) or
`STRATA_DATA_BACKEND=s3` (new), points the gateway at any S3-compatible
endpoint via aws-sdk-go-v2, and Strata serves the same S3 API on top with
its own metadata and IAM model.

This is a P1 entry under "Alternative data backends" (a new ROADMAP section
this PRD also creates).

## Goals

- New `internal/data/s3/` backend implementing `data.Backend`, equal-tier
  with RADOS
- Native shape — one Strata object = one backend S3 object; no chunk
  fragmentation on the backend side
- Single code path via aws-sdk-go-v2 — works against AWS S3, MinIO, Ceph
  RGW, Garage, Wasabi, B2-S3, any S3-compatible endpoint that exposes the
  PutObject / GetObject / Multipart Upload API surface
- Configurable via env (`STRATA_DATA_BACKEND=s3` +
  `STRATA_S3_BACKEND_*` knobs); no rebuild required to switch backends
- SSE flag with three modes:
  - `passthrough` — rely on backend SSE only (server-side AES on the backend
    bucket; backend manages keys)
  - `strata` — Strata envelope-encrypts the body before PUT; backend stores
    ciphertext blindly
  - `both` — both layers; transparent to the client
- Lifecycle bidirectional mapping — Strata transition rules translate to
  backend S3 lifecycle policies and vice versa where the semantics overlap
- CORS passthrough — backend bucket's CORS configuration reflected in
  Strata's PutBucketCors / GetBucketCors responses
- Presigned URL passthrough — Strata can mint URLs that redirect clients
  directly to the backend (for high-throughput GET workloads that do not
  need Strata's IAM checks at request time)
- CI matrix entry — every PR runs Strata against MinIO in addition to the
  current Cassandra+memory path
- ROADMAP.md sync per the project root CLAUDE.md "Roadmap maintenance"
  rule

## User Stories

### US-001: aws-sdk-go-v2 dependency + `internal/data/s3` package skeleton
**Description:** As a developer, I want a starting `internal/data/s3` package
that compiles, satisfies the `data.Backend` interface with stub
implementations, and is reachable via the existing backend factory so later
stories can fill in real implementations.

**Acceptance Criteria:**
- [ ] Add `github.com/aws/aws-sdk-go-v2` and submodules (`config`,
      `service/s3`, `feature/s3/manager`) to `go.mod`. Run `go mod tidy`
- [ ] New `internal/data/s3/backend.go` with `type Backend struct{}` and
      stub methods returning `errors.ErrUnsupported`. Methods conform to
      `data.Backend` interface
- [ ] New `internal/data/s3/backend_test.go` with one happy-path unit
      test that asserts the stub is wired (calls `Put`, expects
      `ErrUnsupported`)
- [ ] No production dispatch yet — `STRATA_DATA_BACKEND=s3` is reserved but
      not selectable until US-009 lands
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: Streaming PUT via backend multipart upload
**Description:** As a Strata client uploading an object, I want the bytes to
stream straight to the backend without buffering, using the backend's S3
multipart-upload protocol so objects of any size go through one consistent
code path with bounded memory.

**Acceptance Criteria:**
- [ ] `s3.Backend.Put(ctx, oid, reader, size)` uses
      `manager.NewUploader(client).Upload(...)` from the SDK's
      `feature/s3/manager` — handles small objects via single PutObject and
      large objects via multipart, transparently
- [ ] Default part size = 16 MiB (configurable via
      `STRATA_S3_BACKEND_PART_SIZE`); concurrency = 4 (configurable via
      `STRATA_S3_BACKEND_UPLOAD_CONCURRENCY`)
- [ ] Memory bound: never buffers > `part_size * upload_concurrency` bytes
      (default 64 MiB peak)
- [ ] Returns the backend ETag AND backend `VersionId` (string;
      empty when the backend does not support versioning, returns "null"
      when versioning is suspended) in the response struct so the
      manifest can record both
- [ ] Aborts the multipart on context cancel — no orphaned multipart
      sessions left in the backend bucket
- [ ] Test: 100 MiB streaming upload against in-process MinIO (testcontainer)
      completes with peak RSS under 100 MiB
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: Streaming GET with Range support
**Description:** As a Strata client GETing an object (full or partial), I
want the backend to stream the body back without loading it into memory and
to honor `Range` headers natively.

**Acceptance Criteria:**
- [ ] `s3.Backend.GetRange(ctx, oid, off, len) (io.ReadCloser, error)`
      issues `GetObject` with `Range: bytes=<off>-<off+len-1>` header to the
      backend
- [ ] `s3.Backend.Get(ctx, oid) (io.ReadCloser, error)` issues plain
      `GetObject` (no Range)
- [ ] Body returned is the SDK's response `Body` (HTTP stream); caller is
      responsible for `Close()`
- [ ] Test: 100 MiB object, range-read of [50 MiB, 50 MiB+1 KiB) returns
      exactly 1 KiB without loading the rest into memory
- [ ] Maps backend `NoSuchKey` → `data.ErrNotFound` so the gateway returns
      `404 NoSuchKey` (not `500`)
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: Delete + best-effort batch delete (version-aware)
**Description:** As Strata's GC worker, I want to delete backend objects
when the manifest's reference count drops to zero, ideally batched to
reduce request volume, and to delete by `VersionId` when one was recorded
so a backend bucket with versioning enabled does NOT silently leak the
old version into a delete-marker.

**Acceptance Criteria:**
- [ ] `s3.Backend.Delete(ctx, oid, versionID)` issues `DeleteObject`:
  - When `versionID == ""`: plain `DeleteObject(key)` (frees bytes
    immediately on non-versioned buckets and on versioning-suspended
    buckets where the bytes never had a version-id assigned)
  - When `versionID != ""`: `DeleteObject(key, VersionId: versionID)`
    (deletes the specific version; on a versioned backend this skips
    delete-marker creation entirely and the bytes are freed)
- [ ] Idempotent — success on already-deleted objects (NoSuchKey →
      no-op)
- [ ] `s3.Backend.DeleteBatch(ctx, refs []ObjectRef)` issues
      `DeleteObjects` with up to 1000 entries per request (S3 protocol
      limit). `ObjectRef = {Key, VersionID}`; the SDK's
      `DeleteObjectsInput.Delete.Objects[].VersionId` field is set when
      non-empty
- [ ] On partial failure, returns the per-ref error map
- [ ] GC worker reads `BackendRef.VersionID` from each manifest's
      `BackendRef`, populates the `ObjectRef` slice, calls
      `DeleteBatch`. When `BackendRef == nil` (RADOS path), the GC
      worker uses the existing chunk-delete code path (no version-id
      involved)
- [ ] Test 1: batch delete 100 keys with all `VersionID == ""` in one
      request against in-process MinIO (versioning off), asserts only
      1 HTTP call observed and bytes freed
- [ ] Test 2: batch delete 100 keys with all `VersionID != ""` in one
      request against in-process MinIO with versioning enabled, asserts
      no delete-markers created (verified via `ListObjectVersions` —
      empty after delete)
- [ ] Test 3: mixed batch (some with VersionID, some without) in one
      request — asserts each entry handled per its own version-id
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: Configuration via env + path-style support
**Description:** As an operator, I want to point Strata at any
S3-compatible endpoint with environment variables only — no code changes,
no rebuilds.

**Acceptance Criteria:**
- [ ] New env vars (loaded via existing koanf-based `internal/config`):
  - `STRATA_S3_BACKEND_ENDPOINT` — full URL (default empty = AWS S3
    auto-resolved by region)
  - `STRATA_S3_BACKEND_REGION` — required (`us-east-1` default for MinIO)
  - `STRATA_S3_BACKEND_BUCKET` — required; the single backend bucket all
    Strata objects land in (with random-prefix keys to avoid hot-prefix
    throttling)
  - `STRATA_S3_BACKEND_ACCESS_KEY` / `STRATA_S3_BACKEND_SECRET_KEY` —
    static creds; if both empty, falls back to the SDK's default chain
    (env, ~/.aws, IRSA, IMDS)
  - `STRATA_S3_BACKEND_FORCE_PATH_STYLE=true` — required for MinIO + Ceph
    RGW (vhost-style not always supported); default `false` for AWS
- [ ] `s3.Backend` constructor reads these env vars at startup (via
      koanf) and resolves credentials once
- [ ] Misconfiguration (empty bucket, bogus region) fails fast at startup
      with a clear error message — never at first request
- [ ] Writability probe at startup: `PutObject` + `DeleteObject` on a
      sentinel key `.strata-readyz-canary` against the backend bucket.
      Refuse to start (non-zero exit + clear error) if either fails —
      catches read-only mounts, missing IAM permissions, expired creds,
      and bucket-existence regressions. Probe runs once on boot, not
      per-request
- [ ] Document the env vars in `README.md`'s environment-variable table
- [ ] Typecheck passes
- [ ] Tests pass

### US-006: SDK retry + per-op timeout
**Description:** As an operator running Strata against a remote S3 endpoint
under occasional slowdowns or transient 503s, I want bounded retries and
per-op timeouts so a stuck request never hangs forever.

**Acceptance Criteria:**
- [ ] Configure SDK retry mode = adaptive (default in v2 SDK), max attempts
      = 5 (configurable via `STRATA_S3_BACKEND_MAX_RETRIES`)
- [ ] Per-op timeout via `context.WithTimeout`; default 30 s for small ops
      (Get/Put/Delete on objects under part-size), 10 min for multipart
      complete; configurable via `STRATA_S3_BACKEND_OP_TIMEOUT_SECS`
- [ ] Retries on: `503 SlowDown`, `429 TooManyRequests`, network errors,
      5xx
- [ ] No retry on: `404 NoSuchKey`, `403 AccessDenied`, `400` malformed
      requests (these are programmer errors, not transient)
- [ ] Each retry logs at WARN with op + attempt + delay
- [ ] Test: stub S3 client returning 503 for first 2 attempts, success on
      3rd — `Put` succeeds without surfacing the 503 to the gateway
- [ ] Typecheck passes
- [ ] Tests pass

### US-007: Prometheus metrics for backend ops
**Description:** As an operator, I want per-op latency + error rates for
the S3 backend so I can alert on backend regressions.

**Acceptance Criteria:**
- [ ] New histogram `strata_data_s3_backend_op_duration_seconds{op,status}`
      with op in `{put, get, delete, batch_delete, multipart_init,
      multipart_part, multipart_complete, multipart_abort}` and status in
      `{ok, error, retried}`
- [ ] New counter `strata_data_s3_backend_op_total{op,status}`
- [ ] New counter `strata_data_s3_backend_retry_total{op}` for visibility
      into adaptive retry pressure
- [ ] Observers piggyback on the SDK's `middleware.Stack` — single
      `endOperationMiddleware` registered once on client init
- [ ] Test: a known sequence of ops produces matching counter values
- [ ] Typecheck passes
- [ ] Tests pass

### US-008: Manifest schema additive — backend object reference
**Description:** As a developer fixing the manifest format for the new
backend, I need an additive `BackendRef` field on `data.Manifest` that
records the backend S3 key + ETag + size + version-id — without breaking
RADOS-mode manifests.

**Acceptance Criteria:**
- [ ] Add `BackendRef *BackendRef` field on `data.Manifest`. Struct:
      `Backend string` (`"rados"` | `"s3"`), `Key string`, `ETag string`,
      `Size int64`, `VersionID string` (omitempty — present only when
      the backend returned a version-id at write time; empty for
      non-versioning backends)
- [ ] All struct fields tagged `json:",omitempty"` and registered in
      `manifest.proto` with fresh, monotonically-increasing field numbers
      (additive — no renumbering)
- [ ] **Protobuf field-number reservation** (avoid collision with US-013
      `Manifest.SSE.Mode`): inspect the highest field number in
      `manifest.proto` at story-start time, name it N. This story
      reserves N+1 for the `BackendRef` message + N+2..N+6 for its
      sub-fields (`Backend`, `Key`, `ETag`, `Size`, `VersionID`).
      US-013 reserves N+7 for `SSE.Mode`. Document the assignment in
      a `// US-008 / US-013 field-number assignment` comment block at
      the top of `manifest.proto`
- [ ] When `BackendRef` is set, the manifest's existing `Chunks` slice is
      empty (1:1 mapping; the whole object lives at `BackendRef.Key`).
      When `Chunks` is non-empty, `BackendRef` is nil (RADOS path)
- [ ] `BackendRef.VersionID` semantics:
  - Empty string `""` — backend does not support versioning, OR
    versioning was off at PUT time, OR the SDK response did not carry a
    version-id. Plain Delete works (frees bytes)
  - `"null"` — backend versioning is currently Suspended; S3 spec
    returns the literal `"null"` version-id. Versioned-Delete with
    `VersionId="null"` cleans correctly
  - Any other non-empty string — UUID-shaped version-id from a
    versioning-enabled backend. Versioned-Delete with this string
    skips delete-marker creation
- [ ] Encoder/decoder round-trips all three shapes
      (`VersionID == ""` / `"null"` / `<uuid>`) via
      `data.EncodeManifest` / `DecodeManifest` in JSON and protobuf
      modes
- [ ] Old (RADOS) manifests still decode with `BackendRef=nil`; gateway
      uses `Chunks` as today
- [ ] Typecheck passes
- [ ] Tests pass

### US-009: Gateway backend dispatch — `STRATA_DATA_BACKEND=s3`
**Description:** As an operator, I want to switch the gateway's data plane
to S3 by setting one env var, and have the gateway route writes / reads
through the new backend without any other config changes.

**Acceptance Criteria:**
- [ ] `internal/serverapp.buildDataBackend` reads
      `STRATA_DATA_BACKEND` (existing for `rados` / `memory`) and dispatches
      to `internal/data/s3.New(...)` when value is `s3`
- [ ] Gateway PUT path: when backend is S3, sets
      `Manifest.BackendRef = {Backend: "s3", Key: <random>, ETag: <fromBackend>,
      Size: <total>, VersionID: <fromBackend>}` and leaves
      `Manifest.Chunks` empty. `VersionID` is whatever the SDK
      returned — empty string, `"null"`, or UUID — without
      interpretation at the gateway layer
- [ ] Gateway GET path: when `Manifest.BackendRef != nil`, calls
      `s3.Backend.GetRange(ctx, BackendRef.Key, off, len)`; falls through
      to chunk-based path otherwise
- [ ] Backend object key format: `<bucket-uuid>/<object-uuid>` — UUID
      prefix gives random prefix distribution to avoid hot-prefix
      throttling on AWS
- [ ] Smoke pass (`make smoke`) passes against in-process MinIO with
      `STRATA_DATA_BACKEND=s3`
- [ ] Typecheck passes
- [ ] Tests pass

### US-010: Client multipart pass-through to backend multipart
**Description:** As a Strata client doing an S3 multipart upload, I want
each `UploadPart` to map 1:1 to a backend `UploadPart` so we end up with
exactly one S3 object on the backend, not N — and `CompleteMultipartUpload`
on Strata triggers `CompleteMultipartUpload` on the backend.

**Acceptance Criteria:**
- [ ] `meta.MultipartUpload` schema additive: `BackendUploadID string`.
      All three meta backends update in lockstep:
      - Cassandra: `ALTER TABLE multipart_uploads ADD backend_upload_id text`
        via `alterStatements` in `internal/meta/cassandra/schema.go`
      - Memory: add field on the in-memory struct + serialize through
        existing copy/marshal sites
      - TiKV: extend the multipart-upload encoder/decoder in
        `internal/meta/tikv/keys.go` (or its multipart-state file) so
        `BackendUploadID` round-trips. No DDL — schema is implicit in
        key encoding
- [ ] `meta.MultipartPart` schema additive: `BackendETag string` — same
      lockstep update across all three backends
- [ ] `InitiateMultipartUpload` on Strata calls
      `CreateMultipartUpload` on backend, stores returned `UploadId` in
      `BackendUploadID`
- [ ] Strata `UploadPart` calls backend `UploadPart` with the same
      `BackendUploadID`; stores backend's per-part ETag in
      `multipart_uploads.parts[N].backend_etag`
- [ ] `CompleteMultipartUpload` on Strata calls
      `CompleteMultipartUpload` on backend with the part list (PartNumber
      + backend ETag); on success, sets `Manifest.BackendRef` like
      US-009 but with the multipart-completed object key.
      `BackendRef.VersionID` is populated from
      `CompleteMultipartUploadOutput.VersionId` (empty / `"null"` /
      UUID — same semantics as US-002 single-shot PUT)
- [ ] `AbortMultipartUpload` on Strata aborts the backend multipart too
- [ ] Per-part composite checksum interop:
      - If `prd-s3-tests-90.md` cycle has shipped (manifest carries
        `PartChunks[].ChecksumValue`), backend's per-part ETags + the
        existing checksum machinery flow through without modification
      - If not yet shipped, this AC is deferred — leave per-part
        checksums as today (Strata gateway computes its own; backend
        per-part ETag stored in `BackendETag` for the multipart
        passthrough but not yet wired into the composite checksum
        response). Document the deferral in the commit message
- [ ] Test: multipart upload of 50 MiB across 5 parts produces exactly one
      backend object visible in `aws s3 ls s3://<backend-bucket>/`
- [ ] Typecheck passes
- [ ] Tests pass

### US-011: docker-compose `s3-backend` profile + MinIO sidecar
**Description:** As a developer or CI runner, I want to bring up Strata
running on top of MinIO with one compose command.

**Acceptance Criteria:**
- [ ] `deploy/docker/docker-compose.yml` gains a `minio` service under
      profile `s3-backend` (image `minio/minio:RELEASE.2025-XX`,
      data volume, MINIO_ROOT_USER/MINIO_ROOT_PASSWORD env)
- [ ] `strata` service gains conditional env vars: when running under
      `--profile s3-backend`, sets `STRATA_DATA_BACKEND=s3`,
      `STRATA_S3_BACKEND_ENDPOINT=http://minio:9000`,
      `STRATA_S3_BACKEND_FORCE_PATH_STYLE=true`,
      `STRATA_S3_BACKEND_REGION=us-east-1`,
      `STRATA_S3_BACKEND_BUCKET=strata-backend`,
      static creds matching the MinIO root creds
- [ ] `make up-s3-backend` brings up `cassandra + minio + strata`
      (no Ceph) and waits for `/readyz`
- [ ] An `init-minio` one-shot service creates the `strata-backend` bucket
      via `mc mb` before strata starts; depends_on with healthcheck
      ordering
- [ ] Idle stack RSS ≤ 3 GB (lighter than the Ceph stack — MinIO single
      node ~500 MB)
- [ ] Typecheck passes (compose validation via `docker compose config -q`)
- [ ] Tests pass

### US-012: Smoke test against MinIO via compose
**Description:** As a CI maintainer, I want `make smoke` to also exercise
the S3 backend path against MinIO so we catch regressions in the new
backend on every PR.

**Acceptance Criteria:**
- [ ] New `make smoke-s3-backend` target: brings up the s3-backend
      compose profile, waits for `/readyz`, runs the existing smoke
      script (PUT / GET / DELETE / multipart) — all against the same
      Strata gateway, just with the new backend underneath
- [ ] Asserts the smoke pass produces exactly one backend object per
      Strata object (verified via `mc ls strata-backend/`)
- [ ] Asserts the multipart smoke produces exactly one backend object
      (not N)
- [ ] Asserts `make down` cleans up the MinIO data volume
- [ ] Typecheck passes
- [ ] Tests pass

### US-013: SSE config flag — passthrough / strata / both
**Description:** As a security-conscious operator, I want to choose how
data-at-rest encryption is handled — let the backend handle it, let Strata
handle it, or apply both layers.

**Acceptance Criteria:**
- [ ] New env var `STRATA_S3_BACKEND_SSE_MODE` with values
      `passthrough` (default), `strata`, `both`. Validated at startup
- [ ] `passthrough`: every Strata `Put` sends
      `x-amz-server-side-encryption: AES256` (or `aws:kms` per
      `STRATA_S3_BACKEND_SSE_KMS_KEY_ID` if set) to the backend; client
      sees the per-Strata-object SSE header in GET responses passed
      through from the backend metadata
- [ ] `strata`: existing Strata SSE-S3 / SSE-KMS envelope encryption runs
      first; backend stores ciphertext as plain `application/octet-stream`
      with no SSE header (or with backend SSE if explicitly opted-in via
      `passthrough` flag — but mode is mutually exclusive, so default
      ciphertext-blind)
- [ ] `both`: Strata envelope encrypts AND backend SSE applies; effectively
      double encryption. Useful for compliance regimes that mandate
      separate encryption boundaries
- [ ] Mode is recorded per-object in `Manifest.SSE.Mode` (additive enum
      tag); GET path decrypts according to the recorded mode
- [ ] Test: round-trip PUT → GET in each of the three modes against
      MinIO; asserts the bytes match the input
- [ ] Typecheck passes
- [ ] Tests pass

### US-014: Lifecycle bidirectional mapping
**Description:** As an operator using S3 lifecycle features, I want
Strata's lifecycle rules to translate to backend S3 lifecycle policies
where the semantics overlap, so backend-side transitions / expirations
work without re-implementing them in Strata.

**Acceptance Criteria:**
- [ ] When a Strata bucket gets a `PutBucketLifecycle` config that
      contains transitions to storage classes that the backend supports
      natively (e.g. `STANDARD_IA`, `GLACIER`, `DEEP_ARCHIVE` on AWS),
      Strata translates to a backend bucket lifecycle rule with the same
      filter + transition; Strata's own lifecycle worker skips those
      objects (the backend handles transitions)
- [ ] Strata-only transition classes (e.g. internal cold-tier pools) keep
      using Strata's lifecycle worker — translation is best-effort
- [ ] Expirations always also translate to backend lifecycle (cleanup of
      orphan backend objects independent of Strata GC)
- [ ] `GetBucketLifecycle` returns the Strata-stored config (the source
      of truth); backend config is derived state
- [ ] If backend reports a translation failure (unsupported transition
      class), Strata logs WARN and falls back to its own worker for that
      rule
- [ ] Documented in `docs/backends/s3.md` with a translation table
- [ ] Test: PUT a lifecycle config with a STANDARD_IA transition;
      asserts backend bucket has the matching rule via
      `s3api.GetBucketLifecycleConfiguration`
- [ ] Typecheck passes
- [ ] Tests pass

### US-015: CORS passthrough
**Description:** As a client doing browser-side CORS-preflight requests,
I want Strata's CORS responses to reflect the backend bucket's CORS
configuration when in S3-backend mode.

**Acceptance Criteria:**
- [ ] `PutBucketCors` on Strata translates to `PutBucketCors` on the
      backend bucket (after Strata's own per-bucket CORS config update)
- [ ] `GetBucketCors` on Strata returns the union of the Strata-stored
      config and the backend-stored config (Strata takes precedence on
      conflict)
- [ ] Preflight `OPTIONS` requests on objects served via presigned URL
      (US-016) hit the backend directly — Strata is out of the path,
      backend's CORS config applies
- [ ] `DeleteBucketCors` removes from both layers
- [ ] Test: round-trip CORS config; asserts both Strata `GetBucketCors`
      and backend `GetBucketCors` (via aws CLI) return the same rule
- [ ] Typecheck passes
- [ ] Tests pass

### US-016: Presigned URL passthrough
**Description:** As a high-throughput GET workload (CDN origin, video
streaming), I want Strata to mint a presigned URL that points the client
directly at the backend, skipping Strata's IAM hop on the data plane after
the initial signed request.

**Acceptance Criteria:**
- [ ] Strata's `PresignGetObject` request handler optionally returns a
      backend-presigned URL (controlled by per-bucket flag
      `BackendPresignEnabled` set via a new admin endpoint
      `PUT /<bucket>?backendPresign`)
- [ ] When enabled, the presigned URL points at the backend bucket
      directly with backend-credentialed signature; expiry forwarded from
      the original request
- [ ] When disabled (default), behaves as today — URL points at Strata
- [ ] Audit log row records `presign_passthrough=true` so the audit
      trail captures the redirect even though the data fetch does not hit
      Strata
- [ ] Strict mode: backend-presigned URLs ONLY work when the client's
      Strata IAM allows GET on the object — Strata pre-checks before
      issuing the URL
- [ ] Test: PresignGetObject with passthrough enabled returns a URL
      whose host is the backend's; curl against that URL fetches the
      object
- [ ] Typecheck passes
- [ ] Tests pass

### US-017: docs/backends/s3.md
**Description:** As an operator evaluating the S3 backend, I need a
single-page operator guide covering setup, capability matrix, caveats,
and tested-against backends.

**Acceptance Criteria:**
- [ ] New `docs/backends/s3.md` (alongside `scylla.md` and `tikv.md`) with:
  - When to choose S3 backend over RADOS (decision matrix)
  - Required env vars + sample compose / Kubernetes config
  - Tested-against backends: AWS S3 (region us-east-1), MinIO (latest),
    Ceph RGW (Reef+), Garage (latest). Mark each as
    "supported"/"works with caveats"/"unsupported"
  - Capability matrix: which S3-compatibility tests pass on each backend
    (e.g. AWS supports lifecycle classes that MinIO does not; document)
  - Performance characteristics: latency floor (one extra HTTP hop), cost
    model (request count + storage), throughput (linear with backend
    capacity)
  - Common operational pitfalls (force-path-style for MinIO, IAM role
    setup for AWS IRSA, key prefix randomization for AWS hot-prefix
    avoidance)
  - **Operator-required backend bucket configuration** (load-bearing
    section, not optional notes):
    - `AbortIncompleteMultipartUpload` lifecycle rule on the backend
      bucket — recommended `DaysAfterInitiation: 7`. Without it, gateway
      crashes mid-multipart leak parts indefinitely (AWS S3 charges
      storage on incomplete parts; MinIO leaks disk). Provide
      copy-pasteable JSON snippet
    - **Do NOT enable versioning on a Strata-managed backend bucket**
      that has been written to with empty `VersionID` rows — plain
      delete will create delete-markers for those legacy rows. Strata
      handles versioned buckets correctly only when versioning was
      enabled before any writes (per US-008's defensive design)
    - Cross-region setup is an anti-pattern: co-locate Strata gateway
      with backend bucket in the same region; cross-region GET incurs
      egress cost ($0.02/GiB on AWS) and adds 50–200 ms latency
- [ ] README.md "How to run" section gains a 4th option:
      `make up-s3-backend` (Strata + Cassandra + MinIO, no Ceph)
- [ ] CLAUDE.md "Big-picture architecture" diagram updated: data backend
      box now reads `data.Backend  memory | rados | s3`
- [ ] Typecheck passes (docs-only)
- [ ] Tests pass

### US-018: CI matrix entry — Strata + MinIO
**Description:** As a maintainer, I want every PR to also exercise the
S3-backend path against MinIO so regressions on the new backend surface
on the same SHA.

**Acceptance Criteria:**
- [ ] New job in `.github/workflows/ci.yml` named `e2e-s3-backend`
      (parallel with existing `e2e` job)
- [ ] Job runs `make up-s3-backend` + `make smoke-s3-backend`
- [ ] Job uploads container logs + Strata logs + MinIO logs as
      `e2e-s3-backend-logs` artefact
- [ ] Job timeout = 30 min
- [ ] Existing CI jobs unchanged; this is purely additive
- [ ] Typecheck passes
- [ ] Tests pass

### US-019: ROADMAP entry + close-flip
**Description:** As a maintainer, I want a new ROADMAP section for
alternative data backends (mirroring the existing "Alternative metadata
backends" section) and the S3 backend item flipped to Done on this
cycle's final commit.

**Acceptance Criteria:**
- [ ] Add new section to `ROADMAP.md` titled "Alternative data backends"
      (placed right after the existing "Alternative metadata backends"
      section)
- [ ] Section text mirrors the metadata-backends prose AND the
      "no community slots" policy already established for meta backends
      in the TiKV cycle (commit `40b45de`). RADOS is primary, S3 backend
      is an equal-tier alternative (this PRD). The supported set is
      **exactly two**: `rados` + `s3` (plus `memory` for tests).
      Filesystem / Azure Blob / GCS are explicitly NOT planned —
      operators who need those use Strata's S3 backend pointed at any
      S3-compatible service (MinIO can front a filesystem; Azure has
      `s3-proxy`; GCS has S3 interop API). Document this rationale in
      the section so future contributors do not propose adding them
- [ ] On the final cycle commit, flip the S3-backend bullet to Done
      close-flip format with the closing SHA (or `(commit pending)`)
- [ ] If any of US-002..US-018 left scope undone (e.g. lifecycle mapping
      partial), the bullet stays open and the partial work is documented
      under the bullet's "what's missing" sub-line
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- FR-1: `internal/data/s3` package implements `data.Backend` against any
  S3-compatible endpoint via aws-sdk-go-v2; one code path, no
  vendor-specific branches
- FR-2: PUT path streams via `manager.NewUploader` (small objects =
  single PutObject; large objects = multipart); peak memory bounded by
  `part_size * upload_concurrency`
- FR-3: GET path streams via `GetObject` with optional `Range` header;
  `NoSuchKey` maps to `data.ErrNotFound`
- FR-4: GC path uses `DeleteObjects` batched (≤1000 keys) when backend is
  S3; falls back to per-OID delete on partial failure
- FR-5: Configuration is env-only (`STRATA_S3_BACKEND_*`); credentials
  fall back to SDK default chain when not explicitly set
- FR-6: SDK retry is adaptive, bounded at 5 attempts; per-op timeout
  defaults are 30 s for short ops and 10 min for multipart-complete
- FR-7: Prometheus metrics expose per-op latency / error / retry counts
  via SDK middleware
- FR-8: `data.Manifest.BackendRef` is additive (JSON omitempty + protobuf
  field numbers); RADOS-mode manifests decode unchanged. The struct
  carries `VersionID` so Delete is correct on backends with versioning
  enabled, suspended, or absent — graceful degradation across all
  S3-compatible backends without per-backend code branches
- FR-9: Strata's client-facing multipart upload maps 1:1 to backend
  multipart upload — `BackendUploadID` stored in
  `multipart_uploads`; Complete on Strata triggers Complete on backend
- FR-10: SSE flag (`passthrough`/`strata`/`both`) is per-deployment;
  recorded per-object in `Manifest.SSE.Mode` for retrieval-side decryption
- FR-11: Lifecycle config translation is best-effort and bidirectional;
  Strata's lifecycle worker skips rules the backend handles natively
- FR-12: CORS config is mirrored to the backend bucket on `PutBucketCors`
  / `DeleteBucketCors`; Strata config is the source of truth on `GET`
- FR-13: Presigned URL passthrough is opt-in per-bucket; default is the
  current "URL points at Strata" behaviour
- FR-14: CI runs the S3-backend smoke pass on every PR alongside the
  existing RADOS smoke
- FR-15: ROADMAP.md gains an "Alternative data backends" section; the
  S3-backend bullet flips to Done on the cycle's final commit per the
  CLAUDE.md "Roadmap maintenance" rule

## Non-Goals

- **No "naive" 4-MiB-chunk-per-S3-object mode.** Native shape only. The
  naive variant would generate ~250× more requests and is not viable for
  production. Operators who want chunk-level addressability stay on RADOS
- **No metadata in S3.** Cassandra (or Scylla) remains the only metadata
  path. DynamoDB-as-meta-backend is a separate, future decision
- **No backend-side bucket creation.** The operator pre-creates the
  backend bucket; Strata refuses to start if the bucket does not exist
  or is not writable. Strata does not own the backend bucket lifecycle
- **No backend bucket per Strata bucket.** All Strata objects live in one
  backend bucket with random-prefix keys. AWS account-level bucket
  count limits (100 buckets default, raised on request) make per-bucket
  mapping unscalable
- **No backend bucket sharding.** One backend bucket per Strata
  deployment. Sharding across multiple backend buckets is a future P3
  story when measured throughput requires it
- **No replication-on-the-backend-side as a feature.** Operators can
  configure backend bucket replication (S3 CRR) themselves; Strata's
  own replicator worker (US-022 family of prior cycles) is not aware of
  it. Document as an operator concern in `docs/backends/s3.md`
- **No chunk dedup across versions.** Native shape stores each version
  as a fresh backend object. Dedup is a future P3 story if measurement
  shows it would matter
- **No DynamoDB / S3-Table backend.** "S3-as-meta" is a fundamentally
  different architecture, out of scope
- **No community-tier data backends.** Filesystem / Azure Blob / GCS /
  any other "community-maintained" data backend slot is explicitly NOT
  planned. The supported set is exactly two: `rados` + `s3`. Operators
  with non-S3-compatible storage point Strata's S3 backend at an
  S3-frontend that wraps their storage (MinIO over a filesystem,
  s3-proxy over Azure, GCS S3-interop). This mirrors the meta-backend
  policy adopted in the TiKV cycle: small supported set, deeper
  guarantees on each, no speculative community slots

## Technical Considerations

### Dependencies
- `aws-sdk-go-v2` core + `service/s3` + `feature/s3/manager` — adds ~6
  transitive deps. Acceptable for a P1 first-class feature
- aws-sdk-go-v1 already not present — no SDK conflict

### Performance characteristics
- Backend GET latency floor: ~50-100 ms (one HTTP hop) vs RADOS ~5 ms.
  Strata's hot path becomes Cassandra (~5 ms) + S3 GET (~80 ms) +
  Cassandra (~5 ms) ≈ 90 ms. Document as expected
- Backend PUT throughput: bounded by S3 per-prefix (3500/sec on AWS),
  raised by random UUID prefixes
- Cost model: AWS S3 PUT = $0.005/1k requests. With native shape, 1
  client PUT = 1 backend PUT (or N parts for multipart). 1M PUT/day =
  $5/day. Document
- Streaming GET memory bound: SDK exposes `Body io.ReadCloser`; gateway
  pipes through to the client. Bounded memory regardless of object size

### Backend compatibility caveats
- AWS S3: full feature set (lifecycle classes, CORS, replication)
- MinIO: lifecycle classes are a subset; document mapping in
  `docs/backends/s3.md`
- Ceph RGW: storage classes are user-defined; lifecycle translation is
  pass-through-strings
- Garage: simpler subset; document which tests fail against it
- Wasabi / B2-S3: similar to AWS, same code path; not in CI matrix

### Schema migrations
- `multipart_uploads` ALTER ADD COLUMN `backend_upload_id text`,
  `parts[N].backend_etag text` — additive via `alterStatements` in
  `internal/meta/cassandra/schema.go`
- `data.Manifest` protobuf field numbers stay stable; new fields get
  fresh numbers
- No destructive migrations

### Backwards compatibility
- Existing RADOS-mode deployments are unaffected — the new code path is
  reachable only via `STRATA_DATA_BACKEND=s3`, which is gated behind
  US-009
- Existing manifests with `Chunks []ChunkRef` decode unchanged
  (`BackendRef=nil`)
- Existing tests against memory + RADOS backends pass without
  modification

### Concurrent writes
- Strata's existing LWT-based metadata coherence does not change. The
  backend S3 is strongly consistent post-Dec 2020 (AWS) and from launch
  for MinIO / Ceph RGW. Eventual-consistent backends (Garage default)
  document caveat — readers may see "object not found" briefly after
  PUT-ack. This is not Strata's concern; readers retry per existing
  conditional-GET semantics

## Success Metrics

- All 19 stories shipped within one Ralph cycle on
  `ralph/s3-over-s3-backend`
- `make smoke-s3-backend` passes on every PR (CI green)
- ≥80% of `scripts/s3-tests/run.sh` default filter passes against the
  S3-backend variant on MinIO (sanity check that the new code path does
  not regress the surface)
- Documentation in `docs/backends/s3.md` is single-page, operator-
  actionable, and tested against every backend in the matrix
- ROADMAP "Alternative data backends" section live; S3-backend item
  flipped to Done

## Resolved Decisions

These were debated during PRD review; recording the chosen path so the
Ralph cycle does not re-litigate them.

- **Q1 — Backend object key format.** Resolved: `<bucket-uuid>/<object-uuid>`.
  UUID v4 first 4-8 hex chars give AWS S3 enough entropy for automatic
  prefix-partitioning; per-bucket grouping is a forensic bonus
  (`aws s3 ls s3://<bb>/<bucket-uuid>/`). No empirical benchmark
  required — UUID prefix distribution is a known good shape
- **Q2 — `STRATA_S3_BACKEND_BUCKET` required vs auto-created.**
  Resolved: required. Strata refuses to start if the bucket does not
  exist or is not writable. Auto-create would leak ownership — Strata
  must not own the backend bucket lifecycle. If operator wants the
  bucket created, they run `aws s3 mb` (or `mc mb`) once, before
  starting Strata
- **Q3 — Multi-backend mode (RADOS + S3 simultaneously, per-Strata-
  bucket policy).** Resolved: out of scope for this PRD. File as P2
  follow-up if a customer asks. Architecture supports it
  (`data.Backend` is per-instance), but the config + dispatch surface
  grows and the use case is unproven
- **Q4 — Cost telemetry.** Resolved: defer to P3 follow-up. Tracking
  pricing data per region adds a maintenance burden; operators can
  calculate from the existing
  `strata_data_s3_backend_op_total{op}` × pricing in their own
  Grafana
- **Q5 — Inter-region operation.** Resolved: document loudly in
  `docs/backends/s3.md` ("if Strata is in region X and the backend
  bucket is in region Y, every GET incurs cross-region egress; co-locate
  for production"); do NOT add startup region-mismatch detection or
  enforcement. Operator concern, not gateway concern
- **Q6 — Versioning on the backend.** Resolved: defensive support via
  per-object `BackendRef.VersionID` (US-002, US-004, US-008, US-010).
  Strata captures the SDK-returned version-id at PUT/Complete time,
  records it in the manifest, passes it back on Delete. Graceful
  degradation:
  - Backend without versioning support (e.g. Backblaze B2): SDK
    returns empty `VersionId`. Plain delete frees bytes. Works
  - Backend with versioning suspended: SDK returns `"null"`. Versioned
    delete with `VersionId="null"` cleans correctly. Works
  - Backend with versioning enabled: SDK returns UUID. Versioned
    delete bypasses delete-marker creation. Works
  - One known sharp edge: existing-bucket migration where operator
    enables versioning on a bucket Strata has been writing to
    (existing rows have `VersionID == ""`). Plain delete now creates
    delete-markers for those legacy rows. Document
    "do not enable versioning on an existing Strata-managed bucket" in
    `docs/backends/s3.md`. Strata does not policy this — operators
    own the backend bucket configuration

## Open Questions

(none — all decisions captured above)
