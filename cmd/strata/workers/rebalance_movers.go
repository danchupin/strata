//go:build !ceph

package workers

import "github.com/danchupin/strata/internal/rebalance"

// rebalanceMovers returns the per-build-tag set of movers wired into
// the rebalance worker's MoverChain emitter. The non-ceph build has no
// RADOS mover; the S3 mover (US-005) plugs in when deps.Data is
// *s3.Backend. Chains with no movers fall back to plan-logging
// behaviour shipped in US-003.
func rebalanceMovers(deps Dependencies, throttle *rebalance.Throttle, inflight int) []rebalance.Mover {
	return s3RebalanceMovers(deps, throttle, inflight)
}
