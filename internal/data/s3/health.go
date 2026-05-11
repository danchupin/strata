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
// HEAD on each configured backend bucket; reachability is the
// meaningful signal here — bytes/objects per (bucket, class) are not
// exposed by the upstream S3 API in O(1).
//
// One pool row per configured class — Name is the class's bucket,
// Class is the storage-class label. Aggregating per-cluster would hide
// per-class routing on a fan-out gateway.
func (b *Backend) DataHealth(ctx context.Context) (*data.DataHealthReport, error) {
	if b == nil || len(b.clusters) == 0 {
		return nil, errors.ErrUnsupported
	}
	report := &data.DataHealthReport{Backend: BackendName}
	for className, class := range b.classes {
		c, err := b.connFor(ctx, class.Cluster)
		if err != nil {
			report.Pools = append(report.Pools, data.PoolStatus{
				Name:  class.Bucket,
				Class: className,
				State: "error",
			})
			report.Warnings = append(report.Warnings, fmt.Sprintf("cluster %s: connect: %v", class.Cluster, err))
			continue
		}
		bucket := class.Bucket
		headCtx, cancel := opCtxFor(ctx, c.opTimeout)
		state := "reachable"
		if _, err := c.client.HeadBucket(headCtx, &awss3.HeadBucketInput{Bucket: &bucket}); err != nil {
			state = "error"
			report.Warnings = append(report.Warnings, fmt.Sprintf("bucket %s: head: %v", bucket, err))
		}
		cancel()
		report.Pools = append(report.Pools, data.PoolStatus{
			Name:  bucket,
			Class: className,
			State: state,
		})
	}
	return report, nil
}

var _ data.HealthProbe = (*Backend)(nil)
