package s3api

import (
	"context"
	"log/slog"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/logging"
	"github.com/danchupin/strata/internal/meta"
)

// dataCtxForPut wraps the request context with the bucket id, object key,
// and per-bucket placement policy so the data backend's PutChunks can
// route chunks via placement.PickCluster (US-002 placement-rebalance).
//
// GetBucketPlacement is consulted ONCE per PutChunks invocation. The
// memory note tracks that meta.Bucket.Placement is NOT populated by
// GetBucket — the hot-path GetBucket stays a single buckets-table read —
// so the policy must be fetched explicitly here.
//
// Errors fetching the policy are logged at WARN and treated as
// "no placement" so a transient meta hiccup never breaks the PUT path.
func dataCtxForPut(ctx context.Context, m meta.Store, b *meta.Bucket, key string) context.Context {
	ctx = data.WithBucketID(ctx, b.ID)
	ctx = data.WithObjectKey(ctx, key)
	policy, err := m.GetBucketPlacement(ctx, b.Name)
	if err != nil {
		if lg := logging.LoggerFromContext(ctx); lg != nil {
			lg.WarnContext(ctx, "placement: GetBucketPlacement failed; routing per class default",
				"bucket", b.Name, "error", err.Error())
		} else {
			slog.WarnContext(ctx, "placement: GetBucketPlacement failed; routing per class default",
				"bucket", b.Name, "error", err.Error())
		}
		return ctx
	}
	return data.WithPlacement(ctx, policy)
}
