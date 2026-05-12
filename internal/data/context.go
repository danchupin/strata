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

// placementKey carries the per-bucket placement policy
// (map[clusterID]weight) onto the data-plane ctx so backend PutChunks
// implementations can route chunks via the placement.PickCluster hash
// without widening the Backend interface (US-002 placement-rebalance).
type placementKey struct{}

// WithPlacement stores the placement policy on ctx. Empty / nil policy
// returns ctx unchanged so the caller path is the same as the
// unconfigured case.
func WithPlacement(ctx context.Context, policy map[string]int) context.Context {
	if len(policy) == 0 {
		return ctx
	}
	return context.WithValue(ctx, placementKey{}, policy)
}

// PlacementFromContext returns the placement policy stored via
// WithPlacement. The second return is false when the request carries no
// policy — callers route to their configured $defaultCluster.
func PlacementFromContext(ctx context.Context) (map[string]int, bool) {
	if ctx == nil {
		return nil, false
	}
	v, ok := ctx.Value(placementKey{}).(map[string]int)
	if !ok || len(v) == 0 {
		return nil, false
	}
	return v, true
}

// objectKeyKey carries the Strata object key on the data-plane ctx so the
// placement hash input is stable across retries
// (placement.PickCluster hashes "<bucketID>/<key>/<chunkIdx>"). The key
// stays out of backend object naming — only the hash input depends on it
// (US-002 placement-rebalance).
type objectKeyKey struct{}

// WithObjectKey stores the Strata object key on ctx. Empty string returns
// ctx unchanged.
func WithObjectKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, objectKeyKey{}, key)
}

// ObjectKeyFromContext returns the object key stored via WithObjectKey.
// The second return is false when no key is present.
func ObjectKeyFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(objectKeyKey{}).(string)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}
