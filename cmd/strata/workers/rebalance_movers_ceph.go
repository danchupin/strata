//go:build ceph

package workers

import (
	"github.com/danchupin/strata/internal/data/rados"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/rebalance"
)

// rebalanceMovers wires the per-build-tag movers into the MoverChain
// emitter. The ceph build pulls in the RADOS-side mover (US-004) when
// deps.Data is *rados.Backend; the S3-side mover (US-005) plugs in for
// *s3.Backend regardless of build tag. Chains with no movers fall back
// to plan-logging behaviour shipped in US-003.
func rebalanceMovers(deps Dependencies, throttle *rebalance.Throttle, inflight int) []rebalance.Mover {
	movers := s3RebalanceMovers(deps, throttle, inflight)
	if rb, ok := deps.Data.(*rados.Backend); ok {
		clusters := rados.RebalanceClusters(rb)
		if len(clusters) > 0 {
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
