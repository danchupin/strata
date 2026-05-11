package tikv

import (
	"context"
	"errors"

	"github.com/danchupin/strata/internal/meta"
)

// errClusterRegistryNotImplemented is the stub error returned by the
// cluster-registry methods on the TiKV backend until US-003 wires the real
// pessimistic-txn CAS-on-Version CRUD against the encoded `clusters/<id>` keys.
var errClusterRegistryNotImplemented = errors.New("tikv: cluster registry not implemented (US-003)")

func (s *Store) ListClusters(ctx context.Context) ([]*meta.ClusterRegistryEntry, error) {
	return nil, errClusterRegistryNotImplemented
}

func (s *Store) GetCluster(ctx context.Context, id string) (*meta.ClusterRegistryEntry, error) {
	return nil, errClusterRegistryNotImplemented
}

func (s *Store) PutCluster(ctx context.Context, e *meta.ClusterRegistryEntry) error {
	return errClusterRegistryNotImplemented
}

func (s *Store) DeleteCluster(ctx context.Context, id string) error {
	return errClusterRegistryNotImplemented
}
