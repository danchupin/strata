---
title: 'Migrations'
weight: 60
bookFlatSection: true
description: 'Operator-facing migration runbooks for major shape changes.'
---

# Migrations

- [Binary consolidation]({{< ref "/architecture/migrations/binary-consolidation" >}}) — moving from 11 `cmd/*` binaries to the single `strata` binary (`server` + `admin` subcommands).
- [GC + lifecycle Phase 2]({{< ref "/architecture/migrations/gc-lifecycle-phase-2" >}}) — sharded leader election cutover.
