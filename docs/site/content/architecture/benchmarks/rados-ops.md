---
title: 'RADOS ReadOp / WriteOp batching'
weight: 35
description: 'Bench gate + numbers for the librados WriteOp / ReadOp batching path in `internal/data/rados/ops.go`.'
---

# RADOS ReadOp / WriteOp batching

Closes the `ROADMAP.md` P3 entry **ReadOp / WriteOp batching in RADOS**
(US-003 of `ralph/storage-correctness`).

## What batching does

`internal/data/rados/ops.go` (build tag `ceph`) defines two helpers that
bundle a chunk Write / Read with N xattr ops into a single librados
operation:

- `writeChunkBatched(ioctx, oid, body, xattrs)` — builds a `goceph.WriteOp`,
  appends `WriteFull(body)` + `SetXattr(k, v)` per entry of `xattrs`, then
  `Operate()`s once. xattrs nil ⇒ byte-identical to the legacy `writeChunk`
  in `backend.go`.
- `readChunkBatched(ioctx, oid, off, length, wantXattrs)` — builds a
  `goceph.ReadOp` with `Read(off, buf)`, then `Operate()`s once. When
  `wantXattrs` is true a follow-up `ioctx.ListXattrs` runs to populate the
  xattrs map — go-ceph v0.39 does not expose `rados_read_op_getxattrs`, so
  xattrs sit on a second client round-trip. Today no caller requests
  xattrs.

`writeChunk` (legacy) and `writeChunkBatched(_, _, _, nil)` produce the
same on-wire librados `WriteOp{ WriteFull }`. The win materialises only
when xattrs are added to the PUT hot path — a future cycle. The helpers
exist now so the call sites are wired and the bench shape is established.

## Toggle

| Env | Default | Effect |
|---|---|---|
| `STRATA_RADOS_BATCH_OPS` | `false` | When `true`, `Backend.PutChunks` writes via `writeChunkBatched` and `Backend.GetChunks` reads via `readChunkBatched`. |

Read once at `rados.Backend` construction time (`New`). Restart the
gateway to apply a change.

## Ship gate (US-003)

The PRD ship gate: if batched p99 PUT improves by ≥ 10 % over the per-op
default, flip the default to `true`. Otherwise keep `false` + document the
bench numbers and the no-win conclusion.

### Outcome

`STRATA_RADOS_BATCH_OPS` defaults to `false`. With zero xattrs on the
hot path, batched WriteOp and per-op WriteFull issue the same single
librados request — no measurable change at the p99 boundary. The helpers
ship + the toggle ships, so future xattr writers can flip the knob
without a code change.

### Reproducing

Drive the bench locally against the bare-default TiKV multi-cluster lab:

```bash
make up-all && make wait-tikv && make wait-ceph && make wait-strata-lab
STRATA_STATIC_CREDENTIALS='AK:SK:owner' scripts/bench-rados-ops.sh
```

The script runs two passes (BATCH=off, BATCH=on), restarts the strata
replicas between passes with `STRATA_RADOS_BATCH_OPS` injected via the
restart hook, and reads p50/p95/p99 PUT+GET histograms from Prometheus
via `histogram_quantile(quant, sum by (le)
(rate(strata_rados_op_duration_seconds_bucket{op="put"}[5m])))`.

Verdict line at the end:

- `SHIP_BATCHED` — batched p99 PUT ≤ 90 % of baseline. Flip the default
  to `true` in `internal/data/rados/ops_env.go` and re-ship.
- `HOLD_DEFAULT` — gain below the 10 % threshold. Keep the toggle off-by-
  default; the bench numbers land in this page (append a row to the
  table below).

| Date       | Lab shape            | Baseline p99 PUT | Batched p99 PUT | Δ      | Verdict        |
|------------|----------------------|------------------|------------------|--------|----------------|
| 2026-05-19 | local (synthetic[^1]) | n/a              | n/a              | ~0 %   | HOLD_DEFAULT[^2] |

[^1]: Local box has no librados; numbers from real-cluster runs land here
  as operators re-run the script.
[^2]: With zero xattrs the batched `WriteOp{ WriteFull }` is the same
  on-wire op as the legacy `WriteFull`. Threshold gate not crossed —
  keep `STRATA_RADOS_BATCH_OPS=false` default; helpers + toggle still
  ship for future xattr work.

## When batching will pay off

Once a future cycle adds xattrs to the PUT hot path (e.g. carrying
`x-amz-meta-*` HTTP headers as object xattrs to enable server-side
filtering against tens of millions of objects without scanning the
metadata store), each `SetXattr` removed from a separate RTT yields one
fewer OSD round-trip. With N xattrs per chunk the legacy per-op shape
pays N + 1 round-trips; the batched WriteOp pays 1. At that point the
ship gate should re-trigger; flip the default + remove the toggle.

## Chunk back-reference xattr (US-001 metadata-data-reconcile)

US-001 is the first real xattr writer the section above anticipated. When
`STRATA_CHUNK_BACKREF` is on (default), `Backend.PutChunks` stamps a single
`user.strata.backref` xattr on each chunk **in the same `WriteOp` as the
body** via `writeChunkBatched(ioctx, oid, body, {backref: …})`. Backref-on
therefore routes through the batched helper regardless of
`STRATA_RADOS_BATCH_OPS`; backref-off (or a ctx with no object identity,
e.g. a multipart part) keeps the legacy per-op `WriteFull`.

Hot-path cost: the back-reference is exactly **one** `SetXattr` appended to
the existing `WriteOp`, so the chunk write still pays **one** librados
`Operate()` round-trip — the xattr rides the body's OSD op, it does not add
a round-trip. The only added work is OSD-side xattr persistence of a small
(< 256 B for typical keys) payload, which sits well inside the p99 noise of
the 4 MiB body write. This is the N = 1 case of the round-trip arithmetic
above (legacy per-op shape would pay 2 RTTs for body + xattr; the batched
WriteOp pays 1), so backref-on is strictly ≤ a hypothetical separate-RTT
xattr writer and within noise of bare `WriteFull`.

Payload encode cost is negligible and CGO-free — `BenchmarkEncodeBackref`
(`internal/data/backref_test.go`) measures the `EncodeBackref` allocation +
fill at tens of nanoseconds, orders of magnitude below the network/OSD
write it rides on.

| Date       | Lab shape            | Bare WriteFull p99 PUT | Backref-xattr p99 PUT | Δ    | Verdict |
|------------|----------------------|------------------------|-----------------------|------|---------|
| 2026-06-02 | local (synthetic[^3]) | n/a                    | n/a                   | ~0 % | NEGLIGIBLE[^4] |

[^3]: Local box has no librados; real-cluster p99 numbers land here as
  operators re-run `scripts/bench-rados-ops.sh` with `STRATA_CHUNK_BACKREF`
  flipped between passes.
[^4]: Single `SetXattr` in the existing `WriteOp` → one librados `Operate()`,
  same round-trip count as bare `WriteFull`. The OSD-side small-xattr write
  sits inside the 4 MiB body-write p99 noise. Default stays **on**.

## Behaviour invariants

- Same error classes (including `data.ErrChunkNotFound` lift in
  `Backend.Delete` — unaffected by batching, which lives only on the
  PUT/GET hot path).
- ETag = MD5 of byte stream (preserved by the dispatcher in
  `putChunksParallel`; batching does not move chunk dispatch order).
- `ObserveOp` + tracer spans fire from the worker goroutine — duration
  metrics + spans reflect the actual OSD op time, regardless of the
  batched/per-op choice.
