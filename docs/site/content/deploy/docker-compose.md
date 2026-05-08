---
title: 'Docker Compose'
weight: 15
description: 'Operator-readable map of the bundled docker-compose.yml: services, ports, env, volumes, profiles.'
---

# Docker Compose deployment

The bundled `deploy/docker/docker-compose.yml` is the canonical reference
shape for a Strata stack on a single host. It ships every supported
runtime profile so an operator can `docker compose --profile <name> up -d`
to switch between Cassandra-backed, TiKV-backed, multi-replica, and
tracing variants without editing YAML.

This page distills the compose file for human readers. The compose file
itself is the source of truth — when it changes, this page lags by one
PR; cross-check container env / port assignments before depending on
them.

## Service map

| Service | Image | Profile | Host port | Container port | Role |
|---|---|---|---|---|---|
| `cassandra` | `cassandra:5.0` | default | 9042 | 9042 | Metadata backend (default). |
| `ceph` | `strata-ceph:local` (built locally) | default | — | — | RADOS data backend. Exposes mon / osd internally. |
| `strata` | `strata:ceph` (built locally) | default | 9999 | 9000 | Cassandra-backed gateway. `STRATA_WORKERS=gc,lifecycle` by default. |
| `strata-features` | `strata:ceph` | `features` | — | — | Second replica running `notify,replicator,access-log,inventory,audit-export`. |
| `webhook-trap` | `mendhak/http-https-echo:34` | `features` | — | 8080 | JSON-echo target for notify worker e2e. |
| `prometheus` | `prom/prometheus:v2.54.1` | default | 9090 | 9090 | Scrapes gateway + worker metrics. |
| `grafana` | `grafana/grafana:11.2.0` | default | 3000 | 3000 | Dashboard provisioned from `deploy/grafana/`. |
| `pd` | `pingcap/pd:v8.5.0` | `tikv`, `lab-tikv` | 2379 | 2379 | TiKV placement driver (single-node, lab only). |
| `tikv` | `pingcap/tikv:v8.5.0` | `tikv`, `lab-tikv` | 20160 | 20160 | TiKV storage node (single-node, lab only). |
| `strata-tikv` | `strata:ceph` | `tikv` | 9998 | 9000 | TiKV-backed gateway replica. Mutually exclusive with `strata` (different host port; both can run side-by-side for comparison). |
| `strata-tikv-a` | `strata:ceph` | `lab-tikv` | 9001 | 9000 | First TiKV-backed replica behind nginx LB. |
| `strata-tikv-b` | `strata:ceph` | `lab-tikv` | 9002 | 9000 | Second TiKV-backed replica behind nginx LB. |
| `strata-tikv-c` | `strata:ceph` | `lab-tikv-3` | 9003 | 9000 | Third replica for the Phase-2 GC fan-out bench. Layered atop `lab-tikv`. |
| `strata-lb-nginx` | `nginx:1.27-alpine` | `lab-tikv` | 9999 | 80 | LB fronting both lab-tikv replicas. |
| `otel-collector` | `otel/opentelemetry-collector-contrib:0.110.0` | `tracing` | 4317, 4318 | 4317, 4318 | OTLP collector — fans incoming spans to Jaeger. |
| `jaeger` | `jaegertracing/all-in-one:1.62` | `tracing` | 16686 (UI), 14250 | 16686, 14250 | All-in-one Jaeger backend + UI. |

The default `docker compose up` brings `cassandra + ceph + strata +
prometheus + grafana`. Every other service is profile-gated and stays
silent until requested.

## Profiles

Compose profiles segregate mutually-exclusive shapes so the default `up`
keeps the smallest reference stack.

| Profile | What it adds | Bring-up |
|---|---|---|
| (default) | cassandra + ceph + cassandra-backed strata + prometheus + grafana | `make up-all` (≡ `docker compose up -d` for the default services + `wait-cassandra` + `wait-ceph`). |
| `tikv` | pd + tikv + tikv-backed strata replica (host port 9998) | `make up-tikv` (≡ `docker compose --profile tikv up -d pd tikv ceph strata-tikv prometheus grafana`). |
| `lab-tikv` | pd + tikv + 2 TiKV-backed replicas (a, b) + nginx LB on 9999 | `make up-lab-tikv` (≡ `docker compose --profile lab-tikv up -d ...`). |
| `lab-tikv-3` | adds the third replica (`strata-tikv-c`) atop `lab-tikv` | `docker compose --profile lab-tikv --profile lab-tikv-3 up -d`. Pair with `STRATA_GC_SHARDS=3` host env. |
| `features` | `strata-features` (notify/replicator/access-log/inventory/audit-export) + `webhook-trap` | `docker compose --profile features up -d strata-features webhook-trap`. |
| `tracing` | OTel collector + Jaeger | `docker compose --profile tracing up -d otel-collector jaeger`. Set `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318` on the gateway. |

Profiles compose: `docker compose --profile lab-tikv --profile tracing
up -d` brings both shapes.

## Env vars

The runtime config is a TOML file mounted at `/etc/strata/strata.toml`
(see `deploy/strata.toml`). Env vars override the file — useful for CI
and local experimentation. Notable knobs:

| Env | Default in compose | Purpose |
|---|---|---|
| `STRATA_CONFIG_FILE` | `/etc/strata/strata.toml` | Path to the TOML config. |
| `STRATA_WORKERS` | `gc,lifecycle` | Comma-separated worker names. Unknown names exit 2 immediately. |
| `STRATA_AUTH_MODE` | (unset → `off` for default profile, `optional` for `lab-tikv`) | `off`, `optional`, `required`. |
| `STRATA_STATIC_CREDENTIALS` | (unset; `admin:adminpass:owner` for `lab-tikv`) | `<access>:<secret>:<role>` triples (comma-separated for multiple). |
| `STRATA_META_BACKEND` | `cassandra` (or `tikv` for tikv-profile services) | Picks the metadata backend at startup. |
| `STRATA_TIKV_PD_ENDPOINTS` | `pd:2379` | TiKV PD endpoints (comma-separated). |
| `STRATA_NODE_ID` | (unset; `strata-a` / `-b` / `-c` for lab-tikv) | Identifies replica in heartbeat / leader-election rows. |
| `STRATA_GC_INTERVAL` / `STRATA_GC_GRACE` | (unset → defaults 30s / 5m) | GC worker cadence. |
| `STRATA_GC_SHARDS` | (unset → 1) | Phase-2 GC fan-out shard count. Set to N when running N replicas. |
| `STRATA_GC_CONCURRENCY` | (unset → cpu) | Per-shard GC parallelism. |
| `STRATA_GC_DUAL_WRITE` | (unset) | Phase-1 → Phase-2 cutover toggle. |
| `STRATA_LIFECYCLE_INTERVAL` / `STRATA_LIFECYCLE_UNIT` | (unset → 30s, day) | Lifecycle worker cadence + unit. |
| `STRATA_NOTIFY_TARGETS` | (unset) | Comma-separated target URLs for the notify worker. |
| `STRATA_PROMETHEUS_URL` | `http://prometheus:9090` | Where the embedded console queries metrics. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (unset) | OTLP/HTTP endpoint. Empty + `STRATA_OTEL_RINGBUF=off` installs a noop tracer. |
| `STRATA_OTEL_SAMPLE_RATIO` | (unset → 0.01) | Tail-sampling head ratio; failing spans always export. |
| `STRATA_OTEL_RINGBUF` / `STRATA_OTEL_RINGBUF_BYTES` | `on` / 4 MiB | In-process span ring buffer for the operator console. |

Full env-knob reference lands in US-008 / the Reference section
expansion (P3 follow-up after this cycle).

## Volumes + mounts

The compose file uses named volumes for stateful storage so `docker
compose down` doesn't lose data, and bind-mounts for config so edits
take effect on next `up`.

| Volume / mount | Used by | Purpose |
|---|---|---|
| `strata-cassandra-data` | cassandra | `/var/lib/cassandra` — keyspace + commitlog. |
| `strata-ceph-etc` | ceph (rw), strata (ro) | `/etc/ceph` — `ceph.conf` + admin keyring. Strata mounts read-only. |
| `strata-ceph-data` | ceph | `/var/lib/ceph` — RADOS object data. |
| `strata-pd-data` | pd | `/data` — PD raft log + region metadata. |
| `strata-tikv-data` | tikv | `/data` — TiKV RocksDB. |
| `strata-prometheus-data` | prometheus | `/prometheus` — TSDB. |
| `strata-grafana-data` | grafana | `/var/lib/grafana` — dashboards + sessions. |
| `strata-jwt-shared` | strata-tikv-a / b / c | `/etc/strata/jwt-shared` — shared JWT secret bootstrap (file-based, O_EXCL). See [Multi-replica cluster]({{< ref "/deploy/multi-replica" >}}). |
| `../strata.toml` (host) | every strata service | Read-only TOML config. |
| `../prometheus/prometheus.yml` (host) | prometheus | Scrape config. |
| `../grafana/datasource.yaml` + `dashboard.yaml` + `strata-dashboard.json` (host) | grafana | Provisioned data source + dashboard. |
| `../nginx/strata-lab.conf` (host) | strata-lb-nginx | LB config (least-conn, streaming-friendly). |
| `../otel/collector-config.yaml` (host) | otel-collector | OTLP collector config (fan-out to Jaeger). |

Tear down + drop state: `make down` (removes containers; named volumes
persist). `docker compose -f deploy/docker/docker-compose.yml down -v`
nukes the volumes too.

## Production checklist

For any compose-managed deployment that survives past the lab phase:

- [ ] Pin every image tag to a SHA, not the `latest` / `vN.Y.Z` mutable form.
- [ ] Replace `STRATA_AUTH_MODE=off` with `required`; rotate `STRATA_STATIC_CREDENTIALS` out of the host env into a secret store + `STRATA_CONFIG_FILE`.
- [ ] Move `cassandra` / `pd` / `tikv` / `ceph` to dedicated hosts — bundled-on-the-same-box is for labs only.
- [ ] Configure backups for every data-bearing volume (`cassandra`, `pd`, `tikv`, `ceph`).
- [ ] Wire Prometheus alertmanager + log shipping (the bundled compose ships scrape + dashboard, not alerts).
- [ ] Front the gateway with a real LB if you run the `lab-tikv` shape outside the bench environment (the bundled nginx is unauthenticated + no TLS).
- [ ] Set `STRATA_OTEL_RINGBUF_BYTES` to a value sized for expected traffic (default 4 MiB ≈ a few thousand spans). Wire OTLP collector for retention beyond the ring.

## Cross-references

- [Single-node deployment]({{< ref "/deploy/single-node" >}}) — when you don't need the full compose stack.
- [Multi-replica cluster]({{< ref "/deploy/multi-replica" >}}) — operator guide for the `lab-tikv` profile.
- [Architecture — Storage]({{< ref "/architecture/storage" >}}) — backend selection + chunking.
- [Architecture — Backends — TiKV]({{< ref "/architecture/backends/tikv" >}}) — why `lab-tikv` exists.
