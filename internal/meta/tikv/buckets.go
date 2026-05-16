package tikv

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// defaultShardCount mirrors the Cassandra-side default. TiKV does not need
// physical sharding for ListObjects (US-005 uses a single ordered range
// scan), but the field is part of meta.Bucket and downstream code reads it
// — so we still record one.
const defaultShardCount = 64

// CreateBucket inserts a new bucket row addressed by name. Concurrent
// creates with the same name conflict at lock-acquire (pessimistic txn) so
// only one returns success; the other gets ErrBucketAlreadyExists. Mirrors
// the Cassandra LWT shape.
func (s *Store) CreateBucket(ctx context.Context, name, owner, defaultClass string) (out *meta.Bucket, err error) {
	ctx, finish := s.observer.Start(ctx, "CreateBucket", "buckets")
	defer func() { finish(err) }()
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	b := &meta.Bucket{
		Name:         name,
		ID:           id,
		Owner:        owner,
		CreatedAt:    time.Now().UTC(),
		DefaultClass: defaultClass,
		Versioning:   meta.VersioningDisabled,
		ShardCount:   defaultShardCount,
	}
	key := BucketKey(name)
	payload, err := encodeBucket(b)
	if err != nil {
		return nil, err
	}
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return nil, err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return nil, err
	}
	_, found, err := txn.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if found {
		return nil, meta.ErrBucketAlreadyExists
	}
	if err = txn.Set(key, payload); err != nil {
		return nil, err
	}
	if err = txn.Commit(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// GetBucket is a single Get against the bucket-by-name row.
func (s *Store) GetBucket(ctx context.Context, name string) (out *meta.Bucket, err error) {
	ctx, finish := s.observer.Start(ctx, "GetBucket", "buckets")
	defer func() { finish(err) }()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, BucketKey(name))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrBucketNotFound
	}
	return decodeBucket(raw)
}

// DeleteBucket asserts the bucket has no remaining bucket-scoped rows
// (objects, configs, multipart uploads, ...) and only then drops the
// bucket-by-name row. The pessimistic txn locks the bucket-by-name key so
// concurrent CreateBucket-after-DeleteBucket is serialised.
func (s *Store) DeleteBucket(ctx context.Context, name string) (err error) {
	ctx, finish := s.observer.Start(ctx, "DeleteBucket", "buckets")
	defer func() { finish(err) }()
	bucketKey := BucketKey(name)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, bucketKey); err != nil {
		return err
	}
	raw, found, err := txn.Get(ctx, bucketKey)
	if err != nil {
		return err
	}
	if !found {
		return meta.ErrBucketNotFound
	}
	b, err := decodeBucket(raw)
	if err != nil {
		return err
	}
	scopedPrefix := PrefixForBucket(b.ID)
	pairs, err := txn.Scan(ctx, scopedPrefix, prefixEnd(scopedPrefix), 1)
	if err != nil {
		return err
	}
	if len(pairs) > 0 {
		return meta.ErrBucketNotEmpty
	}
	if err = txn.Delete(bucketKey); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// ListBuckets is a range scan over the bucket-by-name prefix. The
// optional owner filter is applied in-process — bucket cardinality is low
// (gateway-wide single-digit thousands) so an in-process filter is fine.
func (s *Store) ListBuckets(ctx context.Context, owner string) (out []*meta.Bucket, err error) {
	ctx, finish := s.observer.Start(ctx, "ListBuckets", "buckets")
	defer func() { finish(err) }()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	start := []byte(prefixBucketByName)
	pairs, err := txn.Scan(ctx, start, prefixEnd(start), 0)
	if err != nil {
		return nil, err
	}
	out = make([]*meta.Bucket, 0, len(pairs))
	for _, p := range pairs {
		b, derr := decodeBucket(p.Value)
		if derr != nil {
			return nil, fmt.Errorf("tikv: decode bucket %q: %w", string(p.Key), derr)
		}
		if owner != "" && b.Owner != owner {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// SetBucketVersioning flips the versioning state under a pessimistic txn
// (read-for-update + write). Plain Set here would race with a concurrent
// reader on a different field — every bucket-row mutator therefore goes
// through updateBucket below.
func (s *Store) SetBucketVersioning(ctx context.Context, name, state string) error {
	return s.updateBucket(ctx, name, func(b *meta.Bucket) error {
		b.Versioning = state
		return nil
	})
}

func (s *Store) SetBucketACL(ctx context.Context, name, canned string) error {
	return s.updateBucket(ctx, name, func(b *meta.Bucket) error {
		b.ACL = canned
		return nil
	})
}

func (s *Store) SetBucketObjectLockEnabled(ctx context.Context, name string, enabled bool) error {
	return s.updateBucket(ctx, name, func(b *meta.Bucket) error {
		b.ObjectLockEnabled = enabled
		return nil
	})
}

func (s *Store) SetBucketRegion(ctx context.Context, name, region string) error {
	return s.updateBucket(ctx, name, func(b *meta.Bucket) error {
		b.Region = region
		return nil
	})
}

func (s *Store) SetBucketMfaDelete(ctx context.Context, name, state string) error {
	return s.updateBucket(ctx, name, func(b *meta.Bucket) error {
		b.MfaDelete = state
		return nil
	})
}

// SetBucketPlacementMode persists the per-bucket placement mode on the
// JSON-encoded bucket row (US-001 effective-placement). Empty string
// clears the override (decodes as "" → coerced to "weighted"
// downstream). Validates via meta.ValidatePlacementMode.
func (s *Store) SetBucketPlacementMode(ctx context.Context, name, mode string) error {
	if err := meta.ValidatePlacementMode(mode); err != nil {
		return err
	}
	return s.updateBucket(ctx, name, func(b *meta.Bucket) error {
		b.PlacementMode = mode
		return nil
	})
}

// updateBucket is the pessimistic read-modify-write helper every bucket-row
// mutator routes through. The lesson is the TiKV mirror of CLAUDE.md's
// Cassandra LWT note: a plain Put after a previous LWT-equivalent INSERT
// would risk read-after-write incoherence; pessimistic locking on the row
// key participates in the same conflict-detection lineage as
// CreateBucket/DeleteBucket.
func (s *Store) updateBucket(ctx context.Context, name string, mutate func(*meta.Bucket) error) (err error) {
	ctx, finish := s.observer.Start(ctx, "UpdateBucket", "buckets")
	defer func() { finish(err) }()
	key := BucketKey(name)
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
		return meta.ErrBucketNotFound
	}
	b, err := decodeBucket(raw)
	if err != nil {
		return err
	}
	if err = mutate(b); err != nil {
		return err
	}
	payload, err := encodeBucket(b)
	if err != nil {
		return err
	}
	if err = txn.Set(key, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// rollbackOnError rolls back txn if *err is non-nil at function exit. Used
// via defer in pessimistic write paths so the txn is always closed even
// when a step in the middle errors out. Commit-success paths set *err to
// nil before this fires (or never reach it because Commit is the last
// step).
func rollbackOnError(txn kvTxn, err *error) {
	if *err != nil {
		_ = txn.Rollback()
	}
}

// prefixEnd returns the exclusive upper bound for a "lex starts with prefix"
// range scan. Mirrors github.com/tikv/client-go/v2/kv.PrefixNextKey but
// implemented locally so memBackend tests don't pull the txnkv tree.
func prefixEnd(prefix []byte) []byte {
	out := append([]byte(nil), prefix...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xFF {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}