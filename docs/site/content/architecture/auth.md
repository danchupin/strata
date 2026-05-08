---
title: 'Auth'
weight: 5
description: 'SigV4 verification, presigned URLs, streaming chunk decoder, virtual-hosted-style routing, identity attribution via context.'
---

# Auth

The auth layer lives under `internal/auth/`. Every request enters through
`auth.Middleware`, which verifies an AWS SigV4 signature, derives a stable
identity, and stamps the result onto the request context. The router and
handlers downstream never re-derive identity — they read it from
`auth.FromContext(ctx).Owner`.

## SigV4 verification

`internal/auth/sigv4.go` implements the standard four-step canonicalisation:

1. Parse `Authorization` (or `X-Amz-*` query parameters for presigned URLs)
   to extract `AccessKey`, `Scope`, `SignedHeaders`, and `Signature`.
2. Look up the secret for the access key via the configured static
   credentials store (`internal/auth/static.go`) — no IdP federation in
   this cycle.
3. Build the canonical request string from `Method`, `URL.Path`, the
   sorted `SignedHeaders`, and the body hash (`x-amz-content-sha256`,
   which may be the literal sentinel `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`
   for chunked uploads — see below).
4. Recompute the signature with the derived signing key and constant-time
   compare. Mismatch returns `ErrSignatureInvalid` (HTTP 403, AWS code
   `SignatureDoesNotMatch`).

The middleware MUST run before any URL rewriting. The signed canonical
string includes the original `Host` header and the original
`URL.Path` — if the router rewrites either before verification, the
signature breaks. See the [Router page]({{< ref "/architecture/router" >}})
for the order.

`STRATA_AUTH_MODE` controls the gate:

| Mode | Behaviour |
|---|---|
| `""` (default) | Auth off. Every request is allowed. Used by `make run-memory` for local development. |
| `optional` | Verify if a signature is present; allow unsigned requests through with the anonymous principal. |
| `required` | Reject unsigned requests with `AccessDenied`. |

## Presigned URLs

`internal/auth/presigned.go` parses the `X-Amz-Algorithm`,
`X-Amz-Credential`, `X-Amz-Date`, `X-Amz-Expires`, `X-Amz-SignedHeaders`,
and `X-Amz-Signature` query parameters and re-runs the same canonicalisation
without an `Authorization` header. The expiry check is wall-clock and
strict — the caller is expected to refresh the URL before
`Date + Expires`.

Strata mints presigned URLs through the `presign_mint.go` helper for
internal flows (admin console downloads, replication HTTPDispatcher
fallbacks). The mint side does not differ from the verify side — the same
canonical string and signing key derivation are reused.

## Streaming chunk decoder

Multipart and large PUTs use the `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`
encoding: the body is split into chunks and each chunk carries a per-chunk
signature in the form
`<chunk-size-hex>;chunk-signature=<hex>\r\n<bytes>\r\n`. The decoder lives
in `internal/auth/streaming.go`. The contract:

- The first chunk's signature uses the `Authorization`-header signature
  as `prevSig`.
- Each subsequent chunk's signature is `HMAC(signingKey,
  string-to-sign)` where the string-to-sign explicitly includes the
  previous chunk's signature (`prevSig`). This is the chain-HMAC the
  spec calls out.
- A mismatch on any chunk returns `ErrSignatureInvalid` and the gateway
  drops the connection mid-body. Already-processed bytes are NOT
  committed to the data backend — the upload aborts.

This per-chunk validation was added under US-022. Without it, an
attacker who could intercept chunk boundaries could splice a body. The
streaming decoder is the only path that touches `prevSig`; non-chunked
PUTs verify the whole body's `x-amz-content-sha256` against the
canonical-request hash.

## Identity in context

After verification the middleware calls `auth.WithIdentity(ctx, id)`. The
identity carries:

- `Owner` — canonical user (used as the bucket / object owner on writes).
- `AccessKey` — the SigV4 access key id (audited on every write).
- `Anonymous bool` — set when the request was unsigned and the auth mode
  is `""` or `optional`.

Handlers reach the identity via `auth.FromContext(r.Context())`. The
audit log middleware (`internal/s3api/audit.go`) reads the same value to
fill the `principal` column.

## Virtual-hosted-style routing

`internal/s3api/vhost.go` extracts the bucket from the host header for
clients that prefer `<bucket>.s3.local/key` over the path-style
`s3.local/<bucket>/key`. The pattern list comes from `STRATA_VHOST_PATTERN`
(comma-separated `*.<suffix>`; default `*.s3.local`; `-` disables).

The crucial ordering invariant is: **the auth middleware runs first and
signs the original `Host` + `URL.Path`**. `Server.ServeHTTP` then rewrites
`r.URL.Path` to prepend `/<bucket>` so the path-style router shape works
unchanged. Rewriting before SigV4 verification breaks every signed
request — the canonical string would no longer match what the client
signed.

## Source

- `internal/auth/sigv4.go` — header path.
- `internal/auth/presigned.go` — query path.
- `internal/auth/streaming.go` — chunked body decoder.
- `internal/auth/middleware.go` — context plumbing + mode gate.
- `internal/auth/static.go` — `STRATA_STATIC_CREDENTIALS` parser.
- `internal/s3api/vhost.go` + `internal/s3api/server.go::ServeHTTP` —
  vhost rewrite (after auth).
