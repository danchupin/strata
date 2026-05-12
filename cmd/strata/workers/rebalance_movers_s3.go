package workers

import (
	"github.com/danchupin/strata/internal/data/s3"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/rebalance"
)

// s3RebalanceMovers is the build-tag-free helper that wires the
// S3-side mover (US-005) into the rebalance worker's MoverChain
// emitter. Both rebalance_movers.go (!ceph) and
// rebalance_movers_ceph.go (ceph) call this — the S3 backend works
// without librados so the S3 path lives outside the build-tag branch.
func s3RebalanceMovers(deps Dependencies, throttle *rebalance.Throttle, inflight int) []rebalance.Mover {
	sb, ok := deps.Data.(*s3.Backend)
	if !ok {
		return nil
	}
	clusters := sb.RebalanceClusters()
	if len(clusters) == 0 {
		return nil
	}
	tracer := deps.Tracer.Tracer("strata.worker.rebalance")
	return []rebalance.Mover{
		&rebalance.S3Mover{
			Clusters: clusters,
			BucketBy: sb.BucketOnCluster,
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
