// Per-owner denormalised aggregate row (ralph/storage-correctness US-001).
//
// Maintained in lockstep with bucket_stats so UserQuota.TotalMaxBytes /
// MaxBuckets enforcement resolves to a single point lookup. Mirrors the
// pessimistic-txn shape of bucket_stats.go — LockKeys + Get + Set + Commit.
package tikv

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

func encodeUserStats(s meta.UserStats) ([]byte, error) {
	return json.Marshal(s)
}

func decodeUserStats(blob []byte) (meta.UserStats, error) {
	var out meta.UserStats
	if len(blob) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(blob, &out); err != nil {
		return meta.UserStats{}, fmt.Errorf("tikv: decode user stats: %w", err)
	}
	return out, nil
}

// GetUserStats reads the per-owner aggregate, returning zero values when no
// row exists yet.
func (s *Store) GetUserStats(ctx context.Context, owner string) (out meta.UserStats, err error) {
	ctx, finish := s.observer.Start(ctx, "GetUserStats", "user_stats")
	defer func() { finish(err) }()
	if owner == "" {
		return meta.UserStats{}, nil
	}
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return meta.UserStats{}, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, UserStatsKey(owner))
	if err != nil {
		return meta.UserStats{}, err
	}
	if !found || len(raw) == 0 {
		return meta.UserStats{Owner: owner}, nil
	}
	cur, err := decodeUserStats(raw)
	if err != nil {
		return meta.UserStats{}, err
	}
	cur.Owner = owner
	return cur, nil
}

// BumpUserStats applies (deltaBytes, deltaObjects) atomically. Pessimistic
// txn serialises concurrent bumps from different buckets owned by the same
// user.
func (s *Store) BumpUserStats(ctx context.Context, owner string, deltaBytes, deltaObjects int64) (out meta.UserStats, err error) {
	ctx, finish := s.observer.Start(ctx, "BumpUserStats", "user_stats")
	defer func() { finish(err) }()
	if owner == "" {
		return meta.UserStats{}, nil
	}
	key := UserStatsKey(owner)
	txn, err := s.beginPessimistic(ctx)
	if err != nil {
		return meta.UserStats{}, err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return meta.UserStats{}, err
	}
	if err = bumpUserStatsInTxn(ctx, txn, key, owner, deltaBytes, deltaObjects, 0); err != nil {
		return meta.UserStats{}, err
	}
	if err = txn.Commit(ctx); err != nil {
		return meta.UserStats{}, err
	}
	return s.GetUserStats(ctx, owner)
}

// IncrUserBucketCount applies delta to user_stats.bucket_count. Called from
// CreateBucket / DeleteBucket so the operator-facing MaxBuckets check
// resolves to an O(1) point read on user_stats.
func (s *Store) IncrUserBucketCount(ctx context.Context, owner string, delta int) (out meta.UserStats, err error) {
	ctx, finish := s.observer.Start(ctx, "IncrUserBucketCount", "user_stats")
	defer func() { finish(err) }()
	if owner == "" {
		return meta.UserStats{}, nil
	}
	key := UserStatsKey(owner)
	txn, err := s.beginPessimistic(ctx)
	if err != nil {
		return meta.UserStats{}, err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return meta.UserStats{}, err
	}
	if err = bumpUserStatsInTxn(ctx, txn, key, owner, 0, 0, delta); err != nil {
		return meta.UserStats{}, err
	}
	if err = txn.Commit(ctx); err != nil {
		return meta.UserStats{}, err
	}
	return s.GetUserStats(ctx, owner)
}

// bumpUserStatsInTxn applies the delta inside an existing pessimistic txn.
// Used by both standalone BumpUserStats / IncrUserBucketCount AND by
// CreateBucket / DeleteBucket / BumpBucketStats which need to fold the
// user_stats mutation into the same atomic txn that mutates bucket_stats /
// the bucket row. Caller MUST have already locked key.
func bumpUserStatsInTxn(ctx context.Context, txn kvTxn, key []byte, owner string, deltaBytes, deltaObjects int64, deltaBuckets int) error {
	if owner == "" {
		return nil
	}
	raw, _, err := txn.Get(ctx, key)
	if err != nil {
		return err
	}
	cur, err := decodeUserStats(raw)
	if err != nil {
		return err
	}
	cur.Owner = owner
	cur.UsedBytes += deltaBytes
	cur.UsedObjects += deltaObjects
	cur.BucketCount += deltaBuckets
	cur.UpdatedAt = time.Now().UTC()
	payload, err := encodeUserStats(cur)
	if err != nil {
		return err
	}
	return txn.Set(key, payload)
}
