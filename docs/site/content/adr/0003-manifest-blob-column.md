---
title: 'ADR-0003: Single manifest blob column'
weight: 3
---

# ADR-0003: Single `manifest` blob column for object metadata

## Status

Accepted — April 2026

## Context

Per-object metadata in Strata is rich and growing: chunk OIDs, chunk
sizes, MD5 / SHA256 digests, SSE wrap context, multipart part
boundaries (`PartChunkCounts`, `PartChunks`), backend routing hints
(`BackendRef.Cluster`), and several smaller fields. A normalised
schema would dedicate one column per field, possibly with side
tables for the variable-length arrays (chunks, parts).

The cost of normalised columns on Cassandra is two-fold:

- **Every new field is an `ALTER TABLE`.** Cassandra supports
  additive ALTER, but it propagates schema state through gossip and
  forces a rolling restart contract on every meta-store node before
  the column is safe to read everywhere. For a metadata layer that
  evolves on a per-feature cadence this is heavy ceremony.
- **The meta-store has no business filtering on most of these
  fields.** The list path needs `key`, `version_id`, `size`,
  `last_modified` and a handful of flags. Everything else is "fetch
  the row, hand the bytes to the gateway, get out of the way." A
  blob column models that contract directly.

We considered (a) one column per field, (b) a JSON blob, and (c) a
proto3 wire-format blob. (a) is rejected for the ALTER cost. (b)
ships faster but inflates rows by 2–4×. (c) is compact and forward-
compatible but harder to debug.

## Decision

Per-object metadata lives in a single `objects.manifest` blob
column, encoded via `data.EncodeManifest`. The format is selected
at write time by `data.SetManifestFormat("proto" | "json")` —
`internal/serverapp` reads `STRATA_MANIFEST_FORMAT` (default
`proto` since US-049) once at startup. Reads always go through
`data.DecodeManifest`, which sniffs the first non-whitespace byte
(`{` → JSON, anything else → proto3 wire format) so JSON and proto
rows coexist in the same table indefinitely.

New fields tag both Go shapes — `json:",omitempty"` for the JSON
codec, a fresh field number in `manifest.proto` for the proto
codec — and update the conversion helpers in `manifest_codec.go`.
Old rows decode with zero-values for the new field; no migration is
required for the codepath to land.

A leader-elected `--workers=manifest-rewriter` reads every row,
re-encodes it via the active writer format, and writes it back
in place. Idempotent — re-runs skip rows that already match the
active format.

## Consequences

- **Schema-additive evolution without `ALTER`.** Adding a field is
  a code change (proto field number + Go tag), not a Cassandra DDL
  migration. The on-the-wire blob grows; the table shape does not.
- **Meta-store-can't-filter-on-manifest-innards.** The query
  surface is intentionally narrow. If a new feature needs to filter
  by a manifest-internal field at scale (e.g. "all objects with SSE
  KMS keyId X"), it gets a dedicated side table or denormalised
  column — not a manifest scan. Any caller tempted to grep inside
  the blob from a hot path should add a separate table instead.
- **JSON ↔ proto coexistence on the read path.** The sniff-on-read
  in `data.DecodeManifest` is permanent — there is no flag day. The
  `manifest-rewriter` worker is the planned migration helper for
  operators who want the disk-space win, not a hard requirement.
- **Field-rename gotcha.** Adding a new field whose JSON key
  collides with an existing one requires a custom `UnmarshalJSON`
  on `Manifest` that sniffs `json.RawMessage` of the colliding key
  and tries the new shape first, falling back to the legacy shape.
  The proto side stays wire-compatible by keeping the field number
  and only renaming the label. The
  `Manifest.PartChunkCounts` → `Manifest.PartChunks []PartRange`
  rename in US-047 is the worked example in `manifest_codec.go`.
- **Slightly harder debug story.** A proto blob is not `SELECT
  manifest FROM objects` readable. The `strata admin` tooling and
  the `data.DecodeManifest` helper bridge that gap; operators
  dumping the table for forensics decode through the helper.
