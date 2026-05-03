package cassandra

import (
	"context"

	"github.com/danchupin/strata/internal/meta"
)

// SetBucketBackendPresign flips the per-bucket presign-passthrough flag (US-016).
// LWT-coherent on read after write — uses UPDATE ... IF EXISTS so concurrent
// CreateBucket cannot interleave a half-written row.
func (s *Store) SetBucketBackendPresign(ctx context.Context, name string, enabled bool) error {
	applied, err := s.s.Query(
		`UPDATE buckets SET backend_presign=? WHERE name=? IF EXISTS`,
		enabled, name,
	).WithContext(ctx).ScanCAS()
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrBucketNotFound
	}
	return nil
}
