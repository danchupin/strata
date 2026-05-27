---
title: 'Profiling'
weight: 24
description: 'Operator-facing pprof endpoint behind admin auth — heap, CPU, goroutine, block, mutex, trace recipes.'
---

# Profiling

Strata exposes the Go runtime's standard `/debug/pprof/*` endpoints when
`STRATA_PPROF_ENABLED=true`. Profiles are protected by the same auth
chain that guards `/admin/v1/*` (session cookie or SigV4). The
endpoints are **opt-in** — defense-in-depth — because the heap profile
can leak the contents of in-flight buffers in error paths.

## Quick start

```bash
# Boot Strata with pprof attached to the admin listener.
STRATA_PPROF_ENABLED=true \
STRATA_ADMIN_LISTEN=127.0.0.1:9001 \
STRATA_AUTH_MODE=required \
STRATA_STATIC_CREDENTIALS=AKADMIN:SKADMIN:admin \
  strata server

# Capture a 30s CPU profile (SigV4 from go tool pprof via aws-cli sigv4 wrapper
# OR from a pre-signed URL; for loopback dev use the admin session cookie).
go tool pprof -seconds=30 -http=:7070 \
  -url 'http://127.0.0.1:9001/debug/pprof/profile?seconds=30'
```

## Configuration

| Env var | TOML key | Default | Description |
|---------|----------|---------|-------------|
| `STRATA_PPROF_ENABLED` | `pprof.enabled` | `false` | Master switch. `true` registers `/debug/pprof/*`. |
| `STRATA_PPROF_LISTEN` | `pprof.listen` | empty | Optional dedicated listener (e.g. `127.0.0.1:9002`). Empty → attach to `admin_listen.listen`. One of the two MUST be set when enabled — pprof never attaches to the S3 hot path. |
| `STRATA_PPROF_BLOCK_RATE` | `pprof.block_rate` | `0` | `runtime.SetBlockProfileRate(N)` argument. `0` keeps block profile data empty. |
| `STRATA_PPROF_MUTEX_RATE` | `pprof.mutex_rate` | `0` | `runtime.SetMutexProfileFraction(N)` argument. `0` keeps mutex profile data empty. |

`STRATA_PPROF_ENABLED=true` with neither `STRATA_PPROF_LISTEN` nor
`STRATA_ADMIN_LISTEN` set fails fast at boot — the gateway refuses to
silently expose profiling on the public S3 listener.

## Profile types

| Profile | Endpoint | When to use |
|---------|----------|-------------|
| **heap** | `/debug/pprof/heap` | Suspected leak — what's still allocated. Snapshot view. |
| **allocs** | `/debug/pprof/allocs` | High allocation rate — what's allocating, regardless of liveness. |
| **goroutine** | `/debug/pprof/goroutine` | Goroutine leak or deadlock — stacks of all live goroutines. |
| **cpu (profile)** | `/debug/pprof/profile?seconds=N` | Hot path investigation — sampled CPU time over N seconds. |
| **block** | `/debug/pprof/block` | Lock / channel contention — time spent waiting. Requires `STRATA_PPROF_BLOCK_RATE > 0`. |
| **mutex** | `/debug/pprof/mutex` | Mutex contention — stacks holding contended mutexes. Requires `STRATA_PPROF_MUTEX_RATE > 0`. |
| **trace** | `/debug/pprof/trace?seconds=N` | Scheduling / GC pauses — full execution trace. View via `go tool trace`. |

## Flamegraph workflow

```bash
# Capture once, browse in a local UI.
curl -s -o /tmp/heap.pprof \
  -u AKADMIN:SKADMIN http://127.0.0.1:9001/debug/pprof/heap
go tool pprof -http :7070 /tmp/heap.pprof
```

Open `http://localhost:7070/ui/flamegraph` to drill into the captured
profile.

For environments without `go tool pprof` on the operator host (Alpine /
distroless / BusyBox), point a containerised Go toolchain at the
captured file:

```bash
docker run --rm -v /tmp:/data golang:1.25 go tool pprof -http=:7070 /data/heap.pprof
```

## Validating a captured profile without `go tool pprof`

The Strata test suite ships a Go-native decoder backed by
`github.com/google/pprof/profile`. Operators on hosts without
`go tool pprof` reuse it through `go test` directly:

```bash
STRATA_PPROF_SMOKE_PROFILE=/tmp/heap.pprof \
  go test -run TestPprofDecode -count=1 ./internal/serverapp/...
```

`STRATA_PPROF_SKIP_TOOL_CHECK=1` reserves a future operator-side
override for smoke scripts that prefer the Go-native helper even when
`go tool pprof` is on `PATH`; today the smoke script
(`scripts/smoke-pprof.sh`) prefers the Go-native helper unconditionally.

## Block + mutex profiling — when to enable

Both profiles are off by default because the sampling adds overhead on
every blocking primitive (channel send, sync.Mutex.Lock, etc.).

- **Block profiling**: set `STRATA_PPROF_BLOCK_RATE=1` for a steady
  state where every blocking event is recorded (1 unit = 1ns
  threshold). Set to e.g. `1000` to sample blockages ≥ 1µs.
- **Mutex profiling**: set `STRATA_PPROF_MUTEX_RATE=N` to sample 1/N
  mutex contention events. `1` records every contention; `100`
  reduces overhead at the cost of resolution.

Enable in a maintenance window or against a single canary replica
behind the LB. Disable (unset the env, restart) when the investigation
is done.

## Security caveats

- Heap profiles can include in-flight buffer contents in error paths —
  PII, signed URLs, partial multipart bodies. Treat captured profiles
  as sensitive artifacts.
- pprof MUST NOT share the public S3 listener. The config validator
  rejects `STRATA_PPROF_ENABLED=true` when no admin / dedicated
  listener is set.
- The admin auth chain protects every pprof route the same way it
  protects `/admin/v1/*` (session cookie OR SigV4). Operators on
  shared rigs should additionally bind the listener to `127.0.0.1`
  (or behind a Tailscale / IAP tunnel).

## See also

- [Alerts]({{< relref "/operate/alerts" >}}) — alert runbooks reference
  pprof as the diagnostic next step on latency / panic spikes.
- [Monitoring]({{< relref "/operate/monitoring" >}}) — Prometheus
  metric definitions that point at pprof-worthy hot paths.
- [Single-binary invariant]({{< relref "/architecture/" >}}) — pprof is
  exposed by the same `strata` binary; no sidecar.
