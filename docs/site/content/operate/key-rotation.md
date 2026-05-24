---
title: 'Per-bucket signing key rotation'
weight: 30
description: 'Operator workflow for the KMS-backed per-bucket SigV4 signing keys: rotate, check status, set max-age, troubleshoot fail-closed responses.'
---

# Per-bucket signing key rotation

Strata supports KMS-backed per-bucket SigV4 signing keys (US-001 /
US-002 of `ralph/auth-dx-trailer-lima`). Each opted-in bucket carries a
32-byte DEK wrapped under a KMS CMK (AWS KMS / Vault Transit /
LocalHSMProvider) plus a creation timestamp. The auth middleware
unwraps on cache miss and uses `hex(DEK)` as the SigV4 secret in place
of the IAM access-key secret.

This page covers the day-2 rotation runbook. The
[KMS provider reference]({{< relref "/reference/env-vars" >}}) lists
the per-provider env vars; the
[admin API reference]({{< relref "/reference/admin-api" >}}) documents
the three rotation endpoints.

## When to rotate

| Trigger | Action |
|---------|--------|
| Quarterly / compliance window | Operator runs `POST /signing-key/rotate` |
| `STRATA_KEY_MAX_AGE` elapsed (default 90 days) | Auth returns HTTP 401 `KeyExpired`; operator MUST rotate to recover |
| Suspected DEK exposure (heap dump, lost laptop) | Rotate immediately + invalidate any cached DEKs client-side |
| KMS CMK rotation policy fired | Rotate Strata's bucket envelope under the new CMK alias |

## Quick reference

```bash
# Mint a fresh DEK under the configured default CMK.
curl -sS -X POST -H 'Authorization: <SigV4 or session cookie>' \
  http://strata:9999/admin/v1/buckets/<bucket>/signing-key/rotate

# Mint a fresh DEK under an explicit CMK alias.
curl -sS -X POST -H 'Authorization: <auth>' -H 'Content-Type: application/json' \
  -d '{"key_id":"alias/strata-prod-2026"}' \
  http://strata:9999/admin/v1/buckets/<bucket>/signing-key/rotate

# Check status — age, max-age, and the expired flag.
curl -sS -H 'Authorization: <auth>' \
  http://strata:9999/admin/v1/buckets/<bucket>/signing-key/status

# Drop the per-bucket key (falls back to IAM access-key auth).
curl -sS -X DELETE -H 'Authorization: <auth>' \
  http://strata:9999/admin/v1/buckets/<bucket>/signing-key
```

## Rotation response

`POST /signing-key/rotate` returns the freshly minted plaintext DEK as
`secret_access_key` ONCE. Capture it client-side immediately — the
gateway cannot recover the plaintext after the response is discarded.

```json
{
  "key_id": "alias/strata-prod-2026",
  "secret_access_key": "0a1b2c3d...8e9f",
  "created_at": 1748390400,
  "wrapped_dek_length": 184
}
```

Operators paste `secret_access_key` into the client config exactly as
they would an IAM access-key secret. The access key id stays the same;
the SigV4 derivation chain just uses the new secret material.

## Max-age and fail-closed semantics

`STRATA_KEY_MAX_AGE` (Go duration, default `2160h` = 90 days, range
`[24h, 8760h]`) bounds the age of a signing key. Once a key crosses the
threshold:

- Auth middleware returns HTTP 401 `KeyExpired` (audit row stamped).
- `strata_kms_decrypt_total{outcome="expired"}` increments per attempt.
- The bucket cannot accept signed requests until the operator rotates.

The path is fail-closed by design: the gateway does NOT silently fall
through to the IAM access-key path. Opting a bucket into per-bucket
signing means the operator wants the rotation guarantee enforced.

Other fail-closed responses:

| Symptom | HTTP | Audit verb | Operator action |
|---------|------|------------|-----------------|
| KMS provider unavailable (network / 5xx) | `503 KMSUnavailable` + `Retry-After: 30` | `kms_decrypt_total{outcome="unavailable"}` | Wait for KMS recovery; check Vault unsealed / AWS reachability |
| Wrong CMK on unwrap | `401 KeyDenied` | `outcome="denied"` | Verify `key_id` matches the CMK that wrapped the envelope |
| LocalHSM HMAC mismatch (wrapped DEK tampered) | `401 KeyTampered` | `outcome="tampered"` | Investigate meta corruption; rotate to recover |
| Key past `STRATA_KEY_MAX_AGE` | `401 KeyExpired` | `outcome="expired"` | Rotate via the admin endpoint |
| No KMS configured but bucket has key | `503 KMSUnavailable` | n/a | Configure `STRATA_KMS_*` env vars and restart |

## Status endpoint

`GET /signing-key/status` returns:

```json
{
  "key_id": "alias/strata-prod-2026",
  "created_at": 1748390400,
  "age_days": 1.5,
  "max_age_days": 90.0,
  "expired": false
}
```

Use this in monitoring scripts to fire a rotation alert before
`age_days` crosses `max_age_days`:

```bash
# Alert when within 7 days of expiry.
threshold=$(jq -r '.max_age_days - 7' <<< "$status")
age=$(jq -r '.age_days' <<< "$status")
awk -v age="$age" -v t="$threshold" 'BEGIN{exit !(age > t)}' \
  && echo 'rotate within 1 week'
```

## Tuning

| Env var | Default | Range | Purpose |
|---------|---------|-------|---------|
| `STRATA_KEY_MAX_AGE` | `2160h` (90d) | `[24h, 8760h]` | Auth rejects keys older than this with 401 KeyExpired |
| `STRATA_DEK_CACHE_TTL` | `5m` | `[30s, 1h]` | DEK plaintext lifetime in the auth cache (per process) |
| `STRATA_KMS_DEFAULT_KEY_ID` | unset | any | Default CMK handle for `POST /signing-key/rotate` when the operator omits `key_id`; absence falls back to the bucket name (works for AWS KMS aliases + Vault Transit) |

PCI-DSS / SOX rotation policy is 90 days; this is the shipped default.
Tighten via `STRATA_KEY_MAX_AGE=30d` if your compliance regime
demands. The minimum `24h` exists to prevent operator typos from
locking out a bucket within minutes.

## Audit log shape

Every admin endpoint stamps an audit row through the
`AuditMiddleware` override:

- `POST /rotate` → `admin:RotateBucketSigningKey`
- `GET /status` → `admin:GetBucketSigningKeyStatus`
- `DELETE` → `admin:DeleteBucketSigningKey`

The auth-side `401 KeyExpired` audit row carries `action=GET|PUT|...`
(the original S3 op) plus `error=KeyExpired`. Filter on
`resource:bucket:<name>` to see the full rotation history.

## Recovery from a botched rotation

If a rotation succeeds in KMS but the meta write fails (rare — the
admin handler surfaces the meta error), the new wrapped DEK is lost
because the plaintext was only returned in the response and the meta
row was not updated. The bucket continues using the prior envelope. To
recover:

1. Re-run `POST /rotate` — the next call mints a fresh DEK and persists
   it.
2. Verify with `GET /status` — `created_at` should reflect the new
   timestamp.
3. Update the client's secret material with the new `secret_access_key`.

Operators who lose the plaintext DEK without persisting it client-side
must rotate again — the gateway cannot recover the plaintext after the
response is discarded.
