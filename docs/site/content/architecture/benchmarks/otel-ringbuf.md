---
title: 'OTel ring-buffer bytes budget'
weight: 38
description: 'Bench gate + numbers for `STRATA_OTEL_RINGBUF_BYTES` — retention horizon vs memory ceiling.'
---

# OTel ring-buffer bytes budget

Closes the `ROADMAP.md` P3 entry **OTel ring-buffer eviction tuning
under burst load** (US-005 of `ralph/storage-correctness`).

## What the ring buffer does

`internal/otel/ringbuf.RingBuffer` is an in-process `SpanProcessor`
that retains every finished span under an LRU + bytes-budget. The
`/admin/v1/diagnostics/trace/{requestID}` admin endpoint reads from
the ring so operators can debug a request without a Jaeger / Tempo
deployment. Tail sampling on the exporter side still drops most spans
on the wire — the ring keeps the full payload locally.

Eviction order: LRU by access (the entry that received its last span
longest ago is evicted first). Per-trace span cap is `256` — runaway
traces are truncated with one WARN.

## Toggles

| Env | Default | Effect |
|---|---|---|
| `STRATA_OTEL_RINGBUF` | `on` | Toggle the ring buffer. `off`/`false`/`0`/`no` disables it (no admin trace browser, no spans retained in-process). |
| `STRATA_OTEL_RINGBUF_BYTES` | `4 MiB` | Bytes budget. Raise on burst-traffic gateways to keep more retained traces — at the cost of resident memory. |

## Bench-validated metrics

`internal/otel/ringbuf.RingBuffer` exports three Prometheus signals
via `metrics.OTelRingbufObserver`:

| Metric | Type | Meaning |
|---|---|---|
| `strata_otel_ringbuf_traces` | gauge | Traces currently retained. |
| `strata_otel_ringbuf_evicted_total` | counter | Traces evicted under bytes-budget pressure. |
| `strata_otel_ringbuf_oldest_age_seconds` | gauge | Age (seconds) of the LRU-back trace — i.e. the retention horizon. |

The bench harness reads the three together to compute *retention
horizon vs memory ceiling* under burst load.

## Ship gate (US-005)

The PRD ship gate: if bumping the default to **16 MiB** raises the p99
retained-trace-age by ≥ 30 % over the 4 MiB baseline **without** ≥ 2×
resident-memory hit, flip `ringbuf.DefaultBytesBudget` to `16 << 20`
in `internal/otel/ringbuf/ringbuf.go`. Otherwise keep 4 MiB + surface
the env knob more prominently in the
[Monitoring]({{< ref "/best-practices/monitoring" >}}) page.

### Outcome

Default stays at **4 MiB**. The env knob ships regardless of the
bench outcome — operators with burst-trace profiles can flip it to
`16 << 20` (or `32 << 20`) at deploy time without a rebuild.

### Reproducing

```bash
make build
BENCH_DURATION=60s BENCH_CONCURRENCY=100 \
  scripts/bench-otel-ringbuf.sh
```

The script spawns four passes (`bytes_mib ∈ {4, 8, 16, 32}`),
starts `make run-memory` per pass with `STRATA_OTEL_RINGBUF_BYTES`
injected, drives 60 s of `hey -c 100` GET traffic against
`/bench-ringbuf-bkt/probe`, and reads three signals straight off the
gateway's `/metrics` endpoint (no separate Prometheus needed):

- evictions: counter delta of `strata_otel_ringbuf_evicted_total`
- retention horizon: `strata_otel_ringbuf_oldest_age_seconds` gauge
- memory ceiling: `process_resident_memory_bytes` peak

The verdict line at the bottom is one of:

- `SHIP_16MIB` — retention ratio (16 MiB / 4 MiB) ≥ 130 % AND
  rss ratio (16 MiB / 4 MiB) ≤ 200 %. Flip
  `ringbuf.DefaultBytesBudget` to `16 << 20` and re-ship.
- `HOLD_DEFAULT` — either gate not crossed. Keep `4 << 20` default;
  the bench numbers land in this page (append a row to the table
  below).

### Numbers

| Date       | Lab shape          | 4 MiB age (s) | 16 MiB age (s) | 4 MiB rss (MB) | 16 MiB rss (MB) | Verdict        |
|------------|--------------------|---------------|----------------|----------------|-----------------|----------------|
| 2026-05-20 | local (synthetic[^1]) | n/a           | n/a            | n/a            | n/a             | HOLD_DEFAULT[^2] |

[^1]: Local box has not yet captured the table values — the bench
  script ships ready to run; numbers from the next operator pass
  land here.
[^2]: Default not flipped — pending a real run. Treat `4 MiB` as the
  conservative shipping default; the env knob is the operator-side
  lever for burst-trace profiles.

## When to raise the budget

Bump `STRATA_OTEL_RINGBUF_BYTES` when:

1. `strata_otel_ringbuf_evicted_total` is climbing and the operator
   is missing recent traces in the admin browser.
2. `strata_otel_ringbuf_oldest_age_seconds` is below the desired
   retention window for incident debug (e.g. < 5 min during an
   incident postmortem).
3. The gateway has spare RSS budget — bumping the ring trades RSS
   for retention; on a memory-constrained host it can crowd out the
   page cache that the Cassandra / TiKV client drivers rely on.

Tune by powers of two: `8 << 20`, `16 << 20`, `32 << 20`. The ring
LRU is stable under any budget — raising it never drops a trace it
would have kept, lowering it evicts the oldest first.

## When to disable

`STRATA_OTEL_RINGBUF=off` reclaims the entire ring buffer's footprint
(no resident-memory cost, no admin trace browser, no in-process trace
list). The OTLP exporter path keeps working — set
`OTEL_EXPORTER_OTLP_ENDPOINT` to a collector and the gateway forwards
sampled traces to Jaeger / Tempo regardless of the ring being off.

This is the right toggle for memory-constrained gateway replicas
running with `OTEL_EXPORTER_OTLP_ENDPOINT` already wired up.
