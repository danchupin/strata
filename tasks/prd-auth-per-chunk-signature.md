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
- [ ] AWS-spec test vectors transcribed verbatim into the test file as inline
      Go literals (NOT a runtime URL fetch). Copy the "Example Calculations"
      from
      [https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html](https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html)
      — seed-sig + chunk-1 sig + chunk-2 sig + final-empty sig as a
      `[]struct{ payload []byte; expectedSig string }` table in
      `internal/auth/streaming_test.go`. Inline copy so test is offline-runnable
      and survives AWS doc URL rotations
- [ ] Final empty-chunk signature also computed correctly (zero-length payload still
      participates in the chain) — covered as the last entry in the table
- [ ] Computed signatures compared via `hmac.Equal([]byte, []byte)` — constant-
      time. The exported `chunkSigner.Next()` returns hex-encoded string for
      caller convenience but tests compare via `hmac.Equal` against the
      decoded byte slice
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: Wire validator into the streaming decoder + reject on mismatch
**Description:** As an S3 client uploading with streaming SigV4, I want the gateway to
reject my upload if any chunk signature does not match the chain — so an attacker
mutating bytes mid-stream is detected.

**Acceptance Criteria:**
- [ ] Decoder in `internal/auth/streaming.go` reads each chunk's framed payload
      **fully into a per-chunk buffer** (chunk size ≤8 KiB in typical SDK output;
      we cap the per-chunk buffer at 16 MiB to defend against malformed framing)
      BEFORE forwarding any byte to the consumer. With the full chunk in hand,
      compute SHA-256 of the payload, derive the expected signature via
      `chunkSigner.Next(payload)`, and compare to the client-supplied
      `;chunk-signature=<hex>`
- [ ] Signature comparison uses `hmac.Equal([]byte(expected), []byte(actual))` —
      constant-time, defense against timing side channels (Go stdlib idiom for
      authentication tags)
- [ ] On match: forward the buffered chunk payload to the consumer; advance to
      next chunk
- [ ] On mismatch: return a typed error `errChunkSignatureMismatch` from the
      `Read` loop. **No bytes from the mutated chunk reach the consumer** — the
      buffer-then-validate ordering guarantees this
- [ ] Auth middleware translates `errChunkSignatureMismatch` to
      `403 SignatureDoesNotMatch` with the standard AWS XML error body
- [ ] Final empty chunk validation: failure on the trailer chunk also returns 403
      (a mutated trailer must not slip through)
- [ ] Memory bound: the streaming decoder buffers AT MOST one chunk at a time
      (≤16 MiB cap). The "no full-body buffer" invariant is preserved — for a
      1 GiB body with 8 KiB chunks, peak buffer is still 8 KiB
- [ ] If the outer SigV4 signature was missing/malformed (request rejected before
      the streaming decoder runs), no chain validation occurs — pre-existing 403
      path unchanged
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: Detect `aws-chunked-trailer` format and reject with 501
**Description:** As a maintainer, I want the gateway to surface a clear error
when a client sends the newer `aws-chunked-trailer` format (used by some
aws-cli 2.x variants — see ROADMAP "Known latent bugs"), so the chunk-validation
fix from US-002 does not silently break for those clients (today's decoder
already breaks on trailer-format anyway, but with a confusing error).

**Acceptance Criteria:**
- [ ] `internal/auth/middleware.go` detects trailer-format requests via the
      `x-amz-trailer` request header (presence of any value indicates
      `aws-chunked-trailer` framing)
- [ ] When `x-amz-trailer` is set AND `x-amz-content-sha256` is the streaming
      sentinel (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD` or its trailer-variant
      `STREAMING-UNSIGNED-PAYLOAD-TRAILER`): respond with
      `501 NotImplemented` and an XML error body `<Code>NotImplemented</Code>
      <Message>aws-chunked-trailer format is not yet supported; see ROADMAP
      "Known latent bugs"</Message>`
- [ ] When `x-amz-trailer` is empty or missing: behavior unchanged (legacy
      chained format flows through US-002's validator)
- [ ] Update ROADMAP "Known latent bugs" entry to record that the gateway now
      explicitly rejects trailer-format with 501 (instead of producing a
      confusing decoder error). Bump the bug severity if needed
- [ ] Test: streaming PUT with `x-amz-trailer: x-amz-checksum-sha256` returns
      501; without the header, behavior is unchanged
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: Integration test — mutated chunk in flight returns 403
**Description:** As a security-conscious maintainer, I want an end-to-end test that
issues a properly-signed streaming upload, mutates one chunk byte in flight, and
asserts the gateway returns 403 (not 200).

**Acceptance Criteria:**
- [ ] New test `TestStreamingSigV4_MutatedChunk_Rejected` in
      `internal/s3api/sigv4_streaming_test.go` (or wherever streaming smoke lives)
- [ ] Test builds a 3-chunk streaming PUT body manually (chunk-1, chunk-2,
      final-empty) with correct chain signatures
- [ ] Sends two variants through the harness:
      - **Variant A — pristine body:** PUT returns `200`; subsequent
        `GetObject(bucket, key)` returns `200` with the full reconstructed
        body matching the original input bytes (verifies the chain validator
        is not over-rejecting valid uploads)
      - **Variant B — mutated payload:** same body but byte at offset N inside
        chunk-2's payload XOR'd with `0x01` (chunk-signature header unchanged
        — simulates an attacker mutating in-flight bytes). PUT returns
        `403 SignatureDoesNotMatch`. Subsequent `GetObject(bucket, key)`
        returns `404 NoSuchKey` — proving the body was never written to the
        backend (US-002's "buffer-then-validate" guarantee)
- [ ] Test runs under existing `go test ./internal/s3api/...` (no integration tag —
      uses the in-memory backend)
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: ROADMAP close-flip + CHANGELOG note
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
  backend (enforced by buffer-then-validate ordering: full chunk into per-chunk
  buffer ≤16 MiB, validate, then forward — never the other way around)
- FR-3: No opt-out — there is no env var or flag that disables per-chunk validation.
  Every living AWS SDK sends a correct chain; an opt-out exists only as a
  perpetual security footgun
- FR-4: Streaming-shape preserved — peak buffer is ONE chunk (typical ≤8 KiB,
  capped at 16 MiB). The full request body is never held in memory simultaneously
- FR-5: Signature comparison is constant-time via `hmac.Equal` (defense against
  timing side-channels)
- FR-6: `aws-chunked-trailer` format (newer aws-cli) is detected via the
  `x-amz-trailer` request header and rejected with `501 NotImplemented` rather
  than producing a confusing decoder error. Trailer-format support is a future
  PRD
- FR-7: s3-tests pass-rate captured at cycle start (US-001 commit message) is
  re-asserted at cycle close (US-005) — chunk validation MUST NOT regress
  upload paths. Regression blocks cycle close
- FR-8: ROADMAP.md `## Auth` P2 entry flips to Done in the same commit (or the
  immediate follow-up SHA edit) as US-005 lands

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

- All 5 stories shipped within one short Ralph cycle on
  `ralph/auth-per-chunk-signature` (≤ 5 iterations expected; US-003 is small
  but separated for review clarity)
- AWS test vectors pass exactly (regression-proof against the spec)
- Smoke pass + s3-tests pass-rate **unchanged vs the baseline captured at
  cycle start**. Concrete protocol: US-001's first commit message records
  the current `scripts/s3-tests/run.sh` headline (e.g. "Baseline at cycle
  start: 78.5% / 139 of 177"). US-005's closing commit re-runs
  `scripts/s3-tests/run.sh` and asserts the new headline `>=` the captured
  baseline. A regression on s3-tests blocks the cycle close — chunk
  validation MUST NOT regress upload paths
- ROADMAP.md `## Auth` P2 entry flipped to Done

## Resolved Decisions

These were debated during PRD review (2026-05-01); recording chosen paths so
the Ralph cycle does not re-litigate them.

- **Q1 — Body byte flow on chunk validation failure.** Resolved: buffer the
  full chunk payload (≤16 MiB cap) before forwarding any byte to the consumer.
  Validate signature against the buffered chunk, then forward on match or
  return error on mismatch. Trade-off: small extra latency vs absolute
  guarantee that mutated bytes never reach the backend. Chunks are typically
  ≤8 KiB in SDK output, so buffer overhead is negligible. The "no full-body
  buffer" invariant still holds — peak buffer is one chunk, not the whole
  request body. See US-002 first AC
- **Q2 — `aws-chunked-trailer` format handling.** Resolved: detect via
  `x-amz-trailer` request header and respond `501 NotImplemented` with a
  pointer to the ROADMAP "Known latent bugs" entry. Today's decoder breaks
  on trailer-format with a confusing error; the explicit 501 is friendlier
  for the small number of clients (some aws-cli 2.x variants) that send it.
  Adding actual trailer-format support is a separate future PRD. See US-003
- **Q3 — US-004 test variant phrasing.** Resolved: pristine variant returns
  200 PUT + 200 GET with body-roundtrip; mutated variant returns 403 PUT
  + 404 NoSuchKey on subsequent GET (proving no bytes leaked to backend).
  Original draft had a contradiction ("200 and ErrNotFound") which was a
  typo. See US-004
- **Q4 — Timing-safe signature comparison.** Resolved: use `hmac.Equal` for
  the chunk-signature comparison. Standard Go stdlib idiom for authentication
  tag comparison; defends against timing side-channels. See US-002 second AC
- **Q5 — s3-tests baseline assertion.** Resolved: capture the s3-tests
  headline at cycle start in US-001's commit message; re-run at cycle close
  in US-005 and assert the new headline `>=` the captured baseline. This
  decouples this PRD from the concurrent `ralph/s3-tests-90` cycle's drifting
  baseline (today 78.5%, target ≥90%). Chunk validation is a defensive
  fix — it MUST NOT regress upload paths; a regression blocks cycle close.
  See Success Metrics

## Open Questions

(none — Q1..Q5 resolved above; algorithm is fully spec'd)
