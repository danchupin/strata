package cassandra

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/gocql/gocql"

	"github.com/danchupin/strata/internal/meta"
)

// ListClusters returns every cluster_registry row sorted by id ascending.
// Empty registry returns a nil slice (NOT an error) per the contract.
func (s *Store) ListClusters(ctx context.Context) ([]*meta.ClusterRegistryEntry, error) {
	iter := s.s.Query(
		`SELECT id, backend, spec, created_at, updated_at, version FROM cluster_registry`,
	).WithContext(ctx).Iter()

	var (
		id, backendName      string
		spec                 []byte
		createdAt, updatedAt time.Time
		version              int64
		out                  []*meta.ClusterRegistryEntry
	)
	for iter.Scan(&id, &backendName, &spec, &createdAt, &updatedAt, &version) {
		row := &meta.ClusterRegistryEntry{
			ID:        id,
			Backend:   backendName,
			Spec:      append([]byte(nil), spec...),
			CreatedAt: createdAt,
			UpdatedAt: updatedAt,
			Version:   version,
		}
		out = append(out, row)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// GetCluster returns the row addressed by id, or ErrClusterNotFound.
func (s *Store) GetCluster(ctx context.Context, id string) (*meta.ClusterRegistryEntry, error) {
	var (
		backendName          string
		spec                 []byte
		createdAt, updatedAt time.Time
		version              int64
	)
	err := s.s.Query(
		`SELECT backend, spec, created_at, updated_at, version FROM cluster_registry WHERE id=?`,
		id,
	).WithContext(ctx).Scan(&backendName, &spec, &createdAt, &updatedAt, &version)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrClusterNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta.ClusterRegistryEntry{
		ID:        id,
		Backend:   backendName,
		Spec:      append([]byte(nil), spec...),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
		Version:   version,
	}, nil
}

// PutCluster inserts a new row when no row exists for the given ID, or
// CAS-updates an existing row when e.Version matches the stored Version.
// Returns ErrClusterVersionMismatch on stale writes.
//
// Implementation: try INSERT IF NOT EXISTS first. If applied, success.
// If not applied, Cassandra returns the existing row in the LWT result map;
// we read its version + created_at and follow up with UPDATE IF version=?
// — the LWT-on-LWT pattern from CLAUDE.md (read-after-write coherence on
// rows queried with quorum after a previous LWT write).
func (s *Store) PutCluster(ctx context.Context, e *meta.ClusterRegistryEntry) error {
	if e == nil || e.ID == "" {
		return meta.ErrClusterNotFound
	}
	now := time.Now().UTC()
	createdAt := e.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	existing := map[string]any{}
	applied, err := s.s.Query(
		`INSERT INTO cluster_registry (id, backend, spec, created_at, updated_at, version)
		 VALUES (?, ?, ?, ?, ?, ?) IF NOT EXISTS`,
		e.ID, e.Backend, e.Spec, createdAt, now, int64(1),
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).MapScanCAS(existing)
	if err != nil {
		return err
	}
	if applied {
		e.CreatedAt = createdAt
		e.UpdatedAt = now
		e.Version = 1
		return nil
	}
	curVersion, _ := existing["version"].(int64)
	if e.Version != curVersion {
		return meta.ErrClusterVersionMismatch
	}
	curCreatedAt, _ := existing["created_at"].(time.Time)
	newVersion := curVersion + 1
	applied, err = s.s.Query(
		`UPDATE cluster_registry SET backend=?, spec=?, updated_at=?, version=? WHERE id=? IF version=?`,
		e.Backend, e.Spec, now, newVersion, e.ID, curVersion,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrClusterVersionMismatch
	}
	e.CreatedAt = curCreatedAt
	e.UpdatedAt = now
	e.Version = newVersion
	return nil
}

// DeleteCluster removes the row addressed by id. Returns ErrClusterNotFound
// when no row exists. Uses LWT IF EXISTS so concurrent retries surface a
// deterministic error instead of a silent no-op.
func (s *Store) DeleteCluster(ctx context.Context, id string) error {
	applied, err := s.s.Query(
		`DELETE FROM cluster_registry WHERE id=? IF EXISTS`, id,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrClusterNotFound
	}
	return nil
}
