# PRD: Web UI — Admin (Phase 2 of 3)

> **Status:** Outline. Detailed AC will be authored at story-start.
> Phase 1 (`prd-web-ui-foundation.md`) shipped on 2026-05-03 via commit
> `e27cf21` — embedded console at `/console/`, `/admin/v1/*` JSON API
> with read-only handlers, `internal/adminapi` + `internal/heartbeat`
> packages live on `main`. Phase 2 builds on this foundation.
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
>   Multipart upload tracking. Phase 2 wraps these in UI; no backend
>   changes required.
> - **NEW (s3-over-s3 merge):** `meta.Bucket.BackendPresign` per-bucket
>   toggle and `meta.Store.SetBucketBackendPresign` data-side
>   implementation already exist (US-016 of `prd-s3-over-s3-backend`).
>   Phase 2 needs an admin endpoint + UI toggle on bucket detail
>   to flip the flag — see new US-021 below.
> - **NEW (s3-over-s3 merge):** `STRATA_S3_BACKEND_*` env vars (endpoint,
>   region, bucket, force-path-style, part-size, retries, SSE mode +
>   KMS key). Settings page should expose these read-only.
> - **Phase 1 surface available to Phase 2:** versioned `/admin/v1/*`
>   API with HS256 JWT session cookies (24 h, HttpOnly, SameSite=Strict,
>   Path=/admin) + SigV4 fallback. Add Phase 2 write endpoints under
>   `/admin/v1/`.

## Introduction

Phase 1 (`prd-web-ui-foundation.md`) ships the read-only console. Phase
2 adds **write actions for operators**: bucket lifecycle, IAM users +
access keys, bucket policy / ACL editor, multipart-upload watchdog,
inventory + access-log configuration. After Phase 2 an operator can
do every aws-cli admin task from the console.

Phase 3 (`prd-web-ui-debug.md`) is unchanged — performance debug
tooling lives there.

## Goals (sketch)

- Bucket admin: Create, Delete, edit Versioning, edit Object Lock
  default, edit Lifecycle (visual rule editor), edit CORS, edit
  Bucket Policy (JSON editor with schema validation), edit ACL,
  edit Inventory configurations, edit Access-log target
- IAM admin: Create / Delete users, Create / Rotate / Delete access
  keys, Attach / Detach managed policies, edit per-user ACL
  inheritance
- Object actions: Upload (single + multipart wizard), Delete,
  Set object tags, Set retention / legal-hold (when bucket has
  Object-Lock)
- Multipart watchdog: list pending in-flight multipart uploads
  cluster-wide, abort selected uploads, see age + bytes-uploaded
- Audit-log viewer with filters (date range, action, principal,
  bucket) — read-only but heavy enough to deserve its own page
- Settings page: cluster name, JWT secret rotation, Prometheus URL,
  heartbeat interval, theme defaults

## User Stories (titles only; AC TBD)

- US-001: CreateBucket dialog (name validator, region selector,
  versioning + Object-Lock toggles, owner picker)
- US-002: DeleteBucket flow (confirmation modal, asserts bucket
  empty or offers force-delete with type-to-confirm)
- US-003: Versioning + Object-Lock toggles on bucket detail
- US-004: Lifecycle rule editor (visual rule builder + JSON view tab)
- US-005: CORS rule editor (visual rule builder)
- US-006: Bucket Policy JSON editor (Monaco editor + schema
  validation against IAM policy spec)
- US-007: Bucket ACL editor (canned + grant list)
- US-008: Inventory configurations CRUD
- US-009: Access-log target editor
- US-010: IAM Users CRUD page (list, create, delete)
- US-011: Access keys CRUD per user (list, create, delete, rotate)
- US-012: Managed policies CRUD page
- US-013: Attach / detach policies UI
- US-014: Object upload wizard (single PUT + multipart fallback for
  >5 MiB; progress bar; abort)
- US-015: Object delete + tag editor + retention/legal-hold panel
- US-016: Multipart-uploads watchdog page (list + abort)
- US-017: Audit-log viewer page (filters + pagination)
- US-018: Settings page (cluster + console config)
- US-019: Playwright e2e for admin flows
- US-020: docs/ui.md Phase 2 capability matrix update + ROADMAP
  close-flip
- US-021: BackendPresign toggle (s3-over-s3 backends only).
  PUT `/admin/v1/buckets/{bucket}/backend-presign` flips
  `meta.Bucket.BackendPresign` via the existing
  `meta.Store.SetBucketBackendPresign` helper. UI toggle on bucket
  detail; greyed-out + tooltip when `STRATA_DATA_BACKEND != s3`
- US-022: Settings page exposes S3-backend config read-only —
  `STRATA_S3_BACKEND_ENDPOINT / REGION / BUCKET / FORCE_PATH_STYLE /
  PART_SIZE / UPLOAD_CONCURRENCY / MAX_RETRIES / OP_TIMEOUT_SECS /
  SSE_MODE / SSE_KMS_KEY_ID`. Reads from `cfg.S3Backend` via a new
  `GET /admin/v1/cluster/data-backend` endpoint that returns the
  effective config (secrets masked)

## Non-Goals

- No multi-tenant isolation — admin actions are gated by IAM
  permissions only
- No bulk operations across buckets (e.g. apply policy to N buckets);
  one bucket at a time
- No schedule-based admin actions (cron-like UI); operators script
  these via aws-cli

## Open Questions

- Object upload wizard: does the browser sign each multipart UploadPart
  with SigV4-streaming, or do we proxy uploads through the gateway with
  a session-cookie shortcut? Decide at story-start
- Bucket Policy JSON editor: vendor Monaco (~2 MiB additional bundle)
  or use a lighter `@uiw/react-textarea-code-editor`? Decide at
  story-start based on Phase-1 bundle budget headroom
