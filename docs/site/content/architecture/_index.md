---
title: 'Architecture'
weight: 60
bookFlatSection: true
description: 'Layer-by-layer architecture: auth, router, meta-store, data-backend, workers, sharding, observability, plus deep dives for the four critical flows.'
---

# Architecture

Strata is an S3-compatible gateway built on three swappable tiers: an
HTTP S3 surface, an ordered metadata store, and a chunked data
backend. The responsibilities are deliberately narrow at each layer so
a backend swap (memory → Cassandra → TiKV; memory → RADOS → S3-over-S3)
drops in without touching the router or the workers. A single `strata`
binary plays both gateway and worker roles — the HTTP listener serves
S3 traffic, and `STRATA_WORKERS=` opts the same process into one or
more background loops.

The router (`s3api.Server`) is a flat query-string dispatcher that
mirrors the AWS S3 wire shape: every sub-resource (`?cors`, `?policy`,
`?uploads`, `?uploadId=…`) is keyed by the presence of a query
parameter. Auth runs ahead of any path rewriting so SigV4 signs the
original URL, and the admin carve-out (`/admin/v1/…`) bypasses S3
dispatch entirely. The router never talks to a backend directly — it
funnels through the `meta.Store` and `data.Backend` interfaces.

The metadata layer (`meta.Store`) is intentionally minimal: compare-
and-set on object manifests, range scans with clustering order, blob
config CRUD for per-bucket policies. Three first-class backends
implement it — Cassandra (the original sharded-fan-out shape with
ScyllaDB as a CQL-compatible drop-in) and TiKV (raw KV with native
ordered range scans that short-circuit the fan-out via the optional
`meta.RangeScanStore` interface). The in-memory backend exists for
tests and the smoke pass.

The data backend (`data.Backend`) handles opaque fixed-size chunks
only — the per-object manifest lives in the metadata layer, the data
backend never reads it. RADOS splits every object body into 4 MiB
chunks; S3-over-S3 streams through upstream multipart; the in-memory
backend keeps a `[]byte` per chunk. Multi-cluster routing
(`internal/data/placement/`) is a thin layer that picks one cluster
per PUT from the bucket's placement policy, the per-cluster weight
wheel, and the drain map.

Background workers (gc, lifecycle, notify, replicator, access-log,
inventory, audit-export, manifest-rewriter, rebalance, usage-rollup,
quota-reconcile) run inside the same binary. Each worker is leader-
elected on a per-name lease, panic-recovered with exponential backoff,
and supervised so one worker's failure never touches the gateway or
sibling workers. The fan-out workers (gc, rebalance) split work
across shards with one lease per shard so a single replica can drain
multiple shards in parallel without coordinating with siblings.

## Component map

```mermaid
flowchart LR
    Client["S3 client<br/>(aws-cli, mc, SDK)"] -->|HTTPS SigV4| Auth["auth.Middleware"]
    Auth --> Router["s3api.Server"]
    Router --> Meta[("meta.Store<br/>Cassandra | ScyllaDB | TiKV | memory")]
    Router --> Data[("data.Backend<br/>RADOS | S3 | memory")]
    Router -.-> Admin["/admin/v1/* handlers"]
    Admin --> Meta
    Supervisor["workers.Supervisor"] --> Workers["gc · lifecycle · rebalance · notify ·<br/>replicator · access-log · inventory ·<br/>audit-export · manifest-rewriter · usage-rollup"]
    Workers --> Meta
    Workers --> Data
    Supervisor --> Leases["leader.Session<br/>(per-worker lease)"]
    Leases --> Meta
```

## Critical flows

{{% columns %}}
- {{< card href="/architecture/put-flow/" >}}
  **PUT flow**  
  Object PUT end-to-end — SigV4, manifest compare-and-set, chunk
  write, CAS-loser cleanup. Sequence diagram of the hot path.
  {{< /card >}}

- {{< card href="/architecture/multi-cluster-routing/" >}}
  **Multi-cluster routing**  
  How a PUT picks a cluster — bucket-policy short-circuit, cluster
  weight wheel, drain exclusion, storage-class spec. Flowchart of
  the picker.
  {{< /card >}}

- {{< card href="/architecture/drain-pipeline/" >}}
  **Drain pipeline**  
  Cluster lifecycle (`live` → `draining_*` → `removed`), rebalance
  worker scan loop, deregister-ready gate. State diagram + safety
  rails.
  {{< /card >}}
{{% /columns %}}

{{% columns %}}
- {{< card href="/architecture/worker-leader-election/" >}}
  **Worker + leader election**  
  Supervisor → `leader.Session` → heartbeat chip → shard fan-out.
  How a worker lifts off and how panics are isolated.
  {{< /card >}}
{{% /columns %}}

## Per-layer pages

{{% columns %}}
- {{< card href="/architecture/auth/" >}}
  **Auth**  
  SigV4, presigned URLs, streaming chunk decoder, virtual-hosted-
  style routing, identity attribution.
  {{< /card >}}

- {{< card href="/architecture/router/" >}}
  **Router**  
  `s3api.Server` query-string dispatch shape, vhost rewriting,
  admin path carve-out.
  {{< /card >}}

- {{< card href="/architecture/meta-store/" >}}
  **Meta store**  
  The `meta.Store` contract, LWT semantics, range scans, the
  optional `RangeScanStore` short-circuit.
  {{< /card >}}
{{% /columns %}}

{{% columns %}}
- {{< card href="/architecture/data-backend/" >}}
  **Data backend**  
  RADOS 4 MiB chunking, manifest format (proto vs JSON sniff,
  schema-additive evolution), multi-cluster routing, S3-over-S3.
  {{< /card >}}

- {{< card href="/architecture/workers/" >}}
  **Workers**  
  Supervisor model, registration via `init()`, leader-election
  shape, panic restart with backoff, per-worker reference.
  {{< /card >}}

- {{< card href="/architecture/sharding/" >}}
  **Sharding**  
  `(bucket_id, shard)` partitioning, fan-out merge, gc fan-out,
  online reshard.
  {{< /card >}}
{{% /columns %}}

{{% columns %}}
- {{< card href="/architecture/observability/" >}}
  **Observability**  
  slog, audit log, request_id propagation, OTel tracing
  (tail-sampler + ring buffer), per-storage observers.
  {{< /card >}}

- {{< card href="/architecture/storage/" >}}
  **Storage**  
  Meta + data backend health surfacing in the operator console.
  {{< /card >}}
{{% /columns %}}

## Per-backend deep dives

{{% columns %}}
- {{< card href="/architecture/backends/" >}}
  **Backends**  
  TiKV, ScyllaDB, S3-over-S3 — capabilities, gotchas, when to pick
  which.
  {{< /card >}}

- {{< card href="/architecture/benchmarks/" >}}
  **Benchmarks**  
  GC + lifecycle scaling, meta-backend comparison, RADOS ops,
  parallel-chunk read.
  {{< /card >}}

- {{< card href="/architecture/migrations/" >}}
  **Migrations**  
  Binary consolidation, GC + lifecycle Phase 2, TiKV-default lab,
  drain-progress physicalisation.
  {{< /card >}}
{{% /columns %}}
