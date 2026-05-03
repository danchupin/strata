package memory

import (
	"context"

	"github.com/danchupin/strata/internal/meta"
)

// SetBucketBackendPresign flips the per-bucket presign-passthrough flag (US-016).
func (s *Store) SetBucketBackendPresign(ctx context.Context, name string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return meta.ErrBucketNotFound
	}
	b.BackendPresign = enabled
	return nil
}
