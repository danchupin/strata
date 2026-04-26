// Package bucketstats samples per-bucket / per-class object byte totals on a
// timer and publishes them via a narrow Sink interface. Cmd-layer plugs a
// metrics.BucketBytes gauge updater.
package bucketstats

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// Sink receives per-(bucket, class) byte totals at the end of each sample
// pass. The metrics adapter wraps prometheus.GaugeVec.Set.
type Sink interface {
	SetBucketBytes(bucket, class string, bytes int64)
}

// Sampler walks ListBuckets + ListObjects on every Tick and reports totals
// per (bucket, storage_class) to the Sink. Default Interval=1h. Now and
// PageLimit are testing seams.
type Sampler struct {
	Meta      meta.Store
	Sink      Sink
	Interval  time.Duration
	Logger    *slog.Logger
	PageLimit int
}

// Run loops on Interval until ctx is cancelled. Use RunOnce for tests.
func (s *Sampler) Run(ctx context.Context) error {
	if s.Interval <= 0 {
		s.Interval = time.Hour
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.RunOnce(ctx); err != nil {
				s.Logger.WarnContext(ctx, "bucketstats: sample failed", "error", err.Error())
			}
		}
	}
}

// RunOnce runs a single sample pass; exported for tests + cmd --once flag.
func (s *Sampler) RunOnce(ctx context.Context) error {
	if s.Sink == nil {
		return nil
	}
	if s.PageLimit <= 0 {
		s.PageLimit = 1000
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	buckets, err := s.Meta.ListBuckets(ctx, "")
	if err != nil {
		return err
	}
	for _, b := range buckets {
		if err := ctx.Err(); err != nil {
			return err
		}
		totals, err := s.sampleBucket(ctx, b.ID)
		if err != nil {
			s.Logger.WarnContext(ctx, "bucketstats: bucket failed", "bucket", b.Name, "error", err.Error())
			continue
		}
		for class, bytes := range totals {
			s.Sink.SetBucketBytes(b.Name, class, bytes)
		}
	}
	return nil
}

func (s *Sampler) sampleBucket(ctx context.Context, bucketID uuid.UUID) (map[string]int64, error) {
	totals := map[string]int64{}
	opts := meta.ListOptions{Limit: s.PageLimit}
	for {
		res, err := s.Meta.ListObjects(ctx, bucketID, opts)
		if err != nil {
			return nil, err
		}
		for _, o := range res.Objects {
			class := o.StorageClass
			if class == "" {
				class = "STANDARD"
			}
			totals[class] += o.Size
		}
		if !res.Truncated {
			return totals, nil
		}
		opts.Marker = res.NextMarker
	}
}
