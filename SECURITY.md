# Security policy

Strata is alpha software, but its threat model is that of a production
object gateway: it terminates S3 traffic, evaluates IAM, stores audit
records, holds KMS-wrapped DEKs, and writes RADOS/TiKV/Cassandra. A
vulnerability in any of those surfaces is in scope.

This document is the contract between Strata maintainers and security
researchers reporting vulnerabilities.

## Supported versions

| Version line                  | Receives security patches |
|-------------------------------|---------------------------|
| `main` (HEAD)                 | ✓                         |
| Latest 2 minor release tags   | ✓                         |
| Older tags                    | ✗                         |

The first release tag is `v0.0.1-alpha.1` (cut by US-006 of the
supply-chain-security cycle). Pre-launch SemVer-pre-release shape — see
`ROADMAP.md` for the active release cadence. Support window slides
forward as new minor tags are cut: a new minor drops support for the
oldest of the previously-supported pair.

## Reporting a vulnerability

**Channel: [GitHub Security Advisories (GHSA)](https://github.com/danchupin/strata/security/advisories/new) — exclusive.**

No email backup channel. Rationale:

- GHSA submissions are encrypted in transit and at rest on GitHub's
  infrastructure — no maintainer mailbox to compromise.
- Every action is audit-trailed against the reporter's GitHub identity
  and the maintainer's actions — no `dkim=fail` / `spf=fail`
  forensics later.
- Zero infrastructure overhead: no mailbox to monitor, no SPF/DKIM/DMARC
  to rotate, no PGP key management. Email would add operational cost
  for no security gain over the GHSA channel.
- One channel is a clean contract: the report goes to a known place, with
  known semantics, owned by the platform that hosts the source.

If you do not have a GitHub account, [create one](https://github.com/signup) —
it takes less than a minute and is free.

## SLA and disclosure

- **Acknowledgement: 5 business days** from receipt of a valid report. We
  reply within the advisory thread; no out-of-band confirmation.
- **Disclosure deadline: 90 calendar days** from acknowledgement. By that
  date we have either shipped a fix, downgraded the severity by mutual
  agreement, or — if the fix requires longer — agreed an extended embargo
  in the advisory thread.
- **Coordinated disclosure after fix lands**: the advisory is published
  publicly once the fix has shipped on `main` AND on the supported tagged
  releases. CVE assignment, if applicable, happens through GitHub's CNA.

## Embargo

By submitting a GHSA report you agree to keep the vulnerability private
until the fix has been released. Sharing details with third parties —
including blog drafts, conference submissions, or co-workers — before
disclosure breaks the embargo and invalidates this policy's protections.

If you need to involve a third party (e.g. for verification of a
distributed-systems class bug) raise it inside the GHSA thread and we'll
add the collaborator.

## Patch-class advisory cadence

Strata uses Dependabot for upstream dependency monitoring. Patch-class
updates (`version-update:semver-patch`) auto-merge on green CI — see
[`.github/workflows/dependabot-auto-merge.yml`](.github/workflows/dependabot-auto-merge.yml).

**Practical consequence**: when an upstream Go module / GitHub Action /
npm package / Docker base image publishes a patch-class advisory,
Strata picks up the bump on the next Friday Dependabot run (06:00 UTC)
and merges automatically once required CI checks pass — so downstream
patches reach `main` within **1 week of upstream advisory** absent
unrelated CI flakiness.

Minor and major version updates stay open for human review; they do not
auto-merge.

## In-scope

Reports against the following surfaces are in-scope and prioritised:

- **SigV4 verifier bypass** — any path that accepts an invalid
  signature, replays a signed request, or admits an unsigned request
  outside `STRATA_AUTH_MODE=disabled`.
- **IAM policy evaluator bypass** — privilege escalation via crafted
  bucket policies, ACLs, IAM policy documents, condition keys, or
  principal expansion.
- **Audit-log forgery or omission** — any way to perform a
  state-changing S3 / admin request without producing the matching
  `audit_log` row, or to inject a forged row.
- **KMS / SSE DEK leak** — exposure of unwrapped DEK material via API,
  logs, traces, metrics, or error responses; rewrap-time key
  cross-contamination.
- **Manifest tampering** — any path that admits a CAS-bypassing
  manifest mutation or breaks the per-object versioning invariant.
- **Admin / console privilege escalation** — bypass of admin auth, CSRF
  against the embedded operator console, session fixation, or
  cookie-handling errors that expose admin tokens.
- **Panic-on-malformed-input (DoS-by-crash)** — any client-controlled
  payload that crashes the gateway process or a worker.
- **Cluster drain / rebalance safety** — paths that admit writes into a
  draining cluster, lose data during rebalance, or break the
  `BackendRef` invariant.
- **Cross-tenant data leak** — any path where one IAM principal can
  read another tenant's objects, audit rows, KMS material, or admin
  state.

## Out-of-scope

The following are explicitly **not** vulnerabilities under this policy:

- **Self-hosted operator misconfigurations**, including running with
  `STRATA_AUTH_MODE=disabled` outside a closed lab. The disabled mode
  is documented as lab-only.
- **Network-layer attacks** against TLS termination performed by an
  operator-managed ingress or load-balancer. Strata's own TLS surface
  (`STRATA_TLS_*`) is in-scope; operator-managed termination is not.
- **Denial-of-service via legitimate-but-expensive requests** — large
  ListObjects, multipart with many parts, etc. Use
  `STRATA_RATE_LIMIT_PER_IP` / `STRATA_RATE_LIMIT_PER_KEY` to bound.
- **Issues in third-party dependencies** that have not yet been
  published in a fixed upstream release. Report those upstream first;
  Strata's Dependabot will pick up the fix on the next Friday run.
- **Best-practice / hardening recommendations** that do not exploit a
  concrete weakness. Open a regular issue or PR instead.

## Production hardening cross-reference

For deployment-time defence-in-depth — HTTP timeouts, TLS shapes, mTLS
to backends, trusted proxies, per-IP rate limiting, RADOS cephx — work
through the
[production-hardening checklist](https://danchupin.github.io/strata/best-practices/production-hardening/)
before exposing a Strata replica to untrusted traffic.

## Hall of fame

We acknowledge security researchers who have reported valid issues
under this policy. (No entries yet — be the first.)

| Researcher | Issue                  | Date     | Advisory |
|------------|------------------------|----------|----------|
| —          | —                      | —        | —        |
