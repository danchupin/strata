---
title: 'Backup + restore'
weight: 50
description: 'Bucket inventory, RADOS pool snapshots, Cassandra/TiKV snapshot strategy, cross-region replication.'
---

# Backup + restore

Strata splits backup responsibility along the same tiers the gateway
splits state into:

- **Metadata tier (Cassandra / TiKV):** the source of truth for
  bucket / object / IAM rows. Backed up via the upstream tooling for
  each backend.
- **Data tier (RADOS / S3-over-S3):** chunk bytes. Backed up via Ceph
  pool snapshots or the upstream S3 service's native versioning /
  replication.
- **Cross-region replication (replicator worker):** ships object PUT /
  DELETE events to a peer Strata cluster in near-real time.
- **Inventory worker:** writes manifest.json + CSV.gz pairs that
  document every object in a bucket — useful as an audit ledger for
  external backup tooling.

This page is a backup-strategy overview. Implementation details live in
[Architecture — Workers]({{< ref "/architecture/workers" >}}) (the
inventory + replicator workers) and the upstream docs for Ceph /
Cassandra / TiKV.

## Metadata snapshots

### Cassandra

- **Snapshot:** `nodetool snapshot <keyspace>` per node, on a rolling
  schedule. Hard-links the SSTables into a snapshot directory on the
  same volume — fast, no IO impact, but consumes disk on the same
  filesystem until cleared.
- **Restore:** stage SSTables back into the keyspace per the upstream
  Cassandra runbook. Strata's schema migrations are additive
  (`internal/meta/cassandra/schema.go::tableDDL` + `alterStatements`
  are idempotent), so restoring from an older snapshot + rolling the
  current binary will re-apply any new ALTERs on first boot.
- **Off-cluster ship:** combine `nodetool snapshot` with
  Medusa or a similar streaming backup tool to ship snapshots off-host
  before clearing. The Cassandra LWT consistency story holds across
  snapshots — restored rows preserve the Paxos state at snapshot time.

### TiKV

- **Snapshot:** PD-coordinated `br backup` from the upstream TiKV
  toolchain. Region-consistent snapshot to a local directory or S3.
  Schedule per the upstream guide.
- **Restore:** `br restore` against a fresh PD + TiKV cluster.
  Pessimistic-txn state is preserved since `br` operates on the
  region's raft log.
- **TiKV has no native TTL** — Strata-side row expiry is enforced via
  `ExpiresAt` in the row payload + a leader-elected sweeper goroutine
  (`internal/meta/tikv/sweeper.go`). Snapshots include un-expired and
  not-yet-swept-expired rows; readers lazy-skip expired rows on
  `Get`/`List` so a restored snapshot is correct without sweeper
  catch-up.

### Memory backend

`memory` is for tests + smoke pass — not durable, no backup story.

## Data snapshots

### RADOS (Ceph)

- **Pool snapshots:** `rados mksnap` against the chunk pool freezes a
  point-in-time view; `rados rollback <obj> <snap>` restores per
  object. Whole-pool rollback is supported but disruptive — all clients
  must drain first.
- **Cross-cluster replication:** RBD-mirror / `rados export` /
  `rados import` if the operator wants a passive secondary. Ceph's
  native multi-site is upstream territory.
- **Manifest dependency:** Strata's metadata tier holds the manifest
  that points at chunk OIDs in the pool. A pool snapshot taken without
  a coordinated metadata snapshot will be **inconsistent under writes
  in flight** — pause writes (or accept eventual GC of orphan chunks)
  before snapshotting both tiers.

### S3-over-S3

When `STRATA_DATA_BACKEND=s3`, the upstream S3 endpoint owns the
durability story. Use the upstream's versioning / replication; Strata
sees raw object bytes only.

## Cross-region replication

The replicator worker (`--workers=replicator`) drains the
`replication_queue` table and dispatches PUTs to a peer Strata cluster
over HTTP (HTTPDispatcher). This is **near-real-time replication**, not
backup — failed deliveries retry up to a budget then drop into FAILED
state. Ship metrics:

- `strata_replication_queue_age_seconds{bucket=...}` — oldest pending
  row per source bucket. Alert on > 600 s.
- `strata_replication_queue_depth{rule_id=...}` — pending rows per
  rule. Alert on sustained growth.
- `strata_replication_failed_total{rule_id=...}` — rows that exhausted
  retries. Alert on any non-zero rate.

Configure replication per bucket via
`PUT /<bucket>?replication` (S3 ReplicationConfiguration). Each rule
ships PUTs + DELETEs from a source bucket to a peer-cluster bucket
identified by the rule's `Destination`.

Use replication for:

- Geo-redundancy (peer cluster in another region).
- Read-side fan-out (peer cluster as a read replica for low-latency
  reads from a far region).

Replication is **not**:

- A point-in-time backup — the peer is a live mirror; bad writes /
  deletes propagate.
- A cold archive — the peer needs the same metadata + data backend
  footprint as the source.

For ledger-style cold archive, use the inventory worker or a separate
S3-tier dump.

## Bucket inventory worker

The inventory worker (`--workers=inventory`) ticks per
`(bucket, configID)`, walks every object in the source bucket, and
writes a `manifest.json` + paginated `CSV.gz` pair into the configured
target bucket. Configure via `PUT /<bucket>?inventory` (S3
InventoryConfiguration).

The inventory output is not a backup — it's a manifest that external
tooling (e.g. an S3-tier backup pipeline) can use as the source-of-truth
list of objects to ship. Pair with the replicator (live mirror) or with
a cron'd `aws s3 sync` (cold archive).

Cadence is driven by the inventory configuration; daily is a reasonable
default. The output objects land in the target bucket exactly as AWS S3
inventory does, so any tool that consumes S3 inventory output works
unchanged.

## Audit log retention

Audit rows live in the `audit_log` table (Cassandra) or a TiKV prefix.
Retention is set via `STRATA_AUDIT_RETENTION` (default 30 days):

- Cassandra: `USING TTL` expunges rows for free.
- TiKV: the audit-export worker (`--workers=audit-export`) drains
  partitions older than `STRATA_AUDIT_EXPORT_AFTER` (default 30 d)
  into gzipped JSON-lines objects in the configured export bucket,
  then deletes the source partitions. Set the export bucket via
  `STRATA_AUDIT_EXPORT_BUCKET` and enable the worker via
  `STRATA_WORKERS=...,audit-export`.

The exported JSON-lines bundles are the canonical long-term audit
archive on TiKV deploys. Ship them to off-cluster storage on the same
schedule as the metadata snapshots.

## Coordinated full-cluster snapshot

For a coordinated full-cluster snapshot (rare; usually only needed for
forensic compliance):

1. **Quiesce writes.** Drain the LB, fail readiness on every gateway
   replica.
2. **Snapshot the metadata backend.** `nodetool snapshot` /
   `br backup`.
3. **Snapshot the data backend.** `rados mksnap` against every active
   chunk pool, OR an upstream S3 versioning marker.
4. **Drain the GC + replication queues** so every write recorded in the
   metadata snapshot is also visible in the data snapshot. Conservative
   target: queue depth zero for ≥ one full `STRATA_GC_INTERVAL`.
5. **Resume writes.** Promote readiness, restore traffic.

For routine periodic backup, skip steps 1 + 4 — Strata's GC-aware design
tolerates orphan chunks (the GC worker eventually expunges them) and
orphan metadata rows (manifests pointing at deleted chunks return 404
on GET, which the operator will discover via inventory diff). Routine
backup is a per-tier operation; the coordinated full-cluster shape is
overkill for normal recovery-objective scenarios.

## Restore drills

Periodic restore drills are the only verified-working backup. Strata
ships no built-in restore drill harness; the operator-facing approach:

1. Provision a fresh metadata + data backend stack.
2. Restore the metadata snapshot per the upstream backend's runbook.
3. Restore the data snapshot per the data backend's runbook.
4. Roll a single Strata gateway against the restored stack with
   `STRATA_AUTH_MODE=disabled` (so SigV4 verification doesn't reject
   on a stale clock).
5. Run `make smoke` — the smoke pass exercises every S3 surface that
   Strata implements and is the cheapest end-to-end gate.

If `make smoke` passes, the snapshot is restorable. Schedule the drill
quarterly at a minimum.

## See also

- [Architecture — Workers]({{< ref "/architecture/workers" >}}) for
  the inventory + replicator + audit-export worker shapes.
- [Capacity planning]({{< ref "/best-practices/capacity-planning" >}})
  for storage growth math under lifecycle + replication.
- [Monitoring]({{< ref "/best-practices/monitoring" >}}) for the
  replication / inventory / audit-export metrics.
