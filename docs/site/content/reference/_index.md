---
title: 'Reference'
weight: 60
bookFlatSection: true
description: 'Reference tables — env vars, Admin API, S3 surface.'
---

# Reference

Operator reference. Four pages:

- [STRATA_* environment variables]({{< ref "/reference/env-vars" >}}) — every `STRATA_*` env var with default, range, and consuming layer, grouped by operator concern.
- [Admin API surface]({{< ref "/reference/admin-api" >}}) — flat index of every admin endpoint (method, path, audit verb, summary) with cross-links into the interactive viewer.
- [S3 API operations]({{< ref "/reference/s3-api" >}}) — flat table of supported S3 operations with handler `file:line` pointers and AWS-divergence notes.
- `admin-api-viewer/` — Redoc-rendered `internal/adminapi/openapi.yaml` for interactive exploration. _(authored in a follow-on cycle.)_
