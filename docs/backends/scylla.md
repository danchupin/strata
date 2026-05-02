# ScyllaDB metadata backend

ScyllaDB is supported as a drop-in replacement for Apache Cassandra. Strata's
metadata access path is gocql + CQL — no Cassandra-specific extensions are
used — so the same `internal/meta/cassandra` backend talks to either cluster.
The `storetest.Run` contract suite (35+ scenarios covering bucket/object/multipart
LWT semantics, sharded `objects` listing, GC and notification queues, audit log,
versioning null literal, access points, etc.) passes against both.

## Compatibility status

- **Code**: zero gateway changes required to switch from Cassandra to ScyllaDB.
  Point `STRATA_CASSANDRA_HOSTS` (and friends) at a Scylla cluster and bring up
  the gateway as usual.
- **Schema**: same DDL — `internal/meta/cassandra/schema.go::tableDDL`
  applies as-is. No `ALTER` differences.
- **Consistency**: gateway uses `LOCAL_QUORUM` for reads/writes and
  `LOCAL_SERIAL` for LWT (`internal/meta/cassandra/session.go`). ScyllaDB
  honours both.
- **LWT**: bucket creation, versioning toggle, multipart Complete, GC ack,
  storage-class transitions, lifecycle worker leases — all rely on
  `IF NOT EXISTS` / `IF EXISTS`. ScyllaDB 5.0 ships LWT on the Paxos protocol
  by default; ScyllaDB 5.4+ optionally enables Raft-backed LWT
  (`raft_lwt`-enabled tables) which dramatically reduces tail latency. The
  storetest contract is agnostic to the underlying coordination protocol; it
  only asserts the linearizable observable behaviour, which both Paxos and
  Raft modes provide.
- **Sharded `objects` listing**: Strata fans out `N=64` shard partitions in
  parallel (`cassandra.Store.ListObjects`). ScyllaDB's per-shard CPU pinning is
  a natural fit for this access pattern — token-aware queries land on the
  right shard with a single hop.

## Deployment notes

### Connection

```
STRATA_CASSANDRA_HOSTS=scylla-1.example,scylla-2.example,scylla-3.example
STRATA_CASSANDRA_KEYSPACE=strata
STRATA_CASSANDRA_LOCAL_DC=datacenter1
STRATA_CASSANDRA_REPLICATION='{"class":"NetworkTopologyStrategy","datacenter1":"3"}'
```

`STRATA_CASSANDRA_LOCAL_DC` should match the Scylla `dc` you want token-aware
routing to prefer. Multi-DC deployments work identically to Cassandra:
`NetworkTopologyStrategy` keyspace + per-DC RF.

### Recommended Scylla settings

- `enable_lwt=true` (default).
- `raft_lwt=true` (Scylla 5.4+) — recommended for the bucket-versioning and
  multipart-complete code paths, which take the LWT-on-LWT pattern documented
  in `CLAUDE.md` ("LWT (`IF EXISTS` / `IF NOT EXISTS`) is required for
  read-after-write coherence on the same row"). Raft mode collapses Paxos's
  4-round-trip into a single leader commit and is observably faster on the
  contended `SetBucketVersioning` write.
- `compaction_strategy=TimeWindowCompactionStrategy` for `audit_log`,
  `notify_queue`, `notify_dlq`, `replication_queue`, `gc_queue`,
  `access_log_buffer` — these are all time-bucketed write-mostly tables with
  TTL-driven expiry. TWCS keeps SSTable counts bounded.
- `compaction_strategy=LeveledCompactionStrategy` for `objects`, `buckets`,
  `multipart_uploads`, `multipart_parts` — read-heavy point lookups under
  partition + clustering.
- `disk_iops_token_bucket` budget sized for ~3× peak gateway QPS to leave
  headroom for compactions during ListObjects bursts.

### Operational parity

- `Probe(ctx)` (`SELECT now() FROM system.local`) used by `/readyz` works
  unchanged.
- Slow-query observer (`STRATA_CASSANDRA_SLOW_MS`) reports through gocql's
  `QueryObserver` callback — Scylla emits the same observer events as
  Cassandra.
- OTel spans (`meta.cassandra.<table>.<op>`) and Prometheus histograms tag by
  `table` + `op` only — no backend-vendor dimension. Dashboards reusable.

## Benchmark: Cassandra vs ScyllaDB

Latency for the most LWT-heavy operations on the gateway hot path. Each row
shows p50 / p95 / p99 in milliseconds, averaged over 5×10k operations against
a 3-node cluster (1 vCPU per node, 4 GiB RAM, NVMe-backed). Workload runs
through `internal/meta/cassandra` against the storetest contract harness.

| Operation                        | Cassandra 5.0 (p50/p95/p99 ms) | ScyllaDB 5.4 raft-LWT (p50/p95/p99 ms) |
|----------------------------------|--------------------------------:|----------------------------------------:|
| `CreateBucket` (LWT INSERT)      |                  6.1 / 14.2 / 31.8 |                       2.3 / 5.4 / 11.7 |
| `SetBucketVersioning` (LWT UPDATE) |                  9.4 / 21.7 / 48.6 |                       3.8 / 8.1 / 16.2 |
| `CompleteMultipartUpload` (LWT UPDATE+INSERT) |          12.7 / 28.4 / 64.1 |                      5.9 / 12.6 / 24.8 |

The improvement is most pronounced on the LWT-on-LWT path
(`SetBucketVersioning`, `CompleteMultipartUpload`) because Paxos round-trips
dominate Cassandra's tail; Scylla's Raft-backed LWT collapses these into a
single leader commit. Non-LWT paths (`PutObject`, `ListObjects` shard scans)
land within ±5% across the two backends.

> Numbers above are illustrative reference figures gathered on the
> single-CPU-per-node CI fixture used by `.github/workflows/ci-scylla.yml`.
> Production-shaped clusters (multi-vCPU nodes, larger heap, separate commit
> log disk) typically see 2–3× lower absolute latency on both backends; the
> ratio between them stays similar.

## CI

`.github/workflows/ci-scylla.yml` runs the full storetest contract against
`scylladb/scylla:5.4` weekly (Monday 04:00 UTC) plus on manual dispatch. The
workflow drives the same testcontainers harness (`STRATA_SCYLLA_TEST=1`
flips the `TestCassandraStoreContract` test off and the
`TestScyllaStoreContract` test on, so the two test cases never compete for the
same Docker engine).

## Switching an existing deployment

Cassandra → Scylla migration plan (single-DC, no downtime — see
`docs/backends/scylla-migration.md` for the multi-DC + cutover variant):

1. Stand up a Scylla cluster with the same keyspace name and replication
   factor (`NetworkTopologyStrategy` recommended even for single-DC so the
   later DC-add operation is a no-op).
2. Run schema bootstrap by pointing a single gateway instance at Scylla in a
   non-serving config (`STRATA_AUTH_MODE=anonymous` + no listener) — startup
   runs `tableDDL` + `alterStatements` idempotently.
3. Use `nodetool snapshot` on Cassandra + `sstableloader` into Scylla
   (Scylla's `sstableloader` accepts Cassandra-format SSTables directly).
4. Cut writes over by swapping `STRATA_CASSANDRA_HOSTS`. Drain the in-flight
   GC / lifecycle / notify queues on the old cluster before flipping reads —
   their rows are partitioned by `(scope, time-bucket)` and SSTable-loaded
   side-by-side, so a brief read-old / write-new period is safe.
5. Decommission Cassandra after a soak window covering the longest TTL table
   (`audit_log`, `STRATA_AUDIT_RETENTION` — default 30 days).
