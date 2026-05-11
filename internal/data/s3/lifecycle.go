package s3

import (
	"context"
	"fmt"
	"strings"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/danchupin/strata/internal/data"
)

// nativeTransitionClasses maps Strata's storage-class string vocabulary to
// the SDK's TransitionStorageClass enum for classes the backend handles
// natively (US-014). Classes outside this map fall back to Strata's own
// lifecycle worker.
var nativeTransitionClasses = map[string]s3types.TransitionStorageClass{
	"STANDARD_IA":         s3types.TransitionStorageClassStandardIa,
	"ONEZONE_IA":          s3types.TransitionStorageClassOnezoneIa,
	"GLACIER_IR":          s3types.TransitionStorageClassGlacierIr,
	"GLACIER":             s3types.TransitionStorageClassGlacier,
	"DEEP_ARCHIVE":        s3types.TransitionStorageClassDeepArchive,
	"INTELLIGENT_TIERING": s3types.TransitionStorageClassIntelligentTiering,
}

// IsNativeTransitionClass reports whether the supplied Strata storage
// class is one the s3 backend can translate into a native backend
// lifecycle transition.
func IsNativeTransitionClass(class string) bool {
	_, ok := nativeTransitionClasses[strings.ToUpper(class)]
	return ok
}

// PutBackendLifecycle translates the supplied Strata-shape rules into a
// backend BucketLifecycleConfiguration and applies it.
func (b *Backend) PutBackendLifecycle(ctx context.Context, bucketPrefix string, rules []data.LifecycleRule) ([]string, error) {
	if bucketPrefix == "" {
		return nil, fmt.Errorf("s3: PutBackendLifecycle: bucketPrefix required")
	}
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return nil, err
	}

	var skipped []string
	var sdkRules []s3types.LifecycleRule
	for _, r := range rules {
		hasTransition := r.TransitionDays > 0 && r.TransitionStorageClass != ""
		nativeTransition, native := nativeTransitionClasses[strings.ToUpper(r.TransitionStorageClass)]
		hasExpiration := r.ExpirationDays > 0
		hasAbort := r.AbortIncompleteUploadDays > 0

		if hasTransition && !native {
			skipped = append(skipped, r.ID)
		}

		if !hasExpiration && !hasAbort && !(hasTransition && native) {
			continue
		}

		filterPrefix := bucketPrefix + r.Prefix
		filter := &s3types.LifecycleRuleFilter{Prefix: &filterPrefix}
		ruleID := r.ID
		sdkRule := s3types.LifecycleRule{
			ID:     &ruleID,
			Status: s3types.ExpirationStatusEnabled,
			Filter: filter,
		}
		if hasTransition && native {
			days := int32(r.TransitionDays)
			sdkRule.Transitions = []s3types.Transition{{
				Days:         &days,
				StorageClass: nativeTransition,
			}}
		}
		if hasExpiration {
			days := int32(r.ExpirationDays)
			sdkRule.Expiration = &s3types.LifecycleExpiration{Days: &days}
		}
		if hasAbort {
			days := int32(r.AbortIncompleteUploadDays)
			sdkRule.AbortIncompleteMultipartUpload = &s3types.AbortIncompleteMultipartUpload{
				DaysAfterInitiation: &days,
			}
		}
		sdkRules = append(sdkRules, sdkRule)
	}

	if len(sdkRules) == 0 {
		opCtx, cancel := opCtxFor(ctx, c.opTimeout)
		defer cancel()
		_, err := c.client.DeleteBucketLifecycle(opCtx, &awss3.DeleteBucketLifecycleInput{Bucket: &bucket})
		if err != nil {
			return skipped, fmt.Errorf("s3: clear backend lifecycle %s: %w", bucket, err)
		}
		return skipped, nil
	}

	opCtx, cancel := opCtxFor(ctx, c.opTimeout)
	defer cancel()
	_, err = c.client.PutBucketLifecycleConfiguration(opCtx, &awss3.PutBucketLifecycleConfigurationInput{
		Bucket:                 &bucket,
		LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: sdkRules},
	})
	if err != nil {
		return skipped, fmt.Errorf("s3: put backend lifecycle %s: %w", bucket, err)
	}
	return skipped, nil
}

// DeleteBackendLifecycle clears the backend bucket's lifecycle
// configuration. Idempotent: NoSuchLifecycleConfiguration is treated as
// success.
func (b *Backend) DeleteBackendLifecycle(ctx context.Context, bucketPrefix string) error {
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return err
	}
	opCtx, cancel := opCtxFor(ctx, c.opTimeout)
	defer cancel()
	_, err = c.client.DeleteBucketLifecycle(opCtx, &awss3.DeleteBucketLifecycleInput{Bucket: &bucket})
	if err != nil {
		s := err.Error()
		if strings.Contains(s, "NoSuchLifecycleConfiguration") {
			return nil
		}
		return fmt.Errorf("s3: delete backend lifecycle %s: %w", bucket, err)
	}
	return nil
}

// Compile-time assertion that *Backend satisfies data.LifecycleBackend.
var _ data.LifecycleBackend = (*Backend)(nil)
