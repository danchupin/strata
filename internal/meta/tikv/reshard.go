// Reshard state on TiKV (US-011).
//
// Mirrors the Cassandra reshard.go shape:
//
//   - StartReshard validates target, asserts no job in flight,
//     persists the job row, and stamps bucket.TargetShardCount — all
//     under a single pessimistic txn so a concurrent retry surfaces
//     ErrReshardInProgress instead of racing two workers.
//
//   - GetReshardJob is a single Get over the global ReshardJobKey.
//
//   - UpdateReshardJob is a pessimistic read-modify-write keyed on
//     ReshardJobKey — used by the resize worker after every batch to
//     persist the LastKey watermark for resumability.
//
//   - CompleteReshard atomically flips bucket.ShardCount to the job's
//     target, clears bucket.TargetShardCount, and deletes the job row.
//
//   - ListReshardJobs is a single ordered range scan over the global
//     reshard prefix (cardinality is low — usually <10 in flight).
package tikv

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// StartReshard queues an online shard-resize for bucketID with target
// partition count. The pessimistic txn locks the bucket-by-name key
// and the reshard-job key together so a concurrent StartReshard for
// the same bucket waits at lock-acquire and observes the persisted job
// on the second Get (returning ErrReshardInProgress).
func (s *Store) StartReshard(ctx context.Context, bucketID uuid.UUID, target int) (_ *meta.ReshardJob, err error) {
	if !meta.IsValidShardCount(target) {
		return nil, meta.ErrReshardInvalidTarget
	}
	jobKey := ReshardJobKey(bucketID)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return nil, err
	}
	defer rollbackOnError(txn, &err)

	bucketKey, err := s.findBucketKeyByID(ctx, txn, bucketID)
	if err != nil {
		return nil, err
	}
	if err = txn.LockKeys(ctx, bucketKey, jobKey); err != nil {
		return nil, err
	}
	bucketRaw, found, err := txn.Get(ctx, bucketKey)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrBucketNotFound
	}
	bucket, err := decodeBucket(bucketRaw)
	if err != nil {
		return nil, err
	}
	if target <= bucket.ShardCount {
		return nil, meta.ErrReshardInvalidTarget
	}
	if _, found, gerr := txn.Get(ctx, jobKey); gerr != nil {
		err = gerr
		return nil, err
	} else if found {
		return nil, meta.ErrReshardInProgress
	}
	now := time.Now().UTC()
	job := &meta.ReshardJob{
		BucketID:  bucketID,
		Bucket:    bucket.Name,
		Source:    bucket.ShardCount,
		Target:    target,
		CreatedAt: now,
		UpdatedAt: now,
	}
	jobPayload, err := encodeReshardJob(job)
	if err != nil {
		return nil, err
	}
	if err = txn.Set(jobKey, jobPayload); err != nil {
		return nil, err
	}
	bucket.TargetShardCount = target
	bucketPayload, err := encodeBucket(bucket)
	if err != nil {
		return nil, err
	}
	if err = txn.Set(bucketKey, bucketPayload); err != nil {
		return nil, err
	}
	if err = txn.Commit(ctx); err != nil {
		return nil, err
	}
	out := *job
	return &out, nil
}

// GetReshardJob is an optimistic Get on the job key. ErrReshardNotFound
// when no row exists.
func (s *Store) GetReshardJob(ctx context.Context, bucketID uuid.UUID) (*meta.ReshardJob, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, ReshardJobKey(bucketID))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrReshardNotFound
	}
	return decodeReshardJob(bucketID, raw)
}

// UpdateReshardJob persists a watermark/state update. Pessimistic txn
// + LockKeys so concurrent resize-worker batches serialise on the
// per-bucket job row; mirrors Cassandra's `UPDATE ... IF EXISTS` shape.
// The caller's BucketID is the keying field; LastKey/Done are the
// fields that move forward.
func (s *Store) UpdateReshardJob(ctx context.Context, job *meta.ReshardJob) (err error) {
	if job == nil {
		return nil
	}
	key := ReshardJobKey(job.BucketID)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return err
	}
	raw, found, err := txn.Get(ctx, key)
	if err != nil {
		return err
	}
	if !found {
		return meta.ErrReshardNotFound
	}
	current, err := decodeReshardJob(job.BucketID, raw)
	if err != nil {
		return err
	}
	current.LastKey = job.LastKey
	current.Done = job.Done
	current.UpdatedAt = time.Now().UTC()
	payload, err := encodeReshardJob(current)
	if err != nil {
		return err
	}
	if err = txn.Set(key, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// CompleteReshard atomically flips bucket.ShardCount to the job's
// target, clears bucket.TargetShardCount, and deletes the job row.
// Returns ErrReshardNotFound when no job is in flight for bucketID.
func (s *Store) CompleteReshard(ctx context.Context, bucketID uuid.UUID) (err error) {
	jobKey := ReshardJobKey(bucketID)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	bucketKey, err := s.findBucketKeyByID(ctx, txn, bucketID)
	if err != nil {
		return err
	}
	if err = txn.LockKeys(ctx, bucketKey, jobKey); err != nil {
		return err
	}
	jobRaw, found, err := txn.Get(ctx, jobKey)
	if err != nil {
		return err
	}
	if !found {
		return meta.ErrReshardNotFound
	}
	job, err := decodeReshardJob(bucketID, jobRaw)
	if err != nil {
		return err
	}
	bucketRaw, found, err := txn.Get(ctx, bucketKey)
	if err != nil {
		return err
	}
	if !found {
		return meta.ErrBucketNotFound
	}
	bucket, err := decodeBucket(bucketRaw)
	if err != nil {
		return err
	}
	bucket.ShardCount = job.Target
	bucket.TargetShardCount = 0
	bucketPayload, err := encodeBucket(bucket)
	if err != nil {
		return err
	}
	if err = txn.Set(bucketKey, bucketPayload); err != nil {
		return err
	}
	if err = txn.Delete(jobKey); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// ListReshardJobs is a single ordered range scan over the global
// reshard prefix. Sorted by BucketID byte order; callers that want
// alphabetical-by-bucket-name sort in-process (Cassandra does the
// same).
func (s *Store) ListReshardJobs(ctx context.Context) ([]*meta.ReshardJob, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	prefix := ReshardJobsPrefix()
	pairs, err := txn.Scan(ctx, prefix, prefixEnd(prefix), 0)
	if err != nil {
		return nil, err
	}
	out := make([]*meta.ReshardJob, 0, len(pairs))
	for _, p := range pairs {
		if len(p.Key) != len(prefix)+16 {
			return nil, fmt.Errorf("tikv: reshard key wrong length %d", len(p.Key))
		}
		var id uuid.UUID
		copy(id[:], p.Key[len(prefix):])
		job, err := decodeReshardJob(id, p.Value)
		if err != nil {
			return nil, fmt.Errorf("tikv: decode reshard job %s: %w", id, err)
		}
		out = append(out, job)
	}
	return out, nil
}

// findBucketKeyByID scans the bucket-by-name prefix inside the supplied
// txn and returns the row key of the bucket whose ID matches. Callers
// LockKey the returned key + re-Get the row to obtain the authoritative
// pre-image (the Scan is read-only and may race with concurrent
// mutators). Cardinality is low (gateway-wide single-digit thousands at
// most).
func (s *Store) findBucketKeyByID(ctx context.Context, txn kvTxn, bucketID uuid.UUID) ([]byte, error) {
	start := []byte(prefixBucketByName)
	pairs, err := txn.Scan(ctx, start, prefixEnd(start), 0)
	if err != nil {
		return nil, err
	}
	for _, p := range pairs {
		b, err := decodeBucket(p.Value)
		if err != nil {
			return nil, fmt.Errorf("tikv: decode bucket while resolving id: %w", err)
		}
		if b.ID == bucketID {
			return p.Key, nil
		}
	}
	return nil, meta.ErrBucketNotFound
}
