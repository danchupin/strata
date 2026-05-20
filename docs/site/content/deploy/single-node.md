---
title: 'Single-node deployment'
weight: 10
description: 'One Strata gateway against memory or Cassandra metadata + memory or RADOS data — for labs, dev workstations, single-tenant pilots.'
---

# Single-node Strata

A single-node deployment runs one `strata` process. It is the simplest
shape Strata ships and the recommended starting point for labs,
evaluation, and single-tenant pilots. The gateway is HTTP-stateless —
durability lives in the metadata + data backends — so a single replica
is **not** an HA shape: the box is the SPOF for HTTP traffic. For HA,
see [Multi-replica cluster]({{< ref "/deploy/multi-replica" >}}).

## Prerequisites

- One Linux host (or a macOS workstation for dev).
- Docker (for the full stack) **or** Go 1.23+ (for the in-memory smoke).
- 2 vCPU + 1 GiB RSS baseline (see *Sizing* below).

The pure-memory path needs no other software. The Cassandra-backed
path needs `cassandra:5.0` reachable on `:9042`. The full stack
(Cassandra + RADOS) is bootstrapped from the bundled compose file.

## Install

Three combinations are supported. Pick one.

**Pure memory — zero-dep dev:**

```bash
make run-memory          # boots strata on :8080, no Docker, no persistence
```

**Cassandra metadata + memory data — persistent metadata, ephemeral object bytes:**

```bash
make up && make wait-cassandra
make run-cassandra       # boots strata on :8080 against cassandra:5.0
```

**Full stack — Cassandra + RADOS:**

```bash
make up-all && make wait-cassandra && make wait-ceph
# gateway exposed on host :9999
```

For the TiKV-backed variant, see [Docker Compose]({{< ref "/deploy/docker-compose" >}}).

## Configure

The supported metadata × data combinations:

| Metadata | Data | When |
|---|---|---|
| `memory` | `memory` | `make run-memory`. State lost on restart. |
| `cassandra` | `memory` | `make run-cassandra`. Persistent metadata, ephemeral bytes. |
| `cassandra` | `rados` | `make up-all`. Full reference shape. |
| `tikv` | `rados` | TiKV-backed alternative (compose default lab). |
| `cassandra` or `tikv` | `s3` | S3-over-S3 — upstream S3 replaces RADOS. |

Top env vars (full table at
[Reference — environment variables]({{< ref "/reference/env-vars" >}})):

| Variable | Purpose |
|---|---|
| `STRATA_META_BACKEND` | `memory \| cassandra \| tikv`. |
| `STRATA_DATA_BACKEND` | `memory \| rados \| s3`. |
| `STRATA_AUTH_MODE` | `off`, `optional`, `required`. Lab uses `optional`. |
| `STRATA_STATIC_CREDENTIALS` | `<access>:<secret>:<owner>` triples. |
| `STRATA_LISTEN` | HTTP listen address. Defaults `:9000`. |
| `STRATA_LOG_LEVEL` | `DEBUG \| INFO \| WARN \| ERROR`. Defaults INFO. |
| `STRATA_WORKERS` | Comma-list of workers to run (`gc,lifecycle,...`). |

The `memory` backend is for tests and the smoke pass; never use it for
anything you care about. RADOS requires the `ceph` build tag
(`go build -tags ceph ./...`); the bundled Dockerfile builds that
variant.

## Verify

```bash
curl http://127.0.0.1:8080/healthz   # 200 once HTTP listener is bound
curl http://127.0.0.1:8080/readyz    # 200 once metadata + data probes pass
aws --endpoint-url http://127.0.0.1:8080 --no-sign-request s3 mb s3://test
aws --endpoint-url http://127.0.0.1:8080 --no-sign-request s3 cp README.md s3://test/
```

`make run-memory` runs auth-off (`STRATA_AUTH_MODE=""`), so aws-cli
must pass `--no-sign-request`. To exercise SigV4 locally, set
`STRATA_AUTH_MODE=required` + `STRATA_STATIC_CREDENTIALS=admin:adminpass:owner`
before launching — see [Get Started]({{< ref "/get-started" >}}) for
the full credential table.

The operator console (when the gateway runs on `:8080`) lives at
`http://127.0.0.1:8080/console/` — note the trailing slash.

## Monitor

The gateway exposes two endpoints regardless of `STRATA_AUTH_MODE`:

- `GET /healthz` — liveness. Always 200 once the HTTP listener is
  bound. Use as the Kubernetes `livenessProbe`.
- `GET /readyz` — readiness. Fans out probes to the metadata backend
  and the data backend with a 1 s timeout. Returns 200 only when every
  probe passes. Use as the readiness probe + the LB health-check
  target.

Both bypass auth and the access-log middleware.

Prometheus scrape: `:9000/metrics` on the gateway (or whichever port
`STRATA_LISTEN` advertises). OTel tracing enables when
`OTEL_EXPORTER_OTLP_ENDPOINT` is set; the in-process trace ring buffer
(`STRATA_OTEL_RINGBUF=on`, default) lets the operator console browse
spans without an external collector. Dashboards + alert recipes live
under [Operate](/operate/) and [Best Practices]({{< ref "/best-practices" >}}).

## Sizing

The gateway itself is cheap. The metadata + data backends are where
most of the budget goes. Per-replica baseline (single-tenant, mixed
read/write ≤200 RPS, mean object 256 KiB):

| Resource | Recommendation | Notes |
|---|---|---|
| CPU | 2 vCPU | Bursts on multipart Complete + lifecycle ticks. |
| RSS | ~1 GiB | Manifest decode + chunk fan-out buffers. |
| Disk (gateway) | <100 MiB | Stateless except for `/etc/strata/jwt-shared/`. |
| Disk (Cassandra) | Sized to row count | Each object ≈ 1 KiB metadata. 1M objects ≈ 1 GiB. |
| Disk (RADOS) | Sum of object sizes × replication factor | RADOS replicates per pool config (default `size=3`). |

## Troubleshoot

- **`/readyz` returns 503.** A metadata or data probe is failing.
  Inspect gateway logs (JSON `stdout`) for `probe failed` lines; check
  Cassandra / TiKV connectivity and RADOS pool status.
- **aws-cli `SignatureDoesNotMatch`.** Either the LB / proxy is
  rewriting the `Host` header (SigV4 signs it) or
  `STRATA_STATIC_CREDENTIALS` and the client credentials disagree.
  Run with `--debug` to see the canonical request shape.
- **`make run-memory` fails to bind `:8080`.** Another process owns
  the port. Set `STRATA_LISTEN=:8081` and retry.
- **macOS + lima Docker:** `make up` needs
  `DOCKER_HOST=unix:///Users/.../.lima/.../sock/docker.sock` so the
  Docker socket is reachable.
- **First-bucket PUT works, GET returns 503.** RADOS pool not ready.
  Wait for `make wait-ceph`; check `ceph -s` inside the container.

## Production checklist

When promoting a single-node deployment past lab:

- [ ] `STRATA_AUTH_MODE=required` set; root credentials stored outside the env file.
- [ ] `STRATA_CONFIG_FILE` mounted read-only with the runtime TOML.
- [ ] `STRATA_LOG_LEVEL=INFO` (or `WARN` for noisy clusters); JSON log handler is the default.
- [ ] Prometheus scraping `/metrics`; alerts on `strata_worker_panic_total > 0`.
- [ ] OTel collector pointed at via `OTEL_EXPORTER_OTLP_ENDPOINT`; sample ratio tuned via `STRATA_OTEL_SAMPLE_RATIO`.
- [ ] External log shipper draining `stdout` (JSON) into the central store.
- [ ] `/healthz` + `/readyz` wired into systemd / supervisord / Kubernetes probes.
- [ ] Backups configured for the metadata backend (Cassandra snapshots, TiKV PITR) and the data backend (RADOS pool snapshots or upstream-S3 versioning) — see [Operate — backup & restore](/operate/backup-restore/).
- [ ] Smoke pass scheduled (`make smoke` or `make smoke-signed`) post-deploy.

## Cross-references

- [Get Started]({{< ref "/get-started" >}}) — 5-minute first run.
- [Docker Compose]({{< ref "/deploy/docker-compose" >}}) — bundled compose file and supported profiles.
- [Multi-replica cluster]({{< ref "/deploy/multi-replica" >}}) — when one box is no longer enough.
- [Operate](/operate/) — day-2 workflows.
- [Reference — environment variables]({{< ref "/reference/env-vars" >}}) — full env knob table.
- [Architecture — Storage]({{< ref "/architecture/storage" >}}) — backend selection, object-table sharding, RADOS chunking.
