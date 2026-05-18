---
title: 'Docker Compose'
weight: 15
description: 'Operator-readable map of the bundled docker-compose.yml: services, ports, env, volumes, profiles.'
---

# Docker Compose deployment

The bundled `deploy/docker/docker-compose.yml` is the canonical reference
shape for a Strata stack on a single host. Post-`ralph/tikv-default-lab`,
bare `docker compose up -d` brings up the **TiKV-default 2-replica lab**
(PD + TiKV + ceph + ceph-b + strata-a + strata-b + nginx LB +
prometheus + grafana). The Cassandra-backed regression lab lives under
`--profile cassandra` so an operator can `make up-cassandra` to validate
the Cassandra meta backend side-by-side.

This page distills the compose file for human readers. The compose file
itself is the source of truth — when it changes, this page lags by one
PR; cross-check container env / port assignments before depending on
them.

## Service map

| Service | Image | Profile | Host port | Container port | Role |
|---|---|---|---|---|---|
| `pd` | `pingcap/pd:v8.5.0` | default | 2379 | 2379 | TiKV placement driver (single-node, lab only). |
| `tikv` | `pingcap/tikv:v8.5.0` | default | 20160 | 20160 | TiKV storage node (single-node, lab only). |
| `ceph` | `strata-ceph:local` (built locally) | default | — | — | RADOS data backend `default`. |
| `ceph-b` | `strata-ceph:local` (built locally) | default | — | — | Second RADOS cluster `cephb` — always-on so multi-cluster behaviour (per-bucket placement, rebalance, drain) is exercisable out of the box. |
| `strata-a` | `strata:ceph` (built locally) | default | 10001 | 9000 | First TiKV-backed replica. Mounts both `/etc/ceph-a` + `/etc/ceph-b`; `STRATA_RADOS_CLUSTERS=default:...,cephb:...`. Default workers `gc,lifecycle,rebalance`. Override `STRATA_RADOS_CLUSTERS` at runtime for a single-cluster smoke. Feature workers (notify / replicator / access-log / inventory / audit-export) opt-in via `STRATA_WORKERS`. |
| `strata-b` | `strata:ceph` (built locally) | default | 10002 | 9000 | Second TiKV-backed replica. Mirrors strata-a (multi-cluster mounts, same env shape) with `STRATA_NODE_ID=strata-b`. Shares the `strata-jwt-shared` volume with strata-a so a session JWT issued by either replica validates on the other. |
| `strata-lb-nginx` | `nginx:1.27-alpine` | default | 9999 | 80 | nginx LB fronting strata-a + strata-b (round-robin, streaming-friendly). |
| `cassandra` | `cassandra:5.0` | `cassandra` | 9042 | 9042 | Cassandra metadata backend for the regression lab. Profile-gated so the bare default doesn't auto-start it. |
| `strata-cassandra` | `strata:ceph` (built locally) | `cassandra` | 9998 | 9000 | Cassandra-backed gateway (single replica). Mounts both RADOS clusters; `STRATA_META_BACKEND=cassandra`. |
| `webhook-trap` | `mendhak/http-https-echo:34` | `webhook-trap` | — | 8080 | JSON-echo target for notify worker e2e. |
| `prometheus` | `prom/prometheus:v2.54.1` | default | 9090 | 9090 | Scrapes gateway + worker metrics. |
| `grafana` | `grafana/grafana:11.2.0` | default | 3000 | 3000 | Dashboard provisioned from `deploy/grafana/`. |
| `otel-collector` | `otel/opentelemetry-collector-contrib:0.110.0` | `tracing` | 4317, 4318 | 4317, 4318 | OTLP collector — fans incoming spans to Jaeger. |
| `jaeger` | `jaegertracing/all-in-one:1.62` | `tracing` | 16686 (UI), 14250 | 16686, 14250 | All-in-one Jaeger backend + UI. |

The default `docker compose up -d` brings up nine services: `pd + tikv +
ceph + ceph-b + strata-a + strata-b + strata-lb-nginx + prometheus +
grafana`. Multi-cluster + 2-replica is the canonical shape; override
`STRATA_RADOS_CLUSTERS` at runtime for a single-cluster smoke. Every
profile-gated service stays silent until requested.

## Profiles

Compose profiles segregate mutually-exclusive shapes so the default `up`
keeps the smallest reference stack.

| Profile | What it adds | Bring-up |
|---|---|---|
| (default) | pd + tikv + ceph + ceph-b + strata-a + strata-b + strata-lb-nginx + prometheus + grafana | `make up` / `make up-all` (≡ `docker compose up -d` + `wait-tikv` + `wait-ceph` + `wait-strata-lab`). |
| `cassandra` | cassandra + strata-cassandra (Cassandra-backed regression lab on host port 9998) | `make up-cassandra` (≡ `docker compose --profile cassandra up -d` + `wait-cassandra` + `wait-ceph`). Layers on top of the bare default. |
| `webhook-trap` | webhook-trap (JSON echo for notify worker e2e) | `docker compose --profile webhook-trap up -d webhook-trap`. |
| `tracing` | OTel collector + Jaeger | `docker compose --profile tracing up -d otel-collector jaeger`. Set `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318` on the gateway. |

Profiles compose: `docker compose --profile cassandra --profile tracing
up -d` brings up the bare default + Cassandra-backed gateway + OTel
stack in one shot.

## Env vars

The runtime config is a TOML file mounted at `/etc/strata/strata.toml`
(see `deploy/strata.toml`). Env vars override the file — useful for CI
and local experimentation. Notable knobs:

| Env | Default in compose | Purpose |
|---|---|---|
| `STRATA_CONFIG_FILE` | `/etc/strata/strata.toml` | Path to the TOML config. |
| `STRATA_WORKERS` | `gc,lifecycle,rebalance` (strata-a / -b / -cassandra) | Comma-separated worker names. Unknown names exit 2 immediately. |
| `STRATA_AUTH_MODE` | `optional` (strata-a / -b / -cassandra) | `off`, `optional`, `required`. |
| `STRATA_STATIC_CREDENTIALS` | `admin:adminpass:owner` (strata-a / -b / -cassandra) | `<access>:<secret>:<role>` triples (comma-separated for multiple). |
| `STRATA_META_BACKEND` | `tikv` (strata-a / -b), `cassandra` (strata-cassandra) | Picks the metadata backend at startup. |
| `STRATA_TIKV_PD_ENDPOINTS` | `pd:2379` (strata-a / -b) | TiKV PD endpoints (comma-separated). |
| `STRATA_RADOS_CLUSTERS` | `default:/etc/ceph-a/...,cephb:/etc/ceph-b/...` | Multi-cluster RADOS bindings. Override at runtime for single-cluster smoke. |
| `STRATA_NODE_ID` | `strata-a` / `strata-b` / `strata-cassandra` | Identifies replica in heartbeat / leader-election rows. |
| `STRATA_GC_INTERVAL` / `STRATA_GC_GRACE` | (unset → defaults 30s / 5m) | GC worker cadence. |
| `STRATA_GC_SHARDS` | (unset → 1) | Phase-2 GC fan-out shard count. Set to N when running N replicas. |
| `STRATA_GC_CONCURRENCY` | (unset → cpu) | Per-shard GC parallelism. |
| `STRATA_REBALANCE_INTERVAL` / `STRATA_REBALANCE_RATE_MB_S` | (unset → gateway defaults 1h / 100 MB/s) | Rebalance worker cadence + rate limit. Pre-cycle Cassandra-backed `strata` shipped explicit compose defaults of 30s / 100 — set `STRATA_REBALANCE_INTERVAL=30s` env on `make up` to restore that cadence. |
| `STRATA_LIFECYCLE_INTERVAL` / `STRATA_LIFECYCLE_UNIT` | (unset → 30s, day) | Lifecycle worker cadence + unit. |
| `STRATA_NOTIFY_TARGETS` | (unset) | Comma-separated target URLs for the notify worker. |
| `STRATA_PROMETHEUS_URL` | `http://prometheus:9090` | Where the embedded console queries metrics. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (unset) | OTLP/HTTP endpoint. Empty + `STRATA_OTEL_RINGBUF=off` installs a noop tracer. |
| `STRATA_OTEL_SAMPLE_RATIO` | (unset → 0.01) | Tail-sampling head ratio; failing spans always export. |
| `STRATA_OTEL_RINGBUF` / `STRATA_OTEL_RINGBUF_BYTES` | `on` / 4 MiB | In-process span ring buffer for the operator console. |

Full env-knob reference lands in the Reference section expansion (P3
follow-up — see ROADMAP).

## Volumes + mounts

The compose file uses named volumes for stateful storage so `docker
compose down` doesn't lose data, and bind-mounts for config so edits
take effect on next `up`.

| Volume / mount | Used by | Purpose |
|---|---|---|
| `strata-cassandra-data` | cassandra | `/var/lib/cassandra` — keyspace + commitlog. |
| `strata-ceph-etc` | ceph (rw), strata-a / -b / -cassandra (ro) | `/etc/ceph-a` on strata replicas — `ceph.conf` + admin keyring for the `default` RADOS cluster. |
| `strata-cephb-etc` | ceph-b (rw), strata-a / -b / -cassandra (ro) | `/etc/ceph-b` on strata replicas — second RADOS cluster `cephb`. |
| `strata-ceph-data` | ceph | `/var/lib/ceph` — RADOS object data. |
| `strata-cephb-data` | ceph-b | `/var/lib/ceph` — second cluster's object data. |
| `strata-pd-data` | pd | `/data` — PD raft log + region metadata. |
| `strata-tikv-data` | tikv | `/data` — TiKV RocksDB. |
| `strata-prometheus-data` | prometheus | `/prometheus` — TSDB. |
| `strata-grafana-data` | grafana | `/var/lib/grafana` — dashboards + sessions. |
| `strata-jwt-shared` | strata-a / strata-b | `/etc/strata/jwt-shared` — shared JWT secret bootstrap (file-based, O_EXCL). Round-robin LB requires a session JWT issued by replica A to validate on replica B. See [Multi-replica cluster]({{< ref "/deploy/multi-replica" >}}). |
| `../strata.toml` (host) | every strata service | Read-only TOML config. |
| `../prometheus/prometheus.yml` (host) | prometheus | Scrape config. |
| `../grafana/datasource.yaml` + `dashboard.yaml` + `strata-dashboard.json` (host) | grafana | Provisioned data source + dashboard. |
| `../nginx/strata-lab.conf` (host) | strata-lb-nginx | LB config (round-robin upstream strata-a + strata-b, streaming-friendly). |
| `../otel/collector-config.yaml` (host) | otel-collector | OTLP collector config (fan-out to Jaeger). |

Tear down + drop state: `make down` (removes containers across every
known profile; named volumes persist). `docker compose -f
deploy/docker/docker-compose.yml down -v` nukes the volumes too.

## Production checklist

For any compose-managed deployment that survives past the lab phase:

- [ ] Pin every image tag to a SHA, not the `latest` / `vN.Y.Z` mutable form.
- [ ] Replace `STRATA_AUTH_MODE=optional` with `required`; rotate `STRATA_STATIC_CREDENTIALS` out of the host env into a secret store + `STRATA_CONFIG_FILE`.
- [ ] Move `cassandra` / `pd` / `tikv` / `ceph` to dedicated hosts — bundled-on-the-same-box is for labs only. PD ≥3 + TiKV ≥3 for raft majority in production.
- [ ] Configure backups for every data-bearing volume (`cassandra`, `pd`, `tikv`, `ceph`).
- [ ] Wire Prometheus alertmanager + log shipping (the bundled compose ships scrape + dashboard, not alerts).
- [ ] Front the gateway with a real LB if you run the bare-default 2-replica shape outside the bench environment (the bundled nginx is unauthenticated + no TLS).
- [ ] Set `STRATA_OTEL_RINGBUF_BYTES` to a value sized for expected traffic (default 4 MiB ≈ a few thousand spans). Wire OTLP collector for retention beyond the ring.

## Cross-references

- [Single-node deployment]({{< ref "/deploy/single-node" >}}) — when you don't need the full compose stack.
- [Multi-replica cluster]({{< ref "/deploy/multi-replica" >}}) — operator guide for the bare-default 2-replica TiKV lab.
- [Architecture — Storage]({{< ref "/architecture/storage" >}}) — backend selection + chunking.
- [Architecture — Backends — TiKV]({{< ref "/architecture/backends/tikv" >}}) — why TiKV is the lab default.
- [Architecture — Migrations — TiKV-default lab]({{< ref "/architecture/migrations/tikv-default-lab" >}}) — what changed when the lab default flipped from Cassandra to TiKV.
