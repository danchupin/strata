---
title: 'Get Started'
weight: 10
bookFlatSection: true
description: 'Bring up a local Strata gateway and put your first object in under 5 minutes.'
---

# Get Started

Three boot shapes are available, ordered by setup cost. Pick the one that
matches what you want to evaluate:

- **Option A — pure-memory.** No Docker, no external services. Best for
  poking at the S3 surface or CI smoke jobs.
- **Option B — Cassandra-backed metadata.** Real persistent metadata, in-memory
  data. Closest to production-shape without RADOS.
- **Option C — full stack.** Cassandra metadata + RADOS data via Ceph. Mirrors
  a single-node prod deployment.

## Prerequisites

| Tool | Version | Used by |
| ---- | ------- | ------- |
| Go   | 1.22+   | All options (`go build`) |
| Node + pnpm | any recent | `make build` (embedded Web UI) |
| Docker | 24+ | Options B, C |
| `aws` CLI | 2.x | All options (S3 client examples) |
| `mc` (MinIO client) | optional | Alternative S3 client |

A fresh checkout needs the Hugo theme submodule pulled (only required for
`make docs-serve`, not for the gateway):

```bash
git submodule update --init --recursive
```

## Option A — pure-memory (fastest)

Boots a single gateway with in-memory metadata + in-memory data. Nothing
persists across restarts. No Docker required.

```bash
make run-memory
```

The gateway listens on `:9999`. Default config is auth-off, so any S3 client
can talk to it as long as it does not insist on signing requests against a
real IAM identity. With `aws` CLI, pass `--no-sign-request`:

```bash
# In a second terminal:
export AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://127.0.0.1:9999 --no-sign-request s3 mb s3://test
echo "hello strata" > /tmp/foo.txt
aws --endpoint-url http://127.0.0.1:9999 --no-sign-request s3 cp /tmp/foo.txt s3://test/
aws --endpoint-url http://127.0.0.1:9999 --no-sign-request s3 ls s3://test/
```

Expected output of the final `s3 ls`:

```
2026-05-09 12:00:00         13 foo.txt
```

If you want to exercise SigV4 signing locally, swap `make run-memory` for the
signed equivalent and let the gateway pin a static credential pair:

```bash
STRATA_AUTH_MODE=required \
STRATA_STATIC_CREDENTIALS=adminAccessKey:adminSecretKey:owner \
  make run-memory
```

…then drop `--no-sign-request` and export the matching keys:

```bash
export AWS_ACCESS_KEY_ID=adminAccessKey
export AWS_SECRET_ACCESS_KEY=adminSecretKey
export AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://127.0.0.1:9999 s3 mb s3://test
```

## Option B — Cassandra-backed metadata

Boot Cassandra (in Docker) and run the gateway against it. Object data still
lives in memory; this exercises the real metadata layer end-to-end.

```bash
make up               # boots cassandra (docker compose)
make wait-cassandra   # blocks until cassandra reports healthy
make run-cassandra    # gateway on :9999, STRATA_WORKERS=gc,lifecycle
```

Same client commands as Option A. The default `make run-cassandra` recipe
runs auth-off, so the `--no-sign-request` form works:

```bash
aws --endpoint-url http://127.0.0.1:9999 --no-sign-request s3 mb s3://test
aws --endpoint-url http://127.0.0.1:9999 --no-sign-request s3 cp /tmp/foo.txt s3://test/
aws --endpoint-url http://127.0.0.1:9999 --no-sign-request s3 ls s3://test/
```

Tear the Cassandra container down with `make down`.

## Option C — full stack (Cassandra + Ceph + Strata)

Brings up cassandra, a single-node Ceph cluster, the strata container, and
the Prometheus + Grafana observability pair. Object data is persisted in
RADOS (4 MiB chunks).

```bash
make up-all           # cassandra + ceph + strata + prometheus + grafana
make wait-cassandra
make wait-ceph
```

Service map (host ports):

| Service | Host port | Notes |
| ------- | --------- | ----- |
| `strata`     | 9999 | S3 gateway (default credentials in `deploy/docker/.env.example`) |
| `cassandra`  | 9042 | Metadata |
| `ceph` (mon) | 6789 | RADOS data |
| `prometheus` | 9090 | Metrics |
| `grafana`    | 3000 | Dashboards (admin/admin on first login) |

Same client commands as Option A apply against `http://127.0.0.1:9999`. The
docker-compose stack defaults to auth-off; if `STRATA_AUTH_MODE` is exported
before `make up-all`, the gateway picks it up via the compose env passthrough
(`STRATA_STATIC_CREDENTIALS` similarly).

Tear it all down with `make down`.

## Smoke pass

A scripted PUT / GET / LIST / multipart round-trip lives in `scripts/smoke.sh`.
Drive it against any of the three options above:

```bash
make smoke          # unsigned PUT/GET against :9999
make smoke-signed   # SigV4-signed; needs STRATA_STATIC_CREDENTIALS set
```

Both target `http://127.0.0.1:9999` by default. `smoke-signed` wants the
gateway booted with `STRATA_AUTH_MODE=required` + `STRATA_STATIC_CREDENTIALS`
(see Option A above).

## Verifying readiness

The gateway exposes two health endpoints regardless of auth mode:

```bash
curl -fsS http://127.0.0.1:9999/healthz   # always 200 once bound
curl -fsS http://127.0.0.1:9999/readyz    # 200 only when meta+data probes pass
```

`/readyz` is the right gate for orchestrators (Kubernetes liveness/readiness
probe targets, load-balancer health checks).

## Where to next

- [Deploy → Multi-replica]({{< ref "/deploy/multi-replica" >}}) — wire 3
  replicas behind a load balancer with TiKV metadata + STRATA_GC_SHARDS
  fan-out.
- [Architecture]({{< ref "/architecture" >}}) — layer-by-layer deep dive
  into auth, router, meta-store, data-backend, workers.
- [S3 Compatibility]({{< ref "/s3-compatibility" >}}) — supported vs
  unsupported S3 surface, latest s3-tests pass-rate.
