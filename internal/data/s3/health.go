package s3

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/danchupin/strata/internal/data"
)

// DataHealth implements data.HealthProbe (US-002 web-ui-storage-status).
//
// US-001 (drain-lifecycle) rewrote the iteration: for every registered
// cluster in Backend.clusters and every distinct bucket referenced by
// Backend.classes, emit one PoolStatus row with HeadBucket-derived
// telemetry against that (cluster, bucket) pair. The Pools table
// reflects actual per-cluster reachability instead of just the class
// env routing config.
//
// One row per (cluster, distinct-bucket) cell. Class is the
// sorted-comma-joined list of classes that map to that bucket; bytes /
// object counts are not exposed by upstream S3 in O(1) so reachability
// remains the only meaningful telemetry. Sort order: ascending by
// (Cluster, Name); empty Cluster sorts last.
func (b *Backend) DataHealth(ctx context.Context) (*data.DataHealthReport, error) {
	if b == nil || len(b.clusters) == 0 {
		return nil, errors.ErrUnsupported
	}
	report := &data.DataHealthReport{Backend: BackendName}

	// Collect distinct buckets, classes mapped per bucket.
	classByBucket := make(map[string][]string)
	for class, spec := range b.classes {
		classByBucket[spec.Bucket] = append(classByBucket[spec.Bucket], class)
	}
	buckets := make([]string, 0, len(classByBucket))
	for b := range classByBucket {
		buckets = append(buckets, b)
	}
	sort.Strings(buckets)

	// Cluster ids, empty sorts last (defensive — S3 cluster ids are
	// always non-empty in practice).
	clusterIDs := make([]string, 0, len(b.clusters))
	for id := range b.clusters {
		clusterIDs = append(clusterIDs, id)
	}
	sort.Slice(clusterIDs, func(i, j int) bool {
		ci, cj := clusterIDs[i], clusterIDs[j]
		if ci == "" {
			return false
		}
		if cj == "" {
			return true
		}
		return ci < cj
	})

	for _, clusterID := range clusterIDs {
		c, err := b.connFor(ctx, clusterID)
		if err != nil {
			for _, bucket := range buckets {
				classes := append([]string(nil), classByBucket[bucket]...)
				sort.Strings(classes)
				report.Pools = append(report.Pools, data.PoolStatus{
					Name:    bucket,
					Class:   strings.Join(classes, ","),
					Cluster: clusterID,
					State:   "error",
				})
			}
			report.Warnings = append(report.Warnings, fmt.Sprintf("cluster %s: connect: %v", clusterID, err))
			continue
		}
		for _, bucket := range buckets {
			classes := append([]string(nil), classByBucket[bucket]...)
			sort.Strings(classes)
			headCtx, cancel := opCtxFor(ctx, c.opTimeout)
			state := "reachable"
			bucketName := bucket
			if _, err := c.client.HeadBucket(headCtx, &awss3.HeadBucketInput{Bucket: &bucketName}); err != nil {
				state = "error"
				report.Warnings = append(report.Warnings, fmt.Sprintf("cluster %s bucket %s: head: %v", clusterID, bucket, err))
			}
			cancel()
			report.Pools = append(report.Pools, data.PoolStatus{
				Name:    bucket,
				Class:   strings.Join(classes, ","),
				Cluster: clusterID,
				State:   state,
			})
		}
	}
	return report, nil
}

var _ data.HealthProbe = (*Backend)(nil)
