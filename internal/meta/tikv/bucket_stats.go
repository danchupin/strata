// Per-bucket live counter row (US-004..US-005), sharded fan-out (US-002
// p1-fixes).
//
// The Cassandra path uses a CAS loop on a regular bigint row; the TiKV path
// uses a pessimistic txn (Begin pessimistic + LockKeys + Get + Set + Commit)
// so concurrent bumps serialise without lost updates per the CLAUDE.md
// gotcha "Plain Put on a key with prior LWT history breaks read-after-write
// coherence".
//
// US-002 p1-fixes: a single per-bucket counter row saturated the pessimistic
// LockKeys path under the lab c=128 hot-bucket workload — every concurrent
// BumpBucketStats serialised on the same lock. Fan-out to
// bucketStatsShardCount sibling keys (s/B/<bid>/bs/<0..7>) distributes the
// contention; per-op shard pick via `fnv32a(uuid.NewString()) % shards`
// keeps the distribution uniform across PUT/DELETE without biasing a hot
// key onto one shard. Read path sums all 8 shards inside a single non-
// pessimistic snapshot txn.
package tikv

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// encodeBucketStats serialises a per-shard row to a JSON blob. JSON keeps the
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

// pickBucketStatsShard returns a uniformly distributed shard id in
// [0, bucketStatsShardCount) for the current bump. The PRD-scoped choice is
// `fnv32a(uuid.NewString()) % bucketStatsShardCount`: a fresh UUID per call
// is what makes the distribution unbiased — hashing the bucket id or any
// caller-supplied key would funnel every bump on the same hot key onto a
// single shard and defeat the fan-out.
func pickBucketStatsShard() uint8 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(uuid.NewString()))
	return uint8(h.Sum32() % uint32(bucketStatsShardCount))
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

// GetBucketStats sums the per-shard live counters inside a single
// non-pessimistic snapshot txn (no lock contention with concurrent bumps).
// Empty / absent shards contribute zero.
func (s *Store) GetBucketStats(ctx context.Context, bucketID uuid.UUID) (out meta.BucketStats, err error) {
	ctx, finish := s.observer.Start(ctx, "GetBucketStats", "bucket_stats")
	defer func() { finish(err) }()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return meta.BucketStats{}, err
	}
	defer txn.Rollback()
	var total meta.BucketStats
	for sh := range uint8(bucketStatsShardCount) {
		raw, _, gerr := txn.Get(ctx, BucketStatsShardKey(bucketID, sh))
		if gerr != nil {
			return meta.BucketStats{}, gerr
		}
		cur, derr := decodeBucketStats(raw)
		if derr != nil {
			return meta.BucketStats{}, derr
		}
		total.UsedBytes += cur.UsedBytes
		total.UsedObjects += cur.UsedObjects
		if cur.UpdatedAt.After(total.UpdatedAt) {
			total.UpdatedAt = cur.UpdatedAt
		}
	}
	return total, nil
}

// BumpBucketStats applies (deltaBytes, deltaObjects) atomically to one of
// bucketStatsShardCount per-bucket shard keys inside a pessimistic txn.
// LockKeys + Get + Set + Commit on the picked shard serialises only against
// concurrent bumps that hash to the same shard — fan-out drops the lock-
// contention floor by ~bucketStatsShardCount× at peak. Folds a user_stats
// bump for the bucket's owner into the same txn so both rows commit
// atomically (ralph/storage-correctness US-001). Owner is resolved via the
// bucket_owners pointer seeded at CreateBucket; absence (bucket created
// pre-feature) leaves user_stats untouched and the quotareconcile worker
// repairs the drift. Returns the aggregate cross-shard total (the picked
// shard's post-write value plus the latest committed values of the other
// shards read inside the same txn) so the contract test's per-bump
// cumulative-total assertion holds with fan-out.
func (s *Store) BumpBucketStats(ctx context.Context, bucketID uuid.UUID, deltaBytes, deltaObjects int64) (out meta.BucketStats, err error) {
	ctx, finish := s.observer.Start(ctx, "BumpBucketStats", "bucket_stats")
	defer func() { finish(err) }()
	shard := pickBucketStatsShard()
	shardKey := BucketStatsShardKey(bucketID, shard)
	ownerKey := BucketOwnerKey(bucketID)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return meta.BucketStats{}, err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, shardKey, ownerKey); err != nil {
		return meta.BucketStats{}, err
	}
	raw, _, err := txn.Get(ctx, shardKey)
	if err != nil {
		return meta.BucketStats{}, err
	}
	cur, err := decodeBucketStats(raw)
	if err != nil {
		return meta.BucketStats{}, err
	}
	nextShard := meta.BucketStats{
		UsedBytes:   cur.UsedBytes + deltaBytes,
		UsedObjects: cur.UsedObjects + deltaObjects,
		UpdatedAt:   time.Now().UTC(),
	}
	payload, err := encodeBucketStats(nextShard)
	if err != nil {
		return meta.BucketStats{}, err
	}
	if err = txn.Set(shardKey, payload); err != nil {
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
	total := nextShard
	for sh := range uint8(bucketStatsShardCount) {
		if sh == shard {
			continue
		}
		rawSib, _, gerr := txn.Get(ctx, BucketStatsShardKey(bucketID, sh))
		if gerr != nil {
			err = gerr
			return meta.BucketStats{}, err
		}
		sib, derr := decodeBucketStats(rawSib)
		if derr != nil {
			err = derr
			return meta.BucketStats{}, err
		}
		total.UsedBytes += sib.UsedBytes
		total.UsedObjects += sib.UsedObjects
		if sib.UpdatedAt.After(total.UpdatedAt) {
			total.UpdatedAt = sib.UpdatedAt
		}
	}
	if err = txn.Commit(ctx); err != nil {
		return meta.BucketStats{}, err
	}
	if s.metrics != nil {
		s.metrics.IncBucketStatsShardWrite(int(shard))
	}
	return total, nil
}
