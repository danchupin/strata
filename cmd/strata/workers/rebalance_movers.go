//go:build !ceph

package workers

import "github.com/danchupin/strata/internal/rebalance"

// rebalanceMovers returns the per-build-tag set of movers wired into
// the rebalance worker's MoverChain emitter. The non-ceph build has no
// RADOS movers; the chain falls back to plan-logging-only behaviour
// shipped in US-003. The S3-side mover lands in US-005.
func rebalanceMovers(_ Dependencies, _ *rebalance.Throttle, _ int) []rebalance.Mover {
	return nil
}
