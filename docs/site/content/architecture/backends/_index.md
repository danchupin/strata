---
title: 'Backends'
weight: 40
bookFlatSection: true
description: 'Production-grade metadata + data backend operator guides.'
---

# Backends

Strata ships first-class production backends for both layers.

| Layer | Backends |
|---|---|
| Metadata | Cassandra (default), [ScyllaDB]({{< ref "/architecture/backends/scylla" >}}) (CQL drop-in), [TiKV]({{< ref "/architecture/backends/tikv" >}}) (raw KV with native ordered scan) |
| Data | RADOS (default, 4 MiB chunks), [S3-over-S3]({{< ref "/architecture/backends/s3" >}}) (any S3-compatible upstream), memory (tests only) |
