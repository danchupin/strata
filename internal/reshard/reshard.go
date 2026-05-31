// Package reshard drives the online per-bucket shard-resize flow (US-045).
// A reshard job is started by the operator (CLI / admin endpoint) which
// writes a row to reshard_jobs and stamps the bucket's TargetShardCount
// column. The worker walks every existing object version under the old
// partition layout and re-keys each row under the new layout, persisting a
// LastKey watermark per batch for resumability. Once every key is processed,
// the worker LWT-flips buckets.shard_count to the target and removes the
// job. While the job is in flight, ListObjects is expected to read the
// union of the active and target layouts so clients see the same key set
// throughout — that union read is the responsibility of the meta backend.
//
// The memory and TiKV backends are shard-agnostic (a flat map / a single
// ordered range scan), so a key's physical placement does not depend on the
// shard count: the worker pass against them moves no rows and is an
// immediate-complete state-machine exercise that walks ListObjectVersions to
// bound the scan with LastKey watermarks, then calls CompleteReshard. Cassandra
// implements meta.ReshardMigrator and the worker drives MigrateReshardKey per
// key to physically rewrite each row into the target partition (cleanup of the
// source orphan inline) before the flip — US-003.
package reshard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// Stats counts the work performed by one Run / RunOnce.
type Stats struct {
	JobsScanned   int
	JobsCompleted int
	// ObjectsCopied is the number of rows physically relocated into the target
	// shard layout (sum of MigrateReshardKey moves). Zero on shard-agnostic
	// backends (memory, TiKV) — they have nothing to move.
	ObjectsCopied int
}

// Config wires a Worker.
type Config struct {
	Meta       meta.Store
	Logger     *slog.Logger
	BatchLimit int
	Interval   time.Duration
	Now        func() time.Time
}

// Worker drives queued reshard jobs to completion. Use Run for the
// long-running daemon entry; tests + the admin endpoint call RunOnce for
// deterministic single-pass execution.
type Worker struct {
	cfg Config
}

// New validates cfg and returns a worker.
func New(cfg Config) (*Worker, error) {
	if cfg.Meta == nil {
		return nil, errors.New("reshard: meta store required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = 500
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Worker{cfg: cfg}, nil
}

// Run loops on a ticker until ctx is cancelled. Each tick calls RunOnce.
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		if _, err := w.RunOnce(ctx); err != nil {
			w.cfg.Logger.WarnContext(ctx, "reshard tick failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// RunOnce processes every queued reshard job exactly once. Returns
// aggregated stats and the first error encountered (subsequent jobs are
// not attempted).
func (w *Worker) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	jobs, err := w.cfg.Meta.ListReshardJobs(ctx)
	if err != nil {
		return stats, fmt.Errorf("list reshard jobs: %w", err)
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		stats.JobsScanned++
		copied, err := w.runJob(ctx, job)
		stats.ObjectsCopied += copied
		if err != nil {
			return stats, fmt.Errorf("reshard %s: %w", job.Bucket, err)
		}
		if err := w.cfg.Meta.CompleteReshard(ctx, job.BucketID); err != nil {
			return stats, fmt.Errorf("complete reshard %s: %w", job.Bucket, err)
		}
		stats.JobsCompleted++
		w.cfg.Logger.InfoContext(ctx, "reshard completed",
			"bucket", job.Bucket, "source", job.Source, "target", job.Target, "copied", copied)
	}
	return stats, nil
}

// runJob walks every object key of the bucket under the in-flight union read and
// physically relocates each key's rows from the source shard layout to the target
// layout, persisting a LastKey watermark after each batch so a crash resumes from
// where it stopped. The row move is delegated to the backend's optional
// meta.ReshardMigrator (Cassandra's sharded objects table); shard-agnostic
// backends (memory, TiKV) don't implement it, so the walk moves nothing and the
// caller proceeds straight to CompleteReshard — the immediate-complete no-op.
//
// Cleanup-before-flip: MigrateReshardKey copies a key's rows into the target and
// deletes the source orphans inline, so once this walk drains every key the
// source layout holds no diverging row. RunOnce only then calls CompleteReshard —
// flipping the active count before the orphans were cleaned would let the
// post-flip listing double-emit a moved key. Returns the number of rows moved.
func (w *Worker) runJob(ctx context.Context, job *meta.ReshardJob) (int, error) {
	if job == nil {
		return 0, nil
	}
	migrator, _ := w.cfg.Meta.(meta.ReshardMigrator)
	moved := 0
	cursor := job.LastKey
	lastMigrated := "" // last key handed to the migrator — dedups a key's versions
	for {
		if ctx.Err() != nil {
			return moved, ctx.Err()
		}
		startMarker := cursor
		opts := meta.ListOptions{Limit: w.cfg.BatchLimit, Marker: cursor}
		res, err := w.cfg.Meta.ListObjectVersions(ctx, job.BucketID, opts)
		if err != nil {
			return moved, err
		}
		if len(res.Versions) == 0 {
			break
		}
		for _, v := range res.Versions {
			if migrator != nil && v.Key != lastMigrated {
				n, err := migrator.MigrateReshardKey(ctx, job.BucketID, v.Key)
				if err != nil {
					return moved, err
				}
				moved += n
				lastMigrated = v.Key
			}
			cursor = v.Key
		}
		job.LastKey = cursor
		if err := w.cfg.Meta.UpdateReshardJob(ctx, job); err != nil {
			return moved, err
		}
		if !res.Truncated {
			break
		}
		// Forward-progress guard: the version-marker (key >= marker) is inclusive
		// and the union read does not resume mid-key, so a key with more versions
		// than one page would pin the marker on itself forever. MigrateReshardKey
		// already moved ALL of that key's versions on first sight, so stepping the
		// marker just past it is safe and guarantees termination.
		if cursor == startMarker {
			cursor += "\x00"
		}
	}
	return moved, nil
}

// StartReshard is a thin wrapper for callers that don't want to import
// meta directly. Returns a clone of the queued job.
func StartReshard(ctx context.Context, store meta.Store, bucketID uuid.UUID, target int) (*meta.ReshardJob, error) {
	return store.StartReshard(ctx, bucketID, target)
}
