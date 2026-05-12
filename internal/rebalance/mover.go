package rebalance

import (
	"context"
	"log/slog"

	"github.com/danchupin/strata/internal/meta"
)

// Mover executes a slice of Moves whose ToCluster is owned by this
// mover. Each move copies one chunk between clusters of the same
// backend family (RADOS↔RADOS in US-004, S3↔S3 in US-005). The mover
// is responsible for batching per (BucketID, ObjectKey, VersionID),
// issuing the manifest CAS, and enqueueing GC entries for the losing
// side of the CAS.
type Mover interface {
	// Owns reports whether targetCluster is one of this mover's
	// destinations. The MoverChain emitter routes Moves to the first
	// mover that owns ToCluster.
	Owns(targetCluster string) bool
	// Move executes the supplied plan slice. Implementations log +
	// drop individual move failures so a single bad chunk does not
	// abort the iteration; only ctx cancellation surfaces as an
	// error.
	Move(ctx context.Context, plan []Move) error
}

// MoverChain is a PlanEmitter that fans the per-bucket plan into the
// supplied movers, partitioning by ToCluster. Plans containing moves
// whose target is unknown to every mover log a single WARN per
// iteration and the orphan moves are dropped — the worker will replan
// next tick. Plans containing zero moves short-circuit.
type MoverChain struct {
	Movers []Mover
	Logger *slog.Logger
}

// EmitPlan dispatches moves to the owning mover and logs the
// per-bucket actual/target distribution. Mirrors the planLogger shape
// so operators see one INFO line per bucket per tick even when no
// moves are needed.
func (c *MoverChain) EmitPlan(ctx context.Context, bucket *meta.Bucket, actual map[string]int, target map[string]int, moves []Move) error {
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "rebalance plan",
		"bucket", bucket.Name,
		"moves", len(moves),
		"actual", actual,
		"target", target,
	)
	if len(moves) == 0 || len(c.Movers) == 0 {
		return nil
	}
	groups := make(map[int][]Move, len(c.Movers))
	orphans := 0
	for _, mv := range moves {
		idx := c.ownerIdx(mv.ToCluster)
		if idx < 0 {
			orphans++
			continue
		}
		groups[idx] = append(groups[idx], mv)
	}
	if orphans > 0 {
		logger.WarnContext(ctx, "rebalance: no mover owns target cluster",
			"bucket", bucket.Name, "moves_dropped", orphans)
	}
	for idx, batch := range groups {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.Movers[idx].Move(ctx, batch); err != nil {
			logger.WarnContext(ctx, "rebalance: mover failed",
				"bucket", bucket.Name, "mover_idx", idx, "moves", len(batch), "error", err.Error())
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
		}
	}
	return nil
}

func (c *MoverChain) ownerIdx(cluster string) int {
	for i, m := range c.Movers {
		if m.Owns(cluster) {
			return i
		}
	}
	return -1
}
