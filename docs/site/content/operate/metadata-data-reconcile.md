---
title: 'Reconcile + rebuild-index'
weight: 45
description: 'Post-restore runbook — realign RADOS↔meta after a backup restore (orphan chunks, dangling manifests) and last-resort manifest rebuild from a data-tier scan.'
---

# Reconcile + rebuild-index

Strata splits the S3-key→chunk map (**meta** tier: TiKV / Cassandra)
from the chunk bytes (**data** tier: RADOS / S3-over-S3). That split
is the scale advantage — but it drops the safety net RGW gets by
colocating its bucket index with the data. After you restore one tier
from a backup that is even slightly out of step with the other, the
two tiers diverge silently:

- **Orphan chunks** — data has a chunk no manifest references. GC walks
  meta→data, so it can never see them; they leak storage forever.
- **Dangling manifests** — meta has a manifest pointing at a chunk the
  data tier no longer holds. The client discovers it as a `5xx` on
  `GET`, not on the restore.
- **Lost meta backup** — the bytes are intact in RADOS but unreadable,
  because the key→chunk map is gone.

This page is the post-restore runbook for realigning the two tiers.
It pairs with [Backup + restore]({{< relref "/operate/backup-restore" >}})
(how each tier is snapshotted) — run reconcile **after** the restore
drill, before you promote traffic.

## Shared responsibility — who owns what

The boundary is deliberate. Strata does **not** reimplement `br`,
`nodetool`, or `rados export`.

| Tier | Owner | Tooling |
|---|---|---|
| Metadata (Cassandra / TiKV) | **Operator** | `nodetool snapshot` / `br backup` + restore per the upstream runbook. |
| Data (RADOS / S3) | **Operator** | `rados mksnap` / pool rollback, or the upstream S3 versioning. |
| **Realigning the two tiers after a restore** | **Strata** | The chunk back-reference + the `reconcile` worker + `strata admin rebuild-index`. |

Strata makes every chunk **self-describing**: each chunk carries a
back-reference to its owning object (`{bucket_id, key, version_id,
chunk_idx, mtime}`), stamped at PUT time as a RADOS xattr
(`user.strata.backref`) or an `x-amz-meta-strata-backref` on the S3
backend. On by default; opt out with `STRATA_CHUNK_BACKREF=false`
(then reconcile / rebuild degrade gracefully and log that
back-references are absent). The back-reference carries **no key
material** — it is safe under SSE-S3/KMS.

This is the same shape as **Lustre LFSCK** (filesystem-check that
reconciles the MDT object index against the OST objects) and **HDFS
`fsck`** (block-report reconciliation against the NameNode). The
back-reference is Strata's per-object stamp that makes the data tier
walkable without the index — exactly what LFSCK's OST-object FID
back-pointer does for Lustre.

## The two skew directions

A reconcile job runs in one of two directions, selected by whether you
pass a `bucket=`:

| Direction | Skew | What it finds | Pass |
|---|---|---|---|
| **data → meta** (orphan) | data newer than meta | chunks no manifest references | pool-scoped (`cluster=` + `pool=`) |
| **meta → data** (dangling) | meta newer than data | manifests whose chunk is gone | bucket-scoped (`bucket=`) |

Both run as a **leader-elected worker draining admin-queued jobs**
(the same model as `reshard`): the admin `POST` queues a job and
returns `202` immediately; the worker runs the scan out-of-band on a
tick; you poll progress with `GET`. A live-cluster scan must never
block an HTTP request.

Enable the worker on at least one replica:

```bash
STRATA_WORKERS=...,reconcile
```

## Post-restore procedure

The safe order is **report → review → resolve → rebuild only if meta
is unusable**. Never start with a destructive policy.

### Step 1 — Reconcile in `report` mode (never deletes)

`report` is the default for both directions. Nothing is deleted,
nothing is quarantined — you get counts.

```bash
# Orphan pass (data → meta): scan a cluster's pool.
curl -s -X POST 'http://strata/admin/reconcile?cluster=ceph-a&pool=strata-data&policy=report'
# -> 202 { "id": "<job-id>", "state": "queued", "policy": "report", ... }

# Dangling pass (meta → data): scan one bucket's manifests.
curl -s -X POST 'http://strata/admin/reconcile?bucket=my-bucket&policy=report'
# -> 202 { "id": "<job-id>", "state": "queued", "policy": "report", "bucket": "...", ... }
```

The admin surface is gated to the IAM root principal — sign the request
with the root credentials (presigned URLs are rejected).

Query params:

- **Orphan pass:** `cluster` (required), `pool` (required), `namespace`
  (optional — empty = default namespace; `\x01` = all namespaces),
  `policy` (`report` | `gc`, default `report`).
- **Dangling pass:** `bucket=<name>` (required; resolved to its UUID),
  `policy` (`report` | `quarantine` | `delete`, default `report`).
  `cluster` / `pool` are not used.

### Step 2 — Watch the job converge + read the summary

```bash
curl -s 'http://strata/admin/reconcile?id=<job-id>' | jq .
```

The job is idempotent + resumable from a `cursor` watermark, so a
re-run after a partial pass converges. States: `queued` → `running` →
`done` (or `error`). The response carries the post-run summary you use
for a go/no-go:

| Field | Meaning |
|---|---|
| `scanned` | chunks visited (orphan pass) |
| `orphans_found` / `orphans_gc` / `orphans_report` | orphan chunks, by resolution |
| `absent_backref` | chunks with **no** back-reference — counted, **never deleted** (you can't attribute them, so you can't safely destroy them) |
| `manifests_scanned` / `healthy` | dangling pass progress |
| `dangling_found` / `dangling_quarantine` / `dangling_report` / `dangling_delete` | broken manifests, by resolution |
| `errors` | per-chunk errors skipped (never delete on doubt) |

> **Web console (US-006).** The same trigger + progress + summary is
> available in the operator console under **Diagnostics → Reconcile**
> (`/console/diagnostics/reconcile`, cookie-authenticated `/admin/v1/reconcile`).
> Pick the pass direction + policy, queue the job, and watch the orphan /
> dangling counters converge — mirroring the drain / reshard-progress UX.
> `rebuild-index` is deliberately **not** exposed in the console (a
> destructive last-resort op gated behind shell access); the console links
> back to this runbook instead.

### Step 3 — Resolve

Decide a policy from the report, then re-run that direction with it.

**Orphan chunks** (`policy=gc`) — the object was rolled back; the
chunk is safe to reclaim. Each orphan is enqueued for deletion via the
GC queue (it is **not** deleted inline):

```bash
curl -s -X POST 'http://strata/admin/reconcile?cluster=ceph-a&pool=strata-data&policy=gc'
```

> `restore` (rebuild the manifest from the back-reference, for a
> meta-older-than-data skew) is not yet wired — `StartReconcile`
> rejects it with `InvalidArgument`. Until then, use `rebuild-index`
> (Step 4) for the lost-meta case. Tracked as US-002b.

**Dangling manifests** (`policy=quarantine`) — the chunk is gone; mark
the object unreadable so a `GET` / `HEAD` returns a clear
`503 ObjectQuarantined` instead of a silent corrupt `5xx`:

```bash
curl -s -X POST 'http://strata/admin/reconcile?bucket=my-bucket&policy=quarantine'
```

**Dangling manifests** (`policy=delete`, US-003b) — once you have decided
the version is unrecoverable, `delete` enqueues the version's chunks for
GC (the dual-write `_by_cluster` lookup stays in lockstep; GC dedups by
OID so a re-run never double-deletes) and removes the object-version row.
More aggressive than `quarantine` — review the `report` summary first:

```bash
curl -s -X POST 'http://strata/admin/reconcile?bucket=my-bucket&policy=delete'
```

> The dangling pass needs a real chunk prober: the RADOS backend probes
> via a per-OID `rados stat`, the S3-passthrough backend via a native
> `HEAD` (both rate-limited by `STRATA_RECONCILE_SCAN_RATE`). A go-ceph-free
> build has no prober and records an error rather than flag/delete a
> healthy object on a probe it could not run.

### Step 4 — `rebuild-index` (last resort, only if the meta backup is unusable)

If the metadata backup is **lost or corrupt** and the bytes survive in
RADOS, `strata admin rebuild-index` reconstructs the manifest index
from a data-tier scan. It is a `strata admin` subcommand (single-binary
invariant — not a new binary) that connects directly to the meta + data
backends, mirroring `strata admin rewrap`. It is **not** exposed over
the gateway HTTP surface and is intentionally CLI-only.

**Always `--dry-run` first** to review the recovery report before any
row is written:

```bash
strata admin rebuild-index --cluster ceph-a --pool strata-data --dry-run
```

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `--cluster ID` | default cluster | data-tier cluster to scan |
| `--pool P` | backend pool | RADOS pool to scan |
| `--namespace NS` | default ns | RADOS namespace to scan |
| `--bucket-id UUID` | every bucket | restrict rebuild to one bucket |
| `--force` | off | overwrite manifest rows that already exist (live meta wins by default) |
| `--dry-run` | off | scan + classify + report only; write no rows |

The engine groups chunks by back-reference `{bucket_id, key,
version_id}`, orders by `chunk_idx`, sets `IsLatest` correctly via the
back-reference `mtime` (a `version_id` chain alone cannot, when a
suspended-null version coexists), and writes reconstructed rows. The
text report prints:

```
rebuild-index: cluster=ceph-a pool=strata-data namespace=
  chunks:   scanned=N absent_backref=N
  versions: groups=N rebuilt=N skipped_existing=N
  rejected: gapped=N unrecoverable_sse=N errors=N
```

It is idempotent + resumable: a re-run skips rows already rebuilt and
refuses to clobber a live manifest unless `--force`.

## The `rebuild-index` recovery boundary — read this LOUDLY

`rebuild-index` is a **last resort for plaintext bytes only**. The
metadata backup is the primary recovery path. From the data tier
alone, the following are **unrecoverable**:

- **SSE-S3 / SSE-KMS objects.** The wrapped DEK lived only in the lost
  meta (`meta.Object.SSEKey`); the back-reference carries no key
  material by design. Such objects are reported `unrecoverable_sse`
  and **never** written — Strata will not serve ciphertext as if it
  were the object.
- **Gaps.** A missing `chunk_idx` in the sequence flags the version
  `gapped` and refuses to stitch it into a short object — a partial
  object is reported, never served as whole.
- **Object metadata not in the back-reference** — `Content-Type`,
  user-metadata, tags, ACL, storage-class, the multipart-composite
  ETag. These are reported lost, **not fabricated**. The single-part
  ETag is recomputed from the rebuilt bytes.

In short: plaintext objects recover fully; everything above does not.
If `unrecoverable_sse > 0`, restore the meta backup for those objects —
there is no data-only path.

## Metrics

The worker emits per-iteration metrics (`strata_worker_iteration_total`
/ `strata_worker_tick_duration_seconds`, `worker="reconcile"`) plus:

| Metric | Meaning |
|---|---|
| `strata_reconcile_chunks_scanned_total` | data-tier chunks visited |
| `strata_reconcile_orphans_found_total{resolution}` | orphan chunks, `resolution=report\|gc` |
| `strata_reconcile_dangling_manifests_total{resolution}` | dangling manifests, `resolution=report\|quarantine\|delete` |
| `strata_reconcile_errors_total` | per-chunk errors skipped (never deleted on doubt) |

The scan is rate-limited via `STRATA_RECONCILE_SCAN_RATE` (objects/sec,
read in the data layer; `0` or unset = unlimited) so a live-cluster
pool walk does not saturate the OSDs. Set it during business hours.

## End-to-end validation walkthrough

Before trusting the feature in an incident, validate the whole pipeline —
both skew directions, both passes, every policy, and the last-resort
rebuild — in one exercise. Two artifacts cover it, layered so the
deterministic core runs in CI and the operator-facing contract runs
against the real lab.

### 1. Deterministic core (CI-green, no RADOS)

`internal/reconcile/walkthrough_test.go::TestEndToEndReconcileWalkthrough`
is the single narrative that pins both cycle-promises against the memory
backends (the parity oracle):

1. Seed a healthy object the walk must never disturb.
2. **Skew A — data-older-than-meta** (orphan chunk): `report` makes the
   orphan **visible** and deletes nothing; `gc` enqueues exactly the
   orphan. *No silent leak* — GC walks meta→data and could never see it
   before this cycle.
3. **Skew B — meta-older-than-data** (lost manifest row): `restore`
   rebuilds the manifest from the back-reference; the object is GET-able
   again with correct bytes + recomputed ETag.
4. **Dangling pass** (meta→data): a manifest pointing at a missing chunk
   is detected and `quarantine` flags the object so a GET returns a clear
   `ObjectQuarantined` error. *No silent corrupt GET.*
5. **rebuild-index**: a two-chunk plaintext version recovers fully, a gap
   is flagged (never stitched short + served), an SSE object is reported
   unrecoverable.

Run it directly:

```bash
go test -run TestEndToEndReconcileWalkthrough ./internal/reconcile/
```

The memory backend is the contract oracle for TiKV **and** Cassandra (both
implement `meta.Store` in lockstep), so this proves the backend-agnostic
semantics for both meta backends.

### 2. Operator contract against the running lab

`scripts/smoke-metadata-data-reconcile.sh` (`make
smoke-metadata-data-reconcile`) drives the path the web console (US-006)
rides — the UI is **not** a no-op stub — across **both** labs for parity:

- TiKV-default lab on `:9999`, Cassandra lab on `:9998`.
- Console session login → seed a bucket → queue a **dangling** pass and an
  **orphan** pass via `POST /admin/v1/reconcile` → poll
  `GET /admin/v1/reconcile/{id}` through `queued → running → done|error`
  and assert the progress/summary counters render.
- `strata admin rebuild-index --dry-run` in the gateway container.

Bring a lab up with the worker enabled, then run:

```bash
STRATA_WORKERS=gc,lifecycle,reconcile make up-all && make wait-strata-lab
make smoke-metadata-data-reconcile     # exits 77 (skip) if no lab is up
```

The deep RADOS resolution legs (orphan pool scan, dangling per-OID probe)
are **integration-gated** — on a lab without the real RADOS prober/scanner
a job may converge to `error`, which the smoke accepts as a terminal state
for the queue/progress/summary contract. The deterministic core above is
what proves the resolution semantics.

## See also

- [Backup + restore]({{< relref "/operate/backup-restore" >}}) — how
  each tier is snapshotted + the restore drill that precedes reconcile.
- [Key rotation]({{< relref "/operate/key-rotation" >}}) — why SSE
  objects depend on the meta-held DEK (the rebuild boundary above).
- [Architecture — Workers]({{< relref "/architecture/workers" >}}) —
  the reconcile worker shape + the chunk back-reference internals.
