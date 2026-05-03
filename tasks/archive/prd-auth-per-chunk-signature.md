# PRD: Per-chunk Signature Validation in `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`

## Introduction

When an S3 client uploads with `x-amz-content-sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD`,
the body is framed as a sequence of length-prefixed chunks; each chunk carries a
**per-chunk signature** that chains to the previous chunk's signature, with the chain
seeded by the outer SigV4 signature in the request's `Authorization` header.

`internal/auth/streaming.go` today decodes the framing (length, signature, payload, CRLF)
but does NOT verify the chained HMAC. The decoder reads `;chunk-signature=<hex>`, parses
the byte-length prefix, and forwards the payload to the gateway — never recomputing the
expected signature.

**Security implication:** An attacker who intercepts a valid signed request (e.g. a
proxy on the network path, a stolen presigned-URL plus access to the body stream) can
mutate the chunk payloads while leaving the outer SigV4 signature intact. The outer
signature only covers headers + query + a constant string `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`
— never the body bytes. Without per-chunk validation, body integrity is unprotected.

This is a P2 entry under `## Auth` in `ROADMAP.md`. Closing it requires implementing
the AWS-spec'd chain and rejecting mutated chunks with `403 SignatureDoesNotMatch`.

## Goals

- Implement per-chunk HMAC chain validation per AWS streaming-SigV4 spec
- Reject the request with `403 SignatureDoesNotMatch` on the first mismatched chunk
- Preserve the existing decoder's streaming-shape: no full-body buffering;
  validate per-chunk as bytes flow
- Cover the chain with unit tests using the AWS-published test vectors
- Cover with integration test that mutates a chunk mid-stream and asserts 403
- Mandatory enforcement — no opt-out env flag (treating this as a compatibility knob
  creates a security footgun for the lifetime of the flag; every living AWS SDK
  sends a correct chain)
- ROADMAP P2 entry flips to Done close-flip per CLAUDE.md "Roadmap maintenance"

## User Stories

### US-001: Chain-HMAC validator + AWS test-vector unit tests
**Description:** As a developer, I want a small `chunkSigner` type in
`internal/auth/streaming.go` that, given the seed signature + signing key + scope,
produces the expected per-chunk signature, so the decoder can compare bytes-by-byte.

**Acceptance Criteria:**
- [ ] New unexported type `chunkSigner` in `internal/auth/streaming.go` with method
      `Next(payload []byte) string` that returns the hex-encoded chain signature for
      the next chunk and updates internal state to that signature
- [ ] Construction: `newChunkSigner(seedSig string, signingKey []byte, scope string)`
      where `seedSig` is the outer SigV4 signature from the request's `Authorization`
      header, `signingKey` is the SigV4 derived key (same one the outer canonical
      hash used), `scope` is `<date>/<region>/s3/aws4_request`
- [ ] Algorithm:
      `sig(chunk_n) = HMAC-SHA256(signing_key, "AWS4-HMAC-SHA256-PAYLOAD\n" + <iso8601-date> + "\n" + <scope> + "\n" + <prev-sig> + "\n" + hex(SHA256("")) + "\n" + hex(SHA256(chunk_n_payload)))`
- [ ] AWS-spec test vectors covered as table-tests:
      [https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html](https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html)
      — at minimum the "Example Calculations" section's seed + chunk-1 + chunk-2 +
      final-empty pair
- [ ] Final empty-chunk signature also computed correctly (zero-length payload still
      participates in the chain)
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: Wire validator into the streaming decoder + reject on mismatch
**Description:** As an S3 client uploading with streaming SigV4, I want the gateway to
reject my upload if any chunk signature does not match the chain — so an attacker
mutating bytes mid-stream is detected.

**Acceptance Criteria:**
- [ ] Decoder in `internal/auth/streaming.go` calls `chunkSigner.Next(payload)` for
      each chunk and compares with the `;chunk-signature=<hex>` header value before
      forwarding payload bytes to the consumer
- [ ] Mismatch returns a typed error `errChunkSignatureMismatch` from the `Read` loop;
      auth middleware translates it to `403 SignatureDoesNotMatch` with the standard
      AWS XML body
- [ ] Final empty chunk validation: failure on the trailer chunk also returns 403
      (a mutated trailer must not slip through)
- [ ] Streaming-shape preserved: no full-body buffer. Per-chunk bytes flow through
      the underlying `bufio.Reader` as today; the SHA-256 of each chunk is computed
      via streaming hash (`sha256.New().Write(chunk)`)
- [ ] If the outer SigV4 signature was missing/malformed (request rejected before
      the streaming decoder runs), no chain validation occurs — pre-existing 403
      path unchanged
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: Integration test — mutated chunk in flight returns 403
**Description:** As a security-conscious maintainer, I want an end-to-end test that
issues a properly-signed streaming upload, mutates one chunk byte in flight, and
asserts the gateway returns 403 (not 200).

**Acceptance Criteria:**
- [ ] New test `TestStreamingSigV4_MutatedChunk_Rejected` in
      `internal/s3api/sigv4_streaming_test.go` (or wherever streaming smoke lives)
- [ ] Test builds a 3-chunk streaming PUT body manually (chunk-1, chunk-2,
      final-empty) with correct chain signatures
- [ ] Sends two variants through the harness:
      - Variant A: pristine body — asserts `200` and `data.ErrNotFound` →
        `GetObject` round-trips the bytes
      - Variant B: same body but byte at offset N inside chunk-2's payload XOR'd
        with `0x01` (signature unchanged) — asserts `403 SignatureDoesNotMatch`,
        body never written to backend
- [ ] Test runs under existing `go test ./internal/s3api/...` (no integration tag —
      uses the in-memory backend)
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: ROADMAP close-flip + CHANGELOG note
**Description:** As a maintainer, I want the ROADMAP P2 entry flipped to Done with
the closing SHA, and a brief security-note in `docs/security/` flagging the fix
(operators with old Strata releases should know).

**Acceptance Criteria:**
- [ ] Flip the `## Auth` P2 entry to Done close-flip format per CLAUDE.md "Roadmap
      maintenance" rule:
      `~~**P2 — Per-chunk signature validation in streaming payload.**~~ — **Done.**
      `internal/auth/streaming.go` validates the chained per-chunk HMAC per the AWS
      streaming-SigV4 spec; mismatched chunks are rejected with 403 ... (commit `<sha>` or `(commit pending)`)`
- [ ] New `docs/security/2026-streaming-sigv4-chunk-validation.md` (one-pager):
      describes the gap, the fix, the SDK behaviour (every AWS SDK already sends
      correct chains, so no client-side change needed), and the operator
      implication (deployments running pre-fix releases SHOULD upgrade if they
      receive STREAMING uploads from untrusted networks)
- [ ] If a `docs/security/` directory does not exist, create it with this single
      file as the seed
- [ ] Typecheck passes
- [ ] Tests pass

## Functional Requirements

- FR-1: Streaming-SigV4 uploads compute the per-chunk chain signature and compare
  against the client-supplied `chunk-signature` header for every chunk including
  the final empty trailer
- FR-2: Mismatches return `403 SignatureDoesNotMatch` with the standard AWS XML
  error body; payload bytes from a mutated chunk MUST NOT reach the storage
  backend
- FR-3: No opt-out — there is no env var or flag that disables per-chunk validation.
  Every living AWS SDK sends a correct chain; an opt-out exists only as a
  perpetual security footgun
- FR-4: Streaming-shape preserved — no full-body buffering. Per-chunk SHA-256 is
  computed during the existing streaming read loop
- FR-5: ROADMAP.md `## Auth` P2 entry flips to Done in the same commit (or the
  immediate follow-up SHA edit) as US-004 lands

## Non-Goals

- **No SigV2 support.** SigV2 is deliberately unsupported; this PRD is SigV4-only
- **No `aws-chunked-trailer` (newer aws-cli format) support.** That is a separate
  bug under `## Known latent bugs` in ROADMAP.md. This PRD does not touch the
  trailer format; the legacy chained format is the sole scope
- **No presigned-URL chunk validation.** Presigned URLs use `UNSIGNED-PAYLOAD` —
  there is no chunk chain to validate
- **No body-integrity guarantees beyond what the chain provides.** If the
  attacker has access to the signing key, all bets are off. Out of scope
- **No AWS-API-version-specific telemetry.** We do not record which SDK version
  sent the request

## Technical Considerations

### Where the seed signature comes from
The outer SigV4 verifier in `internal/auth/sigv4.go` already computes:
- the canonical-request hash
- the string-to-sign
- the signing key (HMAC chain over date / region / service / aws4_request)
- the final outer signature (the `Signature=...` value in `Authorization`)

The streaming decoder needs both the **outer signature** (chain seed) and the
**signing key** (HMAC key for every chunk). The verifier today returns only an
`auth.AuthInfo` with identity; this PRD's US-002 extends the verifier's return
shape (or context-stashes both values) so the streaming decoder can pick them up.

### Chain construction reference
AWS-spec'd algorithm:
```
seed_sig    = outer Authorization Signature= value (hex)
signing_key = HMAC(HMAC(HMAC(HMAC("AWS4"+sk, date), region), "s3"), "aws4_request")
scope       = <date>/<region>/s3/aws4_request

For chunk_1 .. chunk_N (N+1 = trailer with empty payload):
  string_to_sign = "AWS4-HMAC-SHA256-PAYLOAD\n"
                 + iso8601_date + "\n"
                 + scope + "\n"
                 + prev_sig + "\n"
                 + hex(SHA256("")) + "\n"
                 + hex(SHA256(chunk_payload))
  expected_sig = hex(HMAC-SHA256(signing_key, string_to_sign))
  prev_sig = expected_sig
```

### Performance envelope
Per-chunk HMAC + SHA-256 over the chunk payload adds ~1 GiB/s of overhead per CPU
core on modern x86 — negligible vs network. The existing streaming decoder already
reads chunk bytes into a buffer; we add a streaming SHA-256 over the same buffer
and a single HMAC-SHA-256 at chunk boundary. No extra allocations beyond the hash
state.

### Backward compatibility
- Existing clients (aws-cli 1.x and 2.x, boto3, aws-sdk-go-v1, aws-sdk-go-v2,
  Java SDK, .NET SDK) all compute the chain correctly out of the box. Smoke
  pass + s3-tests should not regress
- An opt-out flag is explicitly a non-goal (see Non-Goals + FR-3)
- No metadata schema changes; no Cassandra / TiKV / memory backend changes; no
  RADOS / S3-backend changes. Pure auth-layer fix

### Test infrastructure
- Unit tests use the AWS-published test vectors directly (verified against the
  canonical example in
  `https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html`)
- Integration test uses the existing in-process harness in `internal/s3api`
  (no need for a real RADOS / Cassandra)

## Success Metrics

- All 4 stories shipped within one short Ralph cycle on
  `ralph/auth-per-chunk-signature` (≤ 4 iterations expected)
- AWS test vectors pass exactly (regression-proof against the spec)
- Smoke pass + s3-tests pass-rate unchanged (no regression in 80.1% headline)
- ROADMAP.md `## Auth` P2 entry flipped to Done

## Open Questions

(none — algorithm is fully spec'd, the only ambiguity is where the seed signature
flows through `internal/auth/sigv4.go`'s return shape; resolved at US-002 land
time by the implementer's local refactor of the existing helper)
