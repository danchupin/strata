---
title: 'Multi-replica cluster'
weight: 20
description: 'Run N stateless gateway replicas behind a load balancer with shared TiKV / RADOS storage.'
---

# Multi-replica Strata cluster — operator guide

Strata's gateway is **stateless**. Replicas don't form quorum among
themselves; the storage layer (TiKV / RADOS) provides durability +
consistency. A single replica is therefore a **single point of failure for
HTTP traffic** — not for data. Running **≥2 replicas behind a load balancer**
is the minimum HA shape.

This document covers the `lab-tikv` compose profile (2 TiKV-backed replicas
behind nginx at host port 9999) — the reference shape for multi-replica
deployments.

## Why replicas

| Concern | Source of HA |
|---|---|
| Storage durability + consistency | TiKV raft (PD ≥3, TiKV ≥3) / RADOS pool replication |
| Gateway HTTP availability | **Multiple Strata replicas behind a LB** |
| Worker leader continuity (gc / lifecycle / replicator / …) | `internal/leader.Session` — lease-based rotation across replicas |
| Console session continuity | Shared JWT secret (file-based atomic bootstrap) |

The gateway never needs quorum among gateways. Each replica is independent;
the LB picks one per request. Only **workers** elect a leader (via `leader.Session`),
and only one replica runs each worker at a time.

## Replica count

| Count | What it gets you |
|---|---|
| 1 | SPOF for HTTP. Fine for dev / single-node demo. |
| **2** | **Minimum HA.** One replica failure → LB drains it; surviving replica picks up worker leases within ~30 s. |
| 3+ | More headroom under load; no extra correctness guarantee. Typical for sized clusters. |

There is no upper bound. Each replica costs ~1 RSS GiB + the per-replica
heartbeat + worker-lease churn. PD/TiKV/Ceph scaling is independent.

## JWT secret distribution

Console sessions are HS256 JWTs signed with a 32-byte secret. If two replicas
sign with different secrets, a session cookie issued by replica A is rejected
by replica B and the operator gets logged out on every LB-flip.

Resolution order in `internal/serverapp.loadJWTSecret`:

1. `STRATA_CONSOLE_JWT_SECRET` env (operator-managed, plaintext or hex)
2. `STRATA_JWT_SECRET_FILE` env path (operator-managed, file contents read verbatim)
3. **`/etc/strata/jwt-shared/secret`** — file-based atomic bootstrap (NEW; default for `lab-tikv`)
4. Ephemeral 32-byte hex generated on every boot (WARN-logged; fine for dev, never for prod)

**File-based atomic bootstrap** (the lab-tikv default) uses POSIX `O_EXCL`:
the first replica to call `os.OpenFile(path, O_WRONLY|O_CREATE|O_EXCL, 0600)`
wins, writes 32 random bytes hex-encoded, and closes. Concurrent callers see
`EEXIST` and re-read the file with up to 3× / 100 ms backoff to absorb the
fsync race. No lock-manager / no `leader.Session` needed — the bootstrap
runs **before** the locker is built.

When `STRATA_CONSOLE_JWT_SECRET` is set, the shared file is **never touched**
(env-managed deployments aren't surprised by a file write on first boot).

The shared file path is fixed (`/etc/strata/jwt-shared/secret`); the directory
must be a shared writable mount across replicas. The `lab-tikv` profile mounts
the named volume `strata-jwt-shared` at that path on both replicas.

## LB wiring (nginx)

`deploy/nginx/strata-lab.conf`:

- `upstream strata { least_conn; server strata-tikv-a:9000; server strata-tikv-b:9000 max_fails=2 fail_timeout=10s; }`
- Streaming-friendly: `proxy_request_buffering off`, `proxy_buffering off`,
  `client_max_body_size 0`, `proxy_read_timeout 300s`, `proxy_send_timeout 300s`,
  `proxy_http_version 1.1`. Required for SigV4 chunked-streaming uploads + multipart.
- Headers preserved: `Host`, `X-Real-IP`, `X-Forwarded-For`, `X-Forwarded-Proto`.

Host port `9999` → nginx → upstream replicas. `aws --endpoint-url
http://127.0.0.1:9999 …` reaches one of the two replicas; the LB picks per
connection.

`nginx -t` syntax check (CI job `lint-nginx-lab`) requires `--add-host=strata-tikv-{a,b}:127.0.0.1`
because nginx resolves upstream hostnames at parse time, not at request time.

## Bring-up

```bash
make up-lab-tikv      # pd + tikv + ceph + strata-tikv-a + strata-tikv-b + strata-lb-nginx + prometheus + grafana
make wait-strata-lab  # polls 9001 + 9002 + 9999/readyz until all ready
```

Direct replica ports: `127.0.0.1:9001` (replica a), `127.0.0.1:9002` (replica b).
LB: `127.0.0.1:9999`.

Tear down: `make down` (covers all profiles).

## Failure scenarios — expected behaviour

`leader.Session` defaults: TTL 30 s, RenewPeriod TTL/3 (10 s).
Heartbeat row TTL: 30 s, write cadence 10 s.

| Scenario | Expected behaviour |
|---|---|
| Both replicas healthy | LB round-robins (least-conn). Cluster Overview shows 2 healthy nodes. Exactly one replica carries `lifecycle-leader`; exactly one carries `gc-leader` (may be the same or different replicas). |
| Stop one replica (`docker stop strata-tikv-a`) | LB marks the upstream down within `fail_timeout`; client sees no errors. After ~30 s the killed replica's heartbeat row vanishes (TTL eviction in cassandra; `ExpiresAt` lazy-skip in tikv). Within ~30–35 s the surviving replica acquires both worker leases. |
| Restart the replica | After ~30 s `make wait-strata-lab` passes; the new replica writes its heartbeat row again. Worker leases stay where they are (no preemption); they only rotate if the current holder dies. |
| Cross-replica PUT then GET | Object written via replica A is readable via replica B byte-for-byte. Storage layer (TiKV+RADOS) is shared; gateways are interchangeable. |
| Login on replica A, refresh hits replica B | Session cookie verifies because both replicas share the JWT secret via `strata-jwt-shared` volume. Without the shared secret, refresh redirects to login. |

`scripts/multi-replica-smoke.sh` drives all of the above end-to-end without
Playwright (host-side only — needs `curl`, `jq`, `aws`, `docker`).

## Worker-leader rotation — UI signal

`cmd/strata/workers.Supervisor` emits `(workerName, acquired bool)` events on
every lease acquire/release via the buffered `Supervisor.LeaderEvents()` channel.
`internal/serverapp.Run` consumes them in a goroutine and calls
`internal/heartbeat.Heartbeater.SetLeaderFor(worker, owned)`, which mutates
`Node.LeaderFor` under a mutex. The next heartbeat write tick (~10 s)
publishes the updated slice into `cluster_nodes`, where the Cluster Overview
reads it.

End-to-end propagation budget after a leader-holder kill:

```
T+0       holder dies
T+10..30  surviving replica's leader.Session.AwaitAcquire returns (TTL expiry)
T+10..30  Supervisor emits (worker, true) → hb.SetLeaderFor flips
T+10      next heartbeat write tick publishes new LeaderFor slice
T+5       Cluster Overview poll picks up the new row
≤ 35 s    chip moves in the UI
```

The 35 s upper bound matches `DEAD_GRACE` in `scripts/multi-replica-smoke.sh`
and the Playwright `multi-replica.spec.ts` worker-rotation test.

## Smoke + e2e

| Tool | Command | When |
|---|---|---|
| Host smoke | `make smoke-lab-tikv` (after `up-lab-tikv` + `wait-strata-lab`) | Default validation; no Playwright dep |
| Playwright e2e | `pnpm exec playwright test --config=web/playwright.multi-replica.config.ts` | CI job `e2e-ui-multi-replica`, gated by `[multi-replica]` in PR title or `workflow_dispatch` |

The e2e spec uses a standalone Playwright config (no inline `webServer`,
`baseURL: http://127.0.0.1:9999`) so it relies on the compose-managed stack
brought up by `make up-lab-tikv`. The default `e2e-ui` Playwright project
`testIgnore`s the multi-replica spec to keep its budget intact.

## Deploy considerations

- **Storage backend:** lab-tikv targets TiKV; the same shape works for
  Cassandra (replace `STRATA_META_BACKEND=tikv` with `cassandra` and depend
  on the Cassandra service instead).
- **Object data backend:** RADOS in `lab-tikv`. For S3-over-S3 deployments
  the gateways still share the JWT secret + LB the same way; only the data
  backend env differs.
- **Production LB:** nginx as shown is a reference. Any L7 LB that preserves
  Host + supports streaming bodies (HAProxy, Envoy, ELB+ALB target groups)
  works. Avoid request buffering or you will break SigV4 chunked uploads.
- **TLS termination:** terminate at the LB; replicas talk plaintext on the
  internal network. SigV4 signs `Host`, so the LB must forward the original
  Host header (nginx config does).

## STRATA_GC_SHARDS sizing (Phase 2)

The Phase-2 GC fan-out (US-004 in the runtime cycle) splits the GC queue
across `STRATA_GC_SHARDS` logical shards (range `[1, 1024]`, default `1`).
Each shard is leader-elected independently on `gc-leader-<shardID>`, and
the lifecycle worker uses the same shard count for its per-bucket lease
(`lifecycle-leader-<bid>` gated by `fnv32a(bucketID) % STRATA_GC_SHARDS`).

Sizing rule: **`STRATA_GC_SHARDS` should equal the steady-state replica
count** so every replica owns one shard. Example for a 3-replica cluster:

```bash
STRATA_GC_SHARDS=3       # set on every replica
```

Behaviour under failure:

| Replicas alive | Shards held |
|---|---|
| 3 / 3 | One shard per replica. |
| 2 / 3 | The dead replica's shard moves to one of the survivors after lease TTL (~30 s); that survivor now holds 2. |
| 1 / 3 | Sole survivor holds all 3 shards. |
| 0 / 3 | No GC progress until ≥1 replica returns. |

Setting `STRATA_GC_SHARDS` higher than the replica count is safe (replicas
hold multiple shards each) but wastes per-shard heartbeat overhead. Setting
it lower than the replica count starves some replicas of GC work — the
`lifecycle-leader-<bid>` gate becomes the only per-bucket parallelism.

The cutover from Phase 1 (single global `gc-leader`) to Phase 2 is gated
by `STRATA_GC_DUAL_WRITE` — see the migration guide under
[Architecture — Migrations]({{< ref "/architecture/migrations" >}}) for
the playbook.

## Shared S3 vs RADOS data backend

The lab-tikv profile uses RADOS for object data. The same multi-replica
shape works with the S3-over-S3 backend (`STRATA_DATA_BACKEND=s3` plus
the upstream-S3 credentials) — only the data-backend env differs; LB,
JWT bootstrap, and worker leader-election are identical.

| Data backend | Per-replica disk | Cross-replica coherence | Notes |
|---|---|---|---|
| `rados` | none — RADOS pool is shared | RADOS replication factor (default `size=3`) | Reference shape; build tag `ceph` required. |
| `s3` | none — upstream S3 is shared | Upstream durability (e.g. AWS S3 11×9s) | See [Architecture — Backends — S3]({{< ref "/architecture/backends/s3" >}}). |
| `memory` | per-replica | none — never use across replicas | Tests / smoke pass only. |

Multi-replica with `memory` data is not supported: each replica's writes
are invisible to its peers.

## Leader-election shape

The supervisor pattern owns leader-election for every worker. Per replica:

- One goroutine per worker (`gc`, `lifecycle`, `notify`, `replicator`,
  `access-log`, `inventory`, `audit-export`, `manifest-rewriter`).
- Each goroutine acquires a `leader.Session` keyed on `<name>-leader`
  (cassandra LWT lease or in-process for memory backend).
- On lease loss, the worker exits and the supervisor restarts immediately
  (no backoff). On panic, the supervisor recovers, releases the lease, and
  restarts on exponential backoff (1s → 5s → 30s → 2m, reset to 1s after
  5 minutes healthy).
- Workers that own leader-election internally (the gc fan-out is the
  canonical case) register `SkipLease: true` and call `EmitLeader` from
  every per-shard acquire/release transition so the heartbeat chip still
  flips on `LeaderEvents()`.

The heartbeat row in `cluster_nodes` carries `LeaderFor []string` so the
embedded operator console can show which replica owns which worker. UI
propagation budget is ≤35 s after a holder dies (TTL expiry + heartbeat
write tick + UI poll).

Workers run **at most one replica at a time** — there is no cluster-wide
fan-out below the shard level. If you need more parallelism inside one
worker, the knobs are `STRATA_GC_CONCURRENCY` (per-shard goroutines for
GC), `STRATA_LIFECYCLE_CONCURRENCY` (per-bucket goroutines for lifecycle),
and `STRATA_GC_SHARDS` (cluster-wide fan-out).

## Production checklist

When promoting the lab-tikv shape (or its 3-replica variant) to
production:

- [ ] Replica count ≥2 (≥3 recommended for headroom under load).
- [ ] LB health-checks `/readyz` (not `/healthz`) so a replica with a sick metadata backend gets drained.
- [ ] LB preserves Host + supports streaming bodies (no request buffering); SigV4 chunked uploads break otherwise.
- [ ] TLS terminated at the LB; replicas talk plaintext on the internal network.
- [ ] `STRATA_AUTH_MODE=required` (`optional` is for the lab profile only — it accepts unsigned requests).
- [ ] `STRATA_GC_SHARDS` = steady-state replica count.
- [ ] PD ≥3, TiKV ≥3 (raft majority for the metadata backend).
- [ ] RADOS pool `size=3` (or upstream-S3 with multi-AZ + versioning if using S3-over-S3).
- [ ] JWT secret distributed via shared volume (`/etc/strata/jwt-shared/secret` default) or via `STRATA_CONSOLE_JWT_SECRET` env from a secret store. Never fall through to the ephemeral generated secret in production.
- [ ] Prometheus scraping every replica + every PD + every TiKV; alerts on `strata_worker_panic_total > 0`, `strata_replication_queue_age_seconds > <SLO>`, `strata_cassandra_lwt_conflicts_total` rate spike.
- [ ] OTel collector reachable from every replica; ring buffer `STRATA_OTEL_RINGBUF_BYTES` sized for expected traffic.
- [ ] Centralised log shipping draining JSON `stdout` (request_id + node_id are stamped on every line).
- [ ] Disaster recovery runbook: TiKV PITR + RADOS pool snapshots tested end-to-end; cross-region replicator worker (if applicable) configured with a peer endpoint.
- [ ] `make smoke-lab-tikv` passes against a fresh stand-up.

## Cross-references

- [Single-node deployment]({{< ref "/deploy/single-node" >}}) — when one box is enough.
- [Docker Compose]({{< ref "/deploy/docker-compose" >}}) — full compose service map + profiles.
- [Architecture — Storage]({{< ref "/architecture/storage" >}}) — sharded objects table, RADOS chunking, multi-replica scaling rationale.
- [Architecture — Backends — TiKV]({{< ref "/architecture/backends/tikv" >}}) — why TiKV is the recommended metadata backend for multi-replica.
- [Architecture — Backends — S3]({{< ref "/architecture/backends/s3" >}}) — S3-over-S3 data backend.
- [Architecture — Migrations]({{< ref "/architecture/migrations" >}}) — Phase 1 → Phase 2 GC fan-out cutover.
