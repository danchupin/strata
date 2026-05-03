package data

import (
	"context"

	"github.com/google/uuid"
)

// bucketIDKey is the unexported context-key type for the Strata bucket UUID
// associated with a data-plane operation. Backends that key their objects by
// bucket (US-009 s3 backend uses it for the <bucket-uuid>/<object-uuid>
// prefix) read it via BucketIDFromContext; backends that don't (memory,
// rados) ignore it.
type bucketIDKey struct{}

// WithBucketID stores the Strata bucket UUID on ctx so backend implementations
// (currently the s3 backend) can recover it without widening the
// data.Backend interface. Returning ctx unchanged when id is uuid.Nil keeps
// callers free of "did the bucket exist" branching.
func WithBucketID(ctx context.Context, id uuid.UUID) context.Context {
	if id == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, bucketIDKey{}, id)
}

// BucketIDFromContext returns the bucket UUID stored via WithBucketID. The
// second return is false when no bucket id is present — callers may then
// fall back to a random prefix (s3 backend) or skip the dimension entirely
// (other backends).
func BucketIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	if ctx == nil {
		return uuid.Nil, false
	}
	v, ok := ctx.Value(bucketIDKey{}).(uuid.UUID)
	if !ok || v == uuid.Nil {
		return uuid.Nil, false
	}
	return v, true
}
