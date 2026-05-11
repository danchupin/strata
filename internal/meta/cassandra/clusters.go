package cassandra

import (
	"context"
	"errors"

	"github.com/danchupin/strata/internal/meta"
)

// errClusterRegistryNotImplemented is the stub error returned by the
// cluster-registry methods on the Cassandra backend until US-002 wires the
// real LWT-on-LWT CRUD against the cluster_registry table.
var errClusterRegistryNotImplemented = errors.New("cassandra: cluster registry not implemented (US-002)")

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
