---
title: 'Data backend'
weight: 20
description: 'RADOS 4 MiB chunking, manifest format (proto vs JSON sniff, schema-additive evolution), multi-cluster routing, S3-over-S3 backend.'
---

# Data backend

`internal/data/backend.go` defines the `data.Backend` interface every chunk
store implements. The metadata layer keeps the per-object manifest (chunk
list, sizes, content hash); the data backend is responsible for opaque
fixed-size chunks only. The split is what lets us drop in different
backing stores (RADOS, S3, in-memory) without touching the gateway.

## Backends

| Backend | Build tag | Notes |
|---|---|---|
| `memory` | none | In-process map. Used by tests and the smoke pass. |
| `rados` | `ceph` | RADOS pools via `goceph` (cgo, librados). Requires `make build` with `-tags ceph` or the docker-built image. Multi-cluster routing supported. |
| `s3` | none | S3-over-S3 — Strata as a transparent gateway in front of an upstream S3 endpoint. Useful for migrating from MinIO / SeaweedFS / AWS without lifting and shifting data. |

Selection is via `STRATA_DATA_BACKEND` (`memory` / `rados` / `s3`). RADOS
requires the configured pool (`[rados] classes`) to exist; the
[Storage status page]({{< ref "/architecture/storage" >}}) covers the
operator-facing health surface.

## RADOS chunking

The RADOS backend splits every object body into 4 MiB chunks and stores
each as a separate RADOS object under a deterministic OID derived from
the manifest's content hash. Chunk size is constant — one S3 PUT becomes
N chunk writes (`ceil(size / 4MiB)`); one S3 GET becomes a streaming
`librados read` per chunk in order.

Why 4 MiB?

- Matches Ceph's default `osd_max_object_size` and is what RGW writes.
- Big enough to amortise per-OID metadata overhead.
- Small enough that range reads (`Range:` header → partial chunk read)
  don't waste bandwidth.

The chunking happens in `internal/data/rados/backend.go::Put` and
`Get`. The manifest stores chunk count + per-chunk size, so the
gateway can synthesise `Content-Length` without re-querying RADOS.

### Multi-cluster routing

Strata can fan out across multiple RADOS clusters. The
`[rados] classes` map in `STRATA_CONFIG` keys a storage class
(e.g. `STANDARD`, `GLACIER`, `COLD`) onto a `(cluster, pool, namespace)`
tuple. The data backend opens one `*goceph.IOContext` per unique
tuple at startup and reuses it for every PUT/GET.

This lets a single Strata deployment span hot SSD pools and cold HDD
pools, or even bridge two distinct Ceph clusters (e.g. for a
near-online migration). Bucket lifecycle rules drive the
class transitions; the gc / lifecycle workers do the actual chunk
moves. See [Workers]({{< ref "/architecture/workers" >}}).

## Manifest format

`data.Manifest` (defined in `internal/data/manifest.go` + `manifest.proto`)
is the per-object record stored in the meta store's `objects.manifest`
column. It carries:

- Chunk list (`[]ChunkRef` — pool, namespace, OID, size, offset).
- Total size + ETag (MD5 / multipart-style ETag).
- Per-part chunk counts (for multipart uploads — needed by the
  `?partNumber=N` GET path).
- SSE wrap state (encrypted DEK, key id reference) for SSE objects.

### Wire format: proto with a JSON fallback

The manifest is selectable at runtime:

- `STRATA_MANIFEST_FORMAT=proto` (default) — `proto.Marshal` to a
  protobuf wire body. Compact, schema-additive.
- `STRATA_MANIFEST_FORMAT=json` — `json.Marshal`. Used during the
  migration window from JSON-only legacy.

Reads always go through `data.DecodeManifest`, which sniffs the first
non-whitespace byte: `{` → JSON, anything else → proto3 wire format.
The two formats coexist in the same column. `strata server --workers=manifest-rewriter`
walks every bucket once a day and converts JSON rows to proto in place
(idempotent — already-proto rows skip).

### Schema-additive evolution

New fields go in with both a `json:",omitempty"` tag and a fresh
`protobuf` tag in `manifest.proto`. Old rows decode with zero-values,
new code reads the new fields, and Cassandra never gets an `ALTER` —
the column is `blob` and the format is opaque to the meta store. This
is how `Manifest.PartChunkCounts` (SSE multipart locator) and
`Manifest.PartChunks []PartRange` (`?partNumber=N` GET) shipped
without coordinated upgrades.

**Field-rename gotcha.** When a new field collides with an existing
JSON key, rename the old Go field and drop its JSON tag, then write a
custom `UnmarshalJSON` on `Manifest` that sniffs `json.RawMessage` of
the colliding key — try the new shape first, fall back to the legacy
shape. The proto side stays wire-compatible if you keep the field
number and only rename the label.

## S3-over-S3 backend

`internal/data/s3/` implements `data.Backend` against an upstream S3
endpoint. PUTs become upstream PUTs (one upstream object per chunk),
GETs stream upstream GETs. The upstream endpoint, region, and bucket
are configured per storage class through the same
`[storage_class.<name>]` block as RADOS.

This is the mode that turns Strata into a near-transparent migration
proxy — point a Ceph RGW client at Strata, configure Strata to write
chunks back to the same RGW (or any S3 endpoint), and the metadata
plane gets the Cassandra/TiKV listing semantics without re-uploading
the data.

Health probe: `HeadBucket` against the configured upstream bucket;
`/admin/v1/storage/data` reports `state=reachable` or `error`.
Bytes/objects are not surfaced because the upstream API does not
expose them in O(1).

## Source

- `internal/data/backend.go` — interface.
- `internal/data/manifest.go` + `manifest.proto` + `manifest_codec.go` —
  the manifest type and the proto-vs-JSON sniff codec.
- `internal/data/rados/` — RADOS backend (`-tags ceph`).
- `internal/data/s3/` — S3-over-S3 backend.
- `internal/data/memory/` — in-process backend.
- `cmd/strata/workers/manifest_rewriter.go` — JSON → proto rewriter
  worker.
