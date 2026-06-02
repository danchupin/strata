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
	ReconcileError()
}

// Config wires a Worker.
type Config struct {
	Meta    meta.Store
	Scanner ChunkScanner
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
	if w.cfg.Scanner == nil {
		return errors.New("reconcile: no chunk scanner configured for this data backend")
	}
	scope := ScanScope{Cluster: job.Cluster, Pool: job.Pool, Namespace: job.Namespace}
	sinceCheckpoint := 0
	scanErr := w.cfg.Scanner.Scan(ctx, scope, job.Cursor, func(c ScannedChunk, cursor string) error {
		if err := w.classify(ctx, job, c); err != nil {
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
	job.State = meta.ReconcileStateDone
	job.Message = ""
	if err := w.cfg.Meta.UpdateReconcileJob(ctx, job); err != nil {
		return fmt.Errorf("mark done: %w", err)
	}
	return nil
}

// classify decides whether c is an orphan and applies the job policy. It
// mutates the job's in-memory counters; the caller persists them.
func (w *Worker) classify(ctx context.Context, job *meta.ReconcileJob, c ScannedChunk) error {
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
	obj, err := w.cfg.Meta.GetObject(ctx, br.BucketID, br.Key, br.VersionID)
	switch {
	case err == nil:
		if manifestReferencesOID(obj.Manifest, c.OID) {
			return nil // healthy: the live manifest references this chunk.
		}
		// Object exists but this version's manifest no longer references the
		// chunk (overwritten/rolled-back version) -> orphan.
	case errors.Is(err, meta.ErrObjectNotFound), errors.Is(err, meta.ErrBucketNotFound):
		// No manifest references this chunk -> orphan.
	default:
		// Uncertain (transient meta error): never delete on doubt. Count an
		// error and skip.
		job.Errors++
		w.cfg.Obs.ReconcileError()
		return nil
	}

	job.OrphansFound++
	switch job.Policy {
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

func (nopObserver) ChunkScanned()      {}
func (nopObserver) OrphanFound(string) {}
func (nopObserver) ReconcileError()    {}
