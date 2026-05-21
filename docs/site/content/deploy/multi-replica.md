---
title: 'Multi-replica cluster'
weight: 20
description: 'Run N stateless gateway replicas behind a load balancer with shared TiKV / RADOS storage.'
---

# Multi-replica Strata cluster — operator guide

Strata's gateway is **stateless**. Replicas don't form a quorum among
themselves; the storage layer (TiKV / RADOS) provides durability +
consistency. A single replica is therefore a **single point of failure
for HTTP traffic** — not for data. Running **≥2 replicas behind a
load balancer** is the minimum HA shape.

This page covers the bare-default 2-replica TiKV lab (two TiKV-backed
replicas behind nginx at host `:9999`) — the reference shape for
multi-replica deployments. Same wiring works for 3+ replicas in
production.

## Prerequisites

- Two (or more) hosts able to run the `strata:ceph` image.
- An external **metadata cluster**: TiKV (PD ≥3 + TiKV ≥3) or
  Cassandra ≥3 nodes.
- An external **data backend**: a RADOS pool (`size=3` replication) or
  an upstream S3 endpoint.
- An L7 **load balancer** that preserves the `Host` header and streams
  request bodies (no buffering). nginx, HAProxy, Envoy, AWS ALB all
  work; the bundled lab uses nginx.
- TLS terminated at the LB (replicas talk plaintext on the internal
  network).

## Install

The bundled compose stack:

```bash
make up                # docker compose up -d (TiKV-default lab)
make wait-strata-lab   # polls 10001 + 10002 + 9999/readyz until ready
```

Direct replica ports: `127.0.0.1:10001` (replica a),
`127.0.0.1:10002` (replica b). LB: `127.0.0.1:9999`.
Tear down: `make down` (covers all profiles).

For a production deploy, run the `strata:ceph` image on N hosts with
the same env (see *Configure* below), point them all at the shared
metadata + data backends, and front them with your LB.

## Configure

### Why replicas

| Concern | Source of HA |
|---|---|
| Storage durability + consistency | TiKV raft (PD ≥3 + TiKV ≥3) / RADOS pool replication. |
| Gateway HTTP availability | **Multiple Strata replicas behind a LB.** |
| Worker leader continuity (gc / lifecycle / replicator / …) | Lease-based rotation; each worker is elected on a per-worker leader lease. |
| Console session continuity | Shared JWT secret across replicas. |

The gateway never needs quorum among gateways. Each replica is
independent; the LB picks one per request. Only **workers** elect a
leader, and only one replica runs each worker at a time.

### Replica count

| Count | What it gets you |
|---|---|
| 1 | SPOF for HTTP. Fine for dev / single-node demo. |
| **2** | **Minimum HA.** One replica failure → LB drains it; surviving replica picks up worker leases within ~30 s. |
| 3+ | More headroom under load; no extra correctness guarantee. Typical for sized clusters. |

There is no upper bound. Each replica costs ~1 RSS GiB + the per-replica
heartbeat + worker-lease churn. PD / TiKV / Ceph scaling is independent.

### JWT secret distribution

Console sessions are HS256 JWTs signed with a 32-byte secret. If two
replicas sign with different secrets, a session cookie issued by
replica A is rejected by replica B and the operator gets logged out
on every LB flip.

The gateway's JWT secret resolution order:

1. `STRATA_CONSOLE_JWT_SECRET` env (operator-managed, plaintext or hex).
2. `STRATA_JWT_SECRET_FILE` env path (operator-managed, file contents read verbatim).
3. **`/etc/strata/jwt-shared/secret`** — file-based atomic bootstrap (default for the bundled lab).
4. Ephemeral 32-byte hex generated on every boot (WARN-logged; fine for dev, never for prod).

The atomic bootstrap uses POSIX `O_EXCL`: the first replica to create
the file wins, writes 32 random bytes hex-encoded, closes. Concurrent
callers see `EEXIST` and re-read the file with up to 3× / 100 ms
backoff. The shared file path is fixed (`/etc/strata/jwt-shared/secret`);
the directory must be a shared writable mount across replicas. The
bundled lab mounts the named volume `strata-jwt-shared` at that path
on both replicas.

When `STRATA_CONSOLE_JWT_SECRET` is set, the shared file is **never
touched** (env-managed deployments aren't surprised by a file write
on first boot).

### `STRATA_GC_SHARDS` sizing

The GC fan-out splits work across `STRATA_GC_SHARDS` logical shards
(range `[1, 1024]`, default `1`). Each shard is leader-elected
independently; the lifecycle worker uses the same shard count for
per-bucket parallelism.

**Sizing rule:** `STRATA_GC_SHARDS` should equal the steady-state
replica count so every replica owns one shard.

```bash
STRATA_GC_SHARDS=3       # set on every replica when running 3 replicas
```

Behaviour under failure:

| Replicas alive | Shards held |
|---|---|
| 3 / 3 | One shard per replica. |
| 2 / 3 | The dead replica's shard moves to one of the survivors after lease TTL (~30 s); that survivor now holds 2. |
| 1 / 3 | Sole survivor holds all 3 shards. |
| 0 / 3 | No GC progress until ≥1 replica returns. |

Setting `STRATA_GC_SHARDS` higher than the replica count is safe
(replicas hold multiple shards each) but wastes per-shard heartbeat
overhead. Setting it lower starves some replicas of GC work.

### LB wiring (nginx)

The bundled `deploy/nginx/strata-lab.conf`:

- `upstream strata { least_conn; server strata-a:9000; server strata-b:9000 max_fails=2 fail_timeout=10s; }`
- Streaming-friendly: `proxy_request_buffering off`, `proxy_buffering off`,
  `client_max_body_size 0`, `proxy_read_timeout 300s`, `proxy_send_timeout 300s`,
  `proxy_http_version 1.1`. Required for SigV4 chunked-streaming uploads + multipart.
- Headers preserved: `Host`, `X-Real-IP`, `X-Forwarded-For`, `X-Forwarded-Proto`.

Host port `9999` → nginx → upstream replicas. `aws --endpoint-url
http://127.0.0.1:9999 …` reaches one of the two replicas; the LB
picks per connection.

`nginx -t` syntax check (CI job `lint-nginx-lab`) requires
`--add-host=strata-{a,b}:127.0.0.1` because nginx resolves upstream
hostnames at parse time, not at request time.

### Top env vars

Full table at
[Reference — environment variables]({{< ref "/reference/env-vars" >}}).

| Variable | Purpose |
|---|---|
| `STRATA_NODE_ID` | Unique per replica (`strata-a`, `strata-b`, …). |
| `STRATA_META_BACKEND` | `tikv` or `cassandra`. All replicas share the same value. |
| `STRATA_DATA_BACKEND` | `rados` or `s3`. All replicas share. |
| `STRATA_GC_SHARDS` | Steady-state replica count. |
| `STRATA_WORKERS` | Workers this replica runs. Default `gc,lifecycle,rebalance`. |
| `STRATA_CONSOLE_JWT_SECRET` | Recommended in prod — skips the file-based bootstrap. |
| `STRATA_AUTH_MODE` | `required` in prod (`optional` is for the lab profile only). |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTel collector. |

## Verify

```bash
curl http://127.0.0.1:9999/healthz   # nginx LB
curl http://127.0.0.1:9999/readyz    # both replicas + storage probes
curl http://127.0.0.1:10001/readyz   # strata-a direct
curl http://127.0.0.1:10002/readyz   # strata-b direct
aws --endpoint-url http://127.0.0.1:9999 --no-sign-request s3 ls
```

A cross-replica round trip:

```bash
# Force replica A (direct port)
aws --endpoint-url http://127.0.0.1:10001 --no-sign-request s3 cp README.md s3://t/x
# Read from replica B (direct port)
aws --endpoint-url http://127.0.0.1:10002 --no-sign-request s3 cp s3://t/x -
```

The byte stream matches — storage is shared.

`scripts/multi-replica-smoke.sh` drives the above end-to-end without
Playwright (host-side only — needs `curl`, `jq`, `aws`, `docker`).

## Monitor

### Expected behaviour under failure

Leader-lease defaults: TTL 30 s, renew period TTL/3 (10 s).
Heartbeat row TTL: 30 s, write cadence 10 s.

| Scenario | Expected behaviour |
|---|---|
| Both replicas healthy | LB round-robins (least-conn). Cluster Overview shows 2 healthy nodes. Exactly one replica carries `lifecycle-leader`; exactly one carries `gc-leader` (may be the same or different replicas). |
| Stop one replica (`docker stop strata-a`) | LB marks the upstream down within `fail_timeout`; client sees no errors. After ~30 s the killed replica's heartbeat row vanishes. Within ~30–35 s the surviving replica acquires both worker leases. |
| Restart the replica | After ~30 s `make wait-strata-lab` passes; the new replica writes its heartbeat row again. Worker leases stay where they are (no preemption); they only rotate if the current holder dies. |
| Cross-replica PUT then GET | Object written via replica A is readable via replica B byte for byte. Storage layer is shared; gateways are interchangeable. |
| Login on replica A, refresh hits replica B | Session cookie verifies because both replicas share the JWT secret. Without the shared secret, refresh redirects to login. |

### Worker-leader rotation — UI signal

The worker supervisor emits `(workerName, acquired bool)` events on
every lease acquire/release. The heartbeater consumes them and
publishes the updated owner slice on the next heartbeat tick (~10 s).
The Cluster Overview reads that slice and flips the leader chip.

End-to-end propagation budget after a leader-holder kill:

```
T+0       holder dies
T+10..30  surviving replica's leader lease acquires (TTL expiry)
T+10..30  supervisor emits (worker, true) → heartbeater flips
T+10      next heartbeat write tick publishes new LeaderFor slice
T+5       Cluster Overview poll picks up the new row
≤ 35 s    chip moves in the UI
```

The 35 s upper bound matches `DEAD_GRACE` in
`scripts/multi-replica-smoke.sh` and the Playwright
`multi-replica.spec.ts` worker-rotation test.

### Metrics

- **Per-replica:** `:9000/metrics` (request rate, latency, worker
  panic counters, queue depths).
- **Per-PD / TiKV:** PD `:2379/metrics`, TiKV `:20180/metrics`.
- **Provisioned dashboard:** `deploy/grafana/strata-dashboard.json`
  shows gateway + worker + storage metrics in one view.

Suggested alerts: `strata_worker_panic_total > 0`,
`strata_replication_queue_age_seconds > <SLO>`, replica-count drift.

## Troubleshoot

- **Console logs me out on every refresh.** Replicas have different
  JWT secrets. Confirm the shared volume mount or set
  `STRATA_CONSOLE_JWT_SECRET` on every replica.
- **`SignatureDoesNotMatch` from one replica only.** Clock skew. NTP
  every host; SigV4 rejects timestamps off by >15 min.
- **GC backlog rising on one replica.** `STRATA_GC_SHARDS` is set
  lower than the replica count, so some replicas are idle. Bump it to
  the replica count and `docker compose restart strata-a strata-b`.
- **`make wait-strata-lab` reports replica B not ready.** Direct-port
  curl (`:10002/readyz`) returns 503. Likely RADOS isn't reachable
  from `strata-b` (cluster-b mount missing or pool unhealthy).
- **Replica drops out mid-multipart.** The LB request timeout fired
  before the upload completed. Raise `proxy_read_timeout` /
  `proxy_send_timeout` past the slowest part you expect.

## Shared S3 vs RADOS data backend

The lab uses RADOS for object data. The same multi-replica shape
works with the S3-over-S3 backend (`STRATA_DATA_BACKEND=s3` plus the
upstream-S3 credentials) — only the data-backend env differs; LB,
JWT bootstrap, and worker leader-election are identical.

| Data backend | Per-replica disk | Cross-replica coherence | Notes |
|---|---|---|---|
| `rados` | none — RADOS pool is shared | RADOS replication factor (default `size=3`) | Reference shape; build tag `ceph` required. |
| `s3` | none — upstream S3 is shared | Upstream durability (e.g. AWS S3 11×9s) | See [Architecture — Backends — S3]({{< ref "/architecture/backends/s3" >}}). |
| `memory` | per-replica | none — never use across replicas | Tests / smoke pass only. |

Multi-replica with `memory` data is not supported: each replica's
writes are invisible to its peers.

## Leader-election shape

The supervisor pattern owns leader-election for every worker. Per
replica:

- One goroutine per worker (`gc`, `lifecycle`, `notify`, `replicator`,
  `access-log`, `inventory`, `audit-export`, `manifest-rewriter`).
- Each goroutine acquires a leader lease keyed on `<name>-leader`.
- On lease loss, the worker exits and the supervisor restarts
  immediately (no backoff). On panic, the supervisor recovers,
  releases the lease, and restarts on exponential backoff
  (1s → 5s → 30s → 2m, reset to 1s after 5 minutes healthy).
- Workers that own per-shard leader-election internally (the GC
  fan-out is the canonical case) emit leader-acquire/release events
  themselves so the heartbeat chip still flips.

Workers run **at most one replica at a time** — there is no
cluster-wide fan-out below the shard level. If you need more
parallelism inside one worker, the knobs are `STRATA_GC_CONCURRENCY`
(per-shard goroutines for GC), `STRATA_LIFECYCLE_CONCURRENCY`
(per-bucket goroutines for lifecycle), and `STRATA_GC_SHARDS`
(cluster-wide fan-out).

The heartbeat row carries `LeaderFor []string` so the embedded
operator console can show which replica owns which worker. UI
propagation budget is ≤35 s after a holder dies.

## Production checklist

When promoting the 2-replica lab (or its 3-replica variant) to
production:

- [ ] Replica count ≥2 (≥3 recommended for headroom under load).
- [ ] LB health-checks `/readyz` (not `/healthz`) so a replica with a sick metadata backend gets drained.
- [ ] LB preserves Host + supports streaming bodies (no request buffering); SigV4 chunked uploads break otherwise.
- [ ] TLS terminated at the LB; replicas talk plaintext on the internal network.
- [ ] `STRATA_AUTH_MODE=required` (`optional` is for the lab profile only — it accepts unsigned requests).
- [ ] `STRATA_GC_SHARDS` = steady-state replica count.
- [ ] PD ≥3, TiKV ≥3 (raft majority for the metadata backend).
- [ ] RADOS pool `size=3` (or upstream-S3 with multi-AZ + versioning if using S3-over-S3).
- [ ] JWT secret distributed via shared volume or via `STRATA_CONSOLE_JWT_SECRET` env from a secret store. Never fall through to the ephemeral generated secret in production.
- [ ] Prometheus scraping every replica + every PD + every TiKV; alerts on `strata_worker_panic_total > 0`, `strata_replication_queue_age_seconds > <SLO>`.
- [ ] OTel collector reachable from every replica; ring buffer `STRATA_OTEL_RINGBUF_BYTES` sized for expected traffic.
- [ ] Centralised log shipping draining JSON `stdout` (request_id + node_id are stamped on every line).
- [ ] Disaster recovery runbook tested — see [Operate — backup & restore](/operate/backup-restore/).
- [ ] `make smoke-lab-tikv` passes against a fresh stand-up.

## Cross-references

- [Single-node deployment]({{< ref "/deploy/single-node" >}}) — when one box is enough.
- [Docker Compose]({{< ref "/deploy/docker-compose" >}}) — full compose service map + profiles.
- [Kubernetes]({{< ref "/deploy/kubernetes" >}}) — the Kubernetes-native shape.
- [Operate](/operate/) — day-2 workflows (drain, scale, back up).
- [Reference — environment variables]({{< ref "/reference/env-vars" >}}) — full env knob table.
- [Architecture — Storage]({{< ref "/architecture/storage" >}}) — sharded objects table, RADOS chunking, multi-replica scaling rationale.
- [Architecture — Backends — TiKV]({{< ref "/architecture/backends/tikv" >}}) — why TiKV is the recommended metadata backend for multi-replica.
- [Architecture — Backends — S3]({{< ref "/architecture/backends/s3" >}}) — S3-over-S3 data backend.
