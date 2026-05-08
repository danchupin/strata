---
title: 'Router'
weight: 10
description: 'internal/s3api/server.go — bucket-vs-object scoped query-string router pattern, vhost rewriting after auth, admin path carve-out.'
---

# Router

The router lives in `internal/s3api/server.go`. It is a single
`Server.ServeHTTP` method that classifies every request along three axes:

1. **Special prefix** — `/admin/...` is the embedded operator console JSON
   API and bypasses the S3 dispatch entirely.
2. **Bucket vs object scope** — the URL path is split at the first slash
   into `(bucket, key)`. Empty bucket → service-level (ListBuckets, IAM
   actions). Empty key → bucket-scoped. Both populated → object-scoped.
3. **Query-string sub-resource** — within bucket and object scope, the
   sub-operation is dispatched by query parameter (`?cors`, `?policy`,
   `?lifecycle`, `?uploads`, `?uploadId=`, `?tagging`, …) plus the HTTP
   method. This is the AWS S3 wire shape; we mirror it.

## Dispatch order

```
ServeHTTP(w, r):
  1. extractAccessPointAlias(r.Host)        # alias.<host> -> rewrite to /<bucket>/...
  2. extractVHostBucket(r.Host, ...)        # *.s3.local   -> rewrite to /<bucket>/...
  3. if path starts with /admin/            -> handleAdmin
  4. splitPath(r.URL.Path) -> (bucket, key)
  5. bucket == ""    -> IAM action / ListBuckets / DLQ-audit listings
     key == ""       -> handleBucket(...)
     default         -> handleObject(...)
```

Auth middleware runs **before** the access-point and vhost rewrites because
those rewrites only mutate `r.URL.Path` after SigV4 has already validated
the original. See [Auth]({{< ref "/architecture/auth" >}}) for the ordering
rationale.

## Bucket scope (`handleBucket`)

`handleBucket` checks each query-string sub-resource in turn and dispatches
to a per-feature handler. The shape is repetitive on purpose — every S3
sub-resource gets one block:

```go
if q.Has("cors") {
    switch r.Method {
    case http.MethodGet:    s.getBucketCORS(...)
    case http.MethodPut:    s.putBucketCORS(...)
    case http.MethodDelete: s.deleteBucketCORS(...)
    }
}
```

Adding a new bucket sub-resource means appending a block in this style.
Don't introduce a sub-router — flat dispatch is simpler to read and
diff-review.

Sub-resources currently routed: `cors`, `policy`, `publicAccessBlock`,
`ownershipControls`, `lifecycle`, `versioning`, `tagging`, `acl`,
`logging`, `notification`, `replication`, `encryption`, `inventory`,
`accelerate`, `requestPayment`, `website`, `object-lock`, `analytics`,
`metrics`, `intelligent-tiering`. Several are stubbed (return defaults
or `NotImplemented`); the [S3 compatibility page]({{< ref "/s3-compatibility" >}})
spells out the matrix.

## Object scope (`handleObject`)

Same pattern, scoped to a single key. Sub-resources: `uploads` (initiate /
list multipart), `uploadId=<id>` + `partNumber=<n>` (UploadPart),
`uploadId=<id>` (CompleteMultipart, AbortMultipart), `tagging`, `retention`,
`legal-hold`, `acl`, `restore`, `select`. Plain `GET` / `HEAD` / `PUT` /
`DELETE` without any sub-resource fall through to the canonical object
operations.

## Admin carve-out

`/admin/v1/*` is the embedded console's JSON API. It does not follow S3
wire shape — it's a flat `[Verb][Resource]` REST surface. The router
carve-out at the top of `ServeHTTP` keeps it cleanly separated. Audit-log
middleware still wraps it, but with override semantics: admin handlers
call `s3api.SetAuditOverride(ctx, action, resource, bucket, principal)`
to stamp operator-meaningful audit rows (`admin:CreateBucket`,
`bucket:foo`) instead of the path-derived shape. See [Observability]({{< ref "/architecture/observability" >}}).

## Why query-string dispatch (and not gorilla/mux)?

Every S3 sub-resource is keyed by the **presence** of a query parameter,
not a URL path segment. AWS path-style URLs are flat: the same
`/<bucket>/<key>` path serves a dozen distinct operations depending on
which `?<sub-resource>` parameter is set. Wiring a tree-router on top of
a flat URL space adds indirection without saving lines. Reading
`server.go` linearly tells you exactly which combinations are handled.

## Source

- `internal/s3api/server.go` — `ServeHTTP`, `handleBucket`, `handleObject`.
- `internal/s3api/vhost.go` — `extractVHostBucket`.
- `internal/s3api/access_point_routing.go` — access-point alias extraction.
- `internal/s3api/admin.go` — `/admin/v1/*` handler tree.
