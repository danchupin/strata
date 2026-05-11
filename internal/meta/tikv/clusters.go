// Cluster registry CRUD on TiKV (US-003).
//
// One row per cluster at ClusterRegistryKey(id). PutCluster uses the
// pessimistic-txn CAS shape mandated by CLAUDE.md (Begin → LockKeys →
// Get → check-Version → Set → Commit). Stale-Version writes return
// ErrClusterVersionMismatch; the early-return path explicitly calls
// txn.Rollback() so the in-process memBackend used by tests does not
// deadlock the next caller on the same key (the LockKeys-lease-leak
// gotcha noted in CLAUDE.md).
package tikv

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

// clusterRow is the on-wire JSON shape for a registry row. Short field
// names keep the payload tight; the spec is opaque to the registry
// layer.
type clusterRow struct {
	ID        string    `json:"i"`
	Backend   string    `json:"b"`
	Spec      []byte    `json:"s,omitempty"`
	CreatedAt time.Time `json:"c"`
	UpdatedAt time.Time `json:"u"`
	Version   int64     `json:"v"`
}

func encodeCluster(e *meta.ClusterRegistryEntry) ([]byte, error) {
	return json.Marshal(&clusterRow{
		ID:        e.ID,
		Backend:   e.Backend,
		Spec:      e.Spec,
		CreatedAt: e.CreatedAt,
		UpdatedAt: e.UpdatedAt,
		Version:   e.Version,
	})
}

func decodeCluster(raw []byte) (*meta.ClusterRegistryEntry, error) {
	var row clusterRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	return &meta.ClusterRegistryEntry{
		ID:        row.ID,
		Backend:   row.Backend,
		Spec:      row.Spec,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
		Version:   row.Version,
	}, nil
}

// ListClusters scans every row under the cluster-registry prefix and
// returns the catalogue sorted by id ascending. Empty registry returns
// a nil slice (NOT an error) per the contract.
func (s *Store) ListClusters(ctx context.Context) ([]*meta.ClusterRegistryEntry, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	start := ClusterRegistryPrefix()
	pairs, err := txn.Scan(ctx, start, prefixEnd(start), 0)
	if err != nil {
		return nil, err
	}
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make([]*meta.ClusterRegistryEntry, 0, len(pairs))
	for _, p := range pairs {
		e, err := decodeCluster(p.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// GetCluster is a single Get on the per-id row. Returns
// ErrClusterNotFound when no row exists.
func (s *Store) GetCluster(ctx context.Context, id string) (*meta.ClusterRegistryEntry, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, ClusterRegistryKey(id))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrClusterNotFound
	}
	return decodeCluster(raw)
}

// PutCluster inserts a fresh row when none exists for e.ID, or
// CAS-updates an existing row when e.Version matches the stored
// Version. Stale-version writes return ErrClusterVersionMismatch.
//
// Pessimistic txn (LockKeys + Get + CAS + Set) so concurrent PutCluster
// callers serialise on the row. CAS-reject early returns call
// txn.Rollback() explicitly to release the LockKeys lease — see the
// CLAUDE.md gotcha re: in-process memBackend deadlocks.
func (s *Store) PutCluster(ctx context.Context, e *meta.ClusterRegistryEntry) (err error) {
	if e == nil || e.ID == "" {
		return meta.ErrClusterNotFound
	}
	key := ClusterRegistryKey(e.ID)
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
	now := time.Now().UTC()
	var (
		newVersion int64
		createdAt  time.Time
	)
	if !found {
		newVersion = 1
		createdAt = e.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
	} else {
		cur, derr := decodeCluster(raw)
		if derr != nil {
			return derr
		}
		if e.Version != cur.Version {
			// Release the lease before returning a non-error early-result
			// so the in-process memBackend used by tests does not deadlock
			// the next caller. (CLAUDE.md gotcha.)
			_ = txn.Rollback()
			return meta.ErrClusterVersionMismatch
		}
		newVersion = cur.Version + 1
		createdAt = cur.CreatedAt
	}
	written := &meta.ClusterRegistryEntry{
		ID:        e.ID,
		Backend:   e.Backend,
		Spec:      e.Spec,
		CreatedAt: createdAt,
		UpdatedAt: now,
		Version:   newVersion,
	}
	payload, err := encodeCluster(written)
	if err != nil {
		return err
	}
	if err = txn.Set(key, payload); err != nil {
		return err
	}
	if err = txn.Commit(ctx); err != nil {
		return err
	}
	e.CreatedAt = createdAt
	e.UpdatedAt = now
	e.Version = newVersion
	return nil
}

// DeleteCluster removes the row addressed by id. Returns
// ErrClusterNotFound when no row exists — including the idempotent
// retry case.
//
// Pessimistic txn so concurrent Put/Delete serialise on the row. The
// missing-row early return calls txn.Rollback() explicitly per the
// memBackend gotcha.
func (s *Store) DeleteCluster(ctx context.Context, id string) (err error) {
	key := ClusterRegistryKey(id)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return err
	}
	_, found, err := txn.Get(ctx, key)
	if err != nil {
		return err
	}
	if !found {
		_ = txn.Rollback()
		return meta.ErrClusterNotFound
	}
	if err = txn.Delete(key); err != nil {
		return err
	}
	return txn.Commit(ctx)
}
