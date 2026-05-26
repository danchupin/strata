---
title: 'Production hardening'
weight: 60
description: '12-line prod-readiness checklist linking each line to its runbook section. Closes the 6 P0 HTTP-surface gaps from the 2026-05-25 audit shipped by ralph/harden-gateway (US-001..US-010).'
---

# Production hardening

12-line checklist to run through before flipping a Strata replica into
prod traffic. Each line links to the runbook section that explains the
knob, the metric, and the failure mode it prevents. Every knob is opt-in
and zero-by-default — running through the list flips a memory / lab
deployment into a prod-ready shape without touching code.

| # | Check | Runbook |
|---|---|---|
| 1 | HTTP server timeouts non-zero (`STRATA_HTTP_READ_HEADER_TIMEOUT=10s` / `STRATA_HTTP_READ_TIMEOUT=60s` / `STRATA_HTTP_WRITE_TIMEOUT=30m` / `STRATA_HTTP_IDLE_TIMEOUT=120s` / `STRATA_HTTP_MAX_HEADER_BYTES=1048576`). Defaults already match — only verify if you tuned them. | [STRATA_HTTP_*]({{< ref "/reference/env-vars#gateway-core-http" >}}) |
| 2 | TLS terminated on the gateway (`STRATA_TLS_CERT_FILE` / `STRATA_TLS_KEY_FILE`, or `STRATA_TLS_CERT_DIR` for SNI multi-tenant) — or behind an ingress with `STRATA_TRUSTED_PROXIES` set to the ingress source CIDR. | [TLS termination — shapes B/C]({{< ref "/operate/tls-termination#deploy-shapes" >}}) |
| 3 | `STRATA_TLS_MIN_VERSION=TLS1.2` (default) and `STRATA_TLS_CIPHER_PROFILE=mozilla-modern` (default). Bump `MIN_VERSION=TLS1.3` if every client supports it. | [TLS shapes]({{< ref "/operate/tls-termination#deploy-shapes" >}}) |
| 4 | Cert hot-reload enabled (`STRATA_TLS_RELOAD_INTERVAL=60s`, default) so cert-manager / Vault PKI rotation is picked up without restart. | [cert-manager recipe]({{< ref "/operate/tls-termination#cert-manager-kubernetes" >}}) |
| 5 | Admin / console / metrics on a separate listener (`STRATA_ADMIN_LISTEN=127.0.0.1:9001` recommended; loopback or RFC1918 only). Optionally pin operator client certs via `STRATA_ADMIN_TLS_CLIENT_CA_FILE`. | [Shape C — split admin listener]({{< ref "/operate/tls-termination#shape-c--strata-terminated-split-admin--s3-listeners" >}}) |
| 6 | `STRATA_TRUSTED_PROXIES` set to the ingress / LB source CIDR. Default empty = `X-Forwarded-*` ignored. Required for the `Secure` cookie flag + audit-log client-IP fidelity behind any proxy. | [Trusted proxies — README breaking change](https://github.com/danchupin/strata/blob/main/README.md#breaking-changes) |
| 7 | Per-IP + per-key ingress rate limit on (`STRATA_RATE_LIMIT_PER_IP=N` and / or `STRATA_RATE_LIMIT_PER_KEY=N`). Default 0 = disabled. Refusal returns HTTP 429 + `<Code>SlowDown</Code>`. | [STRATA_RATE_LIMIT_*]({{< ref "/reference/env-vars#gateway-core-http" >}}) |
| 8 | Cassandra mTLS (`STRATA_CASSANDRA_TLS_CA_FILE` + `STRATA_CASSANDRA_TLS_CERT_FILE` + `STRATA_CASSANDRA_TLS_KEY_FILE`). `SKIP_VERIFY` must be `false` (default). | [Cassandra mTLS]({{< ref "/operate/tls-termination#cassandra" >}}) |
| 9 | TiKV mTLS (`STRATA_TIKV_TLS_CA_FILE` is **required** when any other TLS knob is set — the upstream silently downgrades on empty CA). PD endpoints accept `https://`. | [TiKV mTLS]({{< ref "/operate/tls-termination#tikv" >}}) |
| 10 | S3-upstream mTLS (`STRATA_S3_TLS_*` global default; per-cluster `tls` override on `STRATA_S3_CLUSTERS` JSON wins outright per cluster). | [S3-upstream mTLS]({{< ref "/operate/tls-termination#s3-upstream" >}}) |
| 11 | RADOS cephx in place — `STRATA_RADOS_KEYRING` populated; `ms_cluster_mode=secure` set in `ceph.conf` if wire-level confidentiality is required. (No Strata-side TLS knob for RADOS.) | [RADOS uses cephx]({{< ref "/operate/tls-termination#rados-uses-cephx-not-tls" >}}) |
| 12 | Prometheus alert on `sum(strata_backend_tls_skip_verify) > 0` (any backend with `SKIP_VERIFY=true`) AND on `rate(strata_ingress_rate_limit_refused_total[5m]) > N` (sustained client floods). | [Monitoring — alert recipes]({{< ref "/operate/monitoring" >}}) |

## What this checklist closes

Every line above closes a P0 gap from the 2026-05-25 prod-readiness
audit:

- 1 — slowloris connection exhaustion (no HTTP server timeouts).
- 2, 3, 4 — operator must run an external TLS sidecar for any
  HTTPS shape; cert rotation requires a restart.
- 5 — public S3 clients could reach `/admin/v1/*` and `/metrics` on
  the same listener.
- 6 — `X-Forwarded-Proto` blindly trusted, allowing a malicious client
  to spoof the `Secure` cookie flag and the audit-log source IP.
- 7 — runaway client could exhaust gateway CPU + meta-backend RPS.
- 8, 9, 10 — backend connections (Cassandra, TiKV, S3-upstream)
  authenticated only by access keys; a network intruder could
  impersonate the gateway.
- 11 — RADOS confidentiality story documented explicitly so operators
  don't reach for a non-existent `STRATA_RADOS_TLS_*` knob.
- 12 — `SKIP_VERIFY` accidentally left on in prod; rate-limit floods
  invisible without dashboards.

The closing-cycle is `ralph/harden-gateway` (US-001..US-010), shipped
2026-05-26. See the
[TLS termination playbook]({{< ref "/operate/tls-termination" >}}) for
the end-to-end deploy shapes and the
[STRATA_* env-vars reference]({{< ref "/reference/env-vars" >}}) for
every knob's range + default + TOML key.

## See also

- [Operate — TLS termination + backend mTLS]({{< ref "/operate/tls-termination" >}})
- [Reference — STRATA_* environment variables]({{< ref "/reference/env-vars" >}})
- [Operate — Monitoring]({{< ref "/operate/monitoring" >}})
