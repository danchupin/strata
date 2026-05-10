// Package quotareconcile drives the leader-elected quota-reconcile worker
// (US-007). On a periodic tick it walks every bucket, sums the bytes /
// object-counts of the live (non-delete-marker) latest version per key via
// meta.Store.ListObjects, compares against the denormalised bucket_stats
// counter (US-004/005) and corrects drift via BumpBucketStats(delta) when it
// exceeds either the byte-percentage threshold (default 0.5%) or the absolute
// byte threshold (default 1 MB). Concurrent live PUT/DELETE during reconcile
// piggyback the atomic BumpBucketStats path; reconcile only adds the observed
// delta on top — successive ticks converge.
package quotareconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
)

// Config wires a Worker. New() applies defaults.
type Config struct {
	Meta meta.Store
	// Interval between reconcile ticks. Default 6h.
	Interval time.Duration
	// MinDriftBytes is the absolute byte threshold below which drift is
	// ignored. Default 1 MiB.
	MinDriftBytes int64
	// MinDriftRatio is the fraction of UsedBytes below which drift is
	// ignored. Default 0.005 (0.5 %).
	MinDriftRatio float64
	// PageLimit is the ListObjects page size. Default 1000.
	PageLimit int
	Logger    *slog.Logger
}

// Worker runs the reconcile loop.
type Worker struct {
	cfg Config
}

// New validates cfg and returns a Worker.
func New(cfg Config) (*Worker, error) {
	if cfg.Meta == nil {
		return nil, errors.New("quotareconcile: meta store required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 6 * time.Hour
	}
	if cfg.MinDriftBytes <= 0 {
		cfg.MinDriftBytes = 1 << 20
	}
	if cfg.MinDriftRatio <= 0 {
		cfg.MinDriftRatio = 0.005
	}
	if cfg.PageLimit <= 0 {
		cfg.PageLimit = 1000
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Worker{cfg: cfg}, nil
}

// Stats summarises a single Run.
type Stats struct {
	BucketsScanned   int
	BucketsCorrected int
	ObjectsScanned   int
}

// Run loops on cfg.Interval until ctx is cancelled. RunOnce is invoked at
// startup so the first reconcile does not wait a full Interval.
func (w *Worker) Run(ctx context.Context) error {
	w.cfg.Logger.InfoContext(ctx, "quotareconcile: starting",
		"interval", w.cfg.Interval,
		"min_drift_bytes", w.cfg.MinDriftBytes,
		"min_drift_ratio", w.cfg.MinDriftRatio,
	)
	if _, err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.cfg.Logger.WarnContext(ctx, "quotareconcile: initial tick failed", "error", err.Error())
	}
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if _, err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.cfg.Logger.WarnContext(ctx, "quotareconcile: tick failed", "error", err.Error())
			}
		}
	}
}

// RunOnce performs a single reconcile pass over every bucket. Returns
// aggregate stats; per-bucket drift is also published via the
// strata_quota_reconcile_drift_bytes gauge.
func (w *Worker) RunOnce(ctx context.Context) (Stats, error) {
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
		corrected, scanned, rerr := w.reconcileBucket(ctx, b)
		stats.ObjectsScanned += scanned
		if rerr != nil {
			w.cfg.Logger.WarnContext(ctx, "quotareconcile: bucket failed",
				"bucket", b.Name, "error", rerr.Error())
			continue
		}
		if corrected {
			stats.BucketsCorrected++
		}
	}
	return stats, nil
}

func (w *Worker) reconcileBucket(ctx context.Context, b *meta.Bucket) (corrected bool, scanned int, err error) {
	walkBytes, walkObjects, scanned, err := w.walkBucket(ctx, b.ID)
	if err != nil {
		return false, scanned, err
	}
	stats, err := w.cfg.Meta.GetBucketStats(ctx, b.ID)
	if err != nil {
		return false, scanned, fmt.Errorf("get stats: %w", err)
	}
	driftBytes := walkBytes - stats.UsedBytes
	driftObjects := walkObjects - stats.UsedObjects
	metrics.QuotaReconcileDriftBytes.WithLabelValues(b.Name).Set(float64(driftBytes))

	if !driftExceedsThreshold(driftBytes, driftObjects, stats.UsedBytes, w.cfg.MinDriftBytes, w.cfg.MinDriftRatio) {
		w.cfg.Logger.DebugContext(ctx, "quotareconcile: bucket within tolerance",
			"bucket", b.Name,
			"drift_bytes", driftBytes,
			"drift_objects", driftObjects,
			"used_bytes", stats.UsedBytes,
			"used_objects", stats.UsedObjects,
		)
		return false, scanned, nil
	}
	if _, err := w.cfg.Meta.BumpBucketStats(ctx, b.ID, driftBytes, driftObjects); err != nil {
		return false, scanned, fmt.Errorf("bump stats: %w", err)
	}
	w.cfg.Logger.InfoContext(ctx, "quotareconcile: drift corrected",
		"bucket", b.Name,
		"drift_bytes", driftBytes,
		"drift_objects", driftObjects,
		"used_bytes_before", stats.UsedBytes,
		"used_objects_before", stats.UsedObjects,
	)
	return true, scanned, nil
}

func (w *Worker) walkBucket(ctx context.Context, bucketID uuid.UUID) (bytes int64, objects int64, scanned int, err error) {
	marker := ""
	for {
		if ctx.Err() != nil {
			return bytes, objects, scanned, ctx.Err()
		}
		res, lerr := w.cfg.Meta.ListObjects(ctx, bucketID, meta.ListOptions{
			Marker: marker,
			Limit:  w.cfg.PageLimit,
		})
		if lerr != nil {
			return bytes, objects, scanned, fmt.Errorf("list objects: %w", lerr)
		}
		for _, o := range res.Objects {
			scanned++
			if o.IsDeleteMarker {
				continue
			}
			bytes += o.Size
			objects++
		}
		if !res.Truncated || res.NextMarker == "" {
			return bytes, objects, scanned, nil
		}
		marker = res.NextMarker
	}
}

// driftExceedsThreshold reports whether the observed drift warrants a
// correction. Object-count drift always trips (it's exact). Byte drift trips
// when |drift| > max(MinDriftBytes, used*MinDriftRatio).
func driftExceedsThreshold(driftBytes, driftObjects, usedBytes int64, minBytes int64, minRatio float64) bool {
	if driftObjects != 0 {
		return true
	}
	if driftBytes == 0 {
		return false
	}
	abs := driftBytes
	if abs < 0 {
		abs = -abs
	}
	threshold := minBytes
	if usedBytes > 0 {
		ratioThreshold := int64(float64(usedBytes) * minRatio)
		if ratioThreshold > threshold {
			threshold = ratioThreshold
		}
	}
	return abs > threshold
}
