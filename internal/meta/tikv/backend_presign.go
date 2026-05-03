package tikv

import (
	"context"

	"github.com/danchupin/strata/internal/meta"
)

// SetBucketBackendPresign flips the per-bucket presign-passthrough flag (US-016).
// Uses the pessimistic-txn shape via the shared updateBucket helper per the
// US-001..US-018 cycle's RMW lesson — plain Put on a row with prior LWT history
// breaks read-after-write coherence.
func (s *Store) SetBucketBackendPresign(ctx context.Context, name string, enabled bool) error {
	return s.updateBucket(ctx, name, func(b *meta.Bucket) error {
		b.BackendPresign = enabled
		return nil
	})
}
