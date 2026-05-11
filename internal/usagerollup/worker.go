// Package usagerollup drives the leader-elected usage-rollup worker (US-008).
// On each scheduled tick (default 24h at 00:00 UTC) the worker walks every
// bucket, samples bucket_stats once, and writes a (bucket_id, storage_class,
// yesterday-UTC, byte_seconds, object_count_avg, object_count_max) row into
// the usage_aggregates feed used by external billing.
//
// v1 byte_seconds approximation: byte_seconds = used_bytes * 86400. This is
// accurate when usage is constant across the day and over-counts spikes that
// fall before the snapshot, under-counts spikes that fall after. Documented
// in docs/site/content/best-practices/quotas-billing.md (US-011).
package usagerollup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/meta"
	strataotel "github.com/danchupin/strata/internal/otel"
)

const secondsPerDay int64 = 86400

// Config wires a Worker. New() applies defaults.
type Config struct {
	Meta meta.Store
	// Interval between rollup ticks. Default 24h.
	Interval time.Duration
	// At is the UTC clock time at which the daily tick fires. Format
	// "HH:MM". Empty defaults to "00:00".
	At     string
	Logger *slog.Logger
	// Now overrides the wall clock for tests. Returns UTC.
	Now func() time.Time
	// Tracer emits per-iteration parent spans (`worker.usage-rollup.tick`)
	// plus `usage_rollup.sample_bucket` sub-op children. Nil falls back to a
	// process-shared no-op tracer.
	Tracer trace.Tracer
}

// Worker runs the rollup loop.
type Worker struct {
	cfg     Config
	atHour  int
	atMin   int
	logger  *slog.Logger
	nowFunc func() time.Time

	iterErrMu sync.Mutex
	iterErr   error
}

func (w *Worker) tracerOrNoop() trace.Tracer {
	if w.cfg.Tracer == nil {
		return strataotel.NoopTracer()
	}
	return w.cfg.Tracer
}

func (w *Worker) recordIterErr(err error) {
	if err == nil {
		return
	}
	w.iterErrMu.Lock()
	if w.iterErr == nil {
		w.iterErr = err
	}
	w.iterErrMu.Unlock()
}

func (w *Worker) takeIterErr() error {
	w.iterErrMu.Lock()
	defer w.iterErrMu.Unlock()
	err := w.iterErr
	w.iterErr = nil
	return err
}

// New validates cfg and returns a Worker.
func New(cfg Config) (*Worker, error) {
	if cfg.Meta == nil {
		return nil, errors.New("usagerollup: meta store required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 24 * time.Hour
	}
	if cfg.At == "" {
		cfg.At = "00:00"
	}
	hh, mm, err := parseAt(cfg.At)
	if err != nil {
		return nil, fmt.Errorf("usagerollup: parse At %q: %w", cfg.At, err)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Worker{
		cfg:     cfg,
		atHour:  hh,
		atMin:   mm,
		logger:  cfg.Logger,
		nowFunc: cfg.Now,
	}, nil
}

func parseAt(s string) (int, int, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, 0, err
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("invalid clock time %q", s)
	}
	return h, m, nil
}

// Stats summarises a single rollup pass.
type Stats struct {
	BucketsScanned  int
	RowsWritten     int
	BucketsErrored  int
}

// Run sleeps until the next scheduled fire time, runs RunOnce, then loops on
// cfg.Interval. ctx cancellation returns immediately.
func (w *Worker) Run(ctx context.Context) error {
	w.logger.InfoContext(ctx, "usagerollup: starting",
		"interval", w.cfg.Interval,
		"at", w.cfg.At,
	)
	for {
		next := w.nextFire(w.nowFunc())
		wait := time.Until(next)
		if wait <= 0 {
			wait = 0
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		if _, err := w.RunOnce(ctx, w.nowFunc()); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.WarnContext(ctx, "usagerollup: tick failed", "error", err.Error())
		}
		// Sleep one Interval before the next fire so a fast clock skew does
		// not refire immediately.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(w.cfg.Interval):
		}
	}
}

// nextFire returns the next time at or after `now` whose clock matches
// (atHour:atMin). If `now` is already past today's fire time, fire is
// scheduled for tomorrow.
func (w *Worker) nextFire(now time.Time) time.Time {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), w.atHour, w.atMin, 0, 0, time.UTC)
	if !today.After(now) {
		today = today.AddDate(0, 0, 1)
	}
	return today
}

// RunOnce performs a single rollup pass: every bucket gets one usage_aggregates
// row for the UTC day strictly before `now`. Returns aggregate stats.
func (w *Worker) RunOnce(ctx context.Context, now time.Time) (Stats, error) {
	iterCtx, span := strataotel.StartIteration(ctx, w.tracerOrNoop(), "usage-rollup")
	stats, err := w.runOnce(iterCtx, now)
	if err == nil {
		err = w.takeIterErr()
	} else {
		_ = w.takeIterErr()
	}
	strataotel.EndIteration(span, err)
	return stats, err
}

func (w *Worker) runOnce(ctx context.Context, now time.Time) (Stats, error) {
	day := previousUTCDay(now)
	var stats Stats
	buckets, err := w.cfg.Meta.ListBuckets(ctx, "")
	if err != nil {
		return stats, fmt.Errorf("list buckets: %w", err)
	}
	for _, b := range buckets {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		stats.BucketsScanned++
		bucketCtx, bucketSpan := w.tracerOrNoop().Start(ctx, "usage_rollup.sample_bucket",
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(
				strataotel.AttrComponentWorker,
				attribute.String(strataotel.WorkerKey, "usage-rollup"),
				attribute.String("strata.usage_rollup.bucket", b.Name),
				attribute.String("strata.usage_rollup.bucket_id", b.ID.String()),
				attribute.String("strata.usage_rollup.day", day.Format("2006-01-02")),
			),
		)
		if rerr := w.rollupBucket(bucketCtx, b, day); rerr != nil {
			stats.BucketsErrored++
			bucketSpan.RecordError(rerr)
			bucketSpan.SetStatus(codes.Error, rerr.Error())
			w.recordIterErr(rerr)
			w.logger.WarnContext(ctx, "usagerollup: bucket failed",
				"bucket", b.Name, "error", rerr.Error())
			bucketSpan.End()
			continue
		}
		stats.RowsWritten++
		bucketSpan.End()
	}
	w.logger.InfoContext(ctx, "usagerollup: tick complete",
		"day", day.Format("2006-01-02"),
		"buckets_scanned", stats.BucketsScanned,
		"rows_written", stats.RowsWritten,
		"buckets_errored", stats.BucketsErrored,
	)
	return stats, nil
}

func (w *Worker) rollupBucket(ctx context.Context, b *meta.Bucket, day time.Time) error {
	bs, err := w.cfg.Meta.GetBucketStats(ctx, b.ID)
	if err != nil {
		return fmt.Errorf("get stats: %w", err)
	}
	storageClass := b.DefaultClass
	if storageClass == "" {
		storageClass = "STANDARD"
	}
	agg := meta.UsageAggregate{
		BucketID:       b.ID,
		Bucket:         b.Name,
		StorageClass:   storageClass,
		Day:            day,
		ByteSeconds:    bs.UsedBytes * secondsPerDay,
		ObjectCountAvg: bs.UsedObjects,
		ObjectCountMax: bs.UsedObjects,
		ComputedAt:     w.nowFunc(),
	}
	return w.cfg.Meta.WriteUsageAggregate(ctx, agg)
}

func previousUTCDay(now time.Time) time.Time {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return today.AddDate(0, 0, -1)
}
