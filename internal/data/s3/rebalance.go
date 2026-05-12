// Package s3 — rebalance facade (US-005).
//
// Exposes per-cluster S3Cluster facades that the rebalance.S3Mover
// consumes, plus the BucketOnCluster lookup the mover uses to resolve
// the bucket name on src/tgt clusters for a given storage class.
// Build-tag-free: minio works without librados, the S3 mover plugs in
// to both `ceph`-built and default binaries.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/danchupin/strata/internal/rebalance"
)

// RebalanceClusters returns an SDK-backed S3Cluster facade per
// configured cluster on b. The cmd binary's rebalance-worker build
// (cmd/strata/workers/rebalance_movers*.go) feeds these into a
// rebalance.S3Mover. The returned facades share the Backend's per-
// cluster SDK client + Uploader cache so rebalance reads/writes hit
// the same connection pool the PUT hot path already warms.
func (b *Backend) RebalanceClusters() map[string]rebalance.S3Cluster {
	if b == nil || len(b.clusters) == 0 {
		return nil
	}
	out := make(map[string]rebalance.S3Cluster, len(b.clusters))
	for id := range b.clusters {
		out[id] = &s3ClusterFacade{backend: b, id: id}
	}
	return out
}

// BucketOnCluster is the exported lookup the rebalance.S3Mover uses to
// resolve the bucket name on a given cluster for a storage class. See
// bucketOnCluster for the resolution rules.
func (b *Backend) BucketOnCluster(class, clusterID string) string {
	if b == nil {
		return ""
	}
	return b.bucketOnCluster(class, clusterID)
}

type s3ClusterFacade struct {
	backend *Backend
	id      string
}

func (f *s3ClusterFacade) ID() string { return f.id }

func (f *s3ClusterFacade) Endpoint() string {
	if f == nil || f.backend == nil {
		return ""
	}
	c, ok := f.backend.clusters[f.id]
	if !ok {
		return ""
	}
	return c.spec.Endpoint
}

func (f *s3ClusterFacade) Region() string {
	if f == nil || f.backend == nil {
		return ""
	}
	c, ok := f.backend.clusters[f.id]
	if !ok {
		return ""
	}
	return c.spec.Region
}

func (f *s3ClusterFacade) Get(ctx context.Context, bucket, key string) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	c, err := f.backend.connFor(ctx, f.id)
	if err != nil {
		return nil, 0, err
	}
	out, err := c.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("s3: rebalance get %s/%s: %w", bucket, key, err)
	}
	size := int64(0)
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

func (f *s3ClusterFacade) Put(ctx context.Context, bucket, key string, body io.Reader, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := f.backend.connFor(ctx, f.id)
	if err != nil {
		return err
	}
	in := &awss3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   body,
	}
	c.applyPutSSE(in)
	if _, err := c.uploader.Upload(ctx, in); err != nil {
		return fmt.Errorf("s3: rebalance put %s/%s: %w", bucket, key, err)
	}
	return nil
}

func (f *s3ClusterFacade) Copy(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := f.backend.connFor(ctx, f.id)
	if err != nil {
		return err
	}
	source := srcBucket + "/" + srcKey
	in := &awss3.CopyObjectInput{
		Bucket:     &dstBucket,
		Key:        &dstKey,
		CopySource: &source,
	}
	if _, err := c.client.CopyObject(ctx, in); err != nil {
		var noSuchKey *s3types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			return fmt.Errorf("s3: rebalance copy %s -> %s/%s: %w", source, dstBucket, dstKey, err)
		}
		// MinIO surfaces NoSuchKey as a generic api error with a
		// "NoSuchKey" code prefix; surface verbatim — the mover treats
		// any non-context error as drop-and-continue.
		_ = strings.TrimSpace
		return fmt.Errorf("s3: rebalance copy %s -> %s/%s: %w", source, dstBucket, dstKey, err)
	}
	return nil
}
