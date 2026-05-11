# TiKV key encoding (US-002)

This document is the source of truth for how Strata's compound metadata
keys are flattened onto TiKV's flat byte-keyed KV store. Every story
under `internal/meta/tikv` (US-003 .. US-016) uses the encoders in
`keys.go` and follows the conventions documented here.

## Top-level namespace

All Strata keys begin with the two-byte prefix `s/`. A single byte plus
a separator is the cheapest non-zero discriminator we can claim, and it
keeps Strata's keyspace small enough to share a TiKV cluster with other
tenants if the operator chooses to.

```
s/<entity discriminator>/<entity-specific tail>
```

The constants live in `keys.go`; do not hand-roll prefixes.

## Variable-length string segments

User-space strings (bucket names, S3 object keys, upload IDs, IAM user
names, access-key IDs, region strings, audit event IDs, lock names) are
**byte-stuffed** rather than length-prefixed:

| Input byte | Wire bytes |
|------------|------------|
| `0x00`     | `0x00 0xFF` |
| any other  | itself      |
| terminator | `0x00 0x00` |

This is the [FoundationDB tuple-layer
encoding](https://apple.github.io/foundationdb/data-modeling.html#tuples).
We pick it over plain length-prefixing because length prefixes break
lex order across heterogeneous-length segments. Range scans like
"all keys in bucket B starting with prefix `foo/`" require ordered
iteration to behave correctly even when some keys are
`foo/`, `foo/bar`, `foo/bar/baz`, etc. Byte-stuffing preserves that
order: every shorter segment terminates with `0x00 0x00`, which is
strictly less than any extension via `0x00 0xFF`.

Helpers: `appendEscaped`, `readEscaped` (private to the package — call
them only from key-builder functions).

## Object key — version-DESC ordering

Object keys end with a fixed 24-byte version-DESC suffix. The suffix is
deterministic from the version-id UUID:

```
[8 bytes: MaxUint64 - timeuuid_ts_100ns, big-endian]
[16 bytes: raw UUID bytes]
```

The inverted timestamp makes a forward-direction range scan emit the
**latest** version first, which is exactly what `GetObject` (no
versionId) and `ListObjectVersions` need. The raw 16 UUID bytes survive
round-trip and tiebreak when timestamps collide (rare; v1 timeuuids
include a clockseq + node MAC that disambiguate).

The null sentinel UUID (`meta.NullVersionID`, all zeros) has timestamp
`0` and thus encodes as `MaxUint64` in the inverted-ts half — it sorts
**last** among the versions of a given object key. The gateway always
addresses null versions by exact lookup (`?versionId=null` resolves to
the sentinel), never by scan position, so this ordering is safe.

Helpers: `EncodeVersionDesc`, `DecodeVersionDesc`.

## Layout summary

| Entity                      | Key shape |
|-----------------------------|-----------|
| Bucket (by name)            | `s/b/<escName>` |
| Bucket-scoped header        | `s/B/<bucketUUID16>/` |
| Object                      | `s/B/<bucketUUID16>/o/<escKey>` + 24B verDesc |
| Object grants               | `s/B/<bucketUUID16>/og/<escKey>` + 24B verDesc |
| Bucket grants               | `s/B/<bucketUUID16>/g` |
| Bucket config blob          | `s/B/<bucketUUID16>/c/<kind>` |
| Inventory config            | `s/B/<bucketUUID16>/i/<escConfigID>` |
| Multipart upload (status)   | `s/B/<bucketUUID16>/u/<escUploadID>` |
| Multipart part              | `s/B/<bucketUUID16>/up/<escUploadID><partNum4-BE>` |
| Multipart completion record | `s/B/<bucketUUID16>/mc/<escUploadID>` |
| Reshard job                 | `s/Rj/<bucket16>` |
| Reshard cursor (per shard)  | `s/B/<bucketUUID16>/Rc/<shard4-BE>` (reserved) |
| SSE rewrap progress         | `s/B/<bucketUUID16>/rw` |
| IAM user                    | `s/iu/<escUserName>` |
| IAM access key (hot path)   | `s/ik/<escAccessKey>` |
| IAM access keys by user     | `s/iuk/<escUserName><escAccessKey>` |
| Access point                | `s/ap/<escName>` |
| Access point alias index    | `s/aa/<escAlias>` |
| Notify queue                | `s/qn/<bucket16><ts8-BE><escEventID>` |
| Notify DLQ                  | `s/qd/<bucket16><ts8-BE><escEventID>` |
| Replication queue           | `s/qr/<bucket16><ts8-BE><escEventID>` |
| Access-log buffer           | `s/qa/<bucket16><ts8-BE><escEventID>` |
| GC queue                    | `s/qg/<escRegion><ts8-BE><escOID>` |
| Audit log                   | `s/A/<bucket16><day4-BE><escEventID>` |
| Leader lock                 | `s/L/<escLockName>` |
| Cluster registry            | `s/cr/<escID>` |

`bucketUUID16` is the raw 16-byte UUID — fixed-width, no escape needed.
`partNum4-BE` is `uint32` big-endian so a forward scan returns parts in
ascending number order. `day4-BE` is `uint32` BE days-since-epoch UTC,
making `(bucket, day)` audit partitions recoverable from the key — the
audit-export worker (US-046 equivalent for TiKV via the sweeper in
US-009) walks days older than the retention window via this layout.

## Why a separate `s/B/<id>/` namespace from `s/b/<name>/`?

Buckets are unique by **name** globally, but every piece of bucket-
scoped data (objects, blobs, multipart, queues, …) is keyed by **id**
because the id is stable across rename (we do not implement rename
today, but the layout costs nothing to be ready). The bucket row at
`s/b/<name>` carries the UUID in its payload; readers of bucket-scoped
data resolve name → id once at the gateway boundary and address every
inner key by id thereafter.

## Why the queue/audit prefixes share a "qX/" / "A/" shape

Forward-direction range scans claim oldest first because timestamps are
**not** inverted in queue keys (vs object keys where they are). For a
queue we want oldest-first; for an object's version list we want
newest-first.

Audit log uses `<day4-BE>` as a cheap partition discriminator: scanning
"every event for bucket B on day D" is a single range scan, and
"every partition older than D" is a range scan with `start = bucket16
+ 0x00000000` and `end = bucket16 + uint32_BE(D)`.

## Round-trip property

Every encoder in `keys.go` either has a paired decoder (`DecodeObjectKey`,
`DecodeVersionDesc`) or is unambiguously recoverable from its
constituent inputs (queue keys, audit log keys, etc.). The 1k-input
property test in `keys_test.go::TestObjectKeyRoundtrip` proves the
property for the busiest encoder (object keys) over random
(bucket, key, version) triples; sibling tests cover the byte-stuffing
layer, ordering invariants, and the null-sentinel case.
