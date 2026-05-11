# PRD: Env-Driven Multi-Cluster S3 Data Backend

## Introduction

`internal/data/s3/backend.go` is single-bucket-per-instance today:
`Backend{bucket, client, uploader, opTimeout, sseMode, sseKMSKeyID}` carries
one S3 endpoint + one bucket per gateway process. The RADOS backend went
multi-cluster in US-044 of a prior cycle (`STRATA_RADOS_CLUSTERS` env +
per-class `ClassSpec.Cluster` routing); the S3 backend has no equivalent.

This PRD covers lifting the S3 backend to a multi-cluster shape, mirroring
the RADOS env-driven model. Two new envs hold the config: `STRATA_S3_CLUSTERS`
is a JSON array of bucket-less cluster specs, `STRATA_S3_CLASSES` is a JSON
object mapping storage class names to `{cluster, bucket}` tuples. Adding /
removing a cluster requires a gateway restart, but multi-instance deployments
hide per-instance downtime via rolling restart. Credentials are never stored
plaintext — `credentials_ref` discriminator (`chain` / `env:VAR1:VAR2` /
`file:/path[:profile]`) resolved at SDK-client-build time. Missing creds at
startup fail-fast — operator sees the error in systemd / k8s status
immediately.

All 13 legacy `STRATA_S3_BACKEND_*` envs retire entirely — registry-style
single source of truth, just config-driven instead of admin-API-driven (per
the 2026-05-11 design decision that nuked the dynamic-clusters cycle).

Closes ROADMAP P2 'Env-driven multi-cluster S3 data backend.' on cycle close.

## Goals

- New `S3ClusterSpec` (bucket-less): endpoint, region, force_path_style, part_size, upload_concurrency, max_retries, op_timeout_secs, sse_mode, sse_kms_key_id, credentials
- New `ClassSpec{Cluster, Bucket}` — both REQUIRED, no DefaultCluster fallback
- `data.s3.Backend` holds `clusters map[string]*s3Cluster, classes map[string]ClassSpec`; per-cluster `*awss3.Client` + `*manager.Uploader` lazy-built on first use via `connFor`
- `STRATA_S3_CLUSTERS` env is a JSON array of cluster specs; `STRATA_S3_CLASSES` env is a JSON object `{className: {cluster, bucket}}`
- All 13 `STRATA_S3_BACKEND_*` legacy envs retire entirely
- Credentials never plaintext — `credentials_ref` discriminator (`chain` / `env:VAR1:VAR2` / `file:/path[:profile]`); missing creds fail-fast at startup
- New `internal/data/s3/s3test` fixture package collapses per-test setup to a single `NewFixture(t)` call
- Closes ROADMAP P2 on cycle close

## User Stories

### US-001: S3ClusterSpec + ClassSpec types + JSON parser
**Description:** As a developer, I want the S3 multi-cluster type shape
defined and parseable from env JSON so subsequent stories build on a stable
surface.

**Acceptance Criteria:**
- [ ] Add `internal/data/s3/cluster.go` with `type S3ClusterSpec struct { ID, Endpoint, Region, SSEMode, SSEKMSKeyID string; ForcePathStyle bool; PartSize, UploadConcurrency, MaxRetries int64; OpTimeoutSecs int; Credentials CredentialsRef }` with full JSON tags
- [ ] `type CredentialsRef struct { Type string; Ref string }`. `Type ∈ {"chain", "env", "file"}`. `Ref` empty for `chain`; for `env` it is `"<ACCESS_KEY_VAR>:<SECRET_KEY_VAR>"`; for `file` it is the credentials file path (with optional `:profile` suffix)
- [ ] JSON marshal / unmarshal round-trip for `S3ClusterSpec`
- [ ] Add `internal/data/s3/class.go` with `type ClassSpec struct { Cluster, Bucket string }`. Both REQUIRED (validation in parser)
- [ ] `ParseClusters(jsonStr string) (map[string]S3ClusterSpec, error)` parses a JSON array; duplicate id → error; empty id / endpoint / region → error; unknown CredentialsRef.Type → error
- [ ] `ParseClasses(jsonStr string) (map[string]ClassSpec, error)` parses a JSON object; empty Cluster / Bucket per class → error; reference to non-existing cluster validated separately at Backend construction
- [ ] Unit test `internal/data/s3/cluster_test.go`: round-trip JSON for each CredentialsRef.Type; reject malformed env-ref (missing colon), reject unknown type; reject duplicate cluster id; reject empty fields
- [ ] Unit test `internal/data/s3/class_test.go`: parse two classes; empty Cluster → error; empty Bucket → error; class object missing required field → error
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: Lift data.s3.Backend to multi-cluster shape
**Description:** As a developer, I want `data.s3.Backend` to hold a map of
per-cluster S3 SDK clients so the gateway can serve traffic across multiple
S3 endpoints concurrently.

**Acceptance Criteria:**
- [ ] Refactor `internal/data/s3/backend.go::Backend` to hold `clusters map[string]*s3Cluster, classes map[string]ClassSpec, mu sync.Mutex` (no top-level `bucket`, `client`, `uploader`, `sseMode`, `sseKMSKeyID` fields — those move into per-cluster `s3Cluster`)
- [ ] `type s3Cluster struct { spec S3ClusterSpec; client *awss3.Client; uploader *manager.Uploader }` (private struct). Built lazily by `connFor(ctx, id) (*s3Cluster, error)` under `b.mu` on first use; mirrors `rados.Backend.connFor` semantics
- [ ] `connFor` resolves `s3Cluster.spec.Credentials` via the SDK loader: `chain` → `awsconfig.LoadDefaultConfig`; `env` → read named env vars + `credentials.NewStaticCredentialsProvider`; `file` → `awsconfig.LoadSharedConfigProfile` with parsed `path:profile`. Caches the built `*awss3.Client` + `*manager.Uploader`
- [ ] `resolveClass(class string) (ClassSpec, error)` mirrors rados shape: looks up `b.classes[class]`; missing class → `ErrUnknownStorageClass`; empty `Cluster` → `ErrClassMissingCluster`
- [ ] `New(cfg Config) (*Backend, error)` validates: every `cfg.Classes[*].Cluster` must reference a known `cfg.Clusters[id]`; every `cfg.Clusters[*].Credentials` resolves successfully at boot (fail-fast — missing env var / missing file → error from `New`); returns `*Backend` ready for traffic
- [ ] Cluster spec validation at boot: a sample `LoadDefaultConfig` call per `chain` cluster verifies creds resolve; a `getenv` check per `env:` ref verifies vars are set; a `os.Stat` per `file:` path verifies the file exists. Failures bubble up from `New` with a descriptive error
- [ ] Existing single-cluster test suite (`internal/data/s3/backend_test.go`, `multipart_test.go`, `presign_test.go`, `cors_test.go`, `lifecycle_test.go`, `metrics_test.go`) compiles green; semantic rewrite of those tests lands in US-005 via the new `s3test` fixture
- [ ] Race detector (`go test -race ./internal/data/s3/...`) clean
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: Bucket arg routing on every S3 data-plane method
**Description:** As a developer, I want every S3 data-plane method to read
the target bucket from the resolved `ClassSpec.Bucket` instead of the old
top-level `Backend.bucket` field so per-class routing works.

**Acceptance Criteria:**
- [ ] `Put` / `Get` / `Delete` / `Head` / `Copy` / `List` / `PutChunks` / `GetChunks` resolve `(cluster, bucket) := b.resolveClass(class)` first; bucket flows into every SDK call (`PutObjectInput.Bucket`, `GetObjectInput.Bucket`, etc.) — no `b.bucket` reference remains
- [ ] Multipart methods (`CreateBackendMultipart` / `UploadBackendPart` / `CompleteBackendMultipart` / `AbortBackendMultipart`) thread the upload's resolved bucket into every SDK call — verify `meta.MultipartUpload.BackendUploadID` carries enough to re-resolve on subsequent calls OR persist the cluster id alongside if needed
- [ ] `Presign*` methods route through the same resolution; SSE-related per-cluster knobs (`SSEMode`, `SSEKMSKeyID`) come from `s3Cluster.spec`, not from a top-level field
- [ ] `objectKey(ctx context.Context)` preserved as-is — keys are bucket-relative
- [ ] New unit test `internal/data/s3/multi_cluster_test.go`: configure two clusters + three classes (class A → cluster-eu/bucket-a, class B → cluster-eu/bucket-b, class C → cluster-us/bucket-c) against in-process httptest S3 fakes; PutChunks for each class lands on the correct (cluster, bucket); cross-class read isolation; `resolveClass` for unknown class → `ErrUnknownStorageClass`
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: Retire STRATA_S3_BACKEND_* env path
**Description:** As a developer, I want the legacy env-based single-cluster
config fully removed so there is exactly ONE source of truth for the S3
backend — the new `STRATA_S3_CLUSTERS` + `STRATA_S3_CLASSES` envs.

**Acceptance Criteria:**
- [ ] Remove `S3Backend` field from `internal/config/config.go::Config` AND all 13 `STRATA_S3_BACKEND_*` entries from the koanf env-map (`_ENDPOINT`, `_REGION`, `_BUCKET`, `_ACCESS_KEY`, `_SECRET_KEY`, `_FORCE_PATH_STYLE`, `_PART_SIZE`, `_UPLOAD_CONCURRENCY`, `_MAX_RETRIES`, `_OP_TIMEOUT_SECS`, `_MULTIPART_TIMEOUT_SECS`, `_SSE_MODE`, `_SSE_KMS_KEY_ID`)
- [ ] Remove the corresponding `S3BackendConfig` struct + `Validate*` body in `config.go`
- [ ] Add `S3 struct { Clusters string \`koanf:"clusters"\`; Classes string \`koanf:"classes"\` }` (top-level holds raw JSON strings) to `Config`. Wire `STRATA_S3_CLUSTERS` → `s3.clusters`, `STRATA_S3_CLASSES` → `s3.classes` in the koanf env-map
- [ ] `internal/serverapp/serverapp.go::buildDataBackend` `case "s3":` parses `cfg.S3.Clusters` via `s3.ParseClusters`, `cfg.S3.Classes` via `s3.ParseClasses`, then calls `s3.New(s3.Config{Clusters, Classes})`. Validation errors from `s3.New` bubble up — gateway refuses to start
- [ ] Update `deploy/docker/docker-compose.yml` if it sets `STRATA_S3_BACKEND_*`; document the new bootstrap path (set `STRATA_S3_CLUSTERS` + `STRATA_S3_CLASSES`)
- [ ] Update `scripts/s3-tests/run.sh` + smoke harness if they reference the legacy envs; switch to the new env shape
- [ ] `make smoke` still passes against an S3 backend path (if smoke profile uses S3; otherwise smoke remains rados-only)
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: s3test fixture package for unit tests
**Description:** As a developer, I want a one-call test fixture that wires
up a `data.s3.Backend` + cluster + class so existing per-file test setup
collapses to a single `s3test.NewFixture(t)` call.

**Acceptance Criteria:**
- [ ] Add `internal/data/s3/s3test/fixture.go` with `Fixture{Backend *s3.Backend, ClusterID, ClassName, Bucket string}` and `NewFixture(t testing.TB) *Fixture` constructor: builds ONE cluster spec pointing at an httptest server, registers ONE class mapped to that cluster + a random bucket name, returns a wired `*s3.Backend`
- [ ] Variants: `WithClusters(specs ...S3ClusterSpec) Option`, `WithClass(name, clusterID, bucket string) Option` for tests that need multiple clusters / classes
- [ ] Existing single-cluster test suite (`backend_test.go`, `multipart_test.go`, `presign_test.go`, `cors_test.go`, `lifecycle_test.go`, `metrics_test.go`) refactored to use `s3test.NewFixture` instead of building Backend + env config inline — net LOC reduction; semantics unchanged
- [ ] Document the fixture in a one-paragraph header on `s3test/fixture.go`
- [ ] Race detector clean
- [ ] Typecheck passes
- [ ] Tests pass

### US-006: Docs + ROADMAP close-flip
**Description:** As a developer, I want operator-facing docs + the ROADMAP
P2 entry flipped in the same commit when the cycle closes.

**Acceptance Criteria:**
- [ ] Add `docs/site/content/best-practices/s3-multi-cluster.md` covering: `S3ClusterSpec` shape (endpoint, region, force-path-style, etc.) + `credentials_ref` discriminator (`chain` / `env:VAR:VAR` / `file:/path:profile`), per-class routing via `ClassSpec{Cluster, Bucket}`, full env JSON examples (`STRATA_S3_CLUSTERS` + `STRATA_S3_CLASSES`), rolling-restart workflow for cluster add / remove, fail-fast credential validation at boot
- [ ] Update `docs/site/content/best-practices/_index.md` table with the new page row
- [ ] Reference doc `docs/site/content/reference/_index.md` — REMOVE the 13 `STRATA_S3_BACKEND_*` rows; ADD `STRATA_S3_CLUSTERS` + `STRATA_S3_CLASSES` rows + deep-links to the new doc page
- [ ] ROADMAP.md close-flip in the same commit per CLAUDE.md Roadmap maintenance rule: `~~**P2 — Env-driven multi-cluster S3 data backend.**~~ — **Done.** ... (commit `<pending>`)`
- [ ] Delete `tasks/prd-s3-multi-cluster.md` per CLAUDE.md PRD lifecycle rule (Ralph snapshot is the canonical record)
- [ ] `make docs-build` clean (Hugo strict-ref resolution catches dangling refs)
- [ ] Closing-SHA backfill follow-up commit on main per established pattern
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- FR-1: `internal/data/s3/cluster.go` defines `S3ClusterSpec` + `CredentialsRef`; `internal/data/s3/class.go` defines `ClassSpec{Cluster, Bucket}` (both REQUIRED)
- FR-2: `ParseClusters(jsonStr) map[string]S3ClusterSpec` and `ParseClasses(jsonStr) map[string]ClassSpec` parse JSON env strings with strict validation (duplicate ids, empty required fields, unknown CredentialsRef.Type all rejected)
- FR-3: `data.s3.Backend` holds `clusters map[string]*s3Cluster` + `classes map[string]ClassSpec`; per-cluster SDK clients lazy-built via `connFor`; data-plane methods route through `resolveClass`
- FR-4: `s3.New` validates every class.Cluster references a known cluster AND every cluster.Credentials resolves at boot (`chain` → `LoadDefaultConfig`; `env:` → env vars set; `file:` → file exists); failure → fail-fast, gateway refuses to start
- FR-5: `credentials_ref` types `chain` / `env:VAR1:VAR2` / `file:/path[:profile]` resolved at SDK-client-build time via AWS SDK loaders; never stored plaintext in any spec or in any meta tier
- FR-6: All 13 `STRATA_S3_BACKEND_*` envs retired in US-004; replaced by `STRATA_S3_CLUSTERS` (JSON array) + `STRATA_S3_CLASSES` (JSON object) — the only S3-related envs
- FR-7: Adding / removing a cluster requires gateway restart; rolling restart of N replicas hides per-instance downtime
- FR-8: `internal/data/s3/s3test.NewFixture(t)` constructs a fully-wired Backend + cluster + class in one call; existing tests adopt it

## Non-Goals

- **No dynamic registry.** Cluster set is config-only; no admin API, no `meta.Store` cluster table. Adding a cluster = update env + rolling restart. Per design decision 2026-05-11
- **No probe-dial on cluster add.** Validation at boot is "credentials resolve" — no actual S3 round-trip. Broken endpoint surfaces on first traffic with `503` upstream
- **No DefaultCluster fallback.** Every storage class MUST have an explicit `Cluster` in `ClassSpec`
- **No chunk-side data migration.** When a cluster is removed, manifests still referencing its bucket fail fast on read. Migration belongs to the separate rebalance worker P2 entry
- **No KMS-fetched credentials.** `credentials_ref.Type=="kms"` is a P3 follow-up. Modern operators use IRSA / IAM roles via `chain` anyway
- **No legacy single-cluster compat mode.** Existing `STRATA_S3_BACKEND_*` deployments break on upgrade — operators MUST migrate to the new env shape
- **No streaming spec reload.** SIGHUP / file-watcher reload is out of scope. Restart-only

## Technical Considerations

### Bucket-less spec + per-class Bucket

`S3ClusterSpec` carries the endpoint + region + transport knobs; the
destination bucket lives on `ClassSpec.Bucket`. Two storage classes can
share one S3 cluster but route to different buckets — useful for cold-
tier-via-glacier-bucket vs hot-tier-via-standard-bucket on the same AWS
account.

### Credentials never plaintext

`CredentialsRef.Type ∈ {"chain", "env", "file"}` keeps secrets out of env
values themselves:
- `chain`: standard AWS SDK default chain (env / shared config / IRSA / EC2 metadata)
- `env:NAME1:NAME2`: read named env vars at gateway startup → `credentials.NewStaticCredentialsProvider`. The env vars holding the actual keys are separate from `STRATA_S3_CLUSTERS` itself
- `file:/path/credentials[:profile]`: load via `awsconfig.LoadSharedConfigProfile`

Operators rotate credentials by updating the source the ref points to —
no `STRATA_S3_CLUSTERS` rewrite needed for key rotation alone.

### Fail-fast at boot

If any cluster's `chain` provider fails to load, or any `env:` ref points
at a missing env var, or any `file:` path doesn't exist, `s3.New` returns
an error. `serverapp.Run` propagates → gateway process exits with
non-zero status → systemd / k8s shows the error immediately. Operators
catch misconfig before it serves traffic.

### Rolling restart workflow

Multi-instance deployment: N gateway replicas behind a load balancer. To
add a new cluster: update `STRATA_S3_CLUSTERS` in the systemd unit /
k8s configmap, rolling-restart replicas one at a time. Each replica
starts with the new spec; the load balancer drains the old replica
before stopping it. No traffic interruption visible to clients.

### Lazy-init on first use

`connFor` builds the `*awss3.Client` + `*manager.Uploader` on first
traffic. Backend construction reserves the placeholder; first PUT pays
the SDK config-load cost. Acceptable — mirrors rados.

### Multi-storage-class bucket gotcha

`ClassSpec.Bucket` per class is a per-tenant grouping decision. Two
classes pointing at the same `(cluster, bucket)` is legal — they share
the AWS-side bucket but the Strata-side class boundary still applies for
lifecycle / inventory / quotas.

## Success Metrics

- Operator can update `STRATA_S3_CLUSTERS` to add a new cluster, rolling-restart N replicas, and traffic lands on the new cluster within one replica's restart
- Two storage classes routing to two different buckets on the same S3 cluster work in parallel; cross-class reads stay isolated
- Race-detector clean; existing single-cluster S3 tests still green under the new shape via `s3test.NewFixture`
- `STRATA_S3_BACKEND_*` env vars fully gone from `internal/config/`, deploy/docker, and docs
- Gateway fails to start with descriptive error if any cluster's credentials don't resolve at boot

## Open Questions

- Should `CredentialsRef.Type == "kms"` (fetch from AWS KMS) be in this cycle or a P3 follow-up? Default: P3 follow-up
- JSON env strings get long — should `STRATA_S3_CLUSTERS_FILE=/path/to.json` env be added as an alternative to inline JSON? Likely yes — single-line escaping for 5+ clusters in docker-compose YAML is painful. Decide in US-001 implementer
- Should the fail-fast validator also do a sample `HeadBucket` per `(cluster, class)` pair to verify the cluster can see the configured bucket? Adds latency at startup (one HEAD per pair); rejects misconfig earlier. Default: skip (fail-fast on creds only); revisit if operators hit silent misconfig
