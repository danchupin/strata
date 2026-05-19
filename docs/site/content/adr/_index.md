---
title: 'Architecture Decision Records'
weight: 45
bookFlatSection: true
description: 'Captured design decisions for Strata — context, decision, and consequences for the load-bearing trade-offs.'
---

# Architecture Decision Records

An Architecture Decision Record (ADR) captures one significant design
decision: the context that forced the choice, the option that was taken,
and the consequences that fall out of it. ADRs are append-only — when a
decision is reversed or replaced, a new ADR is authored and the old one
is marked `Superseded by ADR-XXXX`, not deleted. They exist so a future
contributor (or a future you) can recover the rationale without
spelunking commit history.

Strata's ADRs cover the foundational decisions made during the MVP and
"modern complete" phases. They link to the code paths they constrain so
the contract is visible at the point of change.

## ADR template

```
# ADR-NNNN: <title>

## Status

Accepted | Superseded by ADR-XXXX | Deprecated — <date>

## Context

What problem are we solving? What constraints apply? What did we
consider but reject?

## Decision

The choice we made, in present tense ("We use X to do Y.").

## Consequences

What follows from this decision — positive, negative, and the invariants
the rest of the codebase now depends on.
```

## Index

| ID | Title |
|---|---|
| [ADR-0001]({{< ref "/adr/0001-skip-rados-omap" >}}) | Skip RADOS omap for bucket index |
| [ADR-0002]({{< ref "/adr/0002-islatest-read-time" >}}) | Derive `IsLatest` at read time |
| [ADR-0003]({{< ref "/adr/0003-manifest-blob-column" >}}) | Single `manifest` blob column for object metadata |
| [ADR-0004]({{< ref "/adr/0004-leader-per-worker" >}}) | One leader lease per background worker |
