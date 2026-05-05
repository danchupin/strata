// Package bucketstats samples per-bucket / per-class object byte totals on a
// timer and publishes them via a narrow Sink interface. Cmd-layer plugs a
// metrics.BucketBytes gauge updater.
package bucketstats

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// DefaultTopN caps the per-shard distribution sampling pass to the top N
// buckets by total bytes. Keeps the cardinality of strata_bucket_shard_*
// metrics bounded (top-N * shard_count). Override via Sampler.TopN.
const DefaultTopN = 100

// Sink receives per-(bucket, class) byte totals at the end of each sample
// pass. The metrics adapter wraps prometheus.GaugeVec.Set.
type Sink interface {
	SetBucketBytes(bucket, class string, bytes int64)
}

// ShardSink receives per-(bucket, shard) byte and object totals for the
// top-N buckets at the end of each sample pass (US-012). Implementations
// should treat ResetBucketShard as a wipe of every (bucket, *) series so
// buckets that exit the top-N window do not linger as stale gauges.
type ShardSink interface {
	SetBucketShardBytes(bucket string, shard int, bytes int64)
	SetBucketShardObjects(bucket string, shard int, objects int64)
	ResetBucketShard(bucket string)
}

// Sampler walks ListBuckets + ListObjects on every Tick and reports totals
// per (bucket, storage_class) to the Sink. Default Interval=1h. Now and
// PageLimit are testing seams. When ShardSink is set, the sampler also
// emits per-shard bytes/objects for the top-N buckets via
// meta.Store.SampleBucketShardStats (US-012).
type Sampler struct {
	Meta      meta.Store
	Sink      Sink
	ShardSink ShardSink
	Interval  time.Duration
	Logger    *slog.Logger
	PageLimit int
	// TopN bounds the per-shard distribution sampling pass. Zero/negative
	// falls back to DefaultTopN.
	TopN int

	// prevTopN tracks bucket names from the previous per-shard pass so the
	// sampler can ResetBucketShard for buckets that exit the top-N window.
	prevTopN map[string]struct{}
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
	if s.Sink == nil && s.ShardSink == nil {
		return nil
	}
	if s.PageLimit <= 0 {
		s.PageLimit = 1000
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	topN := s.TopN
	if topN <= 0 {
		topN = DefaultTopN
	}
	buckets, err := s.Meta.ListBuckets(ctx, "")
	if err != nil {
		return err
	}
	type bucketTotal struct {
		bucket *meta.Bucket
		bytes  int64
	}
	totalsPerBucket := make([]bucketTotal, 0, len(buckets))
	for _, b := range buckets {
		if err := ctx.Err(); err != nil {
			return err
		}
		totals, err := s.sampleBucket(ctx, b.ID)
		if err != nil {
			s.Logger.WarnContext(ctx, "bucketstats: bucket failed", "bucket", b.Name, "error", err.Error())
			continue
		}
		var sum int64
		for class, bytes := range totals {
			if s.Sink != nil {
				s.Sink.SetBucketBytes(b.Name, class, bytes)
			}
			sum += bytes
		}
		totalsPerBucket = append(totalsPerBucket, bucketTotal{bucket: b, bytes: sum})
	}

	if s.ShardSink == nil {
		return nil
	}

	sort.Slice(totalsPerBucket, func(i, j int) bool {
		if totalsPerBucket[i].bytes != totalsPerBucket[j].bytes {
			return totalsPerBucket[i].bytes > totalsPerBucket[j].bytes
		}
		return totalsPerBucket[i].bucket.Name < totalsPerBucket[j].bucket.Name
	})
	if len(totalsPerBucket) > topN {
		totalsPerBucket = totalsPerBucket[:topN]
	}

	currentTopN := make(map[string]struct{}, len(totalsPerBucket))
	for _, bt := range totalsPerBucket {
		currentTopN[bt.bucket.Name] = struct{}{}
	}
	for prev := range s.prevTopN {
		if _, still := currentTopN[prev]; !still {
			s.ShardSink.ResetBucketShard(prev)
		}
	}
	s.prevTopN = currentTopN

	for _, bt := range totalsPerBucket {
		if err := ctx.Err(); err != nil {
			return err
		}
		shardCount := bt.bucket.ShardCount
		if shardCount <= 0 {
			continue
		}
		stats, err := s.Meta.SampleBucketShardStats(ctx, bt.bucket.ID, shardCount)
		if err != nil {
			s.Logger.WarnContext(ctx, "bucketstats: shard sample failed", "bucket", bt.bucket.Name, "error", err.Error())
			continue
		}
		// Reset BEFORE re-emitting so a shard that drained to zero since the
		// last pass disappears from the gauge set.
		s.ShardSink.ResetBucketShard(bt.bucket.Name)
		for shard, st := range stats {
			s.ShardSink.SetBucketShardBytes(bt.bucket.Name, shard, st.Bytes)
			s.ShardSink.SetBucketShardObjects(bt.bucket.Name, shard, st.Objects)
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
