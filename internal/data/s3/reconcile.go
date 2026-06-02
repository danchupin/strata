package s3

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"

	"github.com/danchupin/strata/internal/data"
)

// ListChunks implements data.ChunkLister (US-002b): it enumerates the backing
// bucket for (cluster, class) via native ListObjectsV2 and reads each object's
// x-amz-meta-strata-backref user-metadata (stamped at PUT by US-001) via a
// HeadObject. The reconcile worker's S3Scanner drives it for the orphan pass —
// the S3-passthrough backend has no pool-enumeration primitive (US-000 is
// RADOS-only), so native listing IS the enumeration, with no pool dependency.
//
// Resumable per object: the cursor handed to visit is each object's key, and a
// resumed walk passes it back as startCursor (ListObjectsV2 StartAfter), so a
// crashed pass continues at-or-after the last visited key.
func (b *Backend) ListChunks(ctx context.Context, cluster, class, startCursor string, visit func(data.ListedChunk, string) error) error {
	bucket := b.bucketOnCluster(class, cluster)
	if bucket == "" {
		return fmt.Errorf("s3: no bucket registered for class %q on cluster %q", class, cluster)
	}
	c, err := b.connFor(ctx, cluster)
	if err != nil {
		return err
	}
	startAfter := startCursor
	for {
		in := &awss3.ListObjectsV2Input{Bucket: &bucket}
		if startAfter != "" {
			sa := startAfter
			in.StartAfter = &sa
		}
		opCtx, cancel := opCtxFor(ctx, c.opTimeout)
		out, err := c.client.ListObjectsV2(opCtx, in)
		cancel()
		if err != nil {
			return fmt.Errorf("s3: list %s: %w", bucket, err)
		}
		for i := range out.Contents {
			obj := out.Contents[i]
			if obj.Key == nil {
				continue
			}
			key := *obj.Key
			var size int64
			if obj.Size != nil {
				size = *obj.Size
			}
			brBytes, err := b.readBackref(ctx, c, bucket, key)
			if err != nil {
				return err
			}
			if err := visit(data.ListedChunk{OID: key, Size: size, Backref: brBytes}, key); err != nil {
				return err
			}
			startAfter = key
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
	}
	return nil
}

// readBackref HEADs one backing object and returns its decoded
// x-amz-meta-strata-backref payload (nil when absent or malformed — the scanner
// reports such a chunk, never acts on it). The SDK lowercases + strips the
// x-amz-meta- prefix, so the metadata key is data.BackrefMetaKey.
func (b *Backend) readBackref(ctx context.Context, c *s3Cluster, bucket, key string) ([]byte, error) {
	opCtx, cancel := opCtxFor(ctx, c.opTimeout)
	defer cancel()
	head, err := c.client.HeadObject(opCtx, &awss3.HeadObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return nil, fmt.Errorf("s3: head %s: %w", key, err)
	}
	v, ok := head.Metadata[data.BackrefMetaKey]
	if !ok || v == "" {
		return nil, nil
	}
	dec, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, nil // malformed -> treat as absent
	}
	return dec, nil
}

var _ data.ChunkLister = (*Backend)(nil)

// ChunkExists implements data.ChunkStater (US-003b): it answers whether the
// referenced backing object still exists by issuing a native HeadObject. The
// reconcile worker's dangling-manifest pass (meta->data) probes the single
// backing object of an S3-passthrough BackendRef manifest through it — a HEAD
// that 404s means the external object was deleted out from under the meta row.
//
// ref mapping: ref.Cluster selects the cluster (empty -> the single registered
// cluster); ref.Pool is the backing bucket when set (the rebalance chunk-shape
// manifest carries it), else the bucket is resolved from the cluster's class
// registry (bucketOnCluster — the common one-bucket-per-cluster passthrough
// shape); ref.OID is the backing-object key. A missing object returns
// (false, nil); a successful HEAD returns (true, nil); any other error is
// returned verbatim so the worker counts an error and never flags a healthy
// object on a probe it could not run.
func (b *Backend) ChunkExists(ctx context.Context, ref data.ChunkRef) (bool, error) {
	if ref.OID == "" {
		return false, errors.New("s3 chunk-exists: oid (backing key) required")
	}
	var (
		c      *s3Cluster
		bucket string
		err    error
	)
	if ref.Cluster == "" {
		c, bucket, err = b.singleCluster(ctx)
		if err != nil {
			return false, err
		}
	} else {
		c, err = b.connFor(ctx, ref.Cluster)
		if err != nil {
			return false, err
		}
		bucket = ref.Pool
		if bucket == "" {
			bucket = b.bucketOnCluster("", ref.Cluster)
		}
	}
	if bucket == "" {
		return false, fmt.Errorf("s3 chunk-exists: no backing bucket for cluster %q", ref.Cluster)
	}
	key := ref.OID
	opCtx, cancel := opCtxFor(ctx, c.opTimeout)
	defer cancel()
	if _, err := c.client.HeadObject(opCtx, &awss3.HeadObjectInput{Bucket: &bucket, Key: &key}); err != nil {
		if isS3NotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("s3: head %s: %w", key, err)
	}
	return true, nil
}

// isS3NotFound reports whether err is an S3 object-missing error. HEAD returns
// no XML body, so the SDK surfaces a typed *NotFound rather than *NoSuchKey;
// some S3-compatible backends instead return a generic API error coded
// NotFound/NoSuchKey — accept both shapes.
func isS3NotFound(err error) bool {
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var noSuchKey *s3types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true
		}
	}
	return false
}

var _ data.ChunkStater = (*Backend)(nil)
