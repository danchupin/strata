// Per-(bucket, storage_class, day) usage rollup rows (US-008).
//
// Mirrors the cassandra usage_aggregates / usage_aggregates_classes shape:
// the aggregate row carries the byte-seconds + object-count summary, and a
// sibling presence row (UsageClassIndexKey) indexes which storage classes
// have ever been rolled up for a bucket so ListUserUsage can fan out per
// (bucket, class) without a cluster-wide scan.
package tikv

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

const usageDaySuffixLen = 4

func dayEpoch(t time.Time) uint32 {
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return uint32(day.Unix() / 86400)
}

func dayFromEpoch(epoch uint32) time.Time {
	return time.Unix(int64(epoch)*86400, 0).UTC()
}

type usageAggregatePayload struct {
	ByteSeconds    int64     `json:"byte_seconds"`
	ObjectCountAvg int64     `json:"object_count_avg"`
	ObjectCountMax int64     `json:"object_count_max"`
	ComputedAt     time.Time `json:"computed_at"`
}

func (s *Store) WriteUsageAggregate(ctx context.Context, agg meta.UsageAggregate) (err error) {
	day := dayEpoch(agg.Day)
	computed := agg.ComputedAt
	if computed.IsZero() {
		computed = time.Now().UTC()
	}
	payload, err := json.Marshal(usageAggregatePayload{
		ByteSeconds:    agg.ByteSeconds,
		ObjectCountAvg: agg.ObjectCountAvg,
		ObjectCountMax: agg.ObjectCountMax,
		ComputedAt:     computed,
	})
	if err != nil {
		return fmt.Errorf("tikv: encode usage aggregate: %w", err)
	}
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Set(UsageAggregateKey(agg.BucketID, agg.StorageClass, day), payload); err != nil {
		return err
	}
	if err = txn.Set(UsageClassIndexKey(agg.BucketID, agg.StorageClass), []byte{1}); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

func (s *Store) ListUsageAggregates(ctx context.Context, bucketID uuid.UUID, storageClass string, dayFrom, dayTo time.Time) ([]meta.UsageAggregate, error) {
	from := dayEpoch(dayFrom)
	to := dayEpoch(dayTo)
	prefix := UsageAggregateClassPrefix(bucketID, storageClass)
	start := make([]byte, 0, len(prefix)+usageDaySuffixLen)
	start = append(start, prefix...)
	var fb [4]byte
	binary.BigEndian.PutUint32(fb[:], from)
	start = append(start, fb[:]...)
	end := make([]byte, 0, len(prefix)+usageDaySuffixLen)
	end = append(end, prefix...)
	var tb [4]byte
	binary.BigEndian.PutUint32(tb[:], to)
	end = append(end, tb[:]...)
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	pairs, err := txn.Scan(ctx, start, end, 0)
	if err != nil {
		return nil, err
	}
	out := make([]meta.UsageAggregate, 0, len(pairs))
	for _, p := range pairs {
		if len(p.Key) < len(prefix)+usageDaySuffixLen {
			continue
		}
		dayBytes := p.Key[len(p.Key)-usageDaySuffixLen:]
		epoch := binary.BigEndian.Uint32(dayBytes)
		var pl usageAggregatePayload
		if err := json.Unmarshal(p.Value, &pl); err != nil {
			return nil, fmt.Errorf("tikv: decode usage aggregate: %w", err)
		}
		out = append(out, meta.UsageAggregate{
			BucketID:       bucketID,
			StorageClass:   storageClass,
			Day:            dayFromEpoch(epoch),
			ByteSeconds:    pl.ByteSeconds,
			ObjectCountAvg: pl.ObjectCountAvg,
			ObjectCountMax: pl.ObjectCountMax,
			ComputedAt:     pl.ComputedAt,
		})
	}
	return out, nil
}

func (s *Store) ListUserUsage(ctx context.Context, userName string, dayFrom, dayTo time.Time) ([]meta.UsageAggregate, error) {
	buckets, err := s.ListBuckets(ctx, userName)
	if err != nil {
		return nil, err
	}
	type sumKey struct {
		StorageClass string
		Day          int64
	}
	sums := make(map[sumKey]meta.UsageAggregate)
	for _, b := range buckets {
		classes, cerr := s.usageStorageClassesForBucket(ctx, b.ID)
		if cerr != nil {
			return nil, cerr
		}
		for _, cls := range classes {
			rows, lerr := s.ListUsageAggregates(ctx, b.ID, cls, dayFrom, dayTo)
			if lerr != nil {
				return nil, lerr
			}
			for _, r := range rows {
				k := sumKey{StorageClass: r.StorageClass, Day: r.Day.Unix()}
				acc := sums[k]
				acc.StorageClass = r.StorageClass
				acc.Day = r.Day
				acc.ByteSeconds += r.ByteSeconds
				acc.ObjectCountAvg += r.ObjectCountAvg
				if r.ObjectCountMax > acc.ObjectCountMax {
					acc.ObjectCountMax = r.ObjectCountMax
				}
				if r.ComputedAt.After(acc.ComputedAt) {
					acc.ComputedAt = r.ComputedAt
				}
				sums[k] = acc
			}
		}
	}
	out := make([]meta.UsageAggregate, 0, len(sums))
	for _, v := range sums {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Day.Equal(out[j].Day) {
			return out[i].Day.Before(out[j].Day)
		}
		return out[i].StorageClass < out[j].StorageClass
	})
	return out, nil
}

func (s *Store) usageStorageClassesForBucket(ctx context.Context, bucketID uuid.UUID) ([]string, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	prefix := UsageClassIndexPrefix(bucketID)
	pairs, err := txn.Scan(ctx, prefix, prefixEnd(prefix), 0)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if len(p.Key) <= len(prefix) {
			continue
		}
		body := p.Key[len(prefix):]
		cls, _, derr := readEscaped(body)
		if derr != nil {
			return nil, fmt.Errorf("tikv: decode usage class key: %w", derr)
		}
		out = append(out, cls)
	}
	return out, nil
}
