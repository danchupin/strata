// Per-object metadata accessors (US-001 tikv-stubs): tags + ACL grants.
//
// Both endpoints piggyback on the objects-row payload (objects.<row>.Tags +
// objects.<row>.Grants). That mirrors Cassandra's per-column UPDATE shape
// (tags + grants live on the same `objects` row alongside size/etag/mtime/…)
// and keeps the read path to one Get — no separate kind keys to coordinate.
//
// Write paths use a pessimistic txn (per CLAUDE.md "Plain Put on a key with
// prior LWT history breaks read-after-write" gotcha). Read paths take a
// snapshot Get — read-only.
package tikv

import (
	"context"
	"encoding/json"
	"maps"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// SetObjectTags overwrites the object row's tags map. Empty tags map writes
// an empty map (not absent) per S3 PutObjectTagging semantics. Returns
// meta.ErrObjectNotFound when the addressed (key, versionID) row is absent.
func (s *Store) SetObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string, tags map[string]string) (err error) {
	ctx, finish := s.observer.Start(ctx, "SetObjectTags", "objects")
	defer func() { finish(err) }()
	versionID = meta.ResolveVersionID(versionID)
	return s.mutateObjectRow(ctx, bucketID, key, versionID, func(row *objectRow) {
		row.Tags = tags
	})
}

// GetObjectTags returns the tags map persisted on the object row. Returns
// an empty (non-nil) map for an existing object with no tags persisted —
// AWS S3 returns an empty TagSet, not an error, in that case. Returns
// meta.ErrObjectNotFound when the row itself is absent.
func (s *Store) GetObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string) (out map[string]string, err error) {
	ctx, finish := s.observer.Start(ctx, "GetObjectTags", "objects")
	defer func() { finish(err) }()
	row, err := s.snapshotObjectRow(ctx, bucketID, key, versionID)
	if err != nil {
		return nil, err
	}
	out = make(map[string]string, len(row.Tags))
	maps.Copy(out, row.Tags)
	return out, nil
}

// DeleteObjectTags clears the tags map on the object row. Returns
// meta.ErrObjectNotFound when the row is absent.
func (s *Store) DeleteObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string) (err error) {
	ctx, finish := s.observer.Start(ctx, "DeleteObjectTags", "objects")
	defer func() { finish(err) }()
	versionID = meta.ResolveVersionID(versionID)
	return s.mutateObjectRow(ctx, bucketID, key, versionID, func(row *objectRow) {
		row.Tags = nil
	})
}

// SetObjectGrants overwrites the object row's ACL grants blob. Empty list
// writes an empty JSON array (not absent) — matches Cassandra's
// UPDATE … SET grants=? shape, which stores the encoded blob verbatim.
func (s *Store) SetObjectGrants(ctx context.Context, bucketID uuid.UUID, key, versionID string, grants []meta.Grant) (err error) {
	ctx, finish := s.observer.Start(ctx, "SetObjectGrants", "objects")
	defer func() { finish(err) }()
	blob, err := MarshalBlob(grants)
	if err != nil {
		return err
	}
	versionID = meta.ResolveVersionID(versionID)
	return s.mutateObjectRow(ctx, bucketID, key, versionID, func(row *objectRow) {
		row.Grants = blob
	})
}

// GetObjectGrants returns the persisted ACL grants. Returns
// meta.ErrObjectNotFound when the row is absent; meta.ErrNoSuchGrants when
// the row exists but no grants blob has been persisted (matches Cassandra
// + Memory).
func (s *Store) GetObjectGrants(ctx context.Context, bucketID uuid.UUID, key, versionID string) (out []meta.Grant, err error) {
	ctx, finish := s.observer.Start(ctx, "GetObjectGrants", "objects")
	defer func() { finish(err) }()
	row, err := s.snapshotObjectRow(ctx, bucketID, key, versionID)
	if err != nil {
		return nil, err
	}
	if len(row.Grants) == 0 {
		return nil, meta.ErrNoSuchGrants
	}
	if err := UnmarshalBlob(row.Grants, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		// Caller wrote an empty list explicitly. Cassandra parity: an empty
		// `grants` column also surfaces as ErrNoSuchGrants because
		// decodeGrants returns (nil, nil) for an empty blob (see
		// internal/meta/cassandra/codec.go) and GetObjectGrants then maps
		// the nil to ErrNoSuchGrants via the column-empty guard. We match
		// that here so the contract test (and AWS SDK callers) observe one
		// "no grants persisted" signal regardless of which backend handled
		// the write.
		return nil, meta.ErrNoSuchGrants
	}
	return out, nil
}

// SetObjectRetention overwrites the object row's RetainMode + RetainUntil
// fields. Empty mode + zero until clears retention (matches Cassandra
// `nilIfEmpty(mode)` + `*time.Time` semantics — both surface as the
// zero-value fields on the next Get). The COMPLIANCE-immutable check lives
// in internal/s3api/objectlock.go (pre-call audit + ErrAccessDenied);
// meta-layer parity with Cassandra/Memory means this impl never refuses.
// Returns meta.ErrObjectNotFound when the addressed row is absent.
func (s *Store) SetObjectRetention(ctx context.Context, bucketID uuid.UUID, key, versionID, mode string, until time.Time) (err error) {
	ctx, finish := s.observer.Start(ctx, "SetObjectRetention", "objects")
	defer func() { finish(err) }()
	versionID = meta.ResolveVersionID(versionID)
	return s.mutateObjectRow(ctx, bucketID, key, versionID, func(row *objectRow) {
		row.RetainMode = mode
		row.RetainUntil = until
	})
}

// SetObjectLegalHold flips the object row's LegalHold flag. Returns
// meta.ErrObjectNotFound when the row is absent.
func (s *Store) SetObjectLegalHold(ctx context.Context, bucketID uuid.UUID, key, versionID string, on bool) (err error) {
	ctx, finish := s.observer.Start(ctx, "SetObjectLegalHold", "objects")
	defer func() { finish(err) }()
	versionID = meta.ResolveVersionID(versionID)
	return s.mutateObjectRow(ctx, bucketID, key, versionID, func(row *objectRow) {
		row.LegalHold = on
	})
}

// SetObjectRestoreStatus overwrites the object row's RestoreStatus field.
// Empty status clears it (Cassandra parity: `nilIfEmpty(status)` → NULL →
// "" on Get). Returns meta.ErrObjectNotFound when the row is absent.
func (s *Store) SetObjectRestoreStatus(ctx context.Context, bucketID uuid.UUID, key, versionID, status string) (err error) {
	ctx, finish := s.observer.Start(ctx, "SetObjectRestoreStatus", "objects")
	defer func() { finish(err) }()
	versionID = meta.ResolveVersionID(versionID)
	return s.mutateObjectRow(ctx, bucketID, key, versionID, func(row *objectRow) {
		row.RestoreStatus = status
	})
}

// mutateObjectRow is the shared RMW helper: pessimistic txn, lock + Get the
// addressed row, decode to objectRow, apply mutate, re-encode, Set, Commit.
// Mutates against the JSON row directly so callers can touch fields
// (objectRow.Grants) that the meta.Object→objectRow conversion in
// encodeObject does not surface.
func (s *Store) mutateObjectRow(ctx context.Context, bucketID uuid.UUID, key, versionID string, mutate func(*objectRow)) (err error) {
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	objKey, raw, err := loadObjectForUpdate(ctx, txn, bucketID, key, versionID)
	if err != nil {
		return err
	}
	var row objectRow
	if err = json.Unmarshal(raw, &row); err != nil {
		return err
	}
	mutate(&row)
	payload, err := json.Marshal(&row)
	if err != nil {
		return err
	}
	if err = txn.Set(objKey, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// snapshotObjectRow decodes the (bucketID, key, versionID) row under a
// read-only txn. Empty versionID resolves to the lex-first row (latest per
// the version-DESC suffix encoding); delete markers are NOT filtered —
// matches Cassandra/Memory which read whatever row resolveVersionID /
// findLatest returns. Returns meta.ErrObjectNotFound when no row exists.
func (s *Store) snapshotObjectRow(ctx context.Context, bucketID uuid.UUID, key, versionID string) (row objectRow, err error) {
	versionID = meta.ResolveVersionID(versionID)
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return objectRow{}, err
	}
	defer txn.Rollback()
	var raw []byte
	if versionID != "" {
		objKey, kerr := ObjectKey(bucketID, key, versionID)
		if kerr != nil {
			return objectRow{}, meta.ErrObjectNotFound
		}
		b, found, gerr := txn.Get(ctx, objKey)
		if gerr != nil {
			return objectRow{}, gerr
		}
		if !found {
			return objectRow{}, meta.ErrObjectNotFound
		}
		raw = b
	} else {
		start := append(ObjectPrefixWithKey(bucketID, key), 0x00, 0x00)
		pairs, serr := txn.Scan(ctx, start, prefixEnd(start), 1)
		if serr != nil {
			return objectRow{}, serr
		}
		if len(pairs) == 0 {
			return objectRow{}, meta.ErrObjectNotFound
		}
		raw = pairs[0].Value
	}
	if err = json.Unmarshal(raw, &row); err != nil {
		return objectRow{}, err
	}
	return row, nil
}
