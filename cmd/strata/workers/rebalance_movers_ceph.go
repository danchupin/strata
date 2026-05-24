//go:build ceph

package workers

import (
	"github.com/danchupin/strata/cephimpl"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/rebalance"
)

// rebalanceMovers wires the per-build-tag movers into the MoverChain
// emitter. The ceph build pulls in the RADOS-side mover when
// deps.Data is *cephimpl.Backend (the librados-linked impl now lives
// in its own Go module — see internal/data/rados/cephimpl/); the
// S3-side mover plugs in for *s3.Backend regardless of build tag.
// Chains with no movers fall back to plan-logging behaviour.
func rebalanceMovers(deps Dependencies, throttle *rebalance.Throttle, inflight int) []rebalance.Mover {
	movers := s3RebalanceMovers(deps, throttle, inflight)
	if rb, ok := deps.Data.(*cephimpl.Backend); ok {
		clustersCeph := cephimpl.RebalanceClusters(rb)
		if len(clustersCeph) > 0 {
			// cephimpl returns map[string]cephimpl.RadosCluster — convert
			// to map[string]rebalance.RadosCluster. The two interfaces are
			// structurally identical; each cephimpl.RadosCluster value
			// satisfies rebalance.RadosCluster implicitly.
			clusters := make(map[string]rebalance.RadosCluster, len(clustersCeph))
			for id, c := range clustersCeph {
				clusters[id] = c
			}
			tracer := deps.Tracer.Tracer("strata.worker.rebalance")
			movers = append(movers, &rebalance.RadosMover{
				Clusters: clusters,
				Meta:     deps.Meta,
				Region:   deps.Region,
				Logger:   deps.Logger,
				Metrics:  metrics.RebalanceObserver{},
				Tracer:   tracer,
				Throttle: throttle,
				Inflight: inflight,
			})
		}
	}
	return movers
}
