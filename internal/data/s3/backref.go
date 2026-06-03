package s3

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/danchupin/strata/internal/data"
)

// StampBackref implements data.BackrefStamper (US-001b): it (re)writes the
// US-001 back-reference (x-amz-meta-strata-backref) on the backing object(s) of
// an already-written manifest so a multipart object — whose part backing
// objects were uploaded before the final object identity existed — becomes
// recoverable by the reconcile worker's S3Scanner (US-002b), which pairs a
// backing object to its owner via this metadata and GetObject(bucket,key,version).
//
// The S3-passthrough backend stores one backing object per PutChunks call as a
// BackendRef, so ChunkIdx is 0 (single backing object). A Chunks-shaped manifest
// (defensive — the s3 backend does not emit one today) stamps each OID by
// position.
//
// Round-trip trade-off: re-stamping S3 user-metadata needs a HEAD (to preserve
// the object's existing metadata + content-type) followed by a self-CopyObject
// with MetadataDirective=REPLACE — two round trips per backing object, run at
// CompleteMultipartUpload, never on the UploadPart hot path. Best-effort: the
// object bytes are already durable, so a failure leaves the chunk
// recoverable-degraded (reconcile reports AbsentBackref, never deletes); the
// first error is returned for the caller to log, the object is never corrupted.
func (b *Backend) StampBackref(ctx context.Context, m *data.Manifest, attrs data.BackrefAttrs) error {
	if !b.backref || m == nil {
		return nil
	}
	if m.BackendRef != nil {
		c, bucket, err := b.clusterForClass(ctx, m.Class)
		if err != nil {
			return err
		}
		return b.stampOne(ctx, c, bucket, m.BackendRef.Key, data.Backref{
			BucketID:  attrs.BucketID,
			Key:       attrs.Key,
			VersionID: attrs.VersionID,
			ChunkIdx:  0,
			Mtime:     attrs.Mtime,
			SSEAlgo:   attrs.SSEAlgo,
		})
	}
	var firstErr error
	for idx, chunk := range m.Chunks {
		if chunk.Cluster == "" {
			continue
		}
		c, err := b.connFor(ctx, chunk.Cluster)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if serr := b.stampOne(ctx, c, chunk.Pool, chunk.OID, data.Backref{
			BucketID:  attrs.BucketID,
			Key:       attrs.Key,
			VersionID: attrs.VersionID,
			ChunkIdx:  idx,
			Mtime:     attrs.Mtime,
			SSEAlgo:   attrs.SSEAlgo,
		}); serr != nil && firstErr == nil {
			firstErr = serr
		}
	}
	return firstErr
}

// stampOne rewrites the x-amz-meta-strata-backref on a single backing object,
// preserving its existing user-metadata + content-type via a HEAD-then-copy.
func (b *Backend) stampOne(ctx context.Context, c *s3Cluster, bucket, key string, br data.Backref) error {
	headCtx, headCancel := opCtxFor(ctx, c.opTimeout)
	head, err := c.client.HeadObject(headCtx, &awss3.HeadObjectInput{Bucket: &bucket, Key: &key})
	headCancel()
	if err != nil {
		return fmt.Errorf("s3: stamp backref head %s: %w", key, err)
	}
	meta := make(map[string]string, len(head.Metadata)+1)
	maps.Copy(meta, head.Metadata)
	meta[data.BackrefMetaKey] = base64.StdEncoding.EncodeToString(data.EncodeBackref(br))

	source := bucket + "/" + key
	in := &awss3.CopyObjectInput{
		Bucket:            &bucket,
		Key:               &key,
		CopySource:        &source,
		MetadataDirective: s3types.MetadataDirectiveReplace,
		Metadata:          meta,
	}
	if head.ContentType != nil {
		in.ContentType = head.ContentType
	}
	copyCtx, copyCancel := opCtxFor(ctx, c.opTimeout)
	_, err = c.client.CopyObject(copyCtx, in)
	copyCancel()
	if err != nil {
		return fmt.Errorf("s3: stamp backref copy %s: %w", key, err)
	}
	return nil
}

var _ data.BackrefStamper = (*Backend)(nil)
