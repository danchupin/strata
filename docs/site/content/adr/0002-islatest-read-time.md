---
title: 'ADR-0002: Derive IsLatest at read time'
weight: 2
---

# ADR-0002: Derive `IsLatest` at read time

## Status

Accepted — April 2026

## Context

S3 versioning requires every object row to expose an `IsLatest` bit:
the most recent (un-deleted) version of a key carries `IsLatest=true`,
every older version carries `IsLatest=false`. The naive
implementation flips the bit on the previous head whenever a new PUT
lands — `UPDATE objects SET is_latest=false WHERE bucket_id=? AND
key=? AND version_id=<prev>` immediately after inserting the new
version row.

That approach has two costs on Cassandra:

- **Write amplification.** Every PUT becomes two LWT round trips —
  the insert of the new version, plus the flip of the previous
  head. On a write-heavy bucket the flip dominates p99.
- **Coordination required to find the previous head.** The flip
  needs the previous version-id, which is itself a scan or a
  cached-state read. Either path is a coherence hazard if
  concurrent PUTs race.

We could persist the previous head id alongside the new row to
avoid the scan, but the write-amplification cost remains and the
schema gets more brittle.

## Decision

We do not flip `IsLatest` on PUT. The bit is derived at read time
from the clustering order of the `objects` table:

```
PARTITION BY (bucket_id, shard)
CLUSTERING ORDER BY key ASC, version_id DESC
```

The version-id is encoded so that the lex-largest id sorts first
within the same key. The first row emitted for any key during a
range scan is therefore the latest version — `IsLatest=true` is
synthesised in the scan loop without consulting any persisted
column. `ListObjects` carries an in-memory dedupe pass (one
`cursorHeap`-per-key) that emits `IsLatest=true` once per key, then
`IsLatest=false` for the rest of its versions, all in a single
sequential scan.

For TiKV the same property is preserved by encoding the version
suffix as `[MaxUint64 - ts8-BE][raw-uuid-16]` (24 bytes total); the
inverted timestamp makes an ascending range scan emit the latest
version first. The null sentinel UUID (timestamp 0) sorts last
among versions of a key, so `?versionId=null` resolves via exact
lookup, not scan-position arithmetic. See `internal/meta/tikv/keys.md`
for the full key-layout spec.

## Consequences

- **Zero write amplification on PUT.** A new version is one LWT
  insert. The previous head row is untouched.
- **Schema invariant: clustering order is load-bearing.** Both
  Cassandra `CLUSTERING ORDER BY key ASC, version_id DESC` and the
  TiKV version-suffix encoding are part of the public meta-store
  contract — they cannot be changed without rewriting the listing
  code path. Tests in `internal/meta/storetest/contract.go` exercise
  the ordering against both backends.
- **List path carries the dedupe pass.** `ListObjects` cannot be a
  blind partition scan; it must merge versions per key. The
  `versionHeap` lives in `cassandra/store.go`; the TiKV backend
  short-circuits via its native ordered scan but still emits one
  `IsLatest=true` per key.
- **GET-without-versionId is free.** Resolving the latest version
  is a `LIMIT 1` on the (bucket_id, shard, key) prefix — the first
  row hit is the answer. No "find current head" pre-query.
- **Delete-marker semantics fit naturally.** A delete-marker is a
  version row with `is_delete_marker=true` and the largest
  version-id of the key. The scan emits it as `IsLatest`, so GETs
  return 404 without a special case.
