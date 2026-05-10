// Per-bucket single-document config blobs (US-007).
//
// The Cassandra path uses a setBucketBlob/getBucketBlob/deleteBucketBlob
// trio against per-config tables. The TiKV path collapses to a single
// shared key prefix (BucketBlobKey) with a fixed 2-byte kind discriminator
// — a tiny "table-by-prefix" indirection that keeps the wrappers thin.
//
// The per-config wrappers (SetBucketLifecycle, SetBucketCORS, ...) live in
// this file too so the parity with internal/meta/cassandra/store.go's
// pattern is one-to-one.
package tikv

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// setBucketBlob persists blob under BucketBlobKey(bucketID, kind). Kind
// MUST be one of the BlobX constants from keys.go — never a free-form
// handler string. Mirrors Cassandra's idempotent INSERT shape: last
// writer wins, no LWT needed because the key is its own row.
func (s *Store) setBucketBlob(ctx context.Context, bucketID uuid.UUID, kind string, blob []byte) (err error) {
	key := BucketBlobKey(bucketID, kind)
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Set(key, append([]byte(nil), blob...)); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// getBucketBlob returns the blob persisted for kind, or missing when no
// row exists (or the persisted blob is empty — mirrors Cassandra's
// len(blob)==0 → ErrNoSuchX guard, which protects against the rare case
// of a tombstoned row whose value column came back zero-length).
func (s *Store) getBucketBlob(ctx context.Context, bucketID uuid.UUID, kind string, missing error) ([]byte, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, BucketBlobKey(bucketID, kind))
	if err != nil {
		return nil, err
	}
	if !found || len(raw) == 0 {
		return nil, missing
	}
	return raw, nil
}

// deleteBucketBlob is idempotent — a delete against a missing key
// returns nil.
func (s *Store) deleteBucketBlob(ctx context.Context, bucketID uuid.UUID, kind string) (err error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Delete(BucketBlobKey(bucketID, kind)); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// ----- Per-config wrappers -----

func (s *Store) SetBucketLifecycle(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, bucketID, BlobLifecycle, blob)
}
func (s *Store) GetBucketLifecycle(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, bucketID, BlobLifecycle, meta.ErrNoSuchLifecycle)
}
func (s *Store) DeleteBucketLifecycle(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, bucketID, BlobLifecycle)
}

func (s *Store) SetBucketCORS(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, bucketID, BlobCORS, blob)
}
func (s *Store) GetBucketCORS(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, bucketID, BlobCORS, meta.ErrNoSuchCORS)
}
func (s *Store) DeleteBucketCORS(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, bucketID, BlobCORS)
}

func (s *Store) SetBucketPolicy(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, bucketID, BlobPolicy, blob)
}
func (s *Store) GetBucketPolicy(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, bucketID, BlobPolicy, meta.ErrNoSuchBucketPolicy)
}
func (s *Store) DeleteBucketPolicy(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, bucketID, BlobPolicy)
}

func (s *Store) SetBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, bucketID, BlobPublicAccessBlock, blob)
}
func (s *Store) GetBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, bucketID, BlobPublicAccessBlock, meta.ErrNoSuchPublicAccessBlock)
}
func (s *Store) DeleteBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, bucketID, BlobPublicAccessBlock)
}

func (s *Store) SetBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, bucketID, BlobOwnershipControls, blob)
}
func (s *Store) GetBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, bucketID, BlobOwnershipControls, meta.ErrNoSuchOwnershipControls)
}
func (s *Store) DeleteBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, bucketID, BlobOwnershipControls)
}

func (s *Store) SetBucketEncryption(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, bucketID, BlobEncryption, blob)
}
func (s *Store) GetBucketEncryption(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, bucketID, BlobEncryption, meta.ErrNoSuchEncryption)
}
func (s *Store) DeleteBucketEncryption(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, bucketID, BlobEncryption)
}

func (s *Store) SetBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, bucketID, BlobObjectLockConfig, blob)
}
func (s *Store) GetBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, bucketID, BlobObjectLockConfig, meta.ErrNoSuchObjectLockConfig)
}
func (s *Store) DeleteBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, bucketID, BlobObjectLockConfig)
}

func (s *Store) SetBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, bucketID, BlobNotification, blob)
}
func (s *Store) GetBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, bucketID, BlobNotification, meta.ErrNoSuchNotification)
}
func (s *Store) DeleteBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, bucketID, BlobNotification)
}

func (s *Store) SetBucketWebsite(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, bucketID, BlobWebsite, blob)
}
func (s *Store) GetBucketWebsite(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, bucketID, BlobWebsite, meta.ErrNoSuchWebsite)
}
func (s *Store) DeleteBucketWebsite(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, bucketID, BlobWebsite)
}

func (s *Store) SetBucketReplication(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, bucketID, BlobReplication, blob)
}
func (s *Store) GetBucketReplication(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, bucketID, BlobReplication, meta.ErrNoSuchReplication)
}
func (s *Store) DeleteBucketReplication(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, bucketID, BlobReplication)
}

func (s *Store) SetBucketLogging(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, bucketID, BlobLogging, blob)
}
func (s *Store) GetBucketLogging(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, bucketID, BlobLogging, meta.ErrNoSuchLogging)
}
func (s *Store) DeleteBucketLogging(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, bucketID, BlobLogging)
}

func (s *Store) SetBucketTagging(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, bucketID, BlobTagging, blob)
}
func (s *Store) GetBucketTagging(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, bucketID, BlobTagging, meta.ErrNoSuchTagSet)
}
func (s *Store) DeleteBucketTagging(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, bucketID, BlobTagging)
}

// ----- Quotas (US-001..US-003) -----

func (s *Store) SetBucketQuota(ctx context.Context, bucketID uuid.UUID, q meta.BucketQuota) error {
	blob, err := meta.EncodeBucketQuota(q)
	if err != nil {
		return err
	}
	return s.setBucketBlob(ctx, bucketID, BlobQuota, blob)
}

func (s *Store) GetBucketQuota(ctx context.Context, bucketID uuid.UUID) (meta.BucketQuota, bool, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return meta.BucketQuota{}, false, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, BucketBlobKey(bucketID, BlobQuota))
	if err != nil {
		return meta.BucketQuota{}, false, err
	}
	if !found || len(raw) == 0 {
		return meta.BucketQuota{}, false, nil
	}
	q, err := meta.DecodeBucketQuota(raw)
	if err != nil {
		return meta.BucketQuota{}, false, err
	}
	return q, true, nil
}

func (s *Store) DeleteBucketQuota(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, bucketID, BlobQuota)
}

func (s *Store) SetUserQuota(ctx context.Context, userName string, q meta.UserQuota) (err error) {
	blob, err := meta.EncodeUserQuota(q)
	if err != nil {
		return err
	}
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Set(UserQuotaKey(userName), append([]byte(nil), blob...)); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

func (s *Store) GetUserQuota(ctx context.Context, userName string) (meta.UserQuota, bool, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return meta.UserQuota{}, false, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, UserQuotaKey(userName))
	if err != nil {
		return meta.UserQuota{}, false, err
	}
	if !found || len(raw) == 0 {
		return meta.UserQuota{}, false, nil
	}
	q, err := meta.DecodeUserQuota(raw)
	if err != nil {
		return meta.UserQuota{}, false, err
	}
	return q, true, nil
}

func (s *Store) DeleteUserQuota(ctx context.Context, userName string) (err error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Delete(UserQuotaKey(userName)); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// ----- Per-(bucket, configID) inventory configs -----
//
// Inventory configs do not fit the single-document blob shape — there can
// be many configs per bucket, each addressed by configID. The key prefix
// (subInventoryConfig) is distinct from the blob prefix so listing within
// the inventory namespace stays bounded.

func (s *Store) SetBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string, blob []byte) (err error) {
	key := InventoryConfigKey(bucketID, configID)
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Set(key, append([]byte(nil), blob...)); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

func (s *Store) GetBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string) ([]byte, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, InventoryConfigKey(bucketID, configID))
	if err != nil {
		return nil, err
	}
	if !found || len(raw) == 0 {
		return nil, meta.ErrNoSuchInventoryConfig
	}
	return raw, nil
}

func (s *Store) DeleteBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string) (err error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Delete(InventoryConfigKey(bucketID, configID)); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

func (s *Store) ListBucketInventoryConfigs(ctx context.Context, bucketID uuid.UUID) (map[string][]byte, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	prefix := InventoryConfigPrefix(bucketID)
	pairs, err := txn.Scan(ctx, prefix, prefixEnd(prefix), 0)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte)
	for _, p := range pairs {
		if len(p.Key) <= len(prefix) {
			continue
		}
		body := p.Key[len(prefix):]
		configID, _, err := readEscaped(body)
		if err != nil {
			return nil, fmt.Errorf("tikv: decode inventory key: %w", err)
		}
		if len(p.Value) == 0 {
			continue
		}
		out[configID] = append([]byte(nil), p.Value...)
	}
	return out, nil
}
