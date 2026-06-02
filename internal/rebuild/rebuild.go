// Package rebuild reconstructs the manifest index for a bucket/pool from a
// data-tier scan when the metadata backup is lost (US-004
// metadata-data-reconcile). It is the LAST-RESORT recovery path: the meta
// backup (TiKV / Cassandra) is the primary one. rebuild walks every chunk in
// the data tier (the US-000 pool-enumeration primitive, surfaced through a
// reconcile.ChunkScanner), groups chunks by their US-001 back-reference
// {bucket_id, key, version_id}, orders by chunk_idx, recomputes the
// single-part ETag from the rebuilt bytes, and writes reconstructed
// meta.Object rows.
//
// PLAINTEXT-ONLY (PRD decision 3): the wrapped DEK lives solely in
// meta.Object.SSEKey, so an SSE-S3/KMS object is undecryptable once the meta
// backup is gone. The back-reference carries the SSE algorithm LABEL (never
// key material); rebuild reports such objects unrecoverable and NEVER writes
// a row that would serve ciphertext as plaintext. Object metadata absent from
// the back-reference (Content-Type, user-metadata, tags, ACL, storage-class,
// multipart-composite ETag) is reported lost, not fabricated.
//
// Safety rails:
//   - Refuses to overwrite a manifest that already exists in meta unless
//     Force is set (live meta wins by default; rebuild is for the empty/lost
//     case).
//   - A gap in the chunk_idx sequence flags the version gapped and skips it —
//     a partial object is NEVER stitched into a short one and served as whole.
//   - A chunk with no back-reference cannot be attributed to an owner and is
//     counted but never rebuilt.
//
// Idempotent + re-runnable: a second pass over already-rebuilt rows skips them
// (SkippedExisting) unless Force.
package rebuild

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/reconcile"
)

// Config wires a Rebuilder.
type Config struct {
	Meta    meta.Store
	Data    data.Backend
	Scanner reconcile.ChunkScanner
	Logger  *slog.Logger
	// Force overwrites a manifest row that already exists in meta. Default
	// false: live meta wins, rebuild only fills the empty/lost case.
	Force bool
	// DryRun groups + classifies + reports without writing any row. Mirrors
	// the rewrap --dry-run safety check.
	DryRun bool
	// BucketFilter, when non-nil, restricts the rebuild to chunks whose
	// back-reference names that bucket — a pool may hold chunks for many
	// buckets, and an operator typically rebuilds one bucket at a time.
	// uuid.Nil (the zero value) means "rebuild every bucket in the scope".
	BucketFilter uuid.UUID
	Now          func() time.Time
}

// Rebuilder reconstructs manifest rows from a data-tier scan.
type Rebuilder struct {
	cfg Config
}

// New validates cfg and returns a Rebuilder.
func New(cfg Config) (*Rebuilder, error) {
	if cfg.Meta == nil {
		return nil, errors.New("rebuild: meta store required")
	}
	if cfg.Data == nil {
		return nil, errors.New("rebuild: data backend required")
	}
	if cfg.Scanner == nil {
		return nil, errors.New("rebuild: chunk scanner required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Rebuilder{cfg: cfg}, nil
}

// Object-report statuses.
const (
	StatusRebuilt          = "rebuilt"
	StatusWouldRebuild     = "would_rebuild" // DryRun: recoverable, not written
	StatusSkippedExisting  = "skipped_exists"
	StatusGapped           = "gapped"
	StatusUnrecoverableSSE = "unrecoverable_sse"
	StatusError            = "error"
)

// ObjectReport is one reconstructed (or rejected) object version. Suitable
// for an operator go/no-go after a rebuild.
type ObjectReport struct {
	BucketID  uuid.UUID
	Key       string
	VersionID string
	Status    string
	Size      int64
	ETag      string
	IsLatest  bool
	Detail    string
}

// Stats summarises one Run.
type Stats struct {
	ChunksScanned int64
	AbsentBackref int64
	GroupsSeen    int
	Rebuilt       int
	SkippedExist  int
	Gapped        int
	Unrecoverable int
	Errors        int
	Reports       []ObjectReport
}

// groupKey identifies one object version across the scan.
type groupKey struct {
	bucketID  uuid.UUID
	key       string
	versionID string
}

// groupState accumulates a single version's chunks during the scan.
type groupState struct {
	chunks  map[int]data.ChunkRef // chunk_idx -> ref
	mtime   time.Time
	sseAlgo string
}

// Run enumerates scope's data-tier chunks, groups them by back-reference, and
// writes reconstructed manifest rows. Returns aggregated Stats (with a
// per-object report) and the first fatal error. A per-object failure is
// recorded on the report (StatusError) and does NOT abort the run.
func (r *Rebuilder) Run(ctx context.Context, scope reconcile.ScanScope) (Stats, error) {
	var stats Stats
	groups := make(map[groupKey]*groupState)

	scanErr := r.cfg.Scanner.Scan(ctx, scope, "", func(c reconcile.ScannedChunk, _ string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		stats.ChunksScanned++
		if !c.HasBackref {
			// Cannot attribute this chunk to an owner (legacy chunk or
			// STRATA_CHUNK_BACKREF=false) — count + report, never rebuild.
			stats.AbsentBackref++
			return nil
		}
		br := c.Backref
		if r.cfg.BucketFilter != uuid.Nil && br.BucketID != r.cfg.BucketFilter {
			// Out of scope for a single-bucket rebuild; not counted as
			// absent (it has a back-reference, just for another bucket).
			return nil
		}
		gk := groupKey{bucketID: br.BucketID, key: br.Key, versionID: br.VersionID}
		g := groups[gk]
		if g == nil {
			g = &groupState{chunks: make(map[int]data.ChunkRef)}
			groups[gk] = g
		}
		// First non-empty SSE label / largest mtime wins; chunks of one
		// version agree on both, but be defensive.
		if br.SSEAlgo != "" {
			g.sseAlgo = br.SSEAlgo
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
		return nil
	})
	if scanErr != nil {
		return stats, fmt.Errorf("scan: %w", scanErr)
	}
	stats.GroupsSeen = len(groups)

	// rebuild-index reconstructs object MANIFESTS, not the bucket row itself
	// (owner / ACL / versioning / shard-count are NOT in the back-reference).
	// If the meta backup is fully gone the bucket row is gone too, so probe
	// each distinct bucket ONCE and report a clear, actionable error rather
	// than letting every PutObject fail with a confusing per-object
	// ErrBucketNotFound. The operator must recreate the bucket first.
	missingBucket := make(map[uuid.UUID]bool)
	for gk := range groups {
		bid := gk.bucketID
		if _, seen := missingBucket[bid]; seen {
			continue
		}
		// GetObject on an absent key distinguishes the two: ErrBucketNotFound
		// (bucket row gone) vs ErrObjectNotFound (bucket present, key absent).
		_, err := r.cfg.Meta.GetObject(ctx, bid, "", "")
		absent := errors.Is(err, meta.ErrBucketNotFound)
		missingBucket[bid] = absent
		if absent {
			r.cfg.Logger.WarnContext(ctx, "rebuild: bucket row absent — recreate the bucket before rebuild can write its objects",
				"bucket", bid)
		}
	}

	// Organise versions per (bucket,key) so IsLatest can be set by the
	// back-reference mtime. NOTE: this is honoured directly only on backends
	// that store IsLatest verbatim (memory, via insertion-order head); the
	// ordered backends (TiKV / Cassandra) re-derive "latest" from version_id
	// clustering order, so a Suspended-null version (which sorts last by
	// version_id) with a later mtime would NOT win there — minting a synthetic
	// version_id to force it would break the invariant that the rebuilt
	// version_id matches the back-reference (so a reconcile pass sees the
	// chunks as healthy). That narrow case is the US-004b RADOS-integration
	// follow-up; for normal Enabled versions (TimeUUID ts ≈ mtime) the two
	// orderings agree.
	type keyKey struct {
		bucketID uuid.UUID
		key      string
	}
	byKey := make(map[keyKey][]groupKey)
	for gk := range groups {
		kk := keyKey{gk.bucketID, gk.key}
		byKey[kk] = append(byKey[kk], gk)
	}

	for _, gks := range byKey {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		// Ascending mtime: the max-mtime version is the latest. For the
		// memory backend (insertion-order head) writing ascending puts the
		// latest at the head; the IsLatest field is also set explicitly so
		// ordered backends agree.
		sort.SliceStable(gks, func(i, j int) bool {
			return groups[gks[i]].mtime.Before(groups[gks[j]].mtime)
		})
		// IsLatest goes on the max-mtime version among those we can actually
		// rebuild (a gapped/SSE latest cannot be served, so the newest
		// recoverable version becomes the served head).
		latestIdx := -1
		for i := len(gks) - 1; i >= 0; i-- {
			g := groups[gks[i]]
			if g.sseAlgo != "" {
				continue
			}
			if _, gap := orderChunks(g); gap {
				continue
			}
			latestIdx = i
			break
		}
		for i, gk := range gks {
			r.processVersion(ctx, &stats, gk, groups[gk], i == latestIdx, missingBucket[gk.bucketID])
		}
	}
	return stats, nil
}

// processVersion classifies one version and, when recoverable, writes (or in
// DryRun would write) its reconstructed manifest row.
func (r *Rebuilder) processVersion(ctx context.Context, stats *Stats, gk groupKey, g *groupState, isLatest, bucketMissing bool) {
	rep := ObjectReport{
		BucketID:  gk.bucketID,
		Key:       gk.key,
		VersionID: gk.versionID,
		IsLatest:  isLatest,
	}

	if bucketMissing {
		// The bucket row is gone (full meta loss). rebuild cannot fabricate it
		// — owner / ACL / versioning / shard-count are not in the
		// back-reference — so it cannot write the object either. Report
		// actionable (the per-bucket WARN already fired once in Run).
		rep.Status = StatusError
		rep.Detail = "bucket row absent in meta — recreate the bucket first (its owner/ACL/versioning/shard-count are NOT recoverable from the back-reference)"
		stats.Errors++
		stats.Reports = append(stats.Reports, rep)
		return
	}

	if g.sseAlgo != "" {
		// PLAINTEXT-ONLY: the wrapped DEK was in the lost meta. Report
		// unrecoverable; NEVER write a row that would serve ciphertext.
		rep.Status = StatusUnrecoverableSSE
		rep.Detail = fmt.Sprintf("server-side encrypted (%s); wrapped DEK lost with meta backup", g.sseAlgo)
		stats.Unrecoverable++
		stats.Reports = append(stats.Reports, rep)
		r.cfg.Logger.WarnContext(ctx, "rebuild: object unrecoverable (SSE)",
			"bucket", gk.bucketID, "key", gk.key, "version", gk.versionID, "algo", g.sseAlgo)
		return
	}

	ordered, gap := orderChunks(g)
	if gap {
		rep.Status = StatusGapped
		rep.Detail = "missing chunk_idx in sequence — partial object, not served as whole"
		stats.Gapped++
		stats.Reports = append(stats.Reports, rep)
		r.cfg.Logger.WarnContext(ctx, "rebuild: object gapped",
			"bucket", gk.bucketID, "key", gk.key, "version", gk.versionID, "have_chunks", len(g.chunks))
		return
	}

	// Recompute the single-part ETag + per-chunk CRC32C from the rebuilt
	// bytes. Read chunk-by-chunk so each chunk's CRC can be stamped back onto
	// the manifest (the original CRC was lost with the meta backup).
	hash := md5.New()
	var size int64
	for i := range ordered {
		b, err := r.readChunk(ctx, ordered[i])
		if err != nil {
			rep.Status = StatusError
			rep.Detail = fmt.Sprintf("read chunk %s: %v", ordered[i].OID, err)
			stats.Errors++
			stats.Reports = append(stats.Reports, rep)
			r.cfg.Logger.WarnContext(ctx, "rebuild: chunk read failed",
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
	rep.Size = size
	rep.ETag = etag

	// Refuse to clobber a live manifest unless Force.
	existing, err := r.cfg.Meta.GetObject(ctx, gk.bucketID, gk.key, gk.versionID)
	switch {
	case err == nil && existing != nil && !r.cfg.Force:
		rep.Status = StatusSkippedExisting
		rep.Detail = "manifest already present in meta (rerun with --force to overwrite)"
		stats.SkippedExist++
		stats.Reports = append(stats.Reports, rep)
		return
	case err != nil && !errors.Is(err, meta.ErrObjectNotFound) && !errors.Is(err, meta.ErrBucketNotFound):
		rep.Status = StatusError
		rep.Detail = fmt.Sprintf("probe existing: %v", err)
		stats.Errors++
		stats.Reports = append(stats.Reports, rep)
		return
	}

	if r.cfg.DryRun {
		rep.Status = StatusWouldRebuild
		rep.Detail = "dry-run: recoverable, no row written"
		stats.Rebuilt++ // counted as recoverable for the dry-run summary
		stats.Reports = append(stats.Reports, rep)
		return
	}

	if err := r.writeObject(ctx, gk, ordered, size, etag, g.mtime, isLatest); err != nil {
		rep.Status = StatusError
		rep.Detail = fmt.Sprintf("write object: %v", err)
		stats.Errors++
		stats.Reports = append(stats.Reports, rep)
		r.cfg.Logger.WarnContext(ctx, "rebuild: write failed",
			"bucket", gk.bucketID, "key", gk.key, "version", gk.versionID, "error", err.Error())
		return
	}
	rep.Status = StatusRebuilt
	rep.Detail = "Content-Type / user-metadata / tags / ACL / storage-class not in back-reference — reported lost, defaulted"
	stats.Rebuilt++
	stats.Reports = append(stats.Reports, rep)
	r.cfg.Logger.InfoContext(ctx, "rebuild: object rebuilt",
		"bucket", gk.bucketID, "key", gk.key, "version", gk.versionID,
		"size", size, "etag", etag, "is_latest", isLatest)
}

// writeObject persists one reconstructed manifest row. The version_id from the
// back-reference is preserved so a subsequent reconcile pass sees the chunks
// as healthy. NullVersionID maps onto the Suspended-null PutObject path.
func (r *Rebuilder) writeObject(ctx context.Context, gk groupKey, chunks []data.ChunkRef, size int64, etag string, mtime time.Time, isLatest bool) error {
	manifest := &data.Manifest{
		Class:     "STANDARD",
		Size:      size,
		ChunkSize: data.DefaultChunkSize,
		ETag:      etag,
		Chunks:    chunks,
	}
	obj := &meta.Object{
		BucketID:     gk.bucketID,
		Key:          gk.key,
		VersionID:    gk.versionID,
		IsLatest:     isLatest,
		Size:         size,
		ETag:         etag,
		StorageClass: "STANDARD",
		Mtime:        mtime,
		Manifest:     manifest,
	}
	if gk.versionID == meta.NullVersionID {
		obj.IsNull = true
	}
	// versioned=true preserves every reconstructed version (prepend/append,
	// never replace) so multiple versions of a key all land.
	return r.cfg.Meta.PutObject(ctx, obj, true)
}

// readChunk reads one chunk's full bytes via a single-chunk manifest. Reading
// per-chunk (rather than a flat GetChunks over the whole object) lets the CRC
// be recomputed per chunk for the rebuilt manifest.
func (r *Rebuilder) readChunk(ctx context.Context, ref data.ChunkRef) ([]byte, error) {
	m := &data.Manifest{
		Size:      ref.Size,
		ChunkSize: data.DefaultChunkSize,
		Chunks:    []data.ChunkRef{ref},
	}
	rc, err := r.cfg.Data.GetChunks(ctx, m, 0, ref.Size)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// orderChunks returns the chunks of g in chunk_idx order [0..n-1] and whether
// the sequence has a gap (a missing index). A gapped sequence must NEVER be
// stitched into a short object.
func orderChunks(g *groupState) ([]data.ChunkRef, bool) {
	n := len(g.chunks)
	out := make([]data.ChunkRef, 0, n)
	for i := 0; i < n; i++ {
		ref, ok := g.chunks[i]
		if !ok {
			return nil, true // gap: index i missing in a 0..n-1 sequence
		}
		out = append(out, ref)
	}
	return out, false
}
