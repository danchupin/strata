---
title: 'Workers'
weight: 40
description: 'The background loops Strata runs alongside the gateway.'
---

# Workers

A Strata deployment runs the same `strata` binary in two modes: the gateway
handles S3 traffic, and one or more **workers** handle the background work
that does not belong on the request path. Workers run inside the same
binary you already deploy — set `STRATA_WORKERS=gc,lifecycle,…` on a
gateway replica and it spawns those loops alongside the HTTP listener.

Every worker is **leader-elected**: across N gateway replicas, one replica
acquires the lease for a given worker and runs it; the others stand by and
take over if the leader's lease expires. Leases are scoped per-worker, so
different replicas can lead different workers — `gc` on replica A,
`lifecycle` on replica B. Workers panic-restart on exponential backoff so a
single iteration error never takes a worker offline permanently.

The workers Strata ships with:

## Garbage collection — `gc`

Walks the chunk-deletion queue and removes chunks that are no longer
referenced by any object manifest. Sources of work include `DeleteObject`,
lifecycle expirations, multipart aborts, and CAS losers from concurrent
overwrites. Scaled across multiple replicas via `STRATA_GC_SHARDS`.

## Lifecycle — `lifecycle`

Applies bucket lifecycle policies. Transitions move objects between
storage classes (`STANDARD` → `INTELLIGENT_TIERING`, for example);
expirations delete objects; multipart-abort rules clean up stale
multipart uploads. Per-bucket lease prevents two replicas from working
the same bucket concurrently.

## Rebalance — `rebalance`

Migrates chunks off draining clusters. Walks every bucket whose placement
policy references the draining cluster, plans per-chunk moves, and
streams the bytes to the destination cluster honoring each bucket's
policy and per-cluster weights. Rate-limited by
`STRATA_REBALANCE_RATE_MB_S`.

## Replicator — `replicator`

Drains the bucket-replication queue. Replication is configured per bucket
via `PutBucketReplication`; the worker streams writes to a peer Strata
endpoint over HTTP PUT, applying filter and storage class transforms.

## Notification — `notify`

Drains the event-notification queue. Bucket notifications configured via
`PutBucketNotification` fan out to webhook or SQS sinks listed in
`STRATA_NOTIFY_TARGETS`. Failed deliveries route to a dead-letter queue
for inspection.

## Access log — `access-log`

Drains the per-bucket access-log buffer. When a bucket has logging
enabled via `PutBucketLogging`, every request gets buffered; this worker
periodically writes one AWS-format log object per flush into the target
bucket.

## Inventory — `inventory`

Walks each bucket on its scheduled cadence and writes an inventory
manifest (`manifest.json` + gzipped CSV) into the configured target
bucket. Inventory configurations are managed via
`PutBucketInventoryConfiguration`.

## Audit export — `audit-export`

Exports audit log rows older than `STRATA_AUDIT_EXPORT_AFTER` into
gzipped JSON-lines objects in the configured export bucket, then deletes
the source rows. Keeps the audit log table from growing without bound
while preserving the history for compliance.

## Manifest rewriter — `manifest-rewriter`

One-shot-style worker that walks every object and rewrites its manifest
blob into the latest encoding format. Idempotent — re-runs skip
already-converted rows. Run this after upgrading to a Strata version that
changes the on-disk manifest encoding.

## Usage rollup — `usage-rollup`

Nightly job that samples per-bucket usage counters and writes one
aggregate row per (bucket, storage class, day) into the usage table. The
output feeds external billing systems.

## Choosing what to run

You can run workers on dedicated replicas (gateway-only replicas plus
worker-only replicas) or on every replica with the supervisor handling
leader election across them. For most deployments, set
`STRATA_WORKERS=gc,lifecycle,notify,access-log,inventory,audit-export,usage-rollup`
on every gateway replica and let leader election sort out the assignments.
Run `rebalance` only on replicas in the region where you want the
migration bandwidth to originate. See
[Architecture: worker leader election]({{< relref "/architecture/" >}})
for the implementation model and
[Monitoring]({{< relref "/operate/monitoring" >}}) for the metrics
each worker exports.
