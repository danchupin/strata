---
title: 'Single-node deployment'
weight: 10
description: 'One Strata gateway against memory or Cassandra metadata + memory or RADOS data — for labs, dev workstations, single-tenant pilots.'
---

# Single-node Strata

A single-node deployment runs one `strata` process. It is the simplest shape
Strata ships and the recommended starting point for labs, evaluation, and
single-tenant pilots. The gateway is HTTP-stateless — durability lives in
the metadata + data backends — so a single replica is **not** an HA shape:
the box is the SPOF for HTTP traffic. For HA, see
[Multi-replica cluster]({{< ref "/deploy/multi-replica" >}}).

## When to pick this shape

| Use case | Notes |
|---|---|
| Local dev / smoke tests | `make run-memory` boots a fully in-memory gateway in <1 s. No Docker. |
| Single-tenant evaluation | One replica + Cassandra + memory data is enough to run the s3-tests suite end-to-end. |
| Lab / classroom | `make up-all` boots cassandra + ceph + strata in one command. |
| Production single-tenant | Acceptable when planned downtime windows allow gateway restarts; pair with external monitoring on `/readyz`. |

For multi-tenant production traffic, scale to ≥2 replicas and pick a real
metadata cluster (Cassandra ≥3 nodes or TiKV with PD ≥3 + TiKV ≥3).

## Backend matrix

A single-node deployment combines one metadata backend and one data backend.
The supported pairs:

| Metadata | Data | Use case |
|---|---|---|
| `memory` | `memory` | `make run-memory`. Zero-dep dev. State lost on restart. |
| `cassandra` | `memory` | `make run-cassandra`. Persistent metadata, ephemeral object bytes. Useful for s3-tests against a single Cassandra. |
| `cassandra` | `rados` | `make up-all`. The full reference shape — what `strata:ceph` ships in the bundled compose. |
| `tikv` | `rados` | TiKV-backed alternative — see the `tikv` profile in [Docker Compose]({{< ref "/deploy/docker-compose" >}}). |
| `cassandra` or `tikv` | `s3` | S3-over-S3 backend (RADOS replaced by an upstream S3 endpoint). See [`architecture/backends/s3`]({{< ref "/architecture/backends/s3" >}}). |

The `memory` backend is for tests and the smoke pass; never use it for
anything you care about. RADOS requires the build tag `ceph` (`go build
-tags ceph ./...`); the Dockerfile under `deploy/docker/Dockerfile` builds
that variant.

## Bring-up — pure-memory

```bash
make run-memory          # boots strata on :8080, no Docker, no persistence
aws --endpoint-url http://127.0.0.1:8080 \
    --no-sign-request s3 mb s3://test
aws --endpoint-url http://127.0.0.1:8080 \
    --no-sign-request s3 cp README.md s3://test/
```

`make run-memory` runs auth-off (`STRATA_AUTH_MODE=""`), so aws-cli must
pass `--no-sign-request`. To exercise SigV4 locally, set
`STRATA_AUTH_MODE=required` + `STRATA_STATIC_CREDENTIALS=admin:adminpass:owner`
before launching — see [Get Started]({{< ref "/get-started" >}}) for the
full credential table.

## Bring-up — Cassandra metadata + memory data

```bash
make up && make wait-cassandra      # boots cassandra:5.0 on 9042
make run-cassandra                  # boots strata on :8080 against it
```

State survives gateway restart; object bytes do not (data backend is still
`memory`). Useful when you want LWT semantics under load without paying
RADOS startup cost.

## Bring-up — full stack (Cassandra + RADOS)

```bash
make up-all && make wait-cassandra && make wait-ceph
# strata is now on host port 9999 (mapped from container 9000).
aws --endpoint-url http://127.0.0.1:9999 --no-sign-request s3 ls
```

`up-all` brings the bundled `strata:ceph` image with `STRATA_WORKERS=gc,lifecycle`
by default. Switch profiles via env (see [Docker Compose]({{< ref "/deploy/docker-compose" >}}))
to enable the wider feature workers.

## Sizing rule of thumb

The gateway itself is cheap. The metadata + data backends are where most
of the budget goes. Per-replica baseline (single-tenant, mixed read/write
≤200 RPS, mean object 256 KiB):

| Resource | Recommendation | Notes |
|---|---|---|
| CPU | 2 vCPU | Bursts on multipart Complete + lifecycle ticks. |
| RSS | ~1 GiB | Manifest decode + chunk fan-out buffers. |
| Disk (gateway) | <100 MiB | The gateway is stateless except for `/etc/strata/jwt-shared/`. |
| Disk (Cassandra) | Sized to row count, not byte count | Each object = ~1 KiB metadata. 1M objects → ~1 GiB. |
| Disk (RADOS) | Sum of object sizes × replication factor | RADOS replicates per pool config (default `size=3`). |

For higher RPS or larger objects, see [Best Practices]({{< ref "/best-practices" >}})
(per-page sizing / monitoring / tuning guides land in US-008).

## Health probes

The gateway exposes two endpoints regardless of `STRATA_AUTH_MODE`:

- `GET /healthz` — liveness. Always 200 once the HTTP listener is bound.
  Use as the Kubernetes `livenessProbe`.
- `GET /readyz` — readiness. Fans out probes to Cassandra (`SELECT now()
  FROM system.local`) and RADOS (stat a canary OID, `STRATA_RADOS_HEALTH_OID`,
  default `strata-readyz-canary`) with a 1 s timeout. Returns 200 only when
  every probe passes. Use as the Kubernetes `readinessProbe` and as the
  load-balancer health-check target.

Both bypass auth and the access-log middleware.

## Observability

Prometheus scrape: `:9000/metrics` on the gateway, `:9100/metrics` for
the gc worker, `:9101/metrics` for the lifecycle worker (when launched
as separate processes — the consolidated `strata server --workers=...`
shape exposes everything on the gateway port). Suggested scrape targets,
dashboards, and key metrics live under [Best Practices]({{< ref "/best-practices" >}}).

OTel tracing: set `OTEL_EXPORTER_OTLP_ENDPOINT` to enable OTLP/HTTP
export. The in-process ring buffer (`STRATA_OTEL_RINGBUF=on`, default)
lets the operator console browse spans without an external collector —
the per-layer architecture pages land in US-007 under
[Architecture]({{< ref "/architecture" >}}).

## Production checklist

When promoting a single-node deployment past "lab":

- [ ] `STRATA_AUTH_MODE=required` set; root credentials stored outside the env file.
- [ ] `STRATA_CONFIG_FILE` mounted read-only with the runtime TOML.
- [ ] `STRATA_LOG_LEVEL=INFO` (or `WARN` for noisy clusters); JSON log handler is the default.
- [ ] Prometheus scraping `/metrics`; alerts on `strata_worker_panic_total > 0`.
- [ ] OTel collector pointed at via `OTEL_EXPORTER_OTLP_ENDPOINT`; sample ratio tuned via `STRATA_OTEL_SAMPLE_RATIO`.
- [ ] External log shipper draining `stdout` (JSON) into the central store.
- [ ] `/healthz` + `/readyz` wired into systemd / supervisord / Kubernetes probes.
- [ ] Backups configured for the metadata backend (Cassandra snapshots, TiKV PITR) and the data backend (RADOS pool snapshots or upstream-S3 versioning).
- [ ] Smoke pass scheduled (`make smoke` or `make smoke-signed`) post-deploy.

## Cross-references

- [Get Started]({{< ref "/get-started" >}}) — 5-minute quick start with the same `make run-memory` flow.
- [Docker Compose]({{< ref "/deploy/docker-compose" >}}) — the bundled compose file and every supported profile.
- [Multi-replica cluster]({{< ref "/deploy/multi-replica" >}}) — when one box is no longer enough.
- [Architecture — Storage]({{< ref "/architecture/storage" >}}) — backend selection, object-table sharding, RADOS chunking.
- [Architecture — Backends]({{< ref "/architecture/backends" >}}) — per-backend pages (Cassandra, TiKV, S3-over-S3).
