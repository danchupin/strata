# PRD: Web UI — Admin (Phase 2 of 3)

> **Status:** Outline. Detailed AC will be authored when
> `prd-web-ui-foundation.md` ships and the embedded console is live.
> This file holds the scope sketch so the foundation PRD's Non-Goals
> have a concrete forward pointer.
>
> **Backend audit (2026-05-02 — main = modern-complete + binary-
> consolidation + tikv-meta-backend merged):** every admin surface
> Phase 2 wraps already exists on main. Bucket lifecycle / CORS /
> policy / ACL / inventory / access-log endpoints shipped in
> `modern-complete`. IAM users + access keys + STS shipped in
> `modern-complete` (US-036 family). Object Lock retention + legal-
> hold shipped. Audit log + retention sweeper shipped. Multipart
> upload tracking shipped. Phase 2 wraps these in UI; no backend
> changes required.

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
