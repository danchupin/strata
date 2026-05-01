# Streaming SigV4 chunk-signature validation

**Date:** 2026-05-01
**Severity:** Medium (signed-request body integrity)
**Affected releases:** all Strata releases prior to the
`ralph/auth-per-chunk-signature` cycle close.
**Status:** Fixed.

## Gap

Strata implements AWS SigV4 for headers + query string. For uploads sent
with `Content-Encoding: aws-chunked` and
`x-amz-content-sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, the body is
framed into chunks; each chunk carries a `;chunk-signature=<hex>` value
that is the next link in an HMAC-SHA256 chain seeded by the outer SigV4
signature. The chain binds every byte of the body to the request.

`internal/auth/streaming.go` decoded the framing (size header,
chunk-signature, payload, CRLF) and forwarded the payload to the
storage backend, but did **not** recompute the chain or compare it
against the client-supplied `chunk-signature`. The outer SigV4
signature in this mode covers only the constant string
`STREAMING-AWS4-HMAC-SHA256-PAYLOAD` in place of the body hash —
nothing about the actual body bytes ever reached the validator.

### Impact

An attacker positioned between a signed AWS SDK client and the gateway
(TLS-terminating proxy, hostile sidecar, on-path adversary against an
`is_secure=False` deployment) could mutate body bytes mid-stream without
invalidating the outer SigV4 signature. The mutated bytes would be
written to the data backend (RADOS or S3) and served back on subsequent
GETs. Confidentiality of the request is preserved by TLS (when used);
**integrity of the body** was not enforced by the gateway.

### What did *not* mitigate the gap

- Outer SigV4 — covers headers + query + the `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`
  constant; never the body.
- ETag / per-chunk SHA-256 in the metadata layer — computed *after*
  bytes were received; doesn't detect mutation in flight.
- `Content-Length` — present in the headers and signed, but only bounds
  the framed body, not its contents.

## Fix

`internal/auth/streaming.go` now implements the AWS-spec chain:

```
sig(chunk_n) = HMAC-SHA256(
  signing_key,
  "AWS4-HMAC-SHA256-PAYLOAD\n"
  + <iso8601-date>           + "\n"
  + <credential-scope>       + "\n"
  + <prev-sig>               + "\n"
  + hex(SHA-256(""))         + "\n"
  + hex(SHA-256(chunk_n_payload))
)
```

The decoder buffers each chunk fully (≤16 MiB cap to bound memory
against malformed framing), computes the expected chain signature,
compares it to the client-supplied value via `hmac.Equal` (constant
time), and only forwards bytes to the storage backend on match. On
mismatch the request fails with `403 SignatureDoesNotMatch` and **the
chunk's bytes never reach the backend** — the buffer-then-validate
ordering is the security guarantee.

Validation is mandatory; there is no opt-out flag. Every AWS SDK
emits a correct chain, so legitimate clients are unaffected.

The newer `aws-chunked-trailer` format (some `aws-cli` 2.x variants
that send `x-amz-trailer: x-amz-checksum-...` plus the
`STREAMING-UNSIGNED-PAYLOAD-TRAILER` sentinel) is detected explicitly
and rejected with `501 NotImplemented` instead of falling into the
chunk decoder with a confusing error. Trailer-format support is a
separate future PRD.

## SDK behaviour — no client change required

Every AWS SDK (`aws-sdk-go-v2`, `boto3`, `aws-cli` 1.x, `aws-sdk-java-2`,
JavaScript v3, etc.) computes the chain correctly. Operators do not
need to update SDK versions, regenerate keys, or change client code.

If a client receives `403 SignatureDoesNotMatch` after the fix:

1. Confirm the SDK is current (any minor release from the last 5 years
   is fine).
2. Confirm clock skew is under 5 minutes (existing SigV4 constraint).
3. If the request goes through a body-rewriting proxy (e.g. a transparent
   gzip layer or a WAF that mutates payloads), that proxy is the
   problem — chunk validation is doing what it should.

## Operator implication

Deployments running pre-fix releases **should upgrade** if they accept
streaming SigV4 uploads from networks that include any party who is
not the original signer (TLS-terminating reverse proxy operators,
multi-tenant control planes, on-path actors against plain HTTP
deployments). Single-tenant deployments with end-to-end TLS between
the SDK and the gateway have a smaller exposure (TLS provides path
integrity), but the defence-in-depth chain validation closes the gap
regardless.

A pre-fix release does **not** corrupt valid uploads; it merely fails
to detect mutation. Existing data on disk is unaffected.

## Verification

- AWS-spec test vectors transcribed verbatim from the AWS S3
  streaming-SigV4 documentation are exercised in
  `internal/auth/streaming_test.go::TestChunkSignerAWSVectors` —
  chunk-1 / chunk-2 / final-empty signatures match published values.
- `internal/auth/streaming_test.go::TestStreamingReader_MutatedChunk_Rejects`
  flips one byte of chunk-2's payload and asserts
  `ErrChunkSignatureMismatch` is returned and the consumer never sees
  any bytes from the mutated chunk.
- `internal/auth/streaming_test.go::TestStreamingReader_MutatedTrailer_Rejects`
  proves the empty-trailer chunk is also chain-validated; an attacker
  cannot truncate the body and forge the final marker.
- `internal/s3api/sigv4_streaming_test.go::TestStreamingSigV4_MutatedChunk_Rejected`
  is the end-to-end test: a signed PUT with one mid-stream byte XOR'd
  returns `403 SignatureDoesNotMatch`, and a subsequent `GetObject`
  returns `404 NoSuchKey` — proving the buffer-then-validate guarantee
  holds against the live HTTP path.

## References

- AWS S3 streaming-SigV4 spec:
  <https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html>
  ("Example Calculations" section is the source of the test vectors.)
- ROADMAP entry (closed): see the **P2 — Per-chunk signature validation
  in streaming payload** bullet in `ROADMAP.md` under `## Auth`.
- Cycle history: `scripts/ralph/progress.txt` 2026-05-01 entries
  US-001..US-005.
