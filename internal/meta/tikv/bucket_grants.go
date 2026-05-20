// Per-bucket ACL grants (US-003 tikv-stubs).
//
// One row per bucket under BucketGrantsKey(bucketID). Payload is a JSON-
// encoded []meta.Grant (empty list collapses to a nil byte slice so the
// shared len(raw)==0→missing guard surfaces ErrNoSuchGrants on the next
// Get — mirrors internal/meta/cassandra encodeGrants + getBucketBlob).
//
// Writes mirror the bucket-blob shape (single optimistic txn, no LWT
// history on this key) — see internal/meta/tikv/blobs.go::setBucketBlob.
// The CLAUDE.md "Plain Put on a key with prior LWT history" gotcha does
// not apply to single-row config docs whose only writers are
// Set/Delete with no prior Read.
package tikv

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// SetBucketGrants overwrites the bucket's ACL grants blob. An empty
// list writes a nil-bytes value so a subsequent GetBucketGrants
// surfaces meta.ErrNoSuchGrants (Cassandra parity — encodeGrants
// collapses an empty list to nil bytes and the SELECT then sees
// len(blob)==0).
func (s *Store) SetBucketGrants(ctx context.Context, bucketID uuid.UUID, grants []meta.Grant) (err error) {
	ctx, finish := s.observer.Start(ctx, "SetBucketGrants", "bucket_acl_grants")
	defer func() { finish(err) }()
	var blob []byte
	if len(grants) > 0 {
		blob, err = json.Marshal(grants)
		if err != nil {
			return err
		}
	}
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Set(BucketGrantsKey(bucketID), blob); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// GetBucketGrants returns the persisted ACL grants. Returns
// meta.ErrNoSuchGrants when no row exists OR the persisted blob is
// zero-length — matches Cassandra getBucketBlob's missing-row guard
// (and memory's nil-map check, both of which surface ErrNoSuchGrants).
func (s *Store) GetBucketGrants(ctx context.Context, bucketID uuid.UUID) (out []meta.Grant, err error) {
	ctx, finish := s.observer.Start(ctx, "GetBucketGrants", "bucket_acl_grants")
	defer func() { finish(err) }()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, BucketGrantsKey(bucketID))
	if err != nil {
		return nil, err
	}
	if !found || len(raw) == 0 {
		return nil, meta.ErrNoSuchGrants
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, meta.ErrNoSuchGrants
	}
	return out, nil
}

// DeleteBucketGrants drops the row. Idempotent — a delete against a
// missing key returns nil (Cassandra DELETE … is also idempotent).
func (s *Store) DeleteBucketGrants(ctx context.Context, bucketID uuid.UUID) (err error) {
	ctx, finish := s.observer.Start(ctx, "DeleteBucketGrants", "bucket_acl_grants")
	defer func() { finish(err) }()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Delete(BucketGrantsKey(bucketID)); err != nil {
		return err
	}
	return txn.Commit(ctx)
}
