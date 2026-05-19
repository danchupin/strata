---
title: 'Admin API viewer'
weight: 40
bookToC: false
bookHidden: false
bookCollapseSection: false
description: 'Interactive Redoc viewer for the Admin API OpenAPI contract.'
---

# Admin API viewer

Interactive [Redoc](https://github.com/Redocly/redoc) render of the canonical
Admin-API OpenAPI contract (`internal/adminapi/openapi.yaml`). The YAML is
copied into the Hugo static dir at build time by `make docs-openapi-copy`
(a prerequisite of `make docs-build` / `make docs-serve`), so the viewer
always reflects the contract at the same SHA as the rest of the docs site.

For a flat index of every admin endpoint with method, path, audit verb,
and a one-line summary, see [Admin API surface]({{< ref "/reference/admin-api" >}}).
For the raw contract, fetch [/openapi.yaml](/openapi.yaml).

<noscript>

**JavaScript required for the interactive viewer.** Static contract: [/openapi.yaml](/openapi.yaml). Operator index: [/reference/admin-api/]({{< ref "/reference/admin-api" >}}).

</noscript>

<redoc spec-url='/openapi.yaml'></redoc>
<script src="https://cdn.jsdelivr.net/npm/redoc@2.5.0/bundles/redoc.standalone.js"></script>
