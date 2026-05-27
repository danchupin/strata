// Package usagerollup drives the leader-elected usage-rollup worker (US-008).
// The worker schedules N intermediate sample ticks per UTC day (default 24 =
// hourly via STRATA_USAGE_ROLLUP_SAMPLES_PER_DAY) plus one daily roll-up tick.
// Each intermediate tick snapshots bucket_stats.used_bytes / used_objects into
// an in-memory ring keyed by (bucket_id, storage_class). On the daily tick the
// worker walks every (bucket, storage class) ring, integrates the samples via
// the trapezoid rule (byte_seconds = Σ (s[i]+s[i+1])/2 × Δt + s[N-1] × Δt) and
// writes one usage_aggregates row per (bucket, storage_class, yesterday-UTC).
// N=1 (or zero samples for that day) degrades to the v0 single-sample math
// (used_bytes × 86400) — intentional fallback when a bucket was created
// mid-day or the worker just booted. Documented in
// docs/site/content/best-practices/quotas-billing.md.
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
	"github.com/danchupin/strata/internal/metrics"
	strataotel "github.com/danchupin/strata/internal/otel"
)

const secondsPerDay int64 = 86400

// DefaultSamplesPerDay is the v1 trapezoid sample count (hourly).
const DefaultSamplesPerDay = 24

// MaxSamplesPerDay caps SamplesPerDay at 1 sample per minute.
const MaxSamplesPerDay = 1440

// Config wires a Worker. New() applies defaults.
type Config struct {
	Meta meta.Store
	// Interval between rollup ticks. Default 24h.
	Interval time.Duration
	// At is the UTC clock time at which the daily tick fires. Format
	// "HH:MM". Empty defaults to "00:00".
	At string
	// SamplesPerDay is the number of intermediate sample ticks fired per
	// Interval. The daily roll-up tick integrates these via the trapezoid
	// rule. Default 24 (hourly). Out-of-range values are clamped to
	// [1, MaxSamplesPerDay] with a WARN log.
	SamplesPerDay int
	Logger        *slog.Logger
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
	ring    *sampleRing

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
	samples := cfg.SamplesPerDay
	clamped := samples
	clamped = max(clamped, 1)
	clamped = min(clamped, MaxSamplesPerDay)
	if clamped != samples && samples != 0 {
		cfg.Logger.Warn("usagerollup: clamped samples_per_day",
			"requested", samples, "applied", clamped,
			"range", fmt.Sprintf("[1, %d]", MaxSamplesPerDay))
	}
	if samples == 0 {
		clamped = DefaultSamplesPerDay
	}
	cfg.SamplesPerDay = clamped
	return &Worker{
		cfg:     cfg,
		atHour:  hh,
		atMin:   mm,
		logger:  cfg.Logger,
		nowFunc: cfg.Now,
		ring:    newSampleRing(clamped),
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
	BucketsScanned int
	RowsWritten    int
	BucketsErrored int
}

// Run sleeps until the next scheduled fire time, runs RunOnce, then loops on
// cfg.Interval. Intermediate sample ticks fire every Interval/SamplesPerDay.
// ctx cancellation returns immediately.
func (w *Worker) Run(ctx context.Context) error {
	w.logger.InfoContext(ctx, "usagerollup: starting",
		"interval", w.cfg.Interval,
		"at", w.cfg.At,
		"samples_per_day", w.cfg.SamplesPerDay,
	)

	var sampleC <-chan time.Time
	if w.cfg.SamplesPerDay > 1 {
		st := time.NewTicker(w.cfg.Interval / time.Duration(w.cfg.SamplesPerDay))
		defer st.Stop()
		sampleC = st.C
	}

	daily := time.NewTimer(time.Until(w.nextFire(w.nowFunc())))
	defer daily.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sampleC:
			if err := w.SampleOnce(ctx, w.nowFunc()); err != nil && !errors.Is(err, context.Canceled) {
				w.logger.WarnContext(ctx, "usagerollup: sample failed", "error", err.Error())
			}
		case <-daily.C:
			if _, err := w.RunOnce(ctx, w.nowFunc()); err != nil && !errors.Is(err, context.Canceled) {
				w.logger.WarnContext(ctx, "usagerollup: tick failed", "error", err.Error())
			}
			next := w.nextFire(w.nowFunc())
			wait := time.Until(next)
			if wait <= 0 {
				wait = w.cfg.Interval
			}
			daily.Reset(wait)
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

// SampleOnce snapshots bucket_stats for every bucket into the in-memory ring.
// Called from the intermediate sample ticker; safe to call manually from tests
// to drive a virtual day.
func (w *Worker) SampleOnce(ctx context.Context, _ time.Time) error {
	buckets, err := w.cfg.Meta.ListBuckets(ctx, "")
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}
	for _, b := range buckets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		bs, err := w.cfg.Meta.GetBucketStats(ctx, b.ID)
		if err != nil {
			w.logger.WarnContext(ctx, "usagerollup: sample bucket failed",
				"bucket", b.Name, "error", err.Error())
			continue
		}
		class := b.DefaultClass
		if class == "" {
			class = "STANDARD"
		}
		w.ring.add(RingKey{BucketID: b.ID, Class: class},
			Sample{Bytes: bs.UsedBytes, Objects: bs.UsedObjects})
	}
	return nil
}

// RunOnce performs a single rollup pass: every bucket gets one usage_aggregates
// row for the UTC day strictly before `now`. Returns aggregate stats.
func (w *Worker) RunOnce(ctx context.Context, now time.Time) (Stats, error) {
	start := time.Now()
	iterCtx, span := strataotel.StartIteration(ctx, w.tracerOrNoop(), "usage-rollup")
	stats, err := w.runOnce(iterCtx, now)
	if err == nil {
		err = w.takeIterErr()
	} else {
		_ = w.takeIterErr()
	}
	strataotel.EndIteration(span, err)
	metrics.ObserveWorkerTick("usage-rollup", err, time.Since(start))
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
	storageClass := b.DefaultClass
	if storageClass == "" {
		storageClass = "STANDARD"
	}
	key := RingKey{BucketID: b.ID, Class: storageClass}
	samples := w.ring.drain(key)
	if len(samples) == 0 {
		bs, err := w.cfg.Meta.GetBucketStats(ctx, b.ID)
		if err != nil {
			return fmt.Errorf("get stats: %w", err)
		}
		samples = []Sample{{Bytes: bs.UsedBytes, Objects: bs.UsedObjects}}
	}
	byteSamples := make([]int64, len(samples))
	objectSamples := make([]int64, len(samples))
	for i, s := range samples {
		byteSamples[i] = s.Bytes
		objectSamples[i] = s.Objects
	}
	agg := meta.UsageAggregate{
		BucketID:       b.ID,
		Bucket:         b.Name,
		StorageClass:   storageClass,
		Day:            day,
		ByteSeconds:    Trapezoid(byteSamples, secondsPerDay),
		ObjectCountAvg: AverageObjects(objectSamples),
		ObjectCountMax: MaxObjects(objectSamples),
		ComputedAt:     w.nowFunc(),
	}
	return w.cfg.Meta.WriteUsageAggregate(ctx, agg)
}

func previousUTCDay(now time.Time) time.Time {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return today.AddDate(0, 0, -1)
}
