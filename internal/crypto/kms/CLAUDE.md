# internal/crypto/kms

KMS-backed wrap/unwrap for SSE-KMS per-object DEKs. Distinct from
`internal/crypto/master`: master keys are single-tenant secrets (env / file /
Vault Transit *export*) used for SSE-S3; KMS keys are tenant-named handles
(`x-amz-server-side-encryption-aws-kms-key-id`) that the KMS itself wraps the
DEK under via `GenerateDataKey` / `Decrypt`. The wrapped DEK is opaque to
Strata — it is whatever the KMS returns and only the KMS can unwrap it.

## Provider interface

```go
type Provider interface {
    GenerateDataKey(ctx, keyID) (plaintextDEK, wrappedDEK []byte, err error)
    UnwrapDEK(ctx, keyID, wrapped) (plaintextDEK []byte, err error)
}
```

Both methods take an explicit `keyID` (the per-request KMS key handle); the
provider does NOT cache "the active key" the way `master.Provider` does.
Empty keyID → `ErrMissingKeyID`. Wrapped-DEK bytes are stored verbatim — Vault
returns a string like `vault:v1:<base64>`, AWS returns binary; both are
treated as opaque by Strata and persisted on `objects.sse_key`.

## Provider precedence in `FromEnv`

`FromEnv(opts ...EnvOption)` checks env vars in this order: vault > aws-kms >
local-hsm. First hit wins; misconfiguration of a higher-priority provider is
fatal (does not silently fall through). aws-kms requires
`WithAWSKMSClientFactory(...)` — without it, setting `STRATA_KMS_AWS_REGION`
errors at startup. local-hsm is dev/test only.

## AWS KMS provider (US-037)

Env: `STRATA_KMS_AWS_REGION`. The cmd binary supplies a `KMSAPI` client
factory via `WithAWSKMSClientFactory(func(region string) (KMSAPI, error))`;
the factory builds a real `*awskms.Client` via the standard AWS credential
chain (env vars, shared config, IRSA, EC2/ECS roles). Same factory-injection
shape as US-010 SQS sink — keeps the SDK out of unit-test deps via the narrow
`KMSAPI` interface (`GenerateDataKey` + `Decrypt`). `KeySpec=AES_256` is
hard-coded; the per-call `keyID` flows through to the SDK input. KeyID
mismatch on `Decrypt` is detected by comparing the response's `KeyId` ARN
suffix to the requested id and surfaces as `ErrKeyIDMismatch` (gateway maps
to AccessDenied).

## Local-HSM provider (US-037, tests/dev only)

Env: `STRATA_KMS_LOCAL_HSM_SEED` (hex 32 bytes). Deterministic in-process
keystore — DEK = `HMAC-SHA256(seed, "strata-local-hsm-dek\x00" || keyID ||
\x00 || nonce)`. Wrapped DEK = `nonce(16) || mac(32)` where the mac binds
the keyID, nonce, and DEK so a wrong keyID on `UnwrapDEK` produces
`ErrKeyIDMismatch`. NOT a security boundary; the seed lives in process
memory.

## Vault Transit provider (US-036)

Env: `STRATA_KMS_VAULT_ADDR` + `STRATA_KMS_VAULT_PATH` (the Transit mount,
e.g. `transit`). AppRole credentials (`STRATA_SSE_VAULT_ROLE_ID` /
`STRATA_SSE_VAULT_SECRET_ID`) are shared with the SSE-S3 master-key Vault
provider — same Vault, same AppRole, different Transit operations.

Calls (per request keyID): POST `/v1/<path>/datakey/plaintext/<keyID>` and
POST `/v1/<path>/decrypt/<keyID>`. Token is cached until lease expiry; a 401
or 403 forces exactly one re-login then a retry — second 401 surfaces as a
plain error (the cached-fallback behaviour from `master.VaultProvider` does
not apply because there is no cached DEK to fall back on; every datakey call
is per-object).

## Adding a new provider

Implement `Provider`, add an env-var gate, wire into `FromEnv()` precedence.
For tests, drive the provider through the same shape as `vault_test.go`:
fake server with a per-test in-memory map for symmetric round-trip; cover
the missing-key-id path explicitly. Keep providers log-free unless you need
non-fatal warnings, in which case inject `*slog.Logger` via the config.
