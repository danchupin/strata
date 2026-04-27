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
