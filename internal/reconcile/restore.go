package reconcile

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// restore.go implements the US-002b restore policy: rebuild the manifest row
// for an orphan chunk whose owner survives in the data tier but whose meta row
// was lost (the meta-older-than-data skew). It reuses the US-004 rebuild
// grouping — group chunks by {bucket_id, key, version_id}, order by chunk_idx,
// recompute the single-part ETag, set IsLatest via the back-reference mtime —
// but is driven by the reconcile worker over the ORPHAN set (not a whole-pool
// scan). rebuild.go (US-004) imports this package and reuses OrderChunks so the
// gap-detection rule lives in exactly one place.
//
// SAFETY RAILS (identical boundary to rebuild-index):
//   - PLAINTEXT-ONLY: an SSE object is reported (OrphansReport), never restored
//     — the wrapped DEK was in the lost meta, so the bytes are ciphertext.
//   - A gap in the chunk_idx sequence is reported, NEVER stitched into a short
//     object served as whole.
//   - Restore writes a version ONLY when its row is genuinely absent. An
//     overwritten version (the row exists with a valid manifest pointing at a
//     different OID) is reported, never clobbered — restoring it from the orphan
//     would corrupt live data.
//   - Idempotent: once restored, a subsequent pass sees the chunk as healthy
//     (the manifest references it) and never re-detects it as an orphan.

// restoreKey identifies one object version accumulated for restore.
type restoreKey struct {
	bucketID  uuid.UUID
	key       string
	versionID string
}

// restoreGroup accumulates a single version's orphan chunks during the scan.
type restoreGroup struct {
	chunks map[int]data.ChunkRef // chunk_idx -> ref
	mtime  time.Time
}

// classifyRestore routes an orphan chunk under the restore policy. SSE objects
// and overwritten-but-intact versions are reported (never restored); a
// genuinely-absent version's chunk is accumulated for post-scan reconstruction.
// OrphansFound was already incremented by the caller.
func (w *Worker) classifyRestore(ctx context.Context, job *meta.ReconcileJob, c ScannedChunk, objectAbsent bool, acc map[restoreKey]*restoreGroup) {
	br := c.Backref
	if br.SSEAlgo != "" {
		// PLAINTEXT-ONLY: the wrapped DEK was lost with the meta backup. Report
		// unrecoverable; never restore a row that would serve ciphertext.
		job.OrphansReport++
		w.cfg.Obs.OrphanFound(meta.ReconcilePolicyReport)
		w.cfg.Logger.WarnContext(ctx, "reconcile restore: orphan unrecoverable (SSE)",
			"bucket", br.BucketID, "key", br.Key, "version", br.VersionID, "algo", br.SSEAlgo)
		return
	}
	if !objectAbsent {
		// Overwritten version: the row is intact with its own valid manifest
		// pointing at a different OID. Restore must NOT clobber it — report the
		// stale chunk instead (gc is the policy that reclaims it).
		job.OrphansReport++
		w.cfg.Obs.OrphanFound(meta.ReconcilePolicyReport)
		return
	}
	gk := restoreKey{bucketID: br.BucketID, key: br.Key, versionID: br.VersionID}
	g := acc[gk]
	if g == nil {
		g = &restoreGroup{chunks: make(map[int]data.ChunkRef)}
		acc[gk] = g
	}
	if br.Mtime.After(g.mtime) {
		g.mtime = br.Mtime
	}
	g.chunks[br.ChunkIdx] = data.ChunkRef{
		Cluster:   c.Cluster,
		Pool:      c.Pool,
		Namespace: c.Namespace,
		OID:       c.OID,
		Size:      c.Size,
	}
}

// resolveRestores reconstructs every accumulated restore group after the scan
// drains. IsLatest is set on the max-mtime restorable version of each
// (bucket,key) so the served head is correct even across a Suspended-null
// version (whose back-reference mtime, not its version_id, orders the chain).
func (w *Worker) resolveRestores(ctx context.Context, job *meta.ReconcileJob, acc map[restoreKey]*restoreGroup) {
	// Group versions per (bucket,key) so IsLatest can be set by mtime.
	type keyKey struct {
		bucketID uuid.UUID
		key      string
	}
	byKey := make(map[keyKey][]restoreKey)
	for gk := range acc {
		kk := keyKey{gk.bucketID, gk.key}
		byKey[kk] = append(byKey[kk], gk)
	}
	for _, gks := range byKey {
		// Ascending mtime; the newest RESTORABLE version (not gapped) becomes
		// the served head.
		sort.SliceStable(gks, func(i, j int) bool {
			return acc[gks[i]].mtime.Before(acc[gks[j]].mtime)
		})
		latestIdx := -1
		for i := len(gks) - 1; i >= 0; i-- {
			if _, gap := OrderChunks(acc[gks[i]].chunks); !gap {
				latestIdx = i
				break
			}
		}
		for i, gk := range gks {
			w.restoreVersion(ctx, job, gk, acc[gk], i == latestIdx)
		}
	}
}

// restoreVersion reconstructs one version's manifest row from its grouped orphan
// chunks. A gap reports (never stitched); a read or write failure counts an
// error (the chunk stays orphan; a re-run retries).
func (w *Worker) restoreVersion(ctx context.Context, job *meta.ReconcileJob, gk restoreKey, g *restoreGroup, isLatest bool) {
	ordered, gap := OrderChunks(g.chunks)
	if gap {
		// Partial object — report, NEVER stitch into a short object served whole.
		job.OrphansReport += int64(len(g.chunks))
		for range g.chunks {
			w.cfg.Obs.OrphanFound(meta.ReconcilePolicyReport)
		}
		w.cfg.Logger.WarnContext(ctx, "reconcile restore: version gapped, not restored",
			"bucket", gk.bucketID, "key", gk.key, "version", gk.versionID, "have_chunks", len(g.chunks))
		return
	}

	// Recompute the single-part ETag + per-chunk CRC32C from the rebuilt bytes
	// (both were lost with the meta backup).
	hash := md5.New()
	var size int64
	for i := range ordered {
		b, err := w.readChunkBytes(ctx, ordered[i])
		if err != nil {
			job.Errors++
			w.cfg.Obs.ReconcileError()
			w.cfg.Logger.WarnContext(ctx, "reconcile restore: chunk read failed",
				"bucket", gk.bucketID, "key", gk.key, "version", gk.versionID,
				"oid", ordered[i].OID, "error", err.Error())
			return
		}
		hash.Write(b)
		ordered[i].Size = int64(len(b))
		ordered[i].Checksum = data.ComputeChunkCRC(b)
		size += int64(len(b))
	}
	etag := hex.EncodeToString(hash.Sum(nil))

	// Re-probe: never clobber a row that reappeared between scan and resolve (a
	// concurrent client write wins). Report on ANY existing row — matches the
	// classify objectAbsent gate exactly, so a delete-marker or manifest-less
	// row at this exact version is never overwritten. An absent row is the
	// expected restore case.
	existing, err := w.cfg.Meta.GetObject(ctx, gk.bucketID, gk.key, gk.versionID)
	switch {
	case err == nil && existing != nil:
		job.OrphansReport += int64(len(ordered))
		for range ordered {
			w.cfg.Obs.OrphanFound(meta.ReconcilePolicyReport)
		}
		return
	case err != nil && !errors.Is(err, meta.ErrObjectNotFound) && !errors.Is(err, meta.ErrBucketNotFound):
		job.Errors++
		w.cfg.Obs.ReconcileError()
		return
	}

	obj := &meta.Object{
		BucketID:     gk.bucketID,
		Key:          gk.key,
		VersionID:    gk.versionID,
		IsLatest:     isLatest,
		Size:         size,
		ETag:         etag,
		StorageClass: "STANDARD",
		Mtime:        g.mtime,
		Manifest: &data.Manifest{
			Class:     "STANDARD",
			Size:      size,
			ChunkSize: data.DefaultChunkSize,
			ETag:      etag,
			Chunks:    ordered,
		},
	}
	if gk.versionID == meta.NullVersionID {
		obj.IsNull = true
	}
	if err := w.cfg.Meta.PutObject(ctx, obj, true); err != nil {
		job.Errors++
		w.cfg.Obs.ReconcileError()
		w.cfg.Logger.WarnContext(ctx, "reconcile restore: write failed (recreate the bucket first if its row is also lost)",
			"bucket", gk.bucketID, "key", gk.key, "version", gk.versionID, "error", err.Error())
		return
	}
	job.OrphansRestore += int64(len(ordered))
	for range ordered {
		w.cfg.Obs.OrphanFound(meta.ReconcilePolicyRestore)
	}
	w.cfg.Logger.InfoContext(ctx, "reconcile restore: version rebuilt",
		"bucket", gk.bucketID, "key", gk.key, "version", gk.versionID,
		"size", size, "etag", etag, "is_latest", isLatest)
}

// readChunkBytes reads one chunk's full bytes via a single-chunk manifest so
// each chunk's CRC can be recomputed for the rebuilt manifest.
func (w *Worker) readChunkBytes(ctx context.Context, ref data.ChunkRef) ([]byte, error) {
	m := &data.Manifest{
		Size:      ref.Size,
		ChunkSize: data.DefaultChunkSize,
		Chunks:    []data.ChunkRef{ref},
	}
	rc, err := w.cfg.Data.GetChunks(ctx, m, 0, ref.Size)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// OrderChunks returns the chunks indexed by chunk_idx in [0..n-1] order and
// whether the sequence has a gap (a missing index). A gapped sequence must
// NEVER be stitched into a short object. Shared by the restore policy (US-002b)
// and rebuild-index (US-004) so the gap rule lives in one place.
func OrderChunks(chunks map[int]data.ChunkRef) ([]data.ChunkRef, bool) {
	n := len(chunks)
	out := make([]data.ChunkRef, 0, n)
	for i := 0; i < n; i++ {
		ref, ok := chunks[i]
		if !ok {
			return nil, true // gap: index i missing in a 0..n-1 sequence
		}
		out = append(out, ref)
	}
	return out, false
}
