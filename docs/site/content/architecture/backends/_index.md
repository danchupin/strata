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
| Metadata | [TiKV]({{< ref "/architecture/backends/tikv" >}}) (raw KV with native ordered scan, **lab default**), Cassandra (first-class in code, available via `--profile cassandra`), [ScyllaDB]({{< ref "/architecture/backends/scylla" >}}) (CQL drop-in for Cassandra) |
| Data | RADOS (default, 4 MiB chunks), [S3-over-S3]({{< ref "/architecture/backends/s3" >}}) (any S3-compatible upstream), memory (tests only) |

## Lab compose shape

The bare `docker compose up -d` is the **TiKV-default 2-replica lab**:
PD + TiKV for metadata, ceph + ceph-b as the multi-cluster RADOS pair,
two strata replicas (`strata-a` / `strata-b`) behind an nginx LB on
`:9999`. `make up-cassandra` (= `docker compose --profile cassandra up
-d`) layers a single Cassandra-backed strata replica
(`strata-cassandra` :9998) on top so both metadata backends can be
exercised side-by-side.

The Cassandra meta backend remains **first-class in code** —
`internal/meta/cassandra/**` is unchanged, the storetest contract
suite still covers it at parity with TiKV, and `make test-integration`
(testcontainers Cassandra) is preserved. The lab default flip is a
deployment shape change, not a backend deprecation. See the
[migration note]({{< ref "/architecture/migrations/tikv-default-lab" >}})
for the operator-facing checklist.
