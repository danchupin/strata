package reconcile

import (
	"context"

	"github.com/danchupin/strata/internal/data"
)

// S3Scanner adapts a data.ChunkLister (the S3-passthrough backend's native
// ListObjects walk) to the ChunkScanner interface (US-002b). Unlike the RADOS
// leg there is no pool-enumeration primitive: the S3 backend lists the objects
// in its backing bucket and reads each one's x-amz-meta-strata-backref
// user-metadata (stamped at PUT by US-001). scope.Pool carries the storage
// class the backend maps onto a (cluster, bucket) pair.
//
// A malformed back-reference payload is treated as absent (HasBackref stays
// false) — reported, never used to delete or restore. We never act on a payload
// we could not decode.
type S3Scanner struct {
	Lister data.ChunkLister
}

// Scan enumerates the scope's backing bucket, decoding each object's
// back-reference. The opaque cursor is the backend's native ListObjects
// continuation token rendered as a string so the worker can persist + resume it.
func (s *S3Scanner) Scan(ctx context.Context, scope ScanScope, startCursor string, visit func(ScannedChunk, string) error) error {
	return s.Lister.ListChunks(ctx, scope.Cluster, scope.Pool, startCursor, func(lc data.ListedChunk, cursor string) error {
		c := ScannedChunk{
			Cluster:   scope.Cluster,
			Pool:      scope.Pool,
			Namespace: scope.Namespace,
			OID:       lc.OID,
			Size:      lc.Size,
		}
		if len(lc.Backref) > 0 {
			if br, err := data.DecodeBackref(lc.Backref); err == nil {
				c.Backref = br
				c.HasBackref = true
			}
		}
		return visit(c, cursor)
	})
}
