---
title: 'Docker Compose'
weight: 15
description: 'Bundled docker-compose.yml: services, ports, env, volumes, profiles — the reference lab stack on a single host.'
---

# Docker Compose deployment

The bundled `deploy/docker/docker-compose.yml` is the canonical
reference shape for a Strata stack on one host. Bare
`docker compose up -d` brings up the **TiKV-default 2-replica lab**
(PD + TiKV + ceph + ceph-b + strata-a + strata-b + nginx LB +
prometheus + grafana). The Cassandra-backed regression lab lives
under `--profile cassandra` so an operator can `make up-cassandra` to
validate the Cassandra meta backend side by side.

The compose file itself is the source of truth. When it changes, this
page may lag by one PR; cross-check ports + env before depending on
them.

## Prerequisites

- Docker Engine ≥24 (or Docker Desktop ≥4.20). macOS via Lima also works
  (`DOCKER_HOST=unix:///Users/.../.lima/.../sock/docker.sock`).
- ≥4 vCPU + 8 GiB free RSS for the full default stack (PD + TiKV +
  two RADOS clusters + two gateway replicas + Prometheus + Grafana).
- `make` (and the [Makefile](https://github.com/danchupin/strata/blob/main/Makefile)
  targets the compose file from the repo root).

## Install

The repo's Makefile wraps the canonical bring-up flow:

```bash
make up        # docker compose up -d (TiKV-default lab)
make wait-tikv && make wait-ceph && make wait-strata-lab
```

`make dev` runs the same sequence plus a tail of gateway logs — that
is the convenience target documented in [Get Started]({{< ref "/get-started" >}}).

For the Cassandra-backed regression lab:

```bash
make up-cassandra        # docker compose --profile cassandra up -d
make wait-cassandra && make wait-ceph
```

For tracing (OTel collector + Jaeger UI on :16686):

```bash
docker compose --profile tracing up -d otel-collector jaeger
# then set OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318 on the gateway
```

Profiles compose: `docker compose --profile cassandra --profile tracing up -d`
brings up the bare default + Cassandra-backed gateway + OTel stack in
one shot.

## Configure

### Service map

| Service | Image | Profile | Host port | Container port | Role |
|---|---|---|---|---|---|
| `pd` | `pingcap/pd:v8.5.0` | default | 2379 | 2379 | TiKV placement driver (single-node, lab only). |
| `tikv` | `pingcap/tikv:v8.5.0` | default | 20160 | 20160 | TiKV storage node (single-node, lab only). |
| `ceph` | `strata-ceph:local` | default | — | — | RADOS data backend `default`. |
| `ceph-b` | `strata-ceph:local` | default | — | — | Second RADOS cluster `cephb` — always on so multi-cluster behaviour is exercisable out of the box. |
| `strata-a` | `strata:ceph` | default | 10001 | 9000 | First TiKV-backed replica. Mounts `/etc/ceph-a` + `/etc/ceph-b`. Default workers `gc,lifecycle,rebalance`. |
| `strata-b` | `strata:ceph` | default | 10002 | 9000 | Second TiKV-backed replica. Mirrors `strata-a`; shares the `strata-jwt-shared` volume so session JWTs validate across replicas. |
| `strata-lb-nginx` | `nginx:1.27-alpine` | default | 9999 | 80 | LB fronting `strata-a` + `strata-b` (round-robin, streaming-friendly). |
| `cassandra` | `cassandra:5.0` | `cassandra` | 9042 | 9042 | Cassandra metadata backend (regression lab). |
| `strata-cassandra` | `strata:ceph` | `cassandra` | 9998 | 9000 | Cassandra-backed gateway. Mounts both RADOS clusters. |
| `webhook-trap` | `mendhak/http-https-echo:34` | `webhook-trap` | — | 8080 | JSON echo target for the notify worker. |
| `prometheus` | `prom/prometheus:v2.54.1` | default | 9090 | 9090 | Scrapes gateway + worker metrics. |
| `grafana` | `grafana/grafana:11.2.0` | default | 3000 | 3000 | Dashboard provisioned from `deploy/grafana/`. |
| `otel-collector` | `otel/opentelemetry-collector-contrib:0.110.0` | `tracing` | 4317, 4318 | 4317, 4318 | OTLP collector — fans incoming spans to Jaeger. |
| `jaeger` | `jaegertracing/all-in-one:1.62` | `tracing` | 16686, 14250 | 16686, 14250 | All-in-one Jaeger backend + UI. |

The default `docker compose up -d` brings up nine services
(pd + tikv + ceph + ceph-b + strata-a + strata-b + strata-lb-nginx +
prometheus + grafana). Every profile-gated service stays silent until
requested.

### Profiles

| Profile | What it adds | Bring-up |
|---|---|---|
| (default) | TiKV-default 2-replica lab + Prometheus + Grafana | `make up` / `make up-all`. |
| `cassandra` | Cassandra + `strata-cassandra` (host `:9998`) | `make up-cassandra`. |
| `webhook-trap` | JSON-echo target for the notify worker | `docker compose --profile webhook-trap up -d webhook-trap`. |
| `tracing` | OTel collector + Jaeger | `docker compose --profile tracing up -d otel-collector jaeger`. |

### Env vars (top knobs)

The runtime config is a TOML file mounted at `/etc/strata/strata.toml`
(see `deploy/strata.toml`). Env vars override the file. Full table
at [Reference — environment variables]({{< ref "/reference/env-vars" >}}).

| Variable | Default in compose | Purpose |
|---|---|---|
| `STRATA_CONFIG_FILE` | `/etc/strata/strata.toml` | Path to the TOML config. |
| `STRATA_WORKERS` | `gc,lifecycle,rebalance` | Comma-separated worker names. |
| `STRATA_AUTH_MODE` | `optional` | `off`, `optional`, `required`. |
| `STRATA_STATIC_CREDENTIALS` | `admin:adminpass:owner` | `<access>:<secret>:<role>` triples. |
| `STRATA_META_BACKEND` | `tikv` (default) / `cassandra` (profile) | Picks the metadata backend at startup. |
| `STRATA_TIKV_PD_ENDPOINTS` | `pd:2379` | TiKV PD endpoints (comma-separated). |
| `STRATA_RADOS_CLUSTERS` | `default:...,cephb:...` | Multi-cluster RADOS bindings. |
| `STRATA_NODE_ID` | per replica | Identifies replica in heartbeat + leader-election rows. |
| `STRATA_GC_SHARDS` | unset → 1 | Phase-2 GC fan-out shard count. Set to N when running N replicas. |
| `STRATA_NOTIFY_TARGETS` | unset | Comma-separated target URLs for the notify worker. |
| `STRATA_PROMETHEUS_URL` | `http://prometheus:9090` | Where the embedded console queries metrics. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset | OTLP/HTTP endpoint. |

### Volumes + mounts

The compose file uses named volumes for state so `docker compose down`
doesn't lose data, and bind-mounts for config so edits take effect on
next `up`.

| Volume / mount | Used by | Purpose |
|---|---|---|
| `strata-cassandra-data` | cassandra | `/var/lib/cassandra`. |
| `strata-ceph-etc` | ceph (rw), strata replicas (ro) | `/etc/ceph-a` — config + keyring for cluster `default`. |
| `strata-cephb-etc` | ceph-b (rw), strata replicas (ro) | `/etc/ceph-b` — config + keyring for cluster `cephb`. |
| `strata-ceph-data` | ceph | `/var/lib/ceph` — RADOS object data. |
| `strata-cephb-data` | ceph-b | `/var/lib/ceph` — second cluster's object data. |
| `strata-pd-data` | pd | `/data` — PD raft log + region metadata. |
| `strata-tikv-data` | tikv | `/data` — TiKV RocksDB. |
| `strata-prometheus-data` | prometheus | `/prometheus` — TSDB. |
| `strata-grafana-data` | grafana | `/var/lib/grafana` — dashboards + sessions. |
| `strata-jwt-shared` | strata-a / strata-b | `/etc/strata/jwt-shared` — shared JWT secret bootstrap (file-based, O_EXCL). |
| `../strata.toml` (host) | every strata service | Read-only TOML config. |
| `../prometheus/prometheus.yml` (host) | prometheus | Scrape config. |
| `../grafana/...` (host) | grafana | Provisioned data source + dashboard. |
| `../nginx/strata-lab.conf` (host) | strata-lb-nginx | LB config (round-robin upstream). |
| `../otel/collector-config.yaml` (host) | otel-collector | OTLP collector config. |

Tear down + drop state: `make down` (removes containers across every
known profile; named volumes persist).
`docker compose -f deploy/docker/docker-compose.yml down -v` nukes the
volumes too.

## Verify

```bash
curl http://127.0.0.1:9999/healthz   # nginx LB → strata-a or strata-b
curl http://127.0.0.1:9999/readyz    # both replicas + RADOS + TiKV
curl http://127.0.0.1:10001/readyz   # strata-a direct
curl http://127.0.0.1:10002/readyz   # strata-b direct
aws --endpoint-url http://127.0.0.1:9999 --no-sign-request s3 ls
```

The operator console: `http://127.0.0.1:9999/console/` (trailing
slash). Prometheus UI: `:9090`. Grafana: `:3000`
(`admin`/`adminpass`). Jaeger (tracing profile): `:16686`.

## Monitor

- **Gateway metrics:** Prometheus auto-scrapes `strata-a:9000/metrics`
  and `strata-b:9000/metrics`. The provisioned Grafana dashboard
  surfaces request rate, GC backlog, replication lag, worker panic
  counters. See [Best Practices — monitoring](/operate/monitoring/).
- **Traces:** the tracing profile spins an OTel collector + Jaeger
  all-in-one. Set `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318`
  on the gateway containers and inspect spans in the Jaeger UI.
- **Logs:** JSON to `stdout`; `docker compose logs -f strata-a strata-b`
  for live tail. Each line carries `request_id` + `node_id`.

## Troubleshoot

- **`make up` hangs on `wait-tikv`.** PD took longer than the timeout
  to elect a leader. `docker compose logs pd` should show `PD leader`
  within ~30 s; if not, restart PD.
- **`wait-strata-lab` fails.** Either RADOS isn't ready (run
  `make wait-ceph`) or the gateway is crash-looping (check
  `docker compose logs strata-a`).
- **aws-cli against `:9999` returns 404 on the wrong bucket.** Path-style
  URLs route by `/<bucket>/<key>`. For virtual-hosted-style, set
  `STRATA_VHOST_PATTERN=*.s3.local` and use
  `--endpoint-url http://bucket.s3.local:9999`.
- **JWT-bootstrap clash:** the `strata-jwt-shared` volume is shared
  between `strata-a` + `strata-b`. If you start `strata-cassandra` on
  top, it joins the same volume — session JWTs validate across all
  three replicas.
- **Cassandra profile won't reach `strata-cassandra:9998`.** Either
  Cassandra isn't ready (`make wait-cassandra`) or you forgot the
  `--profile cassandra` flag on `docker compose up`.

## Production checklist

For any compose-managed deployment that survives past the lab phase:

- [ ] Pin every image tag to a SHA, not the `latest` / `vN.Y.Z` mutable form.
- [ ] Replace `STRATA_AUTH_MODE=optional` with `required`; rotate `STRATA_STATIC_CREDENTIALS` out of the host env into a secret store + `STRATA_CONFIG_FILE`.
- [ ] Move `cassandra` / `pd` / `tikv` / `ceph` to dedicated hosts — bundled-on-the-same-box is for labs only. PD ≥3 + TiKV ≥3 for raft majority in production.
- [ ] Configure backups for every data-bearing volume — see [Operate — backup & restore](/operate/backup-restore/).
- [ ] Wire Prometheus alertmanager + log shipping (the bundled compose ships scrape + dashboard, not alerts).
- [ ] Front the gateway with a real LB if you run the bare-default 2-replica shape outside the lab (the bundled nginx is unauthenticated + no TLS).
- [ ] Size `STRATA_OTEL_RINGBUF_BYTES` for expected traffic (default 4 MiB ≈ a few thousand spans). Wire OTLP collector for retention beyond the ring.

## Cross-references

- [Single-node deployment]({{< ref "/deploy/single-node" >}}) — when you don't need the full compose stack.
- [Multi-replica cluster]({{< ref "/deploy/multi-replica" >}}) — operator guide for the bare-default 2-replica TiKV lab.
- [Operate](/operate/) — day-2 workflows (drain, scale, back up).
- [Reference — environment variables]({{< ref "/reference/env-vars" >}}) — full env knob table.
- [Architecture — Storage]({{< ref "/architecture/storage" >}}) — backend selection + chunking.
- [Architecture — Backends — TiKV]({{< ref "/architecture/backends/tikv" >}}) — why TiKV is the lab default.
- [Architecture — Migrations — TiKV-default lab]({{< ref "/architecture/migrations/tikv-default-lab" >}}) — what changed when the lab default flipped.
