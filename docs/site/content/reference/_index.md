---
title: 'Reference'
weight: 60
bookFlatSection: true
description: 'Operator reference — env vars, Admin API surface, S3 API surface, interactive viewer.'
---

# Reference

Look-up tables for operators wiring Strata into a runbook. Four pages:

{{% columns %}}

- {{< card href="/reference/env-vars/" >}}**`STRATA_*` environment variables** — every knob with default, range, and consuming layer, grouped by operator concern.{{< /card >}}

- {{< card href="/reference/admin-api/" >}}**Admin API surface** — flat index of every `/admin/v1/*` endpoint (method, path, audit verb, summary) with deep-links into the interactive viewer.{{< /card >}}

- {{< card href="/reference/s3-api/" >}}**S3 API operations** — table of supported S3 operations with handler pointers and AWS-divergence notes.{{< /card >}}

{{% /columns %}}

{{% columns %}}

- {{< card href="/reference/admin-api-viewer/" >}}**Admin API viewer** — Redoc-rendered OpenAPI document for interactive exploration.{{< /card >}}

{{% /columns %}}

See also: [Best practices]({{< ref "/best-practices" >}}) for tuning runbooks
that consume these knobs, and [Architecture]({{< ref "/architecture" >}}) for
the deep-dive on the underlying components.
