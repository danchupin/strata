---
title: 'Migrations'
weight: 60
bookFlatSection: true
description: 'Operator-facing migration runbooks for major shape changes.'
---

# Migrations

- [Binary consolidation]({{< ref "/architecture/migrations/binary-consolidation" >}}) — moving from 11 `cmd/*` binaries to the single `strata` binary (`server` + `admin` subcommands).
- [GC + lifecycle Phase 2]({{< ref "/architecture/migrations/gc-lifecycle-phase-2" >}}) — sharded leader election cutover.
- [Compose collapse]({{< ref "/architecture/migrations/compose-collapse" >}}) — single canonical multi-cluster strata service; legacy single-cluster + features sidecars removed; new `lab-cassandra-3` profile for multi-replica HA validation.
- [Drain progress physical chunks]({{< ref "/architecture/migrations/drain-progress-physical" >}}) — `/drain-progress` gains three additive fields (`physical_chunks_on_cluster`, `physical_bytes_on_cluster`, `gc_queue_pending`); UI surfaces physical RADOS count as primary with a 3-state machine; back-compat fallback for memory + S3 backends.
