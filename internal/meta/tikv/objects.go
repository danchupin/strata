package tikv

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// PutObject persists o under ObjectKey(bucketID, key, versionID). For
// non-versioned buckets every prior version row under the user-space key is
// removed atomically inside the same pessimistic txn (mirrors the
// `DELETE WHERE bucket=? AND key=?` purge Cassandra issues before INSERT).
// For Suspended-mode null PUT the prior null-versioned row is replaced —
// other TimeUUID versions are left untouched.
func (s *Store) PutObject(ctx context.Context, o *meta.Object, versioned bool) (err error) {
	switch {
	case !versioned:
		o.VersionID = meta.NullVersionID
		o.IsNull = true
	case o.IsNull:
		o.VersionID = meta.NullVersionID
	case o.VersionID == "":
		o.VersionID = gocql.TimeUUID().String()
	}
	o.IsLatest = true

	objKey, err := ObjectKey(o.BucketID, o.Key, o.VersionID)
	if err != nil {
		return err
	}
	payload, err := encodeObject(o)
	if err != nil {
		return err
	}

	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)

	if err = txn.LockKeys(ctx, objKey); err != nil {
		return err
	}

	// Capture the prior latest row (if any) so we can compute the
	// bucket_stats delta after the txn commits. Markers contribute 0 to
	// either bytes or count via bucketStatsDelta.
	var prior *meta.Object
	if !versioned {
		prefix := append(ObjectPrefixWithKey(o.BucketID, o.Key), 0x00, 0x00)
		end := prefixEnd(prefix)
		pairs, perr := txn.Scan(ctx, prefix, end, 0)
		if perr != nil {
			err = perr
			return err
		}
		if len(pairs) > 0 {
			p, derr := decodeObject(pairs[0].Value)
			if derr != nil {
				err = derr
				return err
			}
			prior = p
		}
		for _, p := range pairs {
			if err = txn.Delete(p.Key); err != nil {
				return err
			}
		}
	} else if o.IsNull {
		nullKey, kerr := ObjectKey(o.BucketID, o.Key, meta.NullVersionID)
		if kerr != nil {
			err = kerr
			return err
		}
		raw, found, gerr := txn.Get(ctx, nullKey)
		if gerr != nil {
			err = gerr
			return err
		}
		if found {
			p, derr := decodeObject(raw)
			if derr != nil {
				err = derr
				return err
			}
			prior = p
		}
		if err = txn.Delete(nullKey); err != nil {
			return err
		}
	}
	if err = txn.Set(objKey, payload); err != nil {
		return err
	}
	if err = txn.Commit(ctx); err != nil {
		return err
	}
	deltaBytes, deltaObjects := bucketStatsDelta(prior, o)
	if deltaBytes != 0 || deltaObjects != 0 {
		if _, berr := s.BumpBucketStats(ctx, o.BucketID, deltaBytes, deltaObjects); berr != nil {
			return berr
		}
	}
	return nil
}

// GetObject returns the object addressed by (bucketID, key, versionID). An
// empty versionID resolves to the latest non-delete-marker version via a
// range scan with limit 1 (the version-DESC suffix encoding makes lex-first
// = latest). The wire literal "null" maps to NullVersionID.
func (s *Store) GetObject(ctx context.Context, bucketID uuid.UUID, key, versionID string) (*meta.Object, error) {
	versionID = meta.ResolveVersionID(versionID)
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()

	if versionID != "" {
		objKey, err := ObjectKey(bucketID, key, versionID)
		if err != nil {
			return nil, meta.ErrObjectNotFound
		}
		raw, found, err := txn.Get(ctx, objKey)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, meta.ErrObjectNotFound
		}
		return decodeObject(raw)
	}

	// "Latest" with versioning history requires comparing the highest-clustering
	// row against the null sentinel by mtime: a Suspended PUT writes a null-
	// sentinel row whose inverted-ts suffix sorts LAST, so the limit-1 scan
	// alone misses it when older real-timeuuid rows exist. (US-005 suspended-copy.)
	start := append(ObjectPrefixWithKey(bucketID, key), 0x00, 0x00)
	end := prefixEnd(start)
	pairs, err := txn.Scan(ctx, start, end, 1)
	if err != nil {
		return nil, err
	}
	var latestByVersion *meta.Object
	if len(pairs) > 0 {
		o, derr := decodeObject(pairs[0].Value)
		if derr != nil {
			return nil, derr
		}
		latestByVersion = o
	}
	var nullRow *meta.Object
	if latestByVersion == nil || !latestByVersion.IsNull {
		nullKey, kerr := ObjectKey(bucketID, key, meta.NullVersionID)
		if kerr == nil {
			raw, found, gerr := txn.Get(ctx, nullKey)
			if gerr != nil {
				return nil, gerr
			}
			if found {
				nr, derr := decodeObject(raw)
				if derr != nil {
					return nil, derr
				}
				nullRow = nr
			}
		}
	}
	var winner *meta.Object
	switch {
	case latestByVersion != nil && nullRow != nil:
		if nullRow.Mtime.After(latestByVersion.Mtime) {
			winner = nullRow
		} else {
			winner = latestByVersion
		}
	case latestByVersion != nil:
		winner = latestByVersion
	case nullRow != nil:
		winner = nullRow
	default:
		return nil, meta.ErrObjectNotFound
	}
	if winner.IsDeleteMarker {
		return nil, meta.ErrObjectNotFound
	}
	return winner, nil
}

// DeleteObject mirrors the memory/Cassandra path:
//   - versionID set: delete that specific version row, return its prior body.
//   - versionID empty + versioned: write a fresh delete-marker row.
//   - versionID empty + !versioned: delete every version row for the key,
//     return the prior latest.
func (s *Store) DeleteObject(ctx context.Context, bucketID uuid.UUID, key, versionID string, versioned bool) (_ *meta.Object, err error) {
	versionID = meta.ResolveVersionID(versionID)

	if versionID != "" {
		objKey, kerr := ObjectKey(bucketID, key, versionID)
		if kerr != nil {
			return nil, meta.ErrObjectNotFound
		}
		txn, err := s.kv.Begin(ctx, true)
		if err != nil {
			return nil, err
		}
		defer rollbackOnError(txn, &err)
		if err = txn.LockKeys(ctx, objKey); err != nil {
			return nil, err
		}
		raw, found, err := txn.Get(ctx, objKey)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, meta.ErrObjectNotFound
		}
		o, err := decodeObject(raw)
		if err != nil {
			return nil, err
		}
		if err = txn.Delete(objKey); err != nil {
			return nil, err
		}
		if err = txn.Commit(ctx); err != nil {
			return nil, err
		}
		db, dc := bucketStatsDelta(o, nil)
		if db != 0 || dc != 0 {
			if _, berr := s.BumpBucketStats(ctx, bucketID, db, dc); berr != nil {
				return nil, berr
			}
		}
		return o, nil
	}

	if versioned {
		marker := &meta.Object{
			BucketID:       bucketID,
			Key:            key,
			VersionID:      gocql.TimeUUID().String(),
			IsLatest:       true,
			IsDeleteMarker: true,
			Mtime:          time.Now().UTC(),
		}
		if err := s.PutObject(ctx, marker, true); err != nil {
			return nil, err
		}
		return marker, nil
	}

	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return nil, err
	}
	defer rollbackOnError(txn, &err)
	prefix := append(ObjectPrefixWithKey(bucketID, key), 0x00, 0x00)
	end := prefixEnd(prefix)
	if err = txn.LockKeys(ctx, prefix); err != nil {
		return nil, err
	}
	pairs, err := txn.Scan(ctx, prefix, end, 0)
	if err != nil {
		return nil, err
	}
	if len(pairs) == 0 {
		return nil, meta.ErrObjectNotFound
	}
	prior, err := decodeObject(pairs[0].Value)
	if err != nil {
		return nil, err
	}
	for _, p := range pairs {
		if err = txn.Delete(p.Key); err != nil {
			return nil, err
		}
	}
	if err = txn.Commit(ctx); err != nil {
		return nil, err
	}
	db, dc := bucketStatsDelta(prior, nil)
	if db != 0 || dc != 0 {
		if _, berr := s.BumpBucketStats(ctx, bucketID, db, dc); berr != nil {
			return nil, berr
		}
	}
	return prior, nil
}

// DeleteObjectNullReplacement implements the Suspended-mode unversioned
// DELETE: drop the prior null-versioned row (if any) and write a fresh
// null-versioned delete marker. TimeUUID-versioned siblings stay.
func (s *Store) DeleteObjectNullReplacement(ctx context.Context, bucketID uuid.UUID, key string) (*meta.Object, error) {
	marker := &meta.Object{
		BucketID:       bucketID,
		Key:            key,
		VersionID:      meta.NullVersionID,
		IsLatest:       true,
		IsDeleteMarker: true,
		IsNull:         true,
		Mtime:          time.Now().UTC(),
	}
	if err := s.PutObject(ctx, marker, true); err != nil {
		return nil, err
	}
	return marker, nil
}

// SetObjectStorage flips an object's storage class with optional CAS on the
// prior class. Empty expectedClass is unconditional. The pessimistic txn is
// the TiKV mirror of Cassandra's `UPDATE ... IF storage_class=?` LWT —
// concurrent lifecycle transitions either serialise or one observes the
// other's commit and returns applied=false.
func (s *Store) SetObjectStorage(ctx context.Context, bucketID uuid.UUID, key, versionID, expectedClass, newClass string, manifest *data.Manifest) (applied bool, err error) {
	versionID = meta.ResolveVersionID(versionID)

	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return false, err
	}
	defer rollbackOnError(txn, &err)

	objKey, raw, err := loadObjectForUpdate(ctx, txn, bucketID, key, versionID)
	if err != nil {
		return false, err
	}
	o, err := decodeObject(raw)
	if err != nil {
		return false, err
	}
	if expectedClass != "" && o.StorageClass != expectedClass {
		// CAS reject. Roll back explicitly so the pessimistic txn's
		// LockKeys lease is released — defer rollbackOnError only fires
		// when err is non-nil.
		_ = txn.Rollback()
		return false, nil
	}
	o.StorageClass = newClass
	o.Manifest = manifest
	payload, err := encodeObject(o)
	if err != nil {
		return false, err
	}
	if err = txn.Set(objKey, payload); err != nil {
		return false, err
	}
	if err = txn.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// loadObjectForUpdate locks and reads the row addressed by (bucketID, key,
// versionID). Empty versionID resolves to the latest version via a one-row
// range scan; the returned objKey is the actual stored key (so the caller
// can Set/Delete on it without re-encoding).
func loadObjectForUpdate(ctx context.Context, txn kvTxn, bucketID uuid.UUID, key, versionID string) ([]byte, []byte, error) {
	if versionID != "" {
		objKey, err := ObjectKey(bucketID, key, versionID)
		if err != nil {
			return nil, nil, meta.ErrObjectNotFound
		}
		if err := txn.LockKeys(ctx, objKey); err != nil {
			return nil, nil, err
		}
		raw, found, err := txn.Get(ctx, objKey)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			return nil, nil, meta.ErrObjectNotFound
		}
		return objKey, raw, nil
	}

	start := append(ObjectPrefixWithKey(bucketID, key), 0x00, 0x00)
	end := prefixEnd(start)
	pairs, err := txn.Scan(ctx, start, end, 1)
	if err != nil {
		return nil, nil, err
	}
	if len(pairs) == 0 {
		return nil, nil, meta.ErrObjectNotFound
	}
	if err := txn.LockKeys(ctx, pairs[0].Key); err != nil {
		return nil, nil, err
	}
	return pairs[0].Key, pairs[0].Value, nil
}

