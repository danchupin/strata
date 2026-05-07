package s3

import (
	"context"
	"errors"
	"fmt"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/danchupin/strata/internal/data"
)

// DataHealth implements data.HealthProbe (US-002 web-ui-storage-status).
//
// HEAD on the configured backend bucket; reachability is the meaningful
// signal here — bytes/objects per (bucket, class) are not exposed by the
// upstream S3 API in O(1), and a list-and-aggregate would be prohibitive
// for the storage page poller. The bucketstats sampler (US-003) covers
// the bytes/object dimensions Strata-side.
//
// State is "reachable" on 200 OK from HeadBucket, "error" otherwise; the
// underlying SDK error message is folded into Warnings so the operator
// can debug without diving into gateway logs.
func (b *Backend) DataHealth(ctx context.Context) (*data.DataHealthReport, error) {
	if b == nil || b.client == nil {
		return nil, errors.ErrUnsupported
	}
	bucket := b.bucket
	headCtx, cancel := b.opCtx(ctx)
	defer cancel()
	state := "reachable"
	var warnings []string
	if _, err := b.client.HeadBucket(headCtx, &awss3.HeadBucketInput{Bucket: &bucket}); err != nil {
		state = "error"
		warnings = append(warnings, fmt.Sprintf("bucket %s: head: %v", bucket, err))
	}
	return &data.DataHealthReport{
		Backend: BackendName,
		Pools: []data.PoolStatus{{
			Name:        bucket,
			Class:       "*",
			BytesUsed:   0,
			ObjectCount: 0,
			NumReplicas: 0,
			State:       state,
		}},
		Warnings: warnings,
	}, nil
}

var _ data.HealthProbe = (*Backend)(nil)
