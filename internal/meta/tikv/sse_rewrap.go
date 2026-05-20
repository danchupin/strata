// SSE master-key rewrap + raw manifest + per-object replication status
// (US-005 tikv-stubs).
//
// All six methods piggyback on the existing object-row payload (objectRow)
// except SetRewrapProgress / GetRewrapProgress, which live under a per-
// bucket single-row key (RewrapProgressKey). The object-row mutators
// reuse the shared mutateObjectRow helper from object_meta.go — same
// pessimistic-txn shape as US-001 / US-002 RMW writes.
package tikv

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// UpdateObjectSSEWrap overwrites the object row's wrapped DEK + key ID
// pair. Empty versionID resolves to the latest version (matches Cassandra
// UpdateObjectSSEWrap which round-trips through GetObject for "" → o.VersionID;
// loadObjectForUpdate folds the same behaviour into the prefix-scan path).
func (s *Store) UpdateObjectSSEWrap(ctx context.Context, bucketID uuid.UUID, key, versionID string, wrapped []byte, keyID string) (err error) {
	ctx, finish := s.observer.Start(ctx, "UpdateObjectSSEWrap", "objects")
	defer func() { finish(err) }()
	versionID = meta.ResolveVersionID(versionID)
	return s.mutateObjectRow(ctx, bucketID, key, versionID, func(row *objectRow) {
		if len(wrapped) == 0 {
			row.SSEKey = nil
		} else {
			row.SSEKey = append([]byte(nil), wrapped...)
		}
		row.SSEKeyID = keyID
	})
}

// SetRewrapProgress persists (target_id, last_key, complete, updated_at)
// under the per-bucket rewrap-progress slot. The store stamps UpdatedAt
// to UTC-now (mirrors Cassandra `time.Now().UTC()` + Memory `time.Now().UTC()`)
// so callers can leave it zero. Nil progress is a no-op (Cassandra +
// Memory parity).
func (s *Store) SetRewrapProgress(ctx context.Context, p *meta.RewrapProgress) (err error) {
	ctx, finish := s.observer.Start(ctx, "SetRewrapProgress", "rewrap_progress")
	defer func() { finish(err) }()
	if p == nil {
		return nil
	}
	cp := *p
	cp.UpdatedAt = time.Now().UTC()
	blob, err := json.Marshal(&cp)
	if err != nil {
		return err
	}
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Set(RewrapProgressKey(p.BucketID), blob); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// GetRewrapProgress reads the per-bucket rewrap-progress row. Returns
// meta.ErrNoRewrapProgress when absent (matches Cassandra +
// Memory).
func (s *Store) GetRewrapProgress(ctx context.Context, bucketID uuid.UUID) (out *meta.RewrapProgress, err error) {
	ctx, finish := s.observer.Start(ctx, "GetRewrapProgress", "rewrap_progress")
	defer func() { finish(err) }()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, RewrapProgressKey(bucketID))
	if err != nil {
		return nil, err
	}
	if !found || len(raw) == 0 {
		return nil, meta.ErrNoRewrapProgress
	}
	var rp meta.RewrapProgress
	if err = json.Unmarshal(raw, &rp); err != nil {
		return nil, err
	}
	rp.BucketID = bucketID
	return &rp, nil
}

// GetObjectManifestRaw returns the raw manifest blob stored on the object
// row. The bytes are emitted verbatim — data.DecodeManifest sniffs JSON-vs-
// proto on read, so callers (manifest_rewriter worker, ops tooling) see
// whatever shape PutObject persisted. Returns meta.ErrObjectNotFound when
// the row is absent.
func (s *Store) GetObjectManifestRaw(ctx context.Context, bucketID uuid.UUID, key, versionID string) (out []byte, err error) {
	ctx, finish := s.observer.Start(ctx, "GetObjectManifestRaw", "objects")
	defer func() { finish(err) }()
	row, err := s.snapshotObjectRow(ctx, bucketID, key, versionID)
	if err != nil {
		return nil, err
	}
	if len(row.ManifestRaw) == 0 {
		return nil, nil
	}
	return append([]byte(nil), row.ManifestRaw...), nil
}

// UpdateObjectManifestRaw overwrites the raw manifest blob on the object
// row without validation — callers (manifest_rewriter) are responsible for
// passing a valid JSON or proto3-wire manifest. Subsequent GetObject reads
// re-decode via data.DecodeManifest (which sniffs the first byte).
func (s *Store) UpdateObjectManifestRaw(ctx context.Context, bucketID uuid.UUID, key, versionID string, raw []byte) (err error) {
	ctx, finish := s.observer.Start(ctx, "UpdateObjectManifestRaw", "objects")
	defer func() { finish(err) }()
	versionID = meta.ResolveVersionID(versionID)
	return s.mutateObjectRow(ctx, bucketID, key, versionID, func(row *objectRow) {
		if len(raw) == 0 {
			row.ManifestRaw = nil
		} else {
			row.ManifestRaw = append([]byte(nil), raw...)
		}
	})
}

// SetObjectReplicationStatus overwrites the object row's
// replication_status field. Empty status clears it (Cassandra parity via
// nilIfEmpty(status)). Returns meta.ErrObjectNotFound when the row is
// absent.
func (s *Store) SetObjectReplicationStatus(ctx context.Context, bucketID uuid.UUID, key, versionID, status string) (err error) {
	ctx, finish := s.observer.Start(ctx, "SetObjectReplicationStatus", "objects")
	defer func() { finish(err) }()
	versionID = meta.ResolveVersionID(versionID)
	return s.mutateObjectRow(ctx, bucketID, key, versionID, func(row *objectRow) {
		row.ReplicationStatus = status
	})
}

