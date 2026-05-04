# PRD: Web UI — Admin (Phase 2 of 3)

> **Status:** Detailed AC. Phase 1 (`tasks/archive/prd-web-ui-foundation.md`)
> shipped 2026-05-03 via commit `e27cf21` — embedded console at
> `/console/`, `/admin/v1/*` JSON API with read-only handlers,
> `internal/adminapi` + `internal/heartbeat` packages live on `main`.
> Phase 2 builds on this foundation.
>
> **Backend audit (2026-05-03, post-merge `main` = modern-complete +
> binary-consolidation + tikv-meta-backend + web-ui-foundation +
> s3-over-s3-backend):**
>
> - Every S3 admin surface Phase 2 wraps already exists on main: bucket
>   lifecycle / CORS / policy / ACL / inventory / access-log /
>   notification / replication / website / encryption / object-lock
>   endpoints (modern-complete). IAM users + access keys + STS
>   (modern-complete US-036 family). Audit log + retention sweeper.
>   Multipart upload tracking. Phase 2 wraps these in UI; **no new
>   backend surface beyond the `/admin/v1/*` write endpoints needed**.
> - **`meta.Bucket.BackendPresign` + `meta.Store.SetBucketBackendPresign`
>   already exist on main** (s3-over-s3 US-016). Phase 2 just wraps
>   them in an admin endpoint + UI toggle.
> - Phase 1 surface: versioned `/admin/v1/*` API with HS256 JWT session
>   cookies (24 h, HttpOnly, SameSite=Strict, Path=/admin) + SigV4
>   fallback. All Phase 2 write endpoints sit under the same prefix
>   with the same auth chain.

## Introduction

Phase 1 ships read-only operator dashboards. Phase 2 adds **write
actions for operators**: bucket admin (create / delete / versioning /
object-lock / lifecycle / CORS / policy / ACL / inventory /
access-log), IAM admin (users + access keys + managed policies),
object actions (upload / delete / tags / retention), multipart
watchdog, audit-log viewer, settings page. After Phase 2 an operator
performs every aws-cli admin task from the console.

Phase 3 (`prd-web-ui-debug.md`) ships performance debug tooling.

## Goals

- Bucket admin: Create / Delete / edit Versioning / edit Object-Lock
  default / edit Lifecycle / edit CORS / edit Bucket Policy / edit
  ACL / edit Inventory / edit Access-log target / toggle BackendPresign
  (s3-over-s3 only)
- IAM admin: Create / Delete users; Create / Rotate / Delete access
  keys; Attach / Detach managed policies; managed-policies CRUD
- Object actions: Upload (single PUT + multipart wizard via per-part
  presigned URLs), Delete, Set tags, Set retention / legal-hold
- Multipart watchdog: list pending in-flight multipart uploads cluster-
  wide, abort selected, see age + bytes-uploaded
- Audit-log viewer: filters (date range, action, principal, bucket),
  pagination, export-CSV
- Settings page: cluster name, JWT secret rotation, Prometheus URL,
  heartbeat interval, theme defaults, S3-backend env-var read-only
- Monaco editor for Bucket Policy JSON (+ IAM-schema validation) and
  Lifecycle / CORS JSON-view tab — `~2 MiB` lazy-loaded only on the
  editor pages so home / metrics bundles stay ≤500 KiB
- Browser-side multipart upload via per-part presigned URLs minted by
  the gateway (gateway holds the access-key secret; browser only sees
  short-lived URLs)
- Playwright e2e suite at `web/e2e/admin.spec.ts` covers the critical
  admin paths (CreateBucket → upload → DeleteObject → DeleteBucket;
  IAM user + key CRUD)
- ROADMAP P3 entries flip to Done close-flip per CLAUDE.md "Roadmap
  maintenance"

## User Stories

### US-001: CreateBucket dialog
**Description:** As an operator, I want a modal that creates a new
bucket with a name validator, region selector, versioning toggle, and
Object-Lock default toggle so I do not have to drop to aws-cli for the
common case.

**Acceptance Criteria:**
- [ ] `POST /admin/v1/buckets` JSON endpoint accepts `{name, region,
      versioning, object_lock_enabled}` and calls
      `meta.Store.CreateBucket` (existing). Returns 201 + bucket
      details on success, 409 BucketAlreadyExists on collision, 400
      InvalidBucketName on reserved name (uses
      `internal/s3api.validBucketName` already on main)
- [ ] `web/src/pages/Buckets.tsx` adds "Create" button → opens
      `<CreateBucketDialog>` (shadcn `<Dialog>`)
- [ ] Dialog form: name (live-validates per `validBucketName` rules
      visible inline as user types), region (defaults to current
      cluster region from `/admin/v1/cluster/status`), Versioning
      toggle (Suspended | Enabled, default Suspended), Object-Lock
      Enabled checkbox (greyed-out when Versioning=Suspended; AWS
      requires Versioning=Enabled to accept Object-Lock at create)
- [ ] Submit → calls `POST /admin/v1/buckets`. On success: dialog
      closes, bucket list refetches, toast "Bucket <name> created"
- [ ] Error path renders the server error code + message inline; the
      dialog stays open
- [ ] Audit log entry recorded by existing `s3api.AuditMiddleware`
      (already wraps `/admin/v1/*` since the s3-over-s3 merge — verify
      a row appears with `Action="admin:CreateBucket"`,
      `Resource="bucket:<name>"`)
- [ ] Typecheck passes (Go + `pnpm run typecheck`)
- [ ] Tests pass (Go-side handler unit test + UI render test)
- [ ] Verify in browser using dev-browser skill

### US-002: DeleteBucket flow
**Description:** As an operator, I want to delete a bucket with a
type-to-confirm guard so I don't nuke a bucket by accident.

**Acceptance Criteria:**
- [ ] `DELETE /admin/v1/buckets/{bucket}` JSON endpoint calls
      `meta.Store.DeleteBucket` (existing). Returns 204 on success,
      409 BucketNotEmpty if rows remain, 404 if bucket missing
- [ ] Bucket detail page adds "Delete" button (red destructive
      variant). Click → `<DeleteBucketDialog>` requires the operator
      to type the bucket name verbatim before "Delete" enables
- [ ] When the bucket is non-empty the dialog shows the object count
      and a "Force delete (delete all objects then bucket)" checkbox.
      With the checkbox set the UI calls `POST /admin/v1/buckets/{bucket}/force-empty`
      (new endpoint, paginated `ListObjects` + `DeleteObject` loop in
      a goroutine — returns 202 with a job-id; client polls
      `GET /admin/v1/buckets/{bucket}/force-empty/{job-id}` until
      `state=done`, then issues the regular `DELETE`)
- [ ] Operation lock: `POST /admin/v1/buckets/{bucket}/force-empty`
      writes a row keyed `bucket-force-empty:<bucket>` into the
      existing `worker_locks` table to prevent concurrent runs;
      returns 409 if a row is already there
- [ ] Audit log records `admin:DeleteBucket` and (when force-empty
      ran) one `admin:ForceEmpty` row per `force-empty` job
- [ ] Typecheck passes
- [ ] Tests pass (handler unit test + force-empty job test)
- [ ] Verify in browser using dev-browser skill

### US-003: Versioning + Object-Lock toggles on bucket detail
**Description:** As an operator, I want to flip Versioning state and
edit the Object-Lock default retention from the bucket detail page so
common config changes do not require aws-cli.

**Acceptance Criteria:**
- [ ] `PUT /admin/v1/buckets/{bucket}/versioning` accepts
      `{state: "Enabled"|"Suspended"}` → calls
      `meta.Store.SetBucketVersioning`. Reject "Disabled" with 400
      (S3 spec — once enabled, only Suspended is valid)
- [ ] `PUT /admin/v1/buckets/{bucket}/object-lock` accepts AWS-shape
      `ObjectLockConfiguration` JSON → calls existing
      `meta.Store.SetBucketObjectLockConfig`
- [ ] Bucket detail page gains an "Overview" tab with two cards:
      "Versioning" (radio: Enabled | Suspended) and "Object-Lock
      default" (Mode: Governance | Compliance | Off; Retention: Days
      | Years numeric). Save buttons under each card; saves are
      independent
- [ ] Versioning toggle: when flipping Suspended → Enabled, show
      tooltip "Once enabled, versioning cannot be disabled — only
      suspended"
- [ ] Object-Lock card greyed-out + tooltip "Object-Lock requires
      Versioning=Enabled" when state=Suspended
- [ ] Audit rows: `admin:SetBucketVersioning`,
      `admin:SetBucketObjectLockConfig`
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-004: Lifecycle rule editor (visual + JSON tab)
**Description:** As an operator, I want a visual editor for bucket
lifecycle rules with a parallel JSON tab for advanced edits so simple
rules are clickable but expert flows survive.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/buckets/{bucket}/lifecycle` returns the existing
      `LifecycleConfiguration` blob via
      `meta.Store.GetBucketLifecycle` (already on main)
- [ ] `PUT /admin/v1/buckets/{bucket}/lifecycle` validates the body
      against `s3api.parseLifecycleConfiguration` (existing) and
      persists via `meta.Store.SetBucketLifecycle`
- [ ] Bucket detail "Lifecycle" tab has two sub-tabs: "Visual" and
      "JSON" (Monaco editor, lazy-loaded `import("monaco-editor")` so
      the Buckets page bundle stays under 500 KiB gzipped)
- [ ] Visual builder per rule: ID, Filter (Prefix + Tags), Status
      (Enabled | Disabled), Transitions (Days OR Date → StorageClass:
      STANDARD_IA | GLACIER | DEEP_ARCHIVE), Expiration (Days OR
      Date), NoncurrentVersionTransitions / NoncurrentVersionExpiration
      (Days), AbortIncompleteMultipartUpload (DaysAfterInitiation)
- [ ] "+ Add rule" button appends a fresh empty rule. Each rule has
      a delete (trash icon) and reorder (drag-handle, react-dnd or
      pointer-based)
- [ ] JSON tab shows the equivalent JSON live-synced to the visual
      state. Schema-validated against
      `https://json-schema.org/draft-07/schema#` of the AWS lifecycle
      shape (vendored at `web/src/schemas/lifecycle.json`); errors
      render in Monaco's gutter
- [ ] Save: visual edits → JSON serialised → POST. JSON-tab edits →
      reparsed back into the visual on tab-switch (with a confirm if
      JSON has unsaved errors)
- [ ] Audit row: `admin:SetBucketLifecycle`
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-005: CORS rule editor (visual + JSON tab)
**Description:** As an operator, I want a visual + JSON CORS rule
editor for the same reasons as the lifecycle editor.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/buckets/{bucket}/cors` calls existing
      `meta.Store.GetBucketCORS`; `PUT` calls `SetBucketCORS` with
      `s3api.parseCORSConfiguration` validation; `DELETE` calls
      `DeleteBucketCORS`
- [ ] "CORS" tab on bucket detail. Visual builder per rule: AllowedMethods
      (multi-select GET/PUT/POST/DELETE/HEAD), AllowedOrigins (chip
      list), AllowedHeaders, ExposeHeaders, MaxAgeSeconds, ID
- [ ] JSON tab uses Monaco (same lazy-load pattern as US-004)
- [ ] "Delete configuration" button (calls DELETE endpoint, full
      removal — different from "delete one rule" in the visual builder)
- [ ] Audit row: `admin:SetBucketCORS` / `admin:DeleteBucketCORS`
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-006: Bucket Policy JSON editor (Monaco + IAM schema)
**Description:** As an operator, I want a Monaco-based JSON editor
for bucket policies with IAM policy spec validation so syntax errors
are caught client-side.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/buckets/{bucket}/policy` calls existing
      `meta.Store.GetBucketPolicy`; `PUT` validates via
      `s3api.parseBucketPolicy` and calls `SetBucketPolicy`; `DELETE`
      calls `DeleteBucketPolicy`
- [ ] "Policy" tab on bucket detail. Monaco editor (lazy-loaded) with
      JSON syntax highlight + IAM-policy schema validation (vendored
      at `web/src/schemas/iam-policy.json` — minimal subset:
      Version, Statement[Effect, Action, Resource, Principal,
      Condition])
- [ ] "Validate" button runs the schema check + the gateway's
      `parseBucketPolicy` (via a `POST /admin/v1/buckets/{bucket}/policy/dry-run`
      endpoint that returns 200 valid / 400 with parse error) without
      saving
- [ ] "Save" button persists; "Delete" button (red) clears the
      configuration with a type-to-confirm
- [ ] "Generate template" dropdown inserts a starter policy
      (PublicRead | ReadOnlyAuthenticated | DenyAllExceptUser)
- [ ] Audit row: `admin:SetBucketPolicy` / `admin:DeleteBucketPolicy`
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-007: Bucket ACL editor
**Description:** As an operator, I want to edit the canned ACL and
explicit grants on a bucket without dropping to aws-cli.

**Acceptance Criteria:**
- [ ] `PUT /admin/v1/buckets/{bucket}/acl` accepts
      `{canned: "private"|"public-read"|"public-read-write"|"authenticated-read"|"log-delivery-write",
      grants: [Grant]}` and calls
      `meta.Store.SetBucketACL` + `SetBucketGrants` (existing on main)
- [ ] "ACL" tab on bucket detail. Top section: canned ACL radio.
      Bottom section: grant list (table: Grantee Type | Identifier |
      Permission). "+ Add grant" row inserter. Permission select:
      FULL_CONTROL | READ | WRITE | READ_ACP | WRITE_ACP. Grantee
      Type select: CanonicalUser | Group | AmazonCustomerByEmail
- [ ] Group grants pre-filled with valid URIs
      (`http://acs.amazonaws.com/groups/global/AllUsers` etc.) via a
      dropdown of canonical group names → URI
- [ ] Save → reload from `GetBucketGrants` to confirm round-trip
- [ ] Audit row: `admin:SetBucketACL`
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-008: Inventory configurations CRUD
**Description:** As an operator, I want to create / edit / delete
S3 Inventory configurations on a bucket.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/buckets/{bucket}/inventory` lists all
      configurations via existing
      `meta.Store.ListBucketInventoryConfigs`
- [ ] `PUT /admin/v1/buckets/{bucket}/inventory/{configID}` parses
      via `s3api.parseInventoryConfig` and calls
      `SetBucketInventoryConfig`
- [ ] `DELETE /admin/v1/buckets/{bucket}/inventory/{configID}` calls
      `DeleteBucketInventoryConfig`
- [ ] "Inventory" tab on bucket detail with configurations table
      (ID | Schedule | Destination Bucket | Format | Enabled).
      "+ Add" opens the inventory editor dialog
- [ ] Editor form: ID, Filter Prefix, IncludedObjectVersions
      (Current | All), Schedule (Daily | Weekly), Destination Bucket
      (autocomplete from `ListBuckets`), Destination Prefix, Format
      (CSV | ORC | Parquet), OptionalFields (multi-select),
      IsEnabled toggle
- [ ] Audit row: `admin:SetInventoryConfig` / `admin:DeleteInventoryConfig`
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-009: Access-log target editor
**Description:** As an operator, I want to configure the access-log
target for a bucket from the UI.

**Acceptance Criteria:**
- [ ] `GET / PUT / DELETE /admin/v1/buckets/{bucket}/logging` wrap
      existing `meta.Store.{Get,Set,Delete}BucketLogging`
- [ ] "Access Log" tab on bucket detail. Form: Target Bucket
      (autocomplete + must be writable by the access-log worker —
      surface a warning if missing `s3:PutObject` for the worker
      identity), Target Prefix, Target Grants (same Grantee Type +
      Permission shape as US-007)
- [ ] "Disable logging" deletes the configuration via the DELETE
      endpoint
- [ ] Inline preview: "Logs will land at `s3://<target-bucket>/<prefix>YYYY-MM-DD-HH-MM-SS-RAND/`"
- [ ] Audit row: `admin:SetBucketLogging` / `admin:DeleteBucketLogging`
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-010: IAM Users CRUD page
**Description:** As an operator, I want to create / list / delete
IAM users from the UI.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/iam/users` paginated via existing
      `meta.Store.ListIAMUsers`. Columns: UserName | UserID |
      CreatedAt | AccessKeyCount (computed via
      `meta.Store.ListIAMAccessKeysByUser`)
- [ ] `POST /admin/v1/iam/users` accepts `{user_name, path}` and
      calls `meta.Store.CreateIAMUser`
- [ ] `DELETE /admin/v1/iam/users/{userName}` cascades: deletes all
      `IAMAccessKey` rows for the user via
      `meta.Store.DeleteIAMAccessKey` per row, then
      `meta.Store.DeleteIAMUser`. Wrapped in a write lock named
      `iam-user:<name>` to prevent concurrent rotations during delete
- [ ] New `web/src/pages/IAM.tsx` with a Users tab (this story) and
      a stub for Access Keys (US-011) and Policies (US-012)
- [ ] CreateUser dialog: UserName (live-validate `^[a-zA-Z0-9_+=,.@-]{1,64}$`),
      Path (default `/`)
- [ ] DeleteUser confirmation modal lists the access keys that will
      be deleted alongside, requires type-to-confirm of the user name
- [ ] Audit rows: `admin:CreateUser`, `admin:DeleteUser`,
      `admin:DeleteAccessKey` (one per cascaded key)
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-011: Access keys CRUD per user
**Description:** As an operator, I want to create / rotate / disable
/ delete access keys per IAM user.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/iam/users/{userName}/access-keys` lists keys for
      a user via `meta.Store.ListIAMAccessKeysByUser` (existing).
      Returns: AccessKeyID, CreatedAt, Disabled. **Never** the
      SecretAccessKey
- [ ] `POST /admin/v1/iam/users/{userName}/access-keys` mints a new
      key via `meta.Store.CreateIAMAccessKey` (existing) and returns
      `{access_key, secret_key}` once — the secret never appears
      again on subsequent GETs
- [ ] `PATCH /admin/v1/iam/access-keys/{accessKey}` accepts
      `{disabled: bool}` and calls
      `meta.Store.UpdateIAMAccessKeyDisabled` (existing). Calls
      `apiHandler.InvalidateCredential(accessKey)` on the in-memory
      MultiStore so the next signed request reads the new state
- [ ] `DELETE /admin/v1/iam/access-keys/{accessKey}` calls
      `meta.Store.DeleteIAMAccessKey` + `InvalidateCredential`
- [ ] User detail page (route `/iam/users/:userName`) shows the access
      keys table with Create / Disable / Enable / Delete actions
- [ ] Create-key dialog returns the new secret key in a "copy to
      clipboard" panel with a banner: "This is the only time the
      secret will be shown. Copy it now"
- [ ] Audit rows: `admin:CreateAccessKey`, `admin:DisableAccessKey`,
      `admin:EnableAccessKey`, `admin:DeleteAccessKey`
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-012: Managed policies CRUD page
**Description:** As an operator, I want to create / list / delete
managed IAM policies that can be attached to users.

**Acceptance Criteria:**
- [ ] Backend gap: managed-policy storage is **NEW** — `internal/meta`
      does not yet have ManagedPolicy. Add: `meta.ManagedPolicy{Arn,
      Name, Path, Description, Document, CreatedAt, UpdatedAt}`,
      `meta.Store.{Create,Get,List,Update,Delete}ManagedPolicy`,
      Cassandra schema `iam_managed_policies`, memory + tikv impls.
      Storage shape: PRIMARY KEY (arn). List uses materialized
      view `iam_managed_policies_by_path` for path-prefix queries
- [ ] `GET /admin/v1/iam/policies` lists; `POST /admin/v1/iam/policies`
      creates; `PUT /admin/v1/iam/policies/{arn}/document` updates
      the document; `DELETE /admin/v1/iam/policies/{arn}` deletes
      (only when no attachments — see US-013)
- [ ] Policies tab on `/iam`. Columns: Name | Path | CreatedAt |
      UpdatedAt | AttachmentCount
- [ ] Create / Edit dialog uses Monaco with the same IAM schema as
      US-006 (Bucket Policy)
- [ ] Audit rows: `admin:CreateManagedPolicy`, `admin:UpdateManagedPolicy`,
      `admin:DeleteManagedPolicy`
- [ ] Storetest contract entries for the new methods
- [ ] Typecheck passes
- [ ] Tests pass (memory + cassandra + tikv contract)
- [ ] Verify in browser using dev-browser skill

### US-013: Attach / detach managed policies
**Description:** As an operator, I want to attach / detach managed
policies to / from IAM users.

**Acceptance Criteria:**
- [ ] Backend gap: attachment table — `meta.Store.{Attach,Detach,List}UserPolicy`
      methods. Cassandra schema `iam_user_policies` PRIMARY KEY
      (user_name, policy_arn). Mirror in memory + tikv
- [ ] `POST /admin/v1/iam/users/{userName}/policies` accepts
      `{policy_arn}` → AttachUserPolicy; `DELETE` → DetachUserPolicy;
      `GET /admin/v1/iam/users/{userName}/policies` → ListUserPolicy
- [ ] User detail page Policies tab: list of attached policies.
      "Attach" opens a dialog with a searchable list of all managed
      policies (from US-012); detach via an X icon per row
- [ ] When `meta.Store.DeleteManagedPolicy` (US-012) finds a non-zero
      attachment count, returns `meta.ErrPolicyAttached` → handler
      surfaces 409 with the attached user list
- [ ] Audit rows: `admin:AttachUserPolicy`, `admin:DetachUserPolicy`
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-014: Object upload wizard (per-part presigned URLs)
**Description:** As an operator, I want to upload an object via the
console with a progress bar and multipart support for >5 MiB files.

**Acceptance Criteria:**
- [ ] `POST /admin/v1/buckets/{bucket}/uploads` mints `{upload_id,
      part_size}` and calls existing `meta.Store.CreateMultipartUpload`
      + `s3api` initiate path. Returns the upload_id
- [ ] `POST /admin/v1/buckets/{bucket}/uploads/{uploadID}/parts/{partNumber}/presign`
      mints a presigned URL for an `UploadPart` request via the
      existing `internal/auth/presigned` package, signed with a server-
      held credential — the operator's session-cookie identity is
      checked, **not** their personal AK/SK. URL expires in 5 minutes
- [ ] `POST /admin/v1/buckets/{bucket}/uploads/{uploadID}/complete`
      passes through to the existing `s3api` Complete handler with
      the part list
- [ ] Single-PUT path for files ≤5 MiB: `POST /admin/v1/buckets/{bucket}/objects/{key}/single-presign`
      → returns a presigned `PUT` URL (5-minute TTL); browser PUTs
      directly. Skips the multipart roundtrip for small objects
- [ ] Bucket detail "Objects" panel adds an "Upload" button → opens
      `<UploadDialog>`. Form: file picker (multi-file accepted),
      Storage Class select (STANDARD | STANDARD_IA | GLACIER |
      DEEP_ARCHIVE), Encryption (AES256 | aws:kms with key-id field),
      Tags input
- [ ] Upload runs in a Web Worker (`web/src/workers/upload.ts`) so
      the UI thread stays responsive. Progress bar per file (bytes
      uploaded / total). "Abort" cancels the in-flight uploads
- [ ] On Abort, calls `DELETE /admin/v1/buckets/{bucket}/uploads/{uploadID}`
      (passes through to existing `AbortMultipartUpload`)
- [ ] Audit rows: `admin:UploadObject` (one per completed object) +
      `admin:AbortMultipartUpload` on aborts
- [ ] Typecheck passes
- [ ] Tests pass (worker test via Playwright + handler unit test)
- [ ] Verify in browser using dev-browser skill

### US-015: Object delete + tag editor + retention/legal-hold panel
**Description:** As an operator, I want to delete an object, edit its
tags, and set retention / legal-hold from the object detail panel.

**Acceptance Criteria:**
- [ ] `DELETE /admin/v1/buckets/{bucket}/objects/{key}?versionId=<id>`
      proxies to existing `s3api.deleteObject` handler
- [ ] `PUT /admin/v1/buckets/{bucket}/objects/{key}/tags` accepts
      `{tags: {string: string}}` → calls `meta.Store.SetObjectTags`
- [ ] `PUT /admin/v1/buckets/{bucket}/objects/{key}/retention` accepts
      `{mode, retain_until}` → calls existing
      `meta.Store.SetObjectRetention`
- [ ] `PUT /admin/v1/buckets/{bucket}/objects/{key}/legal-hold` accepts
      `{enabled: bool}` → calls existing
      `meta.Store.SetObjectLegalHold`
- [ ] Bucket detail object-list row click opens a side panel with
      tabs: Overview | Tags | Retention | Legal Hold | Versions
- [ ] Tags tab: editable key/value list. Save → PUT, refetch
- [ ] Retention tab: Mode (None | Governance | Compliance), Retain
      Until (date picker). When Mode=None, "Save" calls a DELETE
      endpoint that clears the retention row. When the bucket has
      Object-Lock disabled the tab is greyed-out + tooltip
- [ ] Legal Hold tab: toggle. Save → PUT
- [ ] Versions tab: list versions; per row a Delete button
      (versionId-scoped DELETE). DeleteMarker rows are highlighted
- [ ] Audit rows: `admin:DeleteObject`, `admin:SetObjectTags`,
      `admin:SetObjectRetention`, `admin:SetObjectLegalHold`
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-016: Multipart watchdog page
**Description:** As an operator, I want a page that lists in-flight
multipart uploads cluster-wide so I can abort stalled ones.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/multipart/active` paginated — fans out across
      buckets via `meta.Store.ListBuckets` + per-bucket
      `ListMultipartUploads` (existing). Returns rows: Bucket | Key |
      UploadID | InitiatedAt | Age (computed) | StorageClass |
      Initiator (from the underlying access key) | BytesUploaded
      (sum of `meta.MultipartPart.Size` per upload)
- [ ] `POST /admin/v1/multipart/abort` accepts
      `[{bucket, upload_id}]` (batch) — for each, calls existing
      `s3api.abortMultipartUpload` (which queues part chunks for GC).
      Returns per-row results
- [ ] New `web/src/pages/Multipart.tsx`. Filters: bucket
      (autocomplete), min age (≥ N hours, default 24), initiator
      (access-key chip)
- [ ] Bulk-select via row checkboxes; "Abort selected" button issues
      the batch abort request
- [ ] Polls every 30 s (TanStack Query refetch interval)
- [ ] Audit row: `admin:AbortMultipartUpload` (one per aborted upload)
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-017: Audit-log viewer page
**Description:** As an operator, I want to read the audit log with
filters and pagination.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/audit?since=<RFC3339>&until=<RFC3339>&action=<str>&principal=<str>&bucket=<str>&page_token=<base64>`
      wraps existing `meta.Store.ListAudit`. Returns rows + next
      page-token (opaque base64 of the last clustering key)
- [ ] New `web/src/pages/AuditLog.tsx`. Filter panel: date range
      (default last 24 h), Action (multi-select), Principal
      (autocomplete from access-keys), Bucket (autocomplete from
      ListBuckets)
- [ ] Table columns: Time | RequestID | Principal | Action | Resource
      | Result | SourceIP | UserAgent. RequestID links to the OTel
      trace browser (Phase 3 wire-up — meanwhile renders a tooltip
      "Available in Phase 3")
- [ ] "Load more" button uses the page-token. No infinite-scroll;
      explicit pagination is faster on big audit datasets
- [ ] "Export CSV" downloads the current filter view (paginates
      server-side, streams CSV via a new
      `GET /admin/v1/audit.csv?...` endpoint capped at 100k rows)
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-018: Settings page (cluster + console config)
**Description:** As an operator, I want a Settings page with cluster
identity, console config, and operator-only knobs.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/settings` returns
      `{cluster_name, region, version, prometheus_url,
      heartbeat_interval, jwt_ephemeral, console_theme_default,
      audit_retention}`. All read-only (env vars). Secrets masked
      (`STRATA_CONSOLE_JWT_SECRET` returns `"<set>"`/`"<ephemeral>"`)
- [ ] `POST /admin/v1/settings/jwt/rotate` mints a new HS256 secret,
      rotates the in-memory key in `adminapi.Server.JWTSecret`, and
      writes the new value to a sealed file at `STRATA_JWT_SECRET_FILE`
      (default `/etc/strata/jwt-secret`). Existing sessions invalidate
      immediately. **Operator MUST be re-authenticated after rotation
      — handler returns 401 on the next request to force re-login**
- [ ] New `web/src/pages/Settings.tsx`. Tabs: Cluster | Console |
      Backends. Each shows fields read-only; the only action is
      "Rotate JWT secret" with type-to-confirm
- [ ] Backend tab — surfaces meta backend (`STRATA_META_BACKEND`),
      data backend (`STRATA_DATA_BACKEND`), Cassandra hosts /
      keyspace / DC / replication, RADOS conf / pool / classes /
      clusters, TiKV PD endpoints. All read-only
- [ ] Audit row: `admin:RotateJWTSecret`
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-019: BackendPresign toggle (s3-over-s3 backends only)
**Description:** As an operator running on the s3-over-s3 backend, I
want to flip the per-bucket BackendPresign flag from the bucket detail
page so authenticated presigned GETs redirect to backend-minted URLs.

**Acceptance Criteria:**
- [ ] `PUT /admin/v1/buckets/{bucket}/backend-presign` accepts
      `{enabled: bool}` and calls
      `meta.Store.SetBucketBackendPresign` (already on main from the
      s3-over-s3 merge)
- [ ] Bucket detail "Overview" tab adds a card "Backend Presigned URL
      Passthrough" with a toggle. Greyed-out + tooltip "Available
      only on s3-over-s3 backends" when
      `cluster.data_backend != "s3"` (read from
      `/admin/v1/cluster/status`)
- [ ] Toggle save → PUT. Confirmation toast on success
- [ ] Audit row: `admin:SetBucketBackendPresign`
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-020: S3-backend config exposure on Settings page
**Description:** As an operator on the s3-over-s3 backend, I want the
Settings page Backends tab to expose `STRATA_S3_BACKEND_*` config so I
can verify what is wired without shelling onto the host.

**Acceptance Criteria:**
- [ ] `GET /admin/v1/settings/data-backend` returns
      `{kind, endpoint, region, bucket, force_path_style, part_size,
      upload_concurrency, max_retries, op_timeout_secs, sse_mode,
      sse_kms_key_id, access_key_set: bool}`. The `access_key_set`
      bool is true when `STRATA_S3_BACKEND_ACCESS_KEY` is non-empty;
      the actual key never returned. SecretKey same treatment
- [ ] Settings → Backends tab adds a "S3 Backend" subsection visible
      only when `data_backend == "s3"` (collapsed otherwise). Renders
      the response as a key/value list
- [ ] Typecheck passes
- [ ] Tests pass
- [ ] Verify in browser using dev-browser skill

### US-021: Playwright e2e for admin flows
**Description:** As a maintainer, I want a Playwright e2e suite that
covers Phase 2 critical paths so regressions surface in CI.

**Acceptance Criteria:**
- [ ] New `web/e2e/admin.spec.ts` with these flows:
  - `bucket-lifecycle`: login → CreateBucket → upload 5 MB file →
    DeleteObject → DeleteBucket
  - `iam-keys`: CreateUser → CreateAccessKey → DisableKey →
    DeleteKey → DeleteUser
  - `lifecycle-rule`: CreateBucket → add a 30-day expiration rule
    → save → reload → assert rule persists
  - `policy-editor`: open bucket policy → paste a public-read
    template → validate → save → reload → assert
  - `multipart-watchdog`: initiate multipart upload via JS fetch →
    visit Multipart page → assert row appears → bulk-abort → assert
    gone
- [ ] CI workflow `.github/workflows/ci.yml` `e2e-ui` job adds
      `pnpm exec playwright test admin.spec.ts` after the existing
      `critical-path.spec.ts` invocation
- [ ] Suite runs against `make run-memory` (no Cassandra / RADOS
      required — IAM + audit + lifecycle metadata all in memory.Store)
- [ ] Typecheck passes
- [ ] Tests pass

### US-022: docs/ui.md Phase 2 update + ROADMAP close-flip
**Description:** As a developer, I want `docs/ui.md` updated with
Phase 2 capability matrix and the ROADMAP P3 web-ui-admin entry
flipped to Done.

**Acceptance Criteria:**
- [ ] `docs/ui.md` "Capability Matrix" table gains a Phase 2 column
      with row entries: CreateBucket | DeleteBucket | Lifecycle |
      CORS | Policy | ACL | Inventory | Logging | IAM Users |
      AccessKeys | ManagedPolicies | UploadObject | DeleteObject |
      ObjectTags | ObjectRetention | LegalHold | MultipartWatchdog |
      AuditLog | Settings | BackendPresign
- [ ] ROADMAP "Web UI — Phase 2 (admin)" P3 entry flips to:
      `~~**P3 — Web UI — Phase 2 (admin).**~~ — **Done.** <one-line
      summary>. (commit `<sha>`)`
- [ ] Add a new ROADMAP entry under Web UI: P3 hardening items the
      cycle exposed (e.g. "ManagedPolicies + AttachUserPolicy
      contract not yet exercised by `s3-tests`; gap tracked")
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- FR-1: All Phase 2 write endpoints sit under `/admin/v1/*` and reuse
  the existing JWT session-cookie + SigV4 auth chain from Phase 1
- FR-2: Every write endpoint emits one `admin:<Action>` audit row via
  the existing `s3api.AuditMiddleware`
- FR-3: Monaco editor for Bucket Policy / Lifecycle JSON / CORS JSON
  is lazy-loaded so non-editor pages stay ≤500 KiB gzipped
- FR-4: Object upload wizard signs each multipart part via per-part
  presigned URLs minted server-side; the operator's personal AK/SK
  never reaches the browser. Single-PUT for files ≤5 MiB
- FR-5: ManagedPolicy + UserPolicy attachment storage shapes are NEW
  (US-012, US-013) and ship in lockstep across cassandra + memory +
  tikv with storetest contract entries
- FR-6: Settings → Backends tab exposes data + meta backend config
  read-only; secrets are surfaced as `<set>` / `<ephemeral>` only
- FR-7: BackendPresign toggle (US-019) is greyed-out when
  `data_backend != "s3"`; the toggle never appears as a no-op affordance
- FR-8: All Phase 2 endpoints invalidate caches the Phase 1 console
  reads from (TanStack Query `queryClient.invalidateQueries`) so
  follow-up reads on the same page reflect the write immediately
- FR-9: DeleteBucket force-empty is a leader-elected job under the
  `bucket-force-empty:<bucket>` lock — concurrent runs are rejected
  with 409
- FR-10: JWT secret rotation (US-018) invalidates every active
  session; the next request after rotation gets 401

## Non-Goals

- No multi-tenant isolation — admin actions are gated by IAM
  permissions only; "is this user allowed to admin bucket X?" is the
  IAM policy engine's job, not a separate console RBAC layer
- No bulk operations across buckets (e.g. apply policy to N buckets);
  one bucket at a time
- No schedule-based admin actions (cron-like UI); operators script
  these via aws-cli + system cron
- No web terminal (`kubectl exec` shape); SSH is the operator's tool
- No metrics editor (Grafana is the right tool); the cluster metrics
  dashboard from Phase 1 is point-in-time inspection only
- No backend-data migration UI (rados ↔ s3 ↔ memory swaps); operators
  edit env vars and roll the gateway

## Design Considerations

- **Bundle budget**: home / metrics / buckets-list bundles MUST stay
  ≤500 KiB gzipped — verified in CI via `pnpm run build --report`.
  Monaco lives behind `React.lazy` on the editor pages only
- **Component reuse**: extend the Phase 1 shadcn/ui components rather
  than introduce new libraries. Toast, Dialog, Select, Tabs, Table,
  Form already shipped
- **Form state**: `react-hook-form` (already in package.json) for
  every form. Validation via `zod` schemas mirrored from Go
  validators where practical
- **Error UX**: every write path renders `errorResponse.code +
  message` from the gateway response inline, never as a generic
  "Something went wrong"
- **Optimistic UI**: NO. Phase 2 writes are slow enough (LWT round-
  trips) that fake-success UX leads to confusing rollbacks. Wait for
  the response, refetch, then update UI. TanStack Query's default
  behaviour
- **Mobile**: not a goal. Phase 2 is desktop-only — operators work on
  laptops / desktops. Tablet + phone breakpoints are best-effort

## Technical Considerations

- **`internal/adminapi/server.go`** gains 30+ new handler funcs;
  group by domain in separate files: `buckets_admin.go`, `iam.go`,
  `objects.go`, `multipart.go`, `audit.go`, `settings.go`. Existing
  read-only handlers (`buckets.go`, `cluster.go`, `consumers.go`,
  `metrics.go`) stay
- **Storage gaps closed**: ManagedPolicy + UserPolicy attachment
  shapes ship in cassandra + memory + tikv per the storetest contract
- **Web Worker for upload (US-014)**: bundles via Vite's worker
  plugin (`new Worker(new URL("./upload.ts", import.meta.url))`) —
  no separate build step
- **Force-empty job (US-002)**: lives in `internal/adminapi/jobs/`
  as a per-process registry; persists progress in a new
  `admin_jobs` Cassandra table so a restart resumes
- **JWT rotation (US-018)**: writes the new secret to
  `STRATA_JWT_SECRET_FILE` so `loadJWTSecret` in
  `internal/serverapp/serverapp.go` reads it on next start
- **Audit row volume**: every admin write emits one row. At 100 ops/s
  the existing `audit_log` TTL handles it (default 30 days). If a
  cluster sustains higher rates a future story bumps the retention

## Success Metrics

- Phase 2 ships every "common operator task" under 3 clicks from the
  console home (verified manually against a checklist of 10 common
  flows)
- `bin/strata` binary size grows by <2 MiB after Phase 2 — Monaco
  ships only as a lazy chunk under `web/dist/assets/`, the embed
  pulls only the gzipped HTML/JS asset bundle
- All 22 stories close with audit rows visible in the new Audit-log
  viewer page during the cycle's smoke pass
- Playwright `admin.spec.ts` runs in <90 s on CI (chromium-only);
  no flaky test exemptions

## Open Questions

- Managed-policy schema — full IAM JSON (resource ARNs, conditions)
  vs. simplified bucket-scoped subset? Decision: full IAM JSON is the
  AWS-compat choice; the schema validator in
  `web/src/schemas/iam-policy.json` mirrors the AWS `iam:Policy` JSON
  schema. Decided.
- Force-empty rate-limit — should DeleteBucket force-empty cap at N
  delete/s to avoid swamping the gateway? Decision at story-start in
  US-002; default proposal: paginate at 100 keys per `ListObjects`,
  delete sequentially, no rate-limit token bucket
- Audit-log CSV export size cap — 100k rows is plenty for daily
  ad-hoc exports but a year-long export needs a BigQuery export
  shape. Out of scope for Phase 2; Phase 3 may add it
