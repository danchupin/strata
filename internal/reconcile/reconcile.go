// Package reconcile drives the data-tier reconcile pass (US-002
// metadata-data-reconcile). After a backup restore the meta tier (TiKV /
// Cassandra) and the data tier (RADOS / S3) can drift: a chunk may survive in
// the pool with no manifest referencing it (an orphan — a silent storage
// leak, because GC only walks meta->data and never sees it). The reconcile
// worker walks the data tier directly, reads each chunk's back-reference
// (US-001), looks the owner up in the meta store, and resolves orphan chunks
// by an explicit per-run policy.
//
// Execution model mirrors the reshard worker (US-005): the admin POST handler
// only QUEUES a job (meta.StartReconcile -> 202); this leader-elected worker
// drains queued jobs out-of-band on a tick so a live-cluster pool walk never
// blocks an HTTP request. Each job is idempotent + resumable from a per-job
// Cursor watermark persisted after every batch.
//
// Policies (US-002): report (DEFAULT — count + report, never delete) and gc
// (enqueue the orphan chunk for deletion via the GC queue). The restore
// policy (rebuild the manifest row from the back-reference) is deferred to the
// trailing US-002b — it shares the US-004 rebuild-from-back-reference grouping
// and is rejected by meta.StartReconcile until then.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// ScanScope addresses one (cluster, pool, namespace) the scanner walks. A
// reconcile job carries exactly one scope; multi-cluster reconcile is driven
// as multiple jobs.
type ScanScope struct {
	Cluster   string
	Pool      string
	Namespace string
}

// ScannedChunk is one enumerated data-tier chunk handed to the worker's
// classifier. Backref is the decoded owner pointer (US-001); HasBackref is
// false for a legacy / STRATA_CHUNK_BACKREF=false chunk that carries no
// back-reference — those can never be safely attributed to an owner, so they
// are reported but never deleted.
type ScannedChunk struct {
	Cluster    string
	Pool       string
	Namespace  string
	OID        string
	Size       int64
	Backref    data.Backref
	HasBackref bool
}

// ChunkScanner enumerates the data-tier chunks in a scope, invoking visit per
// chunk with a resume cursor pointing at-or-after that chunk. The worker
// persists the cursor handed to visit so a crashed/released pass resumes
// instead of re-walking from the front. A scanner that cannot enumerate the
// backend (e.g. the RADOS scanner against a non-librados build, or an
// unsupported backend) returns an error the worker records on the job.
//
// Scan starts from startCursor ("" == front). Implementations decode the
// opaque string into their native cursor (the RADOS scanner encodes the
// librados PG-hash position as decimal).
type ChunkScanner interface {
	Scan(ctx context.Context, scope ScanScope, startCursor string, visit func(ScannedChunk, string) error) error
}

// Observer receives per-chunk + per-orphan counts so the worker stays free of
// a direct prometheus import (the metrics package wires a concrete impl). All
// methods must tolerate a nil receiver via the helper wrappers below.
type Observer interface {
	ChunkScanned()
	OrphanFound(resolution string)
	// DanglingFound reports a dangling manifest resolved by resolution
	// (report|quarantine) — the US-003 meta->data pass.
	DanglingFound(resolution string)
	ReconcileError()
}

// ChunkProber answers "does this chunk still exist in the data tier?" for the
// dangling-manifest pass (US-003). It mirrors data.ChunkStater; the worker
// takes the narrower interface so tests inject a fake without a data backend.
// A nil Prober means the data backend cannot probe (e.g. a default-tag RADOS
// build) — a dangling job then records an error and quarantines nothing.
type ChunkProber interface {
	ChunkExists(ctx context.Context, ref data.ChunkRef) (bool, error)
}

// Config wires a Worker.
type Config struct {
	Meta    meta.Store
	Scanner ChunkScanner
	// Data is the data backend the restore policy (US-002b) reads chunk bytes
	// from to recompute the single-part ETag of a rebuilt manifest. Nil when the
	// backend cannot serve chunks for restore — a restore job then errors and
	// rebuilds nothing (report/gc passes never touch it).
	Data data.Backend
	// Prober answers chunk-existence for the dangling-manifest pass (US-003).
	// Nil when the data backend cannot probe — a dangling job then errors and
	// quarantines nothing.
	Prober ChunkProber
	// Region is the GC queue region passed to EnqueueChunkDeletion when the
	// policy is gc. Mirrors the gc worker's deps.Region.
	Region string
	Logger *slog.Logger
	Obs    Observer
	// CheckpointEvery persists the job cursor + counters after this many
	// chunks. Zero defaults to 500. Smaller = more resumable, more meta load.
	CheckpointEvery int
	Now             func() time.Time
}

// Worker drains queued reconcile jobs to completion.
type Worker struct {
	cfg Config
}

// Stats summarises one RunOnce.
type Stats struct {
	JobsScanned   int
	JobsCompleted int
	Scanned       int64
	OrphansFound  int64
}

// New validates cfg and returns a worker.
func New(cfg Config) (*Worker, error) {
	if cfg.Meta == nil {
		return nil, errors.New("reconcile: meta store required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Obs == nil {
		cfg.Obs = nopObserver{}
	}
	if cfg.CheckpointEvery <= 0 {
		cfg.CheckpointEvery = 500
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Worker{cfg: cfg}, nil
}

// RunOnce processes every queued reconcile job exactly once. Returns
// aggregated stats and the first error encountered. A per-job scan failure is
// recorded on the job (state=error) and does NOT abort sibling jobs — one bad
// cluster must not stall reconcile of the rest.
func (w *Worker) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	jobs, err := w.cfg.Meta.ListReconcileJobs(ctx)
	if err != nil {
		return stats, fmt.Errorf("list reconcile jobs: %w", err)
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		stats.JobsScanned++
		if err := w.runJob(ctx, job); err != nil {
			if errors.Is(err, context.Canceled) {
				return stats, err
			}
			// Record the failure on the job and move on — a transient OSD
			// error on one cluster must not block reconcile of the others.
			w.cfg.Obs.ReconcileError()
			job.State = meta.ReconcileStateError
			job.Message = err.Error()
			if uerr := w.cfg.Meta.UpdateReconcileJob(ctx, job); uerr != nil {
				w.cfg.Logger.WarnContext(ctx, "reconcile: persist error-state failed",
					"job", job.ID, "error", uerr.Error())
			}
			w.cfg.Logger.WarnContext(ctx, "reconcile job failed",
				"job", job.ID, "cluster", job.Cluster, "pool", job.Pool, "error", err.Error())
			continue
		}
		stats.JobsCompleted++
		stats.Scanned += job.Scanned
		stats.OrphansFound += job.OrphansFound
		w.cfg.Logger.InfoContext(ctx, "reconcile completed",
			"job", job.ID, "cluster", job.Cluster, "pool", job.Pool, "policy", job.Policy,
			"scanned", job.Scanned, "orphans", job.OrphansFound,
			"orphans_gc", job.OrphansGC, "orphans_report", job.OrphansReport,
			"absent_backref", job.AbsentBackref, "errors", job.Errors)
	}
	return stats, nil
}

// runJob scans the job's scope, classifies each chunk, and resolves orphans by
// the job's policy. Resumes from job.Cursor; persists the cursor + counters
// every CheckpointEvery chunks and once at the end. Marks the job done on a
// clean drain.
func (w *Worker) runJob(ctx context.Context, job *meta.ReconcileJob) error {
	if job.State == meta.ReconcileStateQueued {
		job.State = meta.ReconcileStateRunning
		if err := w.cfg.Meta.UpdateReconcileJob(ctx, job); err != nil {
			return fmt.Errorf("mark running: %w", err)
		}
	}
	// Bucket set -> meta->data dangling-manifest pass (US-003); otherwise the
	// data->meta orphan pass (US-002).
	if job.Bucket != "" {
		return w.runDanglingJob(ctx, job)
	}
	if w.cfg.Scanner == nil {
		return errors.New("reconcile: no chunk scanner configured for this data backend")
	}
	// The restore policy (US-002b) rebuilds manifest rows from the grouped
	// orphan chunks AFTER the scan drains (a version's chunks are spread across
	// the pool walk), so it needs to read chunk bytes to recompute the ETag.
	// Fail fast if no data backend is wired rather than scan-then-fail.
	if job.Policy == meta.ReconcilePolicyRestore && w.cfg.Data == nil {
		return errors.New("reconcile: restore policy requires a data backend (none configured)")
	}
	// restoreAcc is in-memory and rebuilt empty each runJob, so a crash-resumed
	// restore (scan restarts at job.Cursor) only sees chunks at-or-after the
	// watermark. A multi-chunk version split across the persisted cursor looks
	// gapped on resume and is REPORTED, not restored — fail-safe (never stitches
	// a short object), but a crashed restore may need a full re-run (cursor "")
	// to rebuild a straddling version. See ROADMAP "Known latent bugs".
	var restoreAcc map[restoreKey]*restoreGroup
	if job.Policy == meta.ReconcilePolicyRestore {
		restoreAcc = make(map[restoreKey]*restoreGroup)
	}
	scope := ScanScope{Cluster: job.Cluster, Pool: job.Pool, Namespace: job.Namespace}
	sinceCheckpoint := 0
	scanErr := w.cfg.Scanner.Scan(ctx, scope, job.Cursor, func(c ScannedChunk, cursor string) error {
		if err := w.classify(ctx, job, c, restoreAcc); err != nil {
			return err
		}
		job.Cursor = cursor
		sinceCheckpoint++
		if sinceCheckpoint >= w.cfg.CheckpointEvery {
			sinceCheckpoint = 0
			if err := w.cfg.Meta.UpdateReconcileJob(ctx, job); err != nil {
				return fmt.Errorf("checkpoint: %w", err)
			}
		}
		return nil
	})
	if scanErr != nil {
		// Persist progress so a retry resumes from the last good cursor.
		_ = w.cfg.Meta.UpdateReconcileJob(ctx, job)
		return scanErr
	}
	// Resolve the accumulated restore groups now the full chunk set is known.
	if restoreAcc != nil {
		w.resolveRestores(ctx, job, restoreAcc)
	}
	job.State = meta.ReconcileStateDone
	job.Message = ""
	if err := w.cfg.Meta.UpdateReconcileJob(ctx, job); err != nil {
		return fmt.Errorf("mark done: %w", err)
	}
	return nil
}

// classify decides whether c is an orphan and applies the job policy. It
// mutates the job's in-memory counters; the caller persists them. restoreAcc is
// non-nil only for the restore policy (US-002b): a restorable orphan is
// accumulated there and resolved after the scan drains (its sibling chunks are
// spread across the pool walk).
func (w *Worker) classify(ctx context.Context, job *meta.ReconcileJob, c ScannedChunk, restoreAcc map[restoreKey]*restoreGroup) error {
	job.Scanned++
	w.cfg.Obs.ChunkScanned()

	// No back-reference: the chunk cannot be safely attributed to an owner, so
	// it is never a delete candidate — count it and report (US-001 graceful
	// degrade for legacy / STRATA_CHUNK_BACKREF=false chunks).
	if !c.HasBackref {
		job.AbsentBackref++
		return nil
	}

	br := c.Backref
	objectAbsent := false
	obj, err := w.cfg.Meta.GetObject(ctx, br.BucketID, br.Key, br.VersionID)
	switch {
	case err == nil:
		if manifestReferencesOID(obj.Manifest, c.OID) {
			return nil // healthy: the live manifest references this chunk.
		}
		// Object exists but this version's manifest no longer references the
		// chunk (overwritten/rolled-back version) -> orphan, but the version
		// row is INTACT (it carries its own valid manifest).
	case errors.Is(err, meta.ErrObjectNotFound), errors.Is(err, meta.ErrBucketNotFound):
		// No manifest references this chunk -> orphan, and the version row is
		// genuinely absent (the meta-older-than-data skew restore repairs).
		objectAbsent = true
	default:
		// Uncertain (transient meta error): never delete on doubt. Count an
		// error and skip.
		job.Errors++
		w.cfg.Obs.ReconcileError()
		return nil
	}

	job.OrphansFound++
	switch job.Policy {
	case meta.ReconcilePolicyRestore:
		w.classifyRestore(ctx, job, c, objectAbsent, restoreAcc)
	case meta.ReconcilePolicyGC:
		ref := data.ChunkRef{
			Cluster:   c.Cluster,
			Pool:      c.Pool,
			Namespace: c.Namespace,
			OID:       c.OID,
			Size:      c.Size,
		}
		if err := w.cfg.Meta.EnqueueChunkDeletion(ctx, w.cfg.Region, []data.ChunkRef{ref}); err != nil {
			// Enqueue failed: the orphan is found but not resolved. Count an
			// error; a re-run re-detects and re-enqueues (idempotent — GC
			// dedups by OID).
			job.Errors++
			w.cfg.Obs.ReconcileError()
			return nil
		}
		job.OrphansGC++
		w.cfg.Obs.OrphanFound(meta.ReconcilePolicyGC)
	default: // report (the safe default)
		job.OrphansReport++
		w.cfg.Obs.OrphanFound(meta.ReconcilePolicyReport)
	}
	return nil
}

// danglingListPageSize bounds one ListObjectVersions page in the dangling
// walk. The walk is resumable per page via job.Cursor (the key marker), so a
// small page is a fetch-granularity choice, never a truncation one.
const danglingListPageSize = 500

// runDanglingJob walks every object version in the job's bucket (meta->data)
// and probes that each manifest-referenced chunk still exists in the data
// tier. A version with a missing chunk is dangling and is resolved by the job
// policy (report — count only; quarantine — mark the object unreadable so a
// GET/HEAD returns a clear error instead of a silent corrupt 5xx). Resumes
// from job.Cursor (the key marker) and persists progress every CheckpointEvery
// manifests; writes a per-bucket summary on a clean drain.
func (w *Worker) runDanglingJob(ctx context.Context, job *meta.ReconcileJob) error {
	if w.cfg.Prober == nil {
		return errors.New("reconcile: no chunk prober configured for this data backend (dangling pass unsupported)")
	}
	bucketID, err := uuid.Parse(job.Bucket)
	if err != nil {
		return fmt.Errorf("parse bucket %q: %w", job.Bucket, err)
	}
	sinceCheckpoint := 0
	marker := job.Cursor
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := w.cfg.Meta.ListObjectVersions(ctx, bucketID, meta.ListOptions{
			Marker: marker,
			Limit:  danglingListPageSize,
		})
		if err != nil {
			_ = w.cfg.Meta.UpdateReconcileJob(ctx, job)
			return fmt.Errorf("list object versions: %w", err)
		}
		for _, obj := range res.Versions {
			if err := w.classifyDangling(ctx, job, obj); err != nil {
				_ = w.cfg.Meta.UpdateReconcileJob(ctx, job)
				return err
			}
			sinceCheckpoint++
			if sinceCheckpoint >= w.cfg.CheckpointEvery {
				sinceCheckpoint = 0
				if err := w.cfg.Meta.UpdateReconcileJob(ctx, job); err != nil {
					return fmt.Errorf("checkpoint: %w", err)
				}
			}
		}
		if !res.Truncated {
			break
		}
		marker = res.NextKeyMarker
		job.Cursor = marker
	}
	job.State = meta.ReconcileStateDone
	job.Message = ""
	if err := w.cfg.Meta.UpdateReconcileJob(ctx, job); err != nil {
		return fmt.Errorf("mark done: %w", err)
	}
	w.cfg.Logger.InfoContext(ctx, "reconcile dangling summary",
		"job", job.ID, "bucket", job.Bucket, "policy", job.Policy,
		"manifests_scanned", job.ManifestsScanned, "healthy", job.Healthy,
		"dangling", job.DanglingFound, "dangling_quarantine", job.DanglingQuarantine,
		"dangling_report", job.DanglingReport, "errors", job.Errors)
	return nil
}

// classifyDangling probes obj's manifest chunks and resolves a dangling
// manifest by the job policy. Mutates the job's in-memory counters.
func (w *Worker) classifyDangling(ctx context.Context, job *meta.ReconcileJob, obj *meta.Object) error {
	job.ManifestsScanned++
	// Delete markers, backend-ref objects, and zero-chunk manifests carry no
	// data-tier chunks to probe — never dangling.
	if obj.IsDeleteMarker || obj.Manifest == nil || len(obj.Manifest.Chunks) == 0 {
		job.Healthy++
		return nil
	}
	missing := false
	for _, ch := range obj.Manifest.Chunks {
		ok, err := w.cfg.Prober.ChunkExists(ctx, ch)
		if err != nil {
			// Uncertain (transient probe error): never quarantine on doubt.
			job.Errors++
			w.cfg.Obs.ReconcileError()
			return nil
		}
		if !ok {
			missing = true
			break
		}
	}
	if !missing {
		job.Healthy++
		return nil
	}
	job.DanglingFound++
	switch job.Policy {
	case meta.ReconcilePolicyQuarantine:
		reason := "reconcile: referenced data chunk missing (dangling manifest)"
		if err := w.cfg.Meta.SetObjectQuarantine(ctx, obj.BucketID, obj.Key, obj.VersionID, reason); err != nil {
			// Quarantine write failed: dangling found but not resolved. Count
			// an error; a re-run re-detects and re-quarantines (idempotent).
			job.Errors++
			w.cfg.Obs.ReconcileError()
			return nil
		}
		job.DanglingQuarantine++
		w.cfg.Obs.DanglingFound(meta.ReconcilePolicyQuarantine)
	default: // report (the safe default — count only, no mutation)
		job.DanglingReport++
		w.cfg.Obs.DanglingFound(meta.ReconcilePolicyReport)
	}
	return nil
}

// manifestReferencesOID reports whether m references the chunk OID. A nil
// manifest (delete marker, or an S3-backend BackendRef object with no native
// chunks) references no chunk OID.
func manifestReferencesOID(m *data.Manifest, oid string) bool {
	if m == nil {
		return false
	}
	for _, ch := range m.Chunks {
		if ch.OID == oid {
			return true
		}
	}
	return false
}

type nopObserver struct{}

func (nopObserver) ChunkScanned()        {}
func (nopObserver) OrphanFound(string)   {}
func (nopObserver) DanglingFound(string) {}
func (nopObserver) ReconcileError()      {}
