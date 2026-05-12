//go:build ceph

package workers

import (
	"github.com/danchupin/strata/internal/data/rados"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/rebalance"
)

// rebalanceMovers wires the RADOS-side mover (US-004) into the
// MoverChain emitter when the binary is built with `-tags ceph`. The
// deps.Data backend is type-asserted to *rados.Backend; mismatched
// backends (memory, s3) skip the mover so the chain falls back to
// plan-logging behaviour. The S3-side mover lands in US-005.
func rebalanceMovers(deps Dependencies, throttle *rebalance.Throttle, inflight int) []rebalance.Mover {
	rb, ok := deps.Data.(*rados.Backend)
	if !ok {
		return nil
	}
	clusters := rados.RebalanceClusters(rb)
	if len(clusters) == 0 {
		return nil
	}
	tracer := deps.Tracer.Tracer("strata.worker.rebalance")
	return []rebalance.Mover{
		&rebalance.RadosMover{
			Clusters: clusters,
			Meta:     deps.Meta,
			Region:   deps.Region,
			Logger:   deps.Logger,
			Metrics:  metrics.RebalanceObserver{},
			Tracer:   tracer,
			Throttle: throttle,
			Inflight: inflight,
		},
	}
}
