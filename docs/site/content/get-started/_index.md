---
title: 'Get Started'
weight: 10
bookFlatSection: true
description: 'Install Strata, verify it is healthy, and put your first object — in under five minutes.'
---

# Get Started

This walkthrough takes a fresh checkout to a running gateway and a verified
PUT/GET round-trip. Pick **One-command dev** to spin up the canonical
multi-cluster lab, or **From source** to run a single in-memory gateway
without Docker.

> ⚠️ **Alpha software.** Strata is pre-launch; no production deploys yet.
> APIs and schemas may change without notice.

## Prerequisites

| You want to … | You need |
| --- | --- |
| Run the full lab (recommended) | Docker 24+ and `make` |
| Run a single in-memory gateway from source | Go 1.23+ and `make` |
| Drive S3 traffic | `aws` CLI 2.x (or any S3 client — `mc`, `s3cmd`) |

**macOS + Lima note.** If your Docker engine is Lima-backed, point Docker at
the Lima socket before running any compose target:

```bash
export DOCKER_HOST=unix:///Users/$USER/.lima/default/sock/docker.sock
```

## Install

Clone the repository and pick a path.

```bash
git clone https://github.com/danchupin/strata.git
cd strata
```

### One-command dev — full lab (Docker)

`make dev` brings up the canonical TiKV-default multi-cluster lab — two
gateway replicas behind an nginx load balancer, two Ceph clusters, a PD/TiKV
metadata cluster, plus Prometheus and Grafana. It blocks on health probes
and tails gateway logs.

```bash
make dev
```

When the log stream attaches, the gateway is ready on
`http://localhost:9999`. Ctrl-C kills only the log follow — the stack stays
up. Re-attach with `make dev-logs`. Tear everything down with
`make dev-down`.

### From source — single in-memory gateway

For poking at the S3 surface or running CI smoke jobs, the in-memory path
needs no Docker:

```bash
make run-memory
```

This binds a single gateway on `:9999` with in-memory metadata and data.
Nothing persists across restarts.

## Verify

The gateway exposes two health endpoints regardless of auth mode.

```bash
curl -fsS http://localhost:9999/healthz   # 200 once the process is bound
curl -fsS http://localhost:9999/readyz    # 200 when metadata + data probes pass
```

`/readyz` is the right gate for orchestrators (Kubernetes probes, load
balancers).

Open the operator console in a browser:

```
http://localhost:9999/console/
```

The trailing slash matters. The console surfaces cluster state, drain
progress, bucket lists, and admin actions.

## First bucket, first object

`make dev` and `make run-memory` both default to auth-off, so any S3 client
can talk to the gateway as long as it does not insist on signing. With
`aws` CLI, pass `--no-sign-request`:

```bash
export AWS_DEFAULT_REGION=us-east-1

aws --endpoint-url http://localhost:9999 --no-sign-request \
  s3 mb s3://demo

echo "hello strata" > /tmp/hello.txt

aws --endpoint-url http://localhost:9999 --no-sign-request \
  s3 cp /tmp/hello.txt s3://demo/

aws --endpoint-url http://localhost:9999 --no-sign-request \
  s3 ls s3://demo/
```

Expected output of the final `s3 ls`:

```
2026-05-20 12:00:00         13 hello.txt
```

Read it back:

```bash
aws --endpoint-url http://localhost:9999 --no-sign-request \
  s3 cp s3://demo/hello.txt -
```

To exercise SigV4 signing, swap to the signed shape — pin a static
credential pair on the gateway and drop `--no-sign-request`:

```bash
STRATA_AUTH_MODE=required \
STRATA_STATIC_CREDENTIALS=adminAccessKey:adminSecretKey:owner \
  make run-memory
```

```bash
export AWS_ACCESS_KEY_ID=adminAccessKey
export AWS_SECRET_ACCESS_KEY=adminSecretKey
export AWS_DEFAULT_REGION=us-east-1

aws --endpoint-url http://localhost:9999 s3 mb s3://demo
```

A scripted PUT/GET/LIST/multipart round-trip lives in `scripts/smoke.sh`:

```bash
make smoke          # unsigned
make smoke-signed   # SigV4; needs STRATA_STATIC_CREDENTIALS set
```

## What's next

- [Concepts]({{< ref "/concepts" >}}) — what Strata is, the S3 surface it
  speaks, how multi-cluster routing and drain work, what the workers do.
- [Deploy]({{< ref "/deploy" >}}) — Docker Compose, Kubernetes, multi-replica
  and single-node prod shapes.
- [Operate](/operate/) — day-2 workflows: drain a cluster, monitor, scale,
  back up.
