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
// The memory backend's storage is shard-agnostic (a flat map per bucket),
// so the worker pass against memory is a state-machine exercise: it
// iterates ListObjectVersions to bound the scan with LastKey watermarks,
// then calls CompleteReshard. Cassandra's per-row partition rewrite lands
// on top of this skeleton in a follow-up story.
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

// runJob walks every object version of the bucket and persists a watermark
// after each batch so a crash resumes from LastKey.
func (w *Worker) runJob(ctx context.Context, job *meta.ReshardJob) (int, error) {
	if job == nil {
		return 0, nil
	}
	copied := 0
	cursor := job.LastKey
	for {
		if ctx.Err() != nil {
			return copied, ctx.Err()
		}
		opts := meta.ListOptions{Limit: w.cfg.BatchLimit, Marker: cursor}
		res, err := w.cfg.Meta.ListObjectVersions(ctx, job.BucketID, opts)
		if err != nil {
			return copied, err
		}
		if len(res.Versions) == 0 {
			break
		}
		for _, v := range res.Versions {
			copied++
			cursor = v.Key
		}
		job.LastKey = cursor
		if err := w.cfg.Meta.UpdateReshardJob(ctx, job); err != nil {
			return copied, err
		}
		if !res.Truncated {
			break
		}
	}
	return copied, nil
}

// StartReshard is a thin wrapper for callers that don't want to import
// meta directly. Returns a clone of the queued job.
func StartReshard(ctx context.Context, store meta.Store, bucketID uuid.UUID, target int) (*meta.ReshardJob, error) {
	return store.StartReshard(ctx, bucketID, target)
}
