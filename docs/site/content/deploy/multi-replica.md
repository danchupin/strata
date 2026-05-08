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
