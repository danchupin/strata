---
title: 'Compliance'
weight: 75
description: 'S3 Object Lock COMPLIANCE workflow — bucket config, per-object retention + legal hold, the three objectlock audit verbs, and the lifecycle-driven expiry path.'
---

# Compliance

Strata implements S3 Object Lock with COMPLIANCE and GOVERNANCE retention
modes plus legal hold. COMPLIANCE rows are immutable until their
`RetainUntilDate` elapses — not even the bucket owner can shorten the
retention window or delete the object early. The audit log records every
COMPLIANCE-mode write and every expiry the lifecycle worker performs so
auditors can grep `audit_log` for retention-policy events with a single
`action LIKE 'objectlock:%'` clause.

This page is the operator workflow guide. For the audit-log shape see
[Monitoring — audit log]({{< ref "/operate/monitoring#audit-log" >}}); for
the underlying retention semantics see the [S3 Compatibility]({{< ref "/s3-compatibility" >}})
matrix.

## Enable Object Lock on a bucket

Object Lock must be opted into at bucket-create time — it cannot be
turned on for an existing bucket. The S3 surface is identical to AWS:

```bash
aws s3api create-bucket \
  --bucket compliance-vault \
  --object-lock-enabled-for-bucket \
  --endpoint-url http://localhost:9999
```

Once enabled, the bucket's Object Lock configuration can carry a
default retention rule that every new object inherits unless overridden:

```bash
aws s3api put-object-lock-configuration \
  --bucket compliance-vault \
  --object-lock-configuration '{
    "ObjectLockEnabled": "Enabled",
    "Rule": {
      "DefaultRetention": {
        "Mode": "COMPLIANCE",
        "Days": 2555
      }
    }
  }' \
  --endpoint-url http://localhost:9999
```

Default retention applies on `PutObject` when the client does not send
its own `x-amz-object-lock-*` headers. The mode + duration land on the
object row and become enforceable immediately.

## Per-object retention

The PUT path accepts three retention shapes:

| Header | Effect |
|---|---|
| `x-amz-object-lock-mode: COMPLIANCE` + `x-amz-object-lock-retain-until-date: <RFC3339>` | Immutable until the date. Cannot be shortened or downgraded. |
| `x-amz-object-lock-mode: GOVERNANCE` + `x-amz-object-lock-retain-until-date: <RFC3339>` | Immutable until the date, but holders of `s3:BypassGovernanceRetention` can delete via the `x-amz-bypass-governance-retention: true` header. |
| `x-amz-object-lock-legal-hold: ON` | Indefinite hold orthogonal to retention. Stays on until explicitly turned `OFF`. |

The bucket's default retention rule applies when the client omits both
the mode and the date. A client may also set retention out-of-band
via `PutObjectRetention`:

```bash
aws s3api put-object-retention \
  --bucket compliance-vault \
  --key receipts/2026-Q1.csv \
  --retention '{
    "Mode": "COMPLIANCE",
    "RetainUntilDate": "2033-12-31T00:00:00Z"
  }' \
  --endpoint-url http://localhost:9999
```

## What COMPLIANCE actually blocks

A COMPLIANCE-retained object with an unexpired `RetainUntilDate` rejects
the following operations with HTTP 403 `AccessDenied`:

- `DeleteObject` (any caller — even the bucket owner).
- `PutObjectRetention` if the new retention would weaken the existing
  one. "Weaken" means: clear the mode, downgrade `COMPLIANCE` →
  `GOVERNANCE`, or shorten the date. Extending the date is allowed.

Reads (`GetObject`, `HeadObject`, `ListObjectVersions`) are unaffected.
Legal hold blocks the same delete + retention-mutation paths regardless
of retention mode or expiry.

GOVERNANCE delete with the bypass header succeeds when the principal
holds the appropriate IAM action — see [IAM]({{< ref "/reference/admin-api" >}})
for the policy shape.

## Audit verbs

The audit middleware stamps three purpose-specific verbs so auditors
can filter retention-policy events without scanning every state-changing
request.

| Verb | When stamped | Principal | Resource |
|---|---|---|---|
| `objectlock:CompliancePut` | `PUT /bkt/key?retention` succeeds with `<Mode>COMPLIANCE</Mode>`. | Caller (`Auth.Owner`). | `object:<bucket>/<key>` |
| `objectlock:ComplianceRetentionAttemptedReduce` | A retention write would weaken an existing COMPLIANCE retention. The request is rejected with `AccessDenied`; the row records the attempt regardless of outcome. | Caller (`Auth.Owner`). | `object:<bucket>/<key>` |
| `objectlock:ComplianceRetentionExpired` | The lifecycle worker successfully expires an object whose COMPLIANCE `RetainUntilDate` has elapsed. | `system:lifecycle-worker`. | `object:<bucket>/<key>` |

The `Resource` shape is always `object:<bucket>/<key>` — operator-facing
rather than the URL-path-derived `/<bucket>/<key>` shape used by generic
S3 writes. This lets compliance dashboards filter on a single substring.

Operator query — every retention-policy event for a bucket over the
last 24 hours (Cassandra):

```sql
SELECT time, action, principal, resource, result
FROM audit_log
WHERE bucket_id = <bucket-uuid>
  AND time > now() - INTERVAL '1 day'
  AND action LIKE 'objectlock:%'
ORDER BY time DESC;
```

Audit retention defaults to 30 days; the audit-export worker drains
older partitions to the configured export bucket. See
[Monitoring — audit log]({{< ref "/operate/monitoring#audit-log" >}})
for the full retention + export shape.

## Lifecycle worker — COMPLIANCE expiry

The lifecycle worker is the only path that can delete a COMPLIANCE-retained
object — and only after its `RetainUntilDate` has elapsed. Per scan:

1. Walk every bucket. For each object whose lifecycle policy matches an
   Expiration rule, compare `RetainUntilDate` to the current UTC time.
2. If the date has elapsed, perform the delete + enqueue chunk
   reclamation in the GC queue (the standard expiry path).
3. Append one `objectlock:ComplianceRetentionExpired` row to
   `audit_log` with `Principal=system:lifecycle-worker`,
   `Action=objectlock:ComplianceRetentionExpired`,
   `Resource=object:<bucket>/<key>`, `Result=200`.

A separate `AuditTTL` knob caps how long the expiry-row stays in
`audit_log` (default 30 days, matching the gateway-side audit
retention). The lifecycle package is intentionally isolated from the
gateway audit-row TTL so the two retention windows can diverge if the
operator needs a longer compliance audit trail.

If the lifecycle policy fires while the retention is still active the
worker silently skips the object — it does not log a "blocked" row.
Object-lock blocking happens at the gateway PUT/DELETE path, not the
lifecycle path.

## Step-by-step compliance workflow

1. **Create the bucket with Object Lock enabled.**
   `aws s3api create-bucket --object-lock-enabled-for-bucket …` — Object
   Lock cannot be turned on after creation.
2. **Set a default retention rule (optional).**
   Default retention applies on every PUT that omits the
   `x-amz-object-lock-*` headers, so per-tenant policies can be enforced
   at the bucket layer.
3. **Verify with a sample object.**
   PUT an object with `--object-lock-mode COMPLIANCE
   --object-lock-retain-until-date <future>`; immediately
   `DeleteObject` and confirm `403 AccessDenied`.
4. **Wire `audit_log` to your SIEM.**
   Tail `audit_log` for `action LIKE 'objectlock:%'` and forward rows
   to your compliance dashboard. The three verbs cover the entire
   retention lifecycle.
5. **Plan audit retention.**
   `STRATA_AUDIT_RETENTION` (default 30 d) trims the gateway-side rows;
   the audit-export worker fans older rows to a configured S3 bucket
   for long-term archive. Set both so the auditor's retention window is
   covered end-to-end.

## Common pitfalls

- **Trying to enable Object Lock on an existing bucket.** Not
  supported by S3. Create a new bucket with `--object-lock-enabled-for-bucket`
  and migrate objects via `aws s3 cp` (or replication if cross-region).
- **Confusing COMPLIANCE with GOVERNANCE.** COMPLIANCE rejects every
  delete + downgrade — there is no bypass. GOVERNANCE accepts deletes
  from principals with `s3:BypassGovernanceRetention` and the explicit
  bypass header. Pick COMPLIANCE for regulatory holds, GOVERNANCE for
  workflow holds.
- **Shortening a COMPLIANCE retention.** Returns `403 AccessDenied`
  and stamps `objectlock:ComplianceRetentionAttemptedReduce`. Use this
  audit verb to alert on policy-violation attempts.
- **Relying on `DeleteObject` for compliance cleanup.** The gateway
  blocks every COMPLIANCE-retained delete. Lifecycle expiry is the
  only path; expect the row in `audit_log` only after
  `RetainUntilDate` elapses + the worker tick fires.
- **Not wiring the audit-export worker.** Compliance audits often
  require multi-year retention; gateway-side `audit_log` defaults to
  30 days. Enable `--workers=audit-export` so older partitions land in
  a long-term archive bucket. See [Monitoring]({{< ref "/operate/monitoring" >}}).

## See also

- [Monitoring]({{< ref "/operate/monitoring" >}}) for the audit-log
  schema, retention knobs, and export pipeline.
- [S3 Compatibility]({{< ref "/s3-compatibility" >}}) for the
  supported / unsupported Object Lock surface.
- [Architecture — Workers]({{< ref "/architecture/workers" >}}) for the
  lifecycle worker shape that drives COMPLIANCE expiry.
- [Reference — Admin API]({{< ref "/reference/admin-api" >}}) for the
  IAM policy shape that gates GOVERNANCE-bypass deletes.
