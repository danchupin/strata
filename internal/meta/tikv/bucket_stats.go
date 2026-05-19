// Per-bucket live counter row (US-004..US-005).
//
// The Cassandra path uses a CAS loop on a regular bigint row; the TiKV path
// uses a pessimistic txn (Begin pessimistic + LockKeys + Get + Set + Commit)
// so concurrent bumps serialise without lost updates per the CLAUDE.md
// gotcha "Plain Put on a key with prior LWT history breaks read-after-write
// coherence".
package tikv

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// encodeBucketStats serialises the row to a JSON blob. JSON keeps the
// additive-fields property the quota blobs use; the row is small so
// per-bump (de)serialisation is negligible.
func encodeBucketStats(s meta.BucketStats) ([]byte, error) {
	return json.Marshal(s)
}

func decodeBucketStats(blob []byte) (meta.BucketStats, error) {
	var out meta.BucketStats
	if len(blob) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(blob, &out); err != nil {
		return meta.BucketStats{}, fmt.Errorf("tikv: decode bucket stats: %w", err)
	}
	return out, nil
}

// bucketStatsDelta returns the (deltaBytes, deltaObjects) bump that should be
// applied to bucket_stats when `next` replaces `prior` (nil = absent). Delete
// markers contribute 0 bytes and 0 to object count — only non-marker rows
// count toward the live tally.
func bucketStatsDelta(prior, next *meta.Object) (int64, int64) {
	var priorBytes, priorObjects int64
	if prior != nil {
		priorBytes = prior.Size
		if !prior.IsDeleteMarker {
			priorObjects = 1
		}
	}
	var nextBytes, nextObjects int64
	if next != nil {
		nextBytes = next.Size
		if !next.IsDeleteMarker {
			nextObjects = 1
		}
	}
	return nextBytes - priorBytes, nextObjects - priorObjects
}

// GetBucketStats reads the live counter row, returning zero stats when no
// row exists yet.
func (s *Store) GetBucketStats(ctx context.Context, bucketID uuid.UUID) (out meta.BucketStats, err error) {
	ctx, finish := s.observer.Start(ctx, "GetBucketStats", "bucket_stats")
	defer func() { finish(err) }()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return meta.BucketStats{}, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, BucketStatsKey(bucketID))
	if err != nil {
		return meta.BucketStats{}, err
	}
	if !found || len(raw) == 0 {
		return meta.BucketStats{}, nil
	}
	return decodeBucketStats(raw)
}

// BumpBucketStats applies (deltaBytes, deltaObjects) atomically inside a
// pessimistic txn. LockKeys + Get + Set + Commit serialises concurrent
// bumps. Folds a user_stats bump for the bucket's owner into the same txn
// so both rows commit atomically (ralph/storage-correctness US-001). Owner
// is resolved via the bucket_owners pointer seeded at CreateBucket; absence
// (bucket created pre-feature) leaves user_stats untouched and the
// quotareconcile worker repairs the drift.
func (s *Store) BumpBucketStats(ctx context.Context, bucketID uuid.UUID, deltaBytes, deltaObjects int64) (out meta.BucketStats, err error) {
	ctx, finish := s.observer.Start(ctx, "BumpBucketStats", "bucket_stats")
	defer func() { finish(err) }()
	key := BucketStatsKey(bucketID)
	ownerKey := BucketOwnerKey(bucketID)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return meta.BucketStats{}, err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key, ownerKey); err != nil {
		return meta.BucketStats{}, err
	}
	raw, _, err := txn.Get(ctx, key)
	if err != nil {
		return meta.BucketStats{}, err
	}
	cur, err := decodeBucketStats(raw)
	if err != nil {
		return meta.BucketStats{}, err
	}
	next := meta.BucketStats{
		UsedBytes:   cur.UsedBytes + deltaBytes,
		UsedObjects: cur.UsedObjects + deltaObjects,
		UpdatedAt:   time.Now().UTC(),
	}
	payload, err := encodeBucketStats(next)
	if err != nil {
		return meta.BucketStats{}, err
	}
	if err = txn.Set(key, payload); err != nil {
		return meta.BucketStats{}, err
	}
	ownerRaw, _, err := txn.Get(ctx, ownerKey)
	if err != nil {
		return meta.BucketStats{}, err
	}
	if owner := string(ownerRaw); owner != "" && (deltaBytes != 0 || deltaObjects != 0) {
		userKey := UserStatsKey(owner)
		if err = txn.LockKeys(ctx, userKey); err != nil {
			return meta.BucketStats{}, err
		}
		if err = bumpUserStatsInTxn(ctx, txn, userKey, owner, deltaBytes, deltaObjects, 0); err != nil {
			return meta.BucketStats{}, err
		}
	}
	if err = txn.Commit(ctx); err != nil {
		return meta.BucketStats{}, err
	}
	return next, nil
}
